package tmux

import (
	"context"
	"encoding/json"
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

// RemovalSessionGuard validates that one exact tmux session remains absent on
// both the KWT and default-server endpoints, or on its protected endpoint and
// both shared endpoints. Live-session removal is rejected because tmux has no
// atomic primitive that can both freeze a shared server's topology and hand
// control back to KWT.
type RemovalSessionGuard interface {
	Quiesce(context.Context, RemovalSessionCondition) (RemovalSessionLease, error)
}

type removalSessionGuard struct {
	command          string
	inspect          func(context.Context, *TmuxCommand) (string, string, error)
	inspectProtected func(context.Context, *TmuxCommand, string) (ProtectedSessionState, error)
	serverCommands   func(RemovalSessionCondition) serverCommands
}

var removalSocketSelectors = []string{"TMUX", "TMUX_TMPDIR"}

// NewRemovalSessionGuard probes both the KWT server and the temporary
// default-server adoption endpoint. Protected topology is derived by the
// lifecycle layer; ambient tmux selectors are never removal authority.
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
	idsOutput, stderr, err := command.runCommandOutputContextWithStderr(
		ctx,
		"list-sessions",
		"-F",
		"#{session_id}",
	)
	if err != nil {
		return "", stderr, err
	}
	ids := strings.Split(strings.TrimSuffix(idsOutput, "\n"), "\n")
	rows := make([]removalSessionRow, 0, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSuffix(rawID, "\r")
		if !validRemovalSessionID(id) {
			return "", "", fmt.Errorf("tmux returned an invalid session id")
		}
		target := id
		name, fieldStderr, fieldErr := command.runCommandOutputContextWithStderr(
			ctx,
			"display-message",
			"-p",
			"-t",
			target,
			"#{session_name}",
		)
		if fieldErr != nil {
			return "", fieldStderr, fieldErr
		}
		identity, fieldStderr, fieldErr := command.runCommandOutputContextWithStderr(
			ctx,
			"display-message",
			"-p",
			"-t",
			target,
			"#{@kwt-workspace-id}",
		)
		if fieldErr != nil {
			return "", fieldStderr, fieldErr
		}
		generation, fieldStderr, fieldErr := command.runCommandOutputContextWithStderr(
			ctx,
			"display-message",
			"-p",
			"-t",
			target,
			"#{@kwt-workspace-generation}",
		)
		if fieldErr != nil {
			return "", fieldStderr, fieldErr
		}
		rows = append(rows, removalSessionRow{
			SessionName:         trimTmuxField(name),
			WorkspaceIdentity:   trimTmuxField(identity),
			WorkspaceGeneration: trimTmuxField(generation),
		})
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return "", "", fmt.Errorf("encode tmux session inventory: %w", err)
	}
	return string(encoded), "", nil
}

func trimTmuxField(value string) string {
	value = strings.TrimSuffix(value, "\n")
	return strings.TrimSuffix(value, "\r")
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
		sharedCondition := condition
		sharedCondition.SocketName = ""
		sharedCondition.ProtectedSocketTopology = false
		if err := g.validateSharedServerAbsence(ctx, sharedCondition); err != nil {
			return nil, err
		}
		return g.quiesceProtected(ctx, condition)
	}
	if err := g.validateSharedServerAbsence(ctx, condition); err != nil {
		return nil, err
	}
	return noRemovalSessionLease{}, nil
}

func (g *removalSessionGuard) validateSharedServerAbsence(
	ctx context.Context,
	condition RemovalSessionCondition,
) error {
	servers := g.removalServerCommands(condition)
	commands := []*TmuxCommand{servers.kwtServer()}
	// compat(kag1): default-server adoption
	commands = append(commands, servers.defaultServer())
	for _, command := range commands {
		if err := g.validateSharedEndpointAbsence(ctx, condition, command); err != nil {
			return err
		}
	}
	return nil
}

func (g *removalSessionGuard) removalServerCommands(
	condition RemovalSessionCondition,
) serverCommands {
	if g.serverCommands != nil {
		return g.serverCommands(condition)
	}
	return newServerCommands(WorkspaceSessionsOptions{
		Command:                 g.command,
		DefaultServerTempDir:    condition.SocketDirectory,
		DefaultServerTempDirSet: true,
		StripNames:              removalStripNames(condition),
	})
}

func (g *removalSessionGuard) validateSharedEndpointAbsence(
	ctx context.Context,
	condition RemovalSessionCondition,
	command *TmuxCommand,
) error {
	inspect := g.inspect
	if inspect == nil {
		inspect = inspectRemovalSessions
	}
	output, stderr, err := inspect(ctx, command)
	if contextErr := ctx.Err(); contextErr != nil {
		return errors.Join(contextErr, err)
	}
	if err != nil {
		if isExplicitlyAbsentRemovalServerDiagnostic(stderr) {
			return nil
		}
		return fmt.Errorf(
			"inspect tmux sessions: %w, stderr: %s", err, strings.TrimSpace(stderr),
		)
	}

	var rows []removalSessionRow
	if err := json.Unmarshal([]byte(output), &rows); err != nil {
		return fmt.Errorf("inspect tmux sessions: malformed session inventory")
	}
	if len(rows) == 0 {
		return fmt.Errorf("inspect tmux sessions: empty session inventory")
	}
	workspaceIdentity := ""
	if condition.WorkspacePath != "" {
		workspaceIdentity = workspacePathIdentity(condition.WorkspacePath)
	}
	for _, row := range rows {
		if err := validateRemovalSessionRow(row); err != nil {
			return fmt.Errorf("inspect tmux sessions: malformed session inventory")
		}
		if row.SessionName == condition.SessionName {
			return &RemovalSessionConditionError{
				Reason: "tmux session started after confirmation",
			}
		}
		if workspaceIdentity != "" &&
			(row.WorkspaceIdentity == workspaceIdentity ||
				MatchesLegacyWorkspaceSessionPath(row.SessionName, condition.WorkspacePath)) {
			return &RemovalSessionConditionError{
				Reason: "tmux session for worktree remains live",
			}
		}
		if condition.WorkspaceGeneration != "" &&
			row.WorkspaceGeneration == condition.WorkspaceGeneration {
			return &RemovalSessionConditionError{
				Reason: "tmux session for worktree remains live",
			}
		}
		if row.WorkspaceGeneration == "" && row.WorkspaceIdentity == "" &&
			IsKWTWorktreeSessionName(row.SessionName) {
			return &RemovalSessionConditionError{
				Reason: "legacy KWT session ownership is indeterminate",
			}
		}
	}
	return nil
}

func isExplicitlyAbsentRemovalServerDiagnostic(stderr string) bool {
	diagnostic := strings.TrimSpace(stderr)
	if strings.HasPrefix(diagnostic, "no server running on ") {
		return true
	}
	return strings.HasPrefix(diagnostic, "error connecting to ") &&
		strings.HasSuffix(diagnostic, "(No such file or directory)")
}

type removalSessionRow struct {
	SessionName         string `json:"session_name"`
	WorkspaceIdentity   string `json:"workspace_identity,omitempty"`
	WorkspaceGeneration string `json:"workspace_generation,omitempty"`
}

func validateRemovalSessionRow(row removalSessionRow) error {
	if row.SessionName == "" ||
		(row.WorkspaceIdentity != "" && !validLowerHex(row.WorkspaceIdentity, 64)) ||
		(row.WorkspaceGeneration != "" && !validLowerHex(row.WorkspaceGeneration, 32)) {
		return errors.New("invalid tmux session inventory")
	}
	return nil
}

func validRemovalSessionID(value string) bool {
	number, ok := strings.CutPrefix(value, "$")
	if !ok || number == "" {
		return false
	}
	parsed, err := strconv.ParseUint(number, 10, 64)
	return err == nil && strconv.FormatUint(parsed, 10) == number
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
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
