package tmux

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kwt/pkg/models"
)

// workspaceTmux is the minimal tmux surface the workspace runner needs.
// *TmuxCommand satisfies it; tests use a mock.
type workspaceTmux interface {
	HasSession(session string) bool
	RunCommandContext(ctx context.Context, args ...string) error
	RunCommandOutputContext(ctx context.Context, args ...string) (string, error)
	SwitchClient(target string) error
	AttachSession(session string) error
	AttachSessionWithoutEnvironment(context.Context, string) error
	KillSession(session string) error
	GlobalEnvironment() (string, error)
	SessionEnvironment(session string) (string, error)
	sessionOption(session, option string) (string, error)
	globalOption(option string) (string, error)
}

// ProtectedWorkspaceSocketName returns the deterministic tmux socket name for
// one protected workspace. The path participates in the identity so equal
// session names from separate repositories cannot share a server.
func ProtectedWorkspaceSocketName(session, worktreeDir string) string {
	sum := sha256.Sum256([]byte(session + "\x00" + worktreeDir))
	return fmt.Sprintf("kwt-pr-%x", sum[:8])
}

var _ workspaceTmux = (*TmuxCommand)(nil)

// SessionSafetyError is safe to return through the machine-readable PR
// contract because it names only the rejected tmux state, never its value.
type SessionSafetyError struct {
	Reason string
}

func (e *SessionSafetyError) Error() string {
	if e == nil {
		return ""
	}
	return e.Reason
}

// WorkspaceRunner creates or reuses a tmux workspace session and attaches to it.
type WorkspaceRunner struct {
	tmux            workspaceTmux
	extraStripNames []string
	credentialNames []string
	protected       bool
}

// NewProtectedWorkspaceRunner requires credential-clean tmux state and a
// matching kwt workspace marker before it reuses an existing session.
func NewProtectedWorkspaceRunner(
	t workspaceTmux,
	credentialNames []string,
) *WorkspaceRunner {
	names := cleanNames(credentialNames)
	return &WorkspaceRunner{
		tmux:            t,
		extraStripNames: names,
		credentialNames: names,
		protected:       true,
	}
}

// NewWorkspaceRunner returns a runner backed by the given tmux surface while
// ensuring caller-owned credential variables cannot reach workspace panes.
func NewWorkspaceRunner(
	t workspaceTmux,
	credentialNames []string,
) *WorkspaceRunner {
	return &WorkspaceRunner{
		tmux:            t,
		extraStripNames: cleanNames(credentialNames),
	}
}

func cleanNames(names []string) []string {
	cleaned := make([]string, 0, len(names))
	seen := make(map[string]bool)
	for _, name := range names {
		name = strings.TrimSpace(name)
		folded := strings.ToLower(name)
		if name == "" || seen[folded] {
			continue
		}
		seen[folded] = true
		cleaned = append(cleaned, name)
	}
	return cleaned
}

// EnsureAndAttach attaches to the workspace session, creating it first if it
// does not already exist. Creation spawns the first pane, applies the session
// bootstrap, spawns and arranges the remaining panes, then sends each pane's
// command to its captured ID. When the session already exists (e.g. an
// external tool ran new-session first), it re-applies the safe bootstrap
// subset — default-command plus the remove-markers — which is idempotent and
// non-destructive on any session, so a session created bare still gets
// consistent behavior for future windows. insideTmux selects switch-client
// (already in tmux) over attach-session.
func (r *WorkspaceRunner) EnsureAndAttach(
	ctx context.Context, session, worktreeDir string, layout models.Layout, insideTmux bool,
) error {
	if err := r.Ensure(ctx, session, worktreeDir, layout); err != nil {
		return err
	}
	return r.attach(session, insideTmux)
}

// Attach presents an already-established workspace session. Callers that
// guard Ensure with a lifecycle lock can release that lock before the ordinary
// tmux client occupies the foreground.
func (r *WorkspaceRunner) Attach(session string, insideTmux bool) error {
	return r.attach(session, insideTmux)
}

// Ensure creates or repairs the workspace session without attaching a client.
// Automation callers can use this to establish kwt's canonical layout and
// bootstrap before handing presentation to another ordinary tmux client.
func (r *WorkspaceRunner) Ensure(
	ctx context.Context, session, worktreeDir string, layout models.Layout,
) error {
	if err := ValidateStartDirectory(worktreeDir); err != nil {
		return err
	}
	sessionExists := r.tmux.HasSession(session)
	if r.protected {
		if err := r.validateProtectedState(
			session,
			worktreeDir,
			sessionExists,
		); err != nil {
			return err
		}
	}
	if sessionExists {
		if err := r.repairBootstrap(ctx, session, worktreeDir); err != nil {
			return err
		}
	} else if err := r.create(ctx, session, worktreeDir, layout); err != nil {
		return err
	}
	return nil
}

// AttachProtected verifies and repairs an existing protected session before
// attaching without tmux's client-environment update. The -E attach is the
// final enforcement point: session options are mutable by pane processes, so
// attachment must not trust update-environment to remain filtered.
func (r *WorkspaceRunner) AttachProtected(
	ctx context.Context,
	session, worktreeDir string,
) error {
	if !r.protected {
		return fmt.Errorf("protected attachment requires a protected workspace runner")
	}
	if err := ValidateStartDirectory(worktreeDir); err != nil {
		return err
	}
	if !r.tmux.HasSession(session) {
		return &SessionSafetyError{Reason: fmt.Sprintf(
			"protected tmux session %q is not running",
			session,
		)}
	}
	if err := r.validateProtectedState(session, worktreeDir, true); err != nil {
		return err
	}
	if err := r.repairBootstrap(ctx, session, worktreeDir); err != nil {
		return err
	}
	return r.tmux.AttachSessionWithoutEnvironment(ctx, session)
}

// ValidateStartDirectory rejects paths tmux would interpret as format syntax
// when they are passed as pane and window start directories.
func ValidateStartDirectory(worktreeDir string) error {
	if strings.Contains(worktreeDir, "#") {
		return fmt.Errorf(
			"workspace path %q contains tmux format syntax ('#'); choose a path without '#'",
			worktreeDir,
		)
	}
	return nil
}

// EnsureAndAttachProtected creates or repairs a protected workspace session
// before attaching without tmux's client-environment update. This is the
// convergent open path for persisted PR imports: the isolated session may not
// exist yet when import omitted startup, or may have disappeared since import.
func (r *WorkspaceRunner) EnsureAndAttachProtected(
	ctx context.Context,
	session, worktreeDir string,
	layout models.Layout,
) error {
	if !r.protected {
		return fmt.Errorf(
			"protected attachment requires a protected workspace runner",
		)
	}
	if err := r.Ensure(ctx, session, worktreeDir, layout); err != nil {
		return err
	}
	return r.tmux.AttachSessionWithoutEnvironment(ctx, session)
}

func (r *WorkspaceRunner) validateProtectedState(
	session, worktreeDir string,
	sessionExists bool,
) error {
	globalEnvironment, err := r.tmux.GlobalEnvironment()
	if err != nil {
		return fmt.Errorf("cannot verify tmux server environment: %w", err)
	}
	if name, ok := firstMatchingName(
		setServerEnvNames(globalEnvironment),
		r.credentialNames,
	); ok {
		return &SessionSafetyError{Reason: fmt.Sprintf(
			"tmux server environment contains sensitive variable %s; remove it before starting the imported workspace",
			name,
		)}
	}
	if !sessionExists {
		return nil
	}

	sessionEnvironment, err := r.tmux.SessionEnvironment(session)
	if err != nil {
		return fmt.Errorf(
			"cannot verify existing tmux session environment: %w",
			err,
		)
	}
	if name, ok := firstMatchingName(
		setServerEnvNames(sessionEnvironment),
		r.credentialNames,
	); ok {
		return &SessionSafetyError{Reason: fmt.Sprintf(
			"existing tmux session contains sensitive variable %s; remove the session before retrying",
			name,
		)}
	}
	workspacePath, err := r.tmux.sessionOption(
		session,
		workspacePathOption,
	)
	workspacePath = strings.TrimSuffix(workspacePath, "\n")
	workspacePath = strings.TrimSuffix(workspacePath, "\r")
	if err != nil || workspacePath != worktreeDir {
		return &SessionSafetyError{Reason: fmt.Sprintf(
			"existing tmux session %q is not verified for workspace %q; remove or rename it before retrying",
			session,
			worktreeDir,
		)}
	}
	return nil
}

func firstMatchingName(names, sensitive []string) (string, bool) {
	sensitiveSet := make(map[string]bool, len(sensitive))
	for _, name := range sensitive {
		sensitiveSet[strings.ToLower(name)] = true
	}
	for _, name := range names {
		if sensitiveSet[strings.ToLower(name)] {
			return name, true
		}
	}
	return "", false
}

// sessionStripNames derives the remove-marker set for session: the
// terminal-agnostic bootstrap kwt applies to every workspace session it
// creates or repairs so launcher-state variables do not leak into panes.
//
// The set is the union of up to four sources so neither a stale server-global
// table nor a stale session-local table can leak variables kwt's own process
// environment happens to lack:
//   - every canonical exact key, unconditionally (a marker for an absent
//     variable is harmless);
//   - the launcher-derived names present in kwt's own environment (this is what
//     picks up prefix-matched variables like VSCODE_* that kwt inherited);
//   - the prefix-matched names read from the server's global environment table,
//     which catches stale variables a pre-existing server holds (e.g. an
//     editor-launched server's VSCODE_*) that kwt's env does not;
//   - only when the session already exists (the repair path), the
//     prefix-matched names read from the session's own environment table,
//     which catches variables a pre-existing session holds ONLY locally —
//     e.g. an editor terminal that created the session captured its VSCODE_*/
//     STARSHIP_* into the session table directly, never promoting them to the
//     server-global table the previous source reads.
//
// A show-environment failure on either query falls back to whichever sources
// remain.
func (r *WorkspaceRunner) sessionStripNames(session string, sessionExists bool) []string {
	launcherEnv := os.Environ()
	launcher := append(
		StripEnvNames(launcherEnv),
		matchingProtectedEnvironmentNames(launcherEnv, r.extraStripNames)...,
	)
	var serverDerived, sessionDerived []string
	if output, err := r.tmux.GlobalEnvironment(); err == nil {
		serverNames := ParseServerEnvNames(output)
		serverDerived = append(
			CanonicalStripNames(serverNames),
			matchingProtectedNames(serverNames, r.extraStripNames)...,
		)
	}
	if sessionExists {
		if output, err := r.tmux.SessionEnvironment(session); err == nil {
			sessionNames := ParseServerEnvNames(output)
			sessionDerived = append(
				CanonicalStripNames(sessionNames),
				matchingProtectedNames(sessionNames, r.extraStripNames)...,
			)
		}
	}
	return MergeStripNames(
		CanonicalStripExactNames(),
		r.extraStripNames,
		launcher,
		serverDerived,
		sessionDerived,
	)
}

func matchingProtectedEnvironmentNames(
	env, protectedNames []string,
) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok {
			names = append(names, name)
		}
	}
	return matchingProtectedNames(names, protectedNames)
}

func matchingProtectedNames(names, protectedNames []string) []string {
	protected := make(map[string]bool, len(protectedNames))
	for _, name := range protectedNames {
		protected[strings.ToLower(name)] = true
	}
	var matched []string
	for _, name := range names {
		if protected[strings.ToLower(name)] {
			matched = append(matched, name)
		}
	}
	return matched
}

// create spawns the first pane as an inert placeholder (it spawns before the
// session exists to carry remove-markers, and a login shell there would run
// profile scripts under the dirty environment and then again after respawn;
// see FirstPanePlaceholderArgv), applies the session bootstrap so the
// strips are in place before any further pane spawns, respawns the first pane
// into the real login shell — its only login-shell spawn, from the stripped
// session environment (see BuildFirstPaneRespawnCommand) — then spawns and
// arranges the remaining panes (capturing one pane ID per pane) and runs the
// pane-command sequence against those IDs. If a command after new-session
// fails, it kills the session it just started rather than leaving a
// half-built workspace — including one whose only pane is still the
// placeholder — behind for a later EnsureAndAttach to find and attach to.
func (r *WorkspaceRunner) create(
	ctx context.Context, session, worktreeDir string, layout models.Layout,
) error {
	// The session does not exist yet, so only the launcher and server-global
	// sources contribute strip names; querying the session table would be a
	// guaranteed-to-fail subprocess.
	stripNames := r.sessionStripNames(session, false)
	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	firstPane, err := r.tmux.RunCommandOutputContext(ctx, createCmd...)
	if err != nil {
		return wrapTmuxErr(createCmd, err)
	}
	paneIDs := make([]string, 0, len(layout.Panes))
	paneIDs = append(paneIDs, strings.TrimSpace(firstPane))

	bootCmd := BuildSessionBootstrapCommand(session, stripNames)
	if r.protected {
		updateEnvironment, updateErr := r.protectedUpdateEnvironment(session)
		if updateErr != nil {
			return r.abort(session, []string{
				"show-options", "-t", session, "update-environment",
			}, updateErr)
		}
		bootCmd = buildProtectedSessionBootstrapCommand(
			session,
			worktreeDir,
			stripNames,
			updateEnvironment,
		)
	}
	if err := r.tmux.RunCommandContext(ctx, bootCmd...); err != nil {
		return r.abort(session, bootCmd, err)
	}
	defaultShellCmd := []string{
		"show-options", "-v", "-t", session, "default-shell",
	}
	defaultShellOut, err := r.tmux.RunCommandOutputContext(ctx, defaultShellCmd...)
	if err != nil {
		return r.abort(session, defaultShellCmd, err)
	}
	defaultShell := strings.TrimSpace(defaultShellOut)
	if defaultShell == "" {
		defaultShellCmd = []string{"show-options", "-gv", "default-shell"}
		defaultShellOut, err = r.tmux.RunCommandOutputContext(
			ctx,
			defaultShellCmd...,
		)
		if err != nil {
			return r.abort(session, defaultShellCmd, err)
		}
		defaultShell = strings.TrimSpace(defaultShellOut)
	}
	if defaultShell == "" {
		return r.abort(session, defaultShellCmd, fmt.Errorf("tmux returned an empty default-shell"))
	}
	respawnCmd := BuildFirstPaneRespawnCommand(paneIDs[0], worktreeDir, defaultShell)
	if err := r.tmux.RunCommandContext(ctx, respawnCmd...); err != nil {
		return r.abort(session, respawnCmd, err)
	}
	for _, args := range BuildRemainingPaneSequence(session, worktreeDir, layout) {
		if args[0] == "split-window" {
			out, err := r.tmux.RunCommandOutputContext(ctx, args...)
			if err != nil {
				return r.abort(session, args, err)
			}
			paneIDs = append(paneIDs, strings.TrimSpace(out))
		} else if err := r.tmux.RunCommandContext(ctx, args...); err != nil {
			return r.abort(session, args, err)
		}
	}
	for _, args := range BuildPaneCommandSequence(paneIDs, layout.Panes) {
		if err := r.tmux.RunCommandContext(ctx, args...); err != nil {
			return r.abort(session, args, err)
		}
	}
	return nil
}

// repairBootstrap re-applies the safe bootstrap subset (default-command and
// the session-scoped remove-markers) to an already-existing session. It runs
// no construction or pane commands, so it is non-destructive on any session,
// including one an external tool created bare with new-session. The session is
// not kwt's to tear down, so a failure is wrapped and returned without killing
// it.
func (r *WorkspaceRunner) repairBootstrap(
	ctx context.Context,
	session, worktreeDir string,
) error {
	stripNames := r.sessionStripNames(session, true)
	bootCmd := BuildSessionBootstrapCommand(session, stripNames)
	if r.protected {
		updateEnvironment, err := r.protectedUpdateEnvironment(session)
		if err != nil {
			return fmt.Errorf("cannot inspect protected tmux update-environment: %w", err)
		}
		bootCmd = buildProtectedSessionBootstrapCommand(
			session,
			worktreeDir,
			stripNames,
			updateEnvironment,
		)
	}
	if err := r.tmux.RunCommandContext(ctx, bootCmd...); err != nil {
		return wrapTmuxErr(bootCmd, err)
	}
	return nil
}

func (r *WorkspaceRunner) protectedUpdateEnvironment(session string) (string, error) {
	value, err := r.tmux.sessionOption(session, "update-environment")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		value, err = r.tmux.globalOption("update-environment")
		if err != nil {
			return "", err
		}
	}
	protected := make(map[string]bool, len(r.credentialNames))
	for _, name := range r.credentialNames {
		protected[name] = true
	}
	kept := make([]string, 0)
	for _, name := range strings.Fields(value) {
		if !protected[name] {
			kept = append(kept, name)
		}
	}
	return strings.Join(kept, " "), nil
}

// abort wraps a construction-command failure and kills the just-created
// session so it does not linger half-built. Killing is best-effort: a failure
// there is annotated onto the returned error but never replaces it.
func (r *WorkspaceRunner) abort(session string, args []string, err error) error {
	wrapped := wrapTmuxErr(args, err)
	if killErr := r.tmux.KillSession(session); killErr != nil {
		return fmt.Errorf("%w (also failed to kill partial session: %v)", wrapped, killErr)
	}
	return wrapped
}

// wrapTmuxErr annotates a failed tmux invocation with the command that failed.
func wrapTmuxErr(args []string, err error) error {
	return fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
}

// attach connects the current client to the session.
func (r *WorkspaceRunner) attach(session string, insideTmux bool) error {
	if insideTmux {
		return r.tmux.SwitchClient(session)
	}
	return r.tmux.AttachSession(session)
}
