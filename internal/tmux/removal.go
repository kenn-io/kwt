package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strconv"
	"strings"
)

// RemovalSessionCondition is the exact tmux state authorized by a guarded
// worktree removal. Absent authorizes removal only while the named session is
// absent; otherwise every captured identity field must still match.
type RemovalSessionCondition struct {
	SessionName     string `json:"session_name"`
	Absent          bool   `json:"absent,omitempty"`
	ServerPID       string `json:"server_pid,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	SocketDirectory string `json:"socket_directory,omitempty"`
	SocketName      string `json:"socket_name,omitempty"`
}

// RemovalSessionConditionError reports that the live tmux state no longer
// matches the caller's confirmed removal authority.
type RemovalSessionConditionError struct{ Reason string }

func (e *RemovalSessionConditionError) Error() string {
	if e == nil || e.Reason == "" {
		return "tmux session state changed"
	}
	return e.Reason
}

// RemovalSessionLease keeps an exact tmux session quiescent while the caller
// performs its final worktree-removal preflight.
type RemovalSessionLease interface {
	Terminate(context.Context) error
	Resume() error
}

// RemovalSessionGuard validates and quiesces one exact tmux session. The
// lifecycle service invokes it while holding the worktree's creation/removal
// lock, then either terminates the lease or resumes it after a failed final
// preflight.
type RemovalSessionGuard interface {
	Quiesce(context.Context, RemovalSessionCondition) (RemovalSessionLease, error)
}

type removalSessionGuard struct{ command string }

var removalSocketSelectors = []string{"TMUX", "TMUX_TMPDIR"}

// NewRemovalSessionGuard targets the canonical default tmux server unless the
// request carries an explicit socket endpoint. Ambient tmux selectors are
// never removal authority.
func NewRemovalSessionGuard(command string) RemovalSessionGuard {
	return &removalSessionGuard{command: command}
}

func (g *removalSessionGuard) Quiesce(
	ctx context.Context,
	condition RemovalSessionCondition,
) (RemovalSessionLease, error) {
	if err := validateRemovalSessionCondition(condition); err != nil {
		return nil, err
	}
	command := newRemovalTmuxCommand(g.command, condition)
	if !condition.Absent {
		return quiesceMatchingSession(ctx, command, condition)
	}
	const format = "#{pid}|#{session_id}|#{session_created}|#{session_name}"
	output, stderr, err := command.runCommandOutputContextWithStderr(
		ctx, "list-sessions", "-F", format,
	)
	if err != nil {
		if isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return noRemovalSessionLease{}, nil
		}
		return nil, fmt.Errorf(
			"inspect tmux sessions: %w, stderr: %s", err, strings.TrimSpace(stderr),
		)
	}

	var matching []string
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) == 4 && parts[3] == condition.SessionName {
			matching = parts
			break
		}
	}
	if matching == nil {
		return noRemovalSessionLease{}, nil
	}
	return nil, &RemovalSessionConditionError{Reason: "tmux session started after confirmation"}
}

type noRemovalSessionLease struct{}

func (noRemovalSessionLease) Terminate(context.Context) error { return nil }
func (noRemovalSessionLease) Resume() error                   { return nil }

type liveRemovalSessionLease struct {
	command   *TmuxCommand
	condition RemovalSessionCondition
	groups    []int
	serverPID int
}

func (l *liveRemovalSessionLease) Terminate(ctx context.Context) error {
	args := terminateMatchingSessionArgs(l.condition)
	cmd := l.command.newCmd(ctx, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return errors.Join(
			fmt.Errorf("start confirmed tmux termination: %w", err),
			l.Resume(),
		)
	}
	serverErr := resumeRemovalServer(l.serverPID)
	l.serverPID = 0
	if serverErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	groupsErr := resumeRemovalProcessGroups(l.groups)
	l.groups = nil
	return errors.Join(
		classifyTerminateMatchingSession(
			stdout.String(), stderr.String(), waitErr,
		),
		serverErr,
		groupsErr,
	)
}

func (l *liveRemovalSessionLease) Resume() error {
	serverPID := l.serverPID
	l.serverPID = 0
	groups := l.groups
	l.groups = nil
	return errors.Join(
		resumeRemovalServer(serverPID),
		resumeRemovalProcessGroups(groups),
	)
}

func quiesceMatchingSession(
	ctx context.Context,
	command *TmuxCommand,
	condition RemovalSessionCondition,
) (_ RemovalSessionLease, resultErr error) {
	groups := make([]int, 0)
	serverPID, err := strconv.Atoi(condition.ServerPID)
	if err != nil || serverPID <= 1 {
		return nil, &RemovalSessionConditionError{Reason: "tmux session identity is invalid"}
	}
	serverSuspended := false
	defer func() {
		if resultErr != nil {
			var serverErr error
			if serverSuspended {
				serverErr = resumeRemovalServer(serverPID)
			}
			resultErr = errors.Join(
				resultErr, serverErr, resumeRemovalProcessGroups(groups),
			)
		}
	}()
	panePIDs, err := matchingSessionPanePIDs(ctx, command, condition)
	if err != nil {
		return nil, err
	}
	if err := suspendRemovalServer(serverPID); err != nil {
		return nil, err
	}
	// tmux immediately continues a stopped pane leader while its server is
	// running. Hold the server only across this final removability transaction
	// so the target process tree cannot resume and mutate the checkout.
	serverSuspended = true
	seen := make(map[int]struct{})
	for range 8 {
		newGroups, err := suspendRemovalProcessSessions(ctx, panePIDs, seen)
		groups = append(groups, newGroups...)
		if err != nil {
			return nil, err
		}
		if len(newGroups) == 0 {
			serverSuspended = false
			return &liveRemovalSessionLease{
				command: command, condition: condition, groups: groups,
				serverPID: serverPID,
			}, nil
		}
	}
	return nil, fmt.Errorf("quiesce confirmed tmux session: process set did not stabilize")
}

func matchingSessionPanePIDs(
	ctx context.Context,
	command *TmuxCommand,
	condition RemovalSessionCondition,
) ([]int, error) {
	identity := removalSessionIdentityFormat(condition)
	target := condition.SessionID + ":"
	const changed = "kwt-removal-identity-changed"
	output, stderr, err := command.runCommandOutputContextWithStderr(
		ctx,
		"if-shell", "-F", "-t", target, identity,
		"list-panes -s -t "+target+" -F '#{pane_pid}'",
		"display-message -p "+changed,
	)
	if err != nil {
		if isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return nil, &RemovalSessionConditionError{Reason: "tmux session exited after confirmation"}
		}
		return nil, fmt.Errorf(
			"inspect confirmed tmux session: %w, stderr: %s",
			err, strings.TrimSpace(stderr),
		)
	}
	if strings.TrimSpace(output) == changed {
		return nil, &RemovalSessionConditionError{Reason: "tmux session identity changed after confirmation"}
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
		if parseErr != nil || pid <= 1 {
			return nil, fmt.Errorf("inspect confirmed tmux session: invalid pane process")
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("inspect confirmed tmux session: no pane processes")
	}
	return pids, nil
}

func newRemovalTmuxCommand(command string, condition RemovalSessionCondition) *TmuxCommand {
	if condition.SocketName != "" {
		if condition.SocketDirectory != "" {
			return NewTmuxCommandForSocketInTempDirWithStripNames(
				command,
				condition.SocketName,
				condition.SocketDirectory,
				removalSocketSelectors,
			)
		}
		return NewTmuxCommandForSocketWithStripNames(
			command, condition.SocketName, removalSocketSelectors,
		)
	}
	tmuxCommand := NewTmuxCommandWithStripNames(command, removalSocketSelectors)
	tmuxCommand.socketTempDir = condition.SocketDirectory
	return tmuxCommand
}

const removalIdentityChanged = "kwt-removal-identity-changed"

func terminateMatchingSessionArgs(condition RemovalSessionCondition) []string {
	identity := removalSessionIdentityFormat(condition)
	target := condition.SessionID + ":"
	return []string{
		"if-shell", "-F", "-t", target, identity,
		"kill-session -t " + target,
		"display-message -p " + removalIdentityChanged,
	}
}

func classifyTerminateMatchingSession(output, stderr string, err error) error {
	if err != nil {
		if isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return &RemovalSessionConditionError{Reason: "tmux session exited after confirmation"}
		}
		return fmt.Errorf(
			"terminate confirmed tmux session: %w, stderr: %s",
			err,
			strings.TrimSpace(stderr),
		)
	}
	if strings.TrimSpace(output) == removalIdentityChanged {
		return &RemovalSessionConditionError{Reason: "tmux session identity changed after confirmation"}
	}
	if strings.TrimSpace(output) != "" {
		return fmt.Errorf("terminate confirmed tmux session: unexpected tmux output")
	}
	return nil
}

func removalSessionIdentityFormat(condition RemovalSessionCondition) string {
	return fmt.Sprintf(
		"#{&&:#{==:#{pid},%s},#{&&:#{==:#{session_id},%s},#{&&:#{==:#{session_created},%s},#{==:#{session_name},%s}}}}",
		condition.ServerPID,
		condition.SessionID,
		condition.CreatedAt,
		escapeTmuxFormatLiteral(condition.SessionName),
	)
}

func escapeTmuxFormatLiteral(value string) string {
	return strings.NewReplacer(
		"#", "##",
		",", "#,",
		"}", "#}",
	).Replace(value)
}

func validateRemovalSessionCondition(condition RemovalSessionCondition) error {
	if condition.SessionName == "" || strings.ContainsAny(condition.SessionName, "\t\r\n") {
		return &RemovalSessionConditionError{Reason: "tmux session condition is invalid"}
	}
	if condition.SocketDirectory != "" && !pathpkg.IsAbs(condition.SocketDirectory) {
		return &RemovalSessionConditionError{Reason: "tmux socket directory is invalid"}
	}
	if condition.SocketName != "" && !validRemovalSocketName(condition.SocketName) {
		return &RemovalSessionConditionError{Reason: "tmux socket name is invalid"}
	}
	if condition.Absent {
		if condition.ServerPID != "" || condition.SessionID != "" || condition.CreatedAt != "" {
			return &RemovalSessionConditionError{Reason: "absent tmux condition includes live identity"}
		}
		return nil
	}
	serverPID, err := strconv.ParseUint(condition.ServerPID, 10, 32)
	if err != nil || strconv.FormatUint(serverPID, 10) != condition.ServerPID ||
		!strings.HasPrefix(condition.SessionID, "$") ||
		len(condition.SessionID) < 2 {
		return &RemovalSessionConditionError{Reason: "tmux session identity is invalid"}
	}
	sessionNumber := strings.TrimPrefix(condition.SessionID, "$")
	parsedSession, err := strconv.ParseUint(sessionNumber, 10, 64)
	if err != nil || strconv.FormatUint(parsedSession, 10) != sessionNumber {
		return &RemovalSessionConditionError{Reason: "tmux session identity is invalid"}
	}
	createdAt, err := strconv.ParseUint(condition.CreatedAt, 10, 64)
	if err != nil || strconv.FormatUint(createdAt, 10) != condition.CreatedAt {
		return &RemovalSessionConditionError{Reason: "tmux session identity is invalid"}
	}
	return nil
}

func validRemovalSocketName(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}
