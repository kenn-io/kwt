package tmux

import (
	"context"
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

// RemovalSessionGuard validates and, when authorized, terminates one exact
// tmux session. The lifecycle service invokes it while holding the worktree's
// creation/removal lock.
type RemovalSessionGuard interface {
	ValidateAndTerminate(context.Context, RemovalSessionCondition) error
}

type removalSessionGuard struct{ command string }

var removalSocketSelectors = []string{"TMUX", "TMUX_TMPDIR"}

// NewRemovalSessionGuard targets the canonical default tmux server unless the
// request carries an explicit socket endpoint. Ambient tmux selectors are
// never removal authority.
func NewRemovalSessionGuard(command string) RemovalSessionGuard {
	return &removalSessionGuard{command: command}
}

func (g *removalSessionGuard) ValidateAndTerminate(
	ctx context.Context,
	condition RemovalSessionCondition,
) error {
	if err := validateRemovalSessionCondition(condition); err != nil {
		return err
	}
	command := newRemovalTmuxCommand(g.command, condition)
	if !condition.Absent {
		return terminateMatchingSession(ctx, command, condition)
	}
	const format = "#{pid}|#{session_id}|#{session_created}|#{session_name}"
	output, stderr, err := command.runCommandOutputContextWithStderr(
		ctx, "list-sessions", "-F", format,
	)
	if err != nil {
		if isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return nil
		}
		return fmt.Errorf("inspect tmux sessions: %w, stderr: %s", err, strings.TrimSpace(stderr))
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
		return nil
	}
	return &RemovalSessionConditionError{Reason: "tmux session started after confirmation"}
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

func terminateMatchingSession(
	ctx context.Context,
	command *TmuxCommand,
	condition RemovalSessionCondition,
) error {
	identity := fmt.Sprintf(
		"#{&&:#{==:#{pid},%s},#{&&:#{==:#{session_id},%s},#{&&:#{==:#{session_created},%s},#{==:#{session_name},%s}}}}",
		condition.ServerPID,
		condition.SessionID,
		condition.CreatedAt,
		escapeTmuxFormatLiteral(condition.SessionName),
	)
	target := condition.SessionID + ":"
	output, stderr, err := command.runCommandOutputContextWithStderr(
		ctx,
		"if-shell", "-F", "-t", target, identity,
		"kill-session -t "+target,
		"display-message -p "+removalIdentityChanged,
	)
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
