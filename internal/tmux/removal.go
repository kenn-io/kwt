package tmux

import (
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
	// WorkspacePath is derived from the generation-locked removal target by
	// the lifecycle service. It is never accepted as caller-supplied authority.
	WorkspacePath string `json:"-"`
	// WorkspaceGeneration is the durable identity of the generation-locked
	// removal target. Unlike WorkspacePath, it survives git worktree move.
	WorkspaceGeneration string `json:"-"`
	// ProtectedSocketTopology is derived from trusted worktree provenance by
	// the lifecycle service. It is never accepted as caller-supplied authority.
	ProtectedSocketTopology bool `json:"-"`
	// ProtectedNames is derived from fresh configuration by the lifecycle
	// service. Removal probes can start tmux servers, so these credentials must
	// not be inherited by any socket topology.
	ProtectedNames []string `json:"-"`
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

// RemovalSessionLease holds a validated absence condition while the caller
// performs its final worktree-removal preflight.
type RemovalSessionLease interface {
	Terminate(context.Context) error
	Resume() error
}

// RemovalSessionGuard validates that one exact tmux session remains absent.
// Live-session removal is rejected because tmux has no atomic primitive that
// can both freeze a shared server's topology and hand control back to KWT.
type RemovalSessionGuard interface {
	Quiesce(context.Context, RemovalSessionCondition) (RemovalSessionLease, error)
}

type removalSessionGuard struct {
	command          string
	inspect          func(context.Context, *TmuxCommand) (string, string, error)
	inspectProtected func(context.Context, *TmuxCommand, string) (ProtectedSessionState, error)
}

var removalSocketSelectors = []string{"TMUX", "TMUX_TMPDIR"}

// NewRemovalSessionGuard targets the canonical default tmux server unless the
// request carries an explicit socket endpoint. Ambient tmux selectors are
// never removal authority.
func NewRemovalSessionGuard(command string) RemovalSessionGuard {
	return &removalSessionGuard{
		command:          command,
		inspect:          inspectRemovalSessions,
		inspectProtected: probeProtectedSessionCommand,
	}
}

func inspectRemovalSessions(
	ctx context.Context,
	command *TmuxCommand,
) (string, string, error) {
	const format = "#{pid}|#{session_id}|#{session_created}|#{session_name}|#{@kwt-workspace-id}|#{@kwt-workspace-generation}"
	return command.runCommandOutputContextWithStderr(ctx, "list-sessions", "-F", format)
}

func (g *removalSessionGuard) Quiesce(
	ctx context.Context,
	condition RemovalSessionCondition,
) (RemovalSessionLease, error) {
	if err := validateRemovalSessionCondition(condition); err != nil {
		return nil, err
	}
	if !condition.Absent {
		return nil, &RemovalSessionConditionError{
			Reason: "stop the session before guarded worktree removal",
		}
	}
	if condition.ProtectedSocketTopology {
		return g.quiesceProtected(ctx, condition)
	}
	command := newRemovalTmuxCommand(g.command, condition)
	output, stderr, err := g.inspect(ctx, command)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, errors.Join(contextErr, err)
	}
	if err != nil {
		if isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return noRemovalSessionLease{}, nil
		}
		return nil, fmt.Errorf(
			"inspect tmux sessions: %w, stderr: %s", err, strings.TrimSpace(stderr),
		)
	}

	rows := strings.TrimSuffix(output, "\n")
	if rows == "" {
		return nil, fmt.Errorf("inspect tmux sessions: empty session inventory")
	}
	workspaceIdentity := ""
	if condition.WorkspacePath != "" {
		workspaceIdentity = workspacePathIdentity(condition.WorkspacePath)
	}
	for _, line := range strings.Split(rows, "\n") {
		row, parseErr := parseRemovalSessionRow(strings.TrimSuffix(line, "\r"))
		if parseErr != nil {
			return nil, fmt.Errorf("inspect tmux sessions: malformed session inventory")
		}
		if row.sessionName == condition.SessionName {
			return nil, &RemovalSessionConditionError{
				Reason: "tmux session started after confirmation",
			}
		}
		if workspaceIdentity != "" &&
			(row.workspaceIdentity == workspaceIdentity ||
				MatchesLegacyWorkspaceSessionPath(row.sessionName, condition.WorkspacePath)) {
			return nil, &RemovalSessionConditionError{
				Reason: "tmux session for worktree remains live",
			}
		}
		if condition.WorkspaceGeneration != "" &&
			row.workspaceGeneration == condition.WorkspaceGeneration {
			return nil, &RemovalSessionConditionError{
				Reason: "tmux session for worktree remains live",
			}
		}
		if row.workspaceGeneration == "" && row.workspaceIdentity == "" &&
			IsKWTWorktreeSessionName(row.sessionName) {
			return nil, &RemovalSessionConditionError{
				Reason: "legacy KWT session ownership is indeterminate",
			}
		}
	}
	return noRemovalSessionLease{}, nil
}

type removalSessionRow struct {
	sessionName         string
	workspaceIdentity   string
	workspaceGeneration string
}

func parseRemovalSessionRow(line string) (removalSessionRow, error) {
	rest := line
	for range 3 {
		field, remaining, ok := strings.Cut(rest, "|")
		if !ok || field == "" {
			return removalSessionRow{}, errors.New("missing fixed field")
		}
		rest = remaining
	}
	lastDelimiter := strings.LastIndexByte(rest, '|')
	if lastDelimiter < 0 {
		return removalSessionRow{}, errors.New("missing session field")
	}
	workspaceGeneration := rest[lastDelimiter+1:]
	rest = rest[:lastDelimiter]
	identityDelimiter := strings.LastIndexByte(rest, '|')
	if identityDelimiter < 0 || identityDelimiter == 0 {
		return removalSessionRow{}, errors.New("missing session field")
	}
	return removalSessionRow{
		sessionName:         rest[:identityDelimiter],
		workspaceIdentity:   rest[identityDelimiter+1:],
		workspaceGeneration: workspaceGeneration,
	}, nil
}

func (g *removalSessionGuard) quiesceProtected(
	ctx context.Context,
	condition RemovalSessionCondition,
) (RemovalSessionLease, error) {
	inspect := g.inspectProtected
	if inspect == nil {
		inspect = probeProtectedSessionCommand
	}
	stripNames := removalStripNames(condition)
	canonical := NewTmuxCommandForSocketWithStripNames(
		g.command,
		condition.SocketName,
		stripNames,
	)
	state, err := inspect(ctx, canonical, condition.SessionName)
	if state == ProtectedSessionAbsent && err == nil && condition.SocketDirectory != "" {
		legacy := NewTmuxCommandForSocketInTempDirWithStripNames(
			g.command,
			condition.SocketName,
			condition.SocketDirectory,
			stripNames,
		)
		state, err = inspect(ctx, legacy, condition.SessionName)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect protected tmux session: %w", err)
	}
	switch state {
	case ProtectedSessionAbsent:
		return noRemovalSessionLease{}, nil
	case ProtectedSessionLive:
		return nil, &RemovalSessionConditionError{
			Reason: "protected tmux session started after confirmation",
		}
	default:
		return nil, &RemovalSessionConditionError{
			Reason: "protected tmux session state is indeterminate",
		}
	}
}

type noRemovalSessionLease struct{}

func (noRemovalSessionLease) Terminate(context.Context) error { return nil }
func (noRemovalSessionLease) Resume() error                   { return nil }

func newRemovalTmuxCommand(command string, condition RemovalSessionCondition) *TmuxCommand {
	stripNames := removalStripNames(condition)
	if condition.SocketName != "" {
		if condition.SocketDirectory != "" {
			return NewTmuxCommandForSocketInTempDirWithStripNames(
				command,
				condition.SocketName,
				condition.SocketDirectory,
				stripNames,
			)
		}
		return NewTmuxCommandForSocketWithStripNames(
			command, condition.SocketName, stripNames,
		)
	}
	tmuxCommand := NewTmuxCommandWithStripNames(command, stripNames)
	tmuxCommand.socketTempDir = condition.SocketDirectory
	return tmuxCommand
}

func removalStripNames(condition RemovalSessionCondition) []string {
	names := make([]string, 0, len(removalSocketSelectors)+len(condition.ProtectedNames))
	names = append(names, removalSocketSelectors...)
	return append(names, condition.ProtectedNames...)
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
