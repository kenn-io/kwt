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
	command string
	inspect func(context.Context, *TmuxCommand) (string, string, error)
}

var removalSocketSelectors = []string{"TMUX", "TMUX_TMPDIR"}

// NewRemovalSessionGuard targets the canonical default tmux server unless the
// request carries an explicit socket endpoint. Ambient tmux selectors are
// never removal authority.
func NewRemovalSessionGuard(command string) RemovalSessionGuard {
	return &removalSessionGuard{command: command, inspect: inspectRemovalSessions}
}

func inspectRemovalSessions(
	ctx context.Context,
	command *TmuxCommand,
) (string, string, error) {
	const format = "#{pid}|#{session_id}|#{session_created}|#{session_name}"
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
