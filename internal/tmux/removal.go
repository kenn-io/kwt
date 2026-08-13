package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"regexp"
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

var (
	removalWindowSnapshotPattern = regexp.MustCompile(`@[0-9]+=([01])\[([^]]*)\]`)
	removalPaneSnapshotPattern   = regexp.MustCompile(`%[0-9]+=([0-9]+)`)
)

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
	command         *TmuxCommand
	condition       RemovalSessionCondition
	frozenCondition RemovalSessionCondition
	groups          []int
	serverPID       int
	freezeWait      <-chan error
}

func (l *liveRemovalSessionLease) Terminate(ctx context.Context) error {
	if l.condition != l.frozenCondition {
		return errors.Join(
			&RemovalSessionConditionError{
				Reason: "tmux session identity changed after confirmation",
			},
			l.Resume(),
		)
	}
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
	groups := l.groups
	if err := terminateRemovalProcessGroups(ctx, groups); err != nil {
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		return errors.Join(err, waitErr, l.Resume())
	}
	l.groups = nil
	serverErr := resumeRemovalServer(l.serverPID)
	if serverErr == nil {
		l.serverPID = 0
	}
	if serverErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	freezeErr := waitRemovalFreezeClient(l.freezeWait, serverErr)
	if freezeErr == nil && serverErr == nil {
		l.freezeWait = nil
	}
	terminationErr := classifyTerminateMatchingSession(
		stdout.String(), stderr.String(), waitErr,
	)
	if terminationErr != nil &&
		(strings.TrimSpace(stdout.String()) == removalIdentityAbsent ||
			isExplicitlyAbsentTmuxDiagnostic(stderr.String())) {
		terminationErr = confirmRemovalSessionAbsent(ctx, l.command, l.condition)
	}
	return errors.Join(
		terminationErr,
		serverErr,
		freezeErr,
	)
}

func (l *liveRemovalSessionLease) Resume() error {
	serverPID := l.serverPID
	l.serverPID = 0
	groups := l.groups
	l.groups = nil
	serverErr := resumeRemovalServer(serverPID)
	if serverErr != nil {
		l.serverPID = serverPID
	}
	freezeErr := waitRemovalFreezeClient(l.freezeWait, serverErr)
	if freezeErr == nil && serverErr == nil {
		l.freezeWait = nil
	}
	return errors.Join(serverErr, freezeErr, resumeRemovalProcessGroups(groups))
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
	var freezeWait <-chan error
	defer func() {
		if resultErr != nil {
			var serverErr error
			if serverSuspended {
				serverErr = resumeRemovalServer(serverPID)
			}
			resultErr = errors.Join(
				resultErr, serverErr,
				waitRemovalFreezeClient(freezeWait, serverErr),
				resumeRemovalProcessGroups(groups),
			)
		}
	}()
	panePIDs, paneSnapshot, err := matchingSessionPaneSnapshot(
		ctx, command, condition,
	)
	if err != nil {
		return nil, err
	}
	pendingFreeze, err := freezeMatchingSession(
		ctx, command, condition, serverPID, paneSnapshot,
	)
	if err != nil {
		return nil, err
	}
	freezeWait = pendingFreeze
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
				command: command, condition: condition, frozenCondition: condition,
				groups:    groups,
				serverPID: serverPID, freezeWait: freezeWait,
			}, nil
		}
	}
	return nil, fmt.Errorf("quiesce confirmed tmux session: process set did not stabilize")
}

func matchingSessionPaneSnapshot(
	ctx context.Context,
	command *TmuxCommand,
	condition RemovalSessionCondition,
) ([]int, string, error) {
	const snapshotFormat = "#{W:#{window_id}=#{window_linked}[#{P:#{pane_id}=#{pane_pid}}]}"
	target := condition.SessionID + ":"
	output, stderr, err := command.runCommandOutputContextWithStderr(
		ctx, "display-message", "-p", "-t", target,
		"#{pid}\t#{session_id}\t#{session_created}\t#{session_name}\t"+snapshotFormat,
	)
	if err != nil {
		if isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return nil, "", &RemovalSessionConditionError{
				Reason: "tmux session exited after confirmation",
			}
		}
		return nil, "", fmt.Errorf(
			"inspect confirmed tmux session: %w, stderr: %s",
			err, strings.TrimSpace(stderr),
		)
	}
	parts := strings.SplitN(strings.TrimSpace(output), "\t", 5)
	if len(parts) != 5 {
		return nil, "", fmt.Errorf("inspect confirmed tmux session: malformed identity")
	}
	if parts[0] != condition.ServerPID || parts[1] != condition.SessionID ||
		parts[2] != condition.CreatedAt || parts[3] != condition.SessionName {
		return nil, "", &RemovalSessionConditionError{
			Reason: "tmux session identity changed after confirmation",
		}
	}
	snapshot := parts[4]
	pids, err := parseRemovalPaneSnapshot(snapshot)
	if err != nil {
		return nil, "", err
	}
	return pids, snapshot, nil
}

func freezeMatchingSession(
	ctx context.Context,
	command *TmuxCommand,
	condition RemovalSessionCondition,
	serverPID int,
	paneSnapshot string,
) (<-chan error, error) {
	args := freezeMatchingSessionArgs(condition, paneSnapshot)
	cmd := command.newCmd(ctx, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tmux removal freeze: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	freezeContext, cancelFreeze := context.WithCancel(ctx)
	defer cancelFreeze()
	frozen := make(chan error, 1)
	go func() { frozen <- waitRemovalServerSuspended(freezeContext, serverPID) }()
	select {
	case waitErr := <-wait:
		if strings.TrimSpace(stdout.String()) == removalIdentityChanged {
			return nil, &RemovalSessionConditionError{
				Reason: "tmux session identity changed after confirmation",
			}
		}
		if waitErr != nil && isExplicitlyAbsentTmuxDiagnostic(stderr.String()) {
			return nil, &RemovalSessionConditionError{
				Reason: "tmux session exited after confirmation",
			}
		}
		return nil, fmt.Errorf(
			"freeze confirmed tmux session: %w, stderr: %s",
			waitErr, strings.TrimSpace(stderr.String()),
		)
	case freezeErr := <-frozen:
		if freezeErr != nil {
			return nil, errors.Join(freezeErr, stopRemovalFreezeClient(wait, serverPID))
		}
		return wait, nil
	}
}

func parseRemovalPaneSnapshot(output string) ([]int, error) {
	var pids []int
	position := 0
	for _, match := range removalWindowSnapshotPattern.FindAllStringSubmatchIndex(output, -1) {
		if match[0] != position {
			return nil, fmt.Errorf("inspect confirmed tmux session: invalid window record")
		}
		position = match[1]
		switch output[match[2]:match[3]] {
		case "0":
		case "1":
			return nil, &RemovalSessionConditionError{
				Reason: "tmux session has a window shared with another session",
			}
		default:
			return nil, fmt.Errorf("inspect confirmed tmux session: invalid window link state")
		}
		panes := output[match[4]:match[5]]
		panePosition := 0
		for _, paneMatch := range removalPaneSnapshotPattern.FindAllStringSubmatchIndex(panes, -1) {
			if paneMatch[0] != panePosition {
				return nil, fmt.Errorf("inspect confirmed tmux session: invalid pane record")
			}
			panePosition = paneMatch[1]
			value := panes[paneMatch[2]:paneMatch[3]]
			pid, parseErr := strconv.Atoi(value)
			if parseErr != nil || pid <= 1 {
				return nil, fmt.Errorf("inspect confirmed tmux session: invalid pane process")
			}
			pids = append(pids, pid)
		}
		if panePosition != len(panes) {
			return nil, fmt.Errorf("inspect confirmed tmux session: invalid pane record")
		}
	}
	if position != len(output) {
		return nil, fmt.Errorf("inspect confirmed tmux session: invalid window record")
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("inspect confirmed tmux session: no pane processes")
	}
	return pids, nil
}

func freezeMatchingSessionArgs(
	condition RemovalSessionCondition,
	paneSnapshot string,
) []string {
	shell := "/bin/kill -STOP " + condition.ServerPID
	target := condition.SessionID + ":"
	conditionFormat := fmt.Sprintf(
		"#{&&:%s,#{==:#{W:#{window_id}=#{window_linked}[#{P:#{pane_id}=#{pane_pid}}]},%s}}",
		removalSessionIdentityFormat(condition),
		escapeTmuxFormatLiteral(paneSnapshot),
	)
	return []string{
		"if-shell", "-F", "-t", target, conditionFormat,
		"run-shell -t " + target + " " + tmuxDoubleQuote(shell),
		"display-message -p " + removalIdentityChanged,
	}
}

func tmuxDoubleQuote(value string) string {
	return `"` + strings.NewReplacer(
		`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`",
	).Replace(value) + `"`
}

func stopRemovalFreezeClient(wait <-chan error, serverPID int) error {
	serverErr := resumeRemovalServer(serverPID)
	return errors.Join(serverErr, waitRemovalFreezeClient(wait, serverErr))
}

func waitRemovalFreezeClient(wait <-chan error, resumeErr error) error {
	if wait == nil || resumeErr != nil {
		return nil
	}
	return <-wait
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
const removalIdentityAbsent = "0"

func terminateMatchingSessionArgs(condition RemovalSessionCondition) []string {
	identity := removalSessionIdentityFormat(condition)
	target := condition.SessionID + ":"
	return []string{
		"if-shell", "-F", "-t", target, identity,
		"kill-session -t " + target,
		"display-message -p '#{m:*" + condition.SessionID +
			"=*,#{S:#{session_id}=}}'",
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
	switch strings.TrimSpace(output) {
	case "1":
		return &RemovalSessionConditionError{Reason: "tmux session identity changed after confirmation"}
	case removalIdentityAbsent:
		return &RemovalSessionConditionError{Reason: "tmux session exited after confirmation"}
	}
	if strings.TrimSpace(output) != "" {
		return fmt.Errorf("terminate confirmed tmux session: unexpected tmux output")
	}
	return nil
}

func confirmRemovalSessionAbsent(
	ctx context.Context,
	command *TmuxCommand,
	condition RemovalSessionCondition,
) error {
	const format = "#{pid}|#{session_id}|#{session_created}|#{session_name}"
	output, stderr, err := command.runCommandOutputContextWithStderr(
		ctx, "list-sessions", "-F", format,
	)
	if err != nil {
		if isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return nil
		}
		return fmt.Errorf(
			"confirm tmux session termination: %w, stderr: %s",
			err, strings.TrimSpace(stderr),
		)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			return fmt.Errorf("confirm tmux session termination: malformed session identity")
		}
		if parts[1] == condition.SessionID || parts[3] == condition.SessionName {
			return &RemovalSessionConditionError{
				Reason: "tmux session identity changed during termination",
			}
		}
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
