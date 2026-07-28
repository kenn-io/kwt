package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

// mockWorkspaceTmux implements workspaceTmux, recording every command and
// returning incrementing fake pane IDs ("%1\n", "%2\n", ...) so tests can
// assert the captured-ID -> send-keys wiring without real tmux. outputErr,
// when set, is returned by every RunCommandOutputContext call instead of a
// pane ID, to exercise create's error-propagation path when new-session
// itself (the first such call) fails. failOutputOnCall, when nonzero, instead
// fails only the Nth RunCommandOutputContext call (1-indexed), so tests can
// make a later construction command (e.g. the first split-window) fail after
// new-session already succeeded. failRunOnCall is the same idea for
// RunCommandContext (1-indexed), letting tests fail select-layout or a
// pane-command invocation instead.
type mockWorkspaceTmux struct {
	hasSession          bool
	calls               [][]string
	paneSeq             int
	switchedTo          string
	attachedTo          string
	protectedAttachedTo string
	protectedAttachCtx  context.Context
	outputErr           error
	failOutputOnCall    int
	outputCalls         int
	failRunOnCall       int
	runCalls            int
	killSessionCalled   bool
	killedSession       string
	sessionDefaultShell string
	globalDefaultShell  string
	globalEnv           string
	globalEnvErr        error
	sessionEnv          string
	sessionEnvErr       error
	sessionEnvQueried   bool
	sessionWorkspace    string
	sessionUpdateEnv    string
	globalUpdateEnv     string
	sessionOptionErr    error
}

func (m *mockWorkspaceTmux) HasSession(string) bool {
	return m.hasSession
}

func (m *mockWorkspaceTmux) GlobalEnvironment() (string, error) {
	return m.globalEnv, m.globalEnvErr
}

func (m *mockWorkspaceTmux) SessionEnvironment(string) (string, error) {
	m.sessionEnvQueried = true
	return m.sessionEnv, m.sessionEnvErr
}

func (m *mockWorkspaceTmux) sessionOption(_ string, option string) (string, error) {
	switch option {
	case workspacePathOption:
		return m.sessionWorkspace, m.sessionOptionErr
	case "update-environment":
		return m.sessionUpdateEnv, m.sessionOptionErr
	default:
		return "", m.sessionOptionErr
	}
}

func (m *mockWorkspaceTmux) globalOption(option string) (string, error) {
	if option == "update-environment" {
		return m.globalUpdateEnv, nil
	}
	return "", nil
}

// expectedBootstrapCommand derives the bootstrap invocation a test should
// expect: the canonical exact names plus the launcher-derived names from the
// test process's own environment, plus any names the mock's server/session
// tables contribute. It mirrors sessionStripNames deliberately so the
// sequencing assertions stay valid in any test environment.
func expectedBootstrapCommand(session string, tableDerived ...[]string) []string {
	sets := append([][]string{CanonicalStripExactNames(), StripEnvNames(os.Environ())}, tableDerived...)
	return BuildSessionBootstrapCommand(session, MergeStripNames(sets...))
}

func (m *mockWorkspaceTmux) RunCommandContext(_ context.Context, args ...string) error {
	m.calls = append(m.calls, args)
	m.runCalls++
	if m.failRunOnCall != 0 && m.runCalls == m.failRunOnCall {
		return fmt.Errorf("boom on call %d", m.runCalls)
	}
	return nil
}

func (m *mockWorkspaceTmux) RunCommandOutputContext(
	_ context.Context, args ...string,
) (string, error) {
	m.calls = append(m.calls, args)
	m.outputCalls++
	if m.outputErr != nil {
		return "", m.outputErr
	}
	if m.failOutputOnCall != 0 && m.outputCalls == m.failOutputOnCall {
		return "", fmt.Errorf("boom on call %d", m.outputCalls)
	}
	if len(args) > 0 && args[0] == "show-options" {
		if len(args) > 1 && args[1] == "-gv" {
			if m.globalDefaultShell != "" {
				return m.globalDefaultShell + "\n", nil
			}
			return "/bin/sh\n", nil
		}
		return m.sessionDefaultShell + "\n", nil
	}
	m.paneSeq++
	return fmt.Sprintf("%%%d\n", m.paneSeq), nil
}

func (m *mockWorkspaceTmux) SwitchClient(target string) error {
	m.switchedTo = target
	return nil
}

func (m *mockWorkspaceTmux) AttachSession(session string) error {
	m.attachedTo = session
	return nil
}

func (m *mockWorkspaceTmux) AttachSessionWithoutEnvironment(
	ctx context.Context,
	session string,
) error {
	m.protectedAttachCtx = ctx
	m.protectedAttachedTo = session
	return nil
}

func (m *mockWorkspaceTmux) KillSession(session string) error {
	m.killSessionCalled = true
	m.killedSession = session
	return nil
}

// The empty pane sits in the middle (index 1) rather than last: since
// BuildPaneCommandSequence skips empty panes, a trailing empty pane would
// never index its captured ID, letting an off-by-one that drops the last
// pane's capture (i < len(layout.Panes)-1) go unnoticed. With the empty pane
// mid-sequence, "vim" (index 2) must target %3, which only exists if the
// capture boundary includes the third (last) pane-creating command.
func TestEnsureAndAttachCreatesSendsToCapturedIDsAndAttaches(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "even-horizontal", Panes: []string{"codex", "", "vim"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, false)
	require.NoError(t, err)

	// The session bootstrap (one batched set-option + remove-marker command)
	// runs after the first pane but before the split-windows, so the strips
	// are in place before any further pane spawns. The first pane spawns as
	// the inert placeholder (a login shell there would run profile scripts
	// under the dirty environment and then again after the respawn) and is
	// respawned into the real login shell — explicitly, since respawn-pane
	// without a command would re-run the placeholder — only once the strips
	// exist.
	want := [][]string{
		{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "s", "-c", "/wt", "sleep", "2147483647"},
		expectedBootstrapCommand("s"),
		{"show-options", "-v", "-t", "s", "default-shell"},
		{"show-options", "-gv", "default-shell"},
		{"respawn-pane", "-k", "-c", "/wt", "-t", "%1", "/bin/sh", "-l"},
		{"split-window", "-P", "-F", "#{pane_id}", "-t", "s", "-c", "/wt"},
		{"split-window", "-P", "-F", "#{pane_id}", "-t", "s", "-c", "/wt"},
		{"select-layout", "-t", "s", "even-horizontal"},
		{"send-keys", "-t", "%1", "-l", "--", "codex"},
		{"send-keys", "-t", "%1", "Enter"},
		{"send-keys", "-t", "%3", "-l", "--", "vim"},
		{"send-keys", "-t", "%3", "Enter"},
		{"select-pane", "-t", "%1"},
	}
	assert.Equal(t, want, m.calls)
	assert.Equal(t, "s", m.attachedTo)
	assert.Empty(t, m.switchedTo)
	assert.False(t, m.sessionEnvQueried,
		"the create path must not query the session table of a session that does not exist yet")
}

func TestEnsureCreatesWorkspaceWithoutAttaching(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false}
	r := NewWorkspaceRunner(m, nil)

	err := r.Ensure(
		context.Background(),
		"workspace",
		"/wt",
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.NotEmpty(t, m.calls)
	assert.Empty(t, m.attachedTo)
	assert.Empty(t, m.switchedTo)
}

func TestEnsureAndAttachQueriesServerDefaultShell(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession:         false,
		globalDefaultShell: "/opt/homebrew/bin/fish",
	}
	r := NewWorkspaceRunner(m, nil)

	err := r.EnsureAndAttach(
		context.Background(), "workspace", "/wt",
		models.Layout{Arrange: "tiled", Panes: []string{""}}, false,
	)
	require.NoError(t, err)

	assert.Contains(t, m.calls,
		[]string{"show-options", "-v", "-t", "workspace", "default-shell"},
		"the session-local shell must be checked first")
	assert.Contains(t, m.calls,
		[]string{"show-options", "-gv", "default-shell"},
		"an inherited shell must fall back to the server value")
	assert.Contains(t, m.calls,
		[]string{
			"respawn-pane", "-k", "-c", "/wt", "-t", "%1",
			"/opt/homebrew/bin/fish", "-l",
		})
}

func TestEnsureAndAttachPrefersSessionDefaultShell(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession:          false,
		sessionDefaultShell: "/bin/bash",
		globalDefaultShell:  "/opt/homebrew/bin/fish",
	}
	r := NewWorkspaceRunner(m, nil)

	err := r.EnsureAndAttach(
		context.Background(), "workspace", "/wt",
		BlankLayout(), false,
	)

	require.NoError(t, err)
	assert.NotContains(t, m.calls,
		[]string{"show-options", "-gv", "default-shell"})
	assert.Contains(t, m.calls,
		[]string{
			"respawn-pane", "-k", "-c", "/wt", "-t", "%1",
			"/bin/bash", "-l",
		})
}

func TestProtectedEnsureRejectsSensitiveServerEnvironment(t *testing.T) {
	m := &mockWorkspaceTmux{
		globalEnv: "kwt_github_token=secret\nKWT_HOME=/tmp/kwt\n",
	}
	r := NewProtectedWorkspaceRunner(
		m,
		[]string{"KWT_GITHUB_TOKEN"},
	)

	err := r.Ensure(
		context.Background(),
		"workspace",
		"/wt",
		BlankLayout(),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kwt_github_token")
	assert.Empty(t, m.calls)
}

func TestProtectedEnsureRejectsUnverifiedExistingSession(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession: true,
		globalEnv:  "PATH=/bin\n",
		sessionEnv: "PATH=/bin\n",
	}
	r := NewProtectedWorkspaceRunner(
		m,
		[]string{"KWT_GITHUB_TOKEN"},
	)

	err := r.Ensure(
		context.Background(),
		"workspace",
		"/wt",
		BlankLayout(),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not verified")
	assert.Empty(t, m.calls)
}

func TestProtectedEnsureRejectsSensitiveExistingSession(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession:       true,
		globalEnv:        "PATH=/bin\n",
		sessionEnv:       "Custom_Fleet_Token=secret\n",
		sessionWorkspace: "/wt",
	}
	r := NewProtectedWorkspaceRunner(
		m,
		[]string{"KWT_GITHUB_TOKEN", "CUSTOM_FLEET_TOKEN"},
	)

	err := r.Ensure(
		context.Background(),
		"workspace",
		"/wt",
		BlankLayout(),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Custom_Fleet_Token")
	assert.Empty(t, m.calls)
}

func TestProtectedEnsureRepairsVerifiedExistingSession(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession:       true,
		globalEnv:        "PATH=/bin\n",
		sessionEnv:       "PATH=/bin\n",
		sessionWorkspace: "/wt",
		sessionUpdateEnv: "DISPLAY SSH_AUTH_SOCK KWT_GITHUB_TOKEN CUSTOM_FLEET_TOKEN",
	}
	r := NewProtectedWorkspaceRunner(
		m,
		[]string{"KWT_GITHUB_TOKEN", "CUSTOM_FLEET_TOKEN"},
	)

	err := r.Ensure(
		context.Background(),
		"workspace",
		"/wt",
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Equal(t, [][]string{buildProtectedSessionBootstrapCommand(
		"workspace",
		"/wt",
		MergeStripNames(
			CanonicalStripExactNames(),
			[]string{"KWT_GITHUB_TOKEN", "CUSTOM_FLEET_TOKEN"},
			StripEnvNames(os.Environ()),
		),
		"DISPLAY SSH_AUTH_SOCK",
	)}, m.calls)
}

func TestProtectedEnsureMarksCreatedSessionBeforeRespawn(t *testing.T) {
	m := &mockWorkspaceTmux{
		globalEnv:       "PATH=/bin\n",
		globalUpdateEnv: "DISPLAY KWT_GITHUB_TOKEN SSH_AUTH_SOCK",
	}
	r := NewProtectedWorkspaceRunner(
		m,
		[]string{"KWT_GITHUB_TOKEN"},
	)

	err := r.Ensure(
		context.Background(),
		"workspace",
		"/wt",
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Contains(t, m.calls, buildProtectedSessionBootstrapCommand(
		"workspace",
		"/wt",
		MergeStripNames(
			CanonicalStripExactNames(),
			[]string{"KWT_GITHUB_TOKEN"},
			StripEnvNames(os.Environ()),
		),
		"DISPLAY SSH_AUTH_SOCK",
	))
}

func TestProtectedAttachRepairsPolicyAndDisablesEnvironmentUpdate(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession:       true,
		globalEnv:        "PATH=/bin\n",
		sessionEnv:       "PATH=/bin\n",
		sessionWorkspace: "/wt",
		sessionUpdateEnv: "DISPLAY KWT_GITHUB_TOKEN KWT_FLEET_TOKEN",
	}
	r := NewProtectedWorkspaceRunner(
		m,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := r.AttachProtected(
		ctx,
		"workspace",
		"/wt",
	)

	require.NoError(t, err)
	assert.Same(t, ctx, m.protectedAttachCtx)
	assert.Equal(t, "workspace", m.protectedAttachedTo)
	assert.Empty(t, m.attachedTo)
	assert.Equal(t, [][]string{buildProtectedSessionBootstrapCommand(
		"workspace",
		"/wt",
		MergeStripNames(
			CanonicalStripExactNames(),
			[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
			StripEnvNames(os.Environ()),
		),
		"DISPLAY",
	)}, m.calls)
}

func TestProtectedEnsureAndAttachCreatesMissingSession(t *testing.T) {
	m := &mockWorkspaceTmux{
		globalEnv:       "PATH=/bin\n",
		globalUpdateEnv: "DISPLAY KWT_GITHUB_TOKEN",
	}
	r := NewProtectedWorkspaceRunner(
		m,
		[]string{"KWT_GITHUB_TOKEN"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := r.EnsureAndAttachProtected(
		ctx,
		"workspace",
		"/wt",
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Same(t, ctx, m.protectedAttachCtx)
	assert.Equal(t, "workspace", m.protectedAttachedTo)
	assert.Contains(t, m.calls, BuildSessionCreateCommand(
		"workspace",
		"/wt",
	))
	assert.Contains(t, m.calls, buildProtectedSessionBootstrapCommand(
		"workspace",
		"/wt",
		MergeStripNames(
			CanonicalStripExactNames(),
			[]string{"KWT_GITHUB_TOKEN"},
			StripEnvNames(os.Environ()),
		),
		"DISPLAY",
	))
}

func TestProtectedWorkspaceSocketNameIsStableAndWorkspaceSpecific(t *testing.T) {
	first := ProtectedWorkspaceSocketName("kwt-workspace-a", "/worktrees/a")

	assert.Equal(t, first, ProtectedWorkspaceSocketName("kwt-workspace-a", "/worktrees/a"))
	assert.NotEqual(t, first, ProtectedWorkspaceSocketName("kwt-workspace-a", "/worktrees/b"))
	assert.Regexp(t, `^kwt-pr-[0-9a-f]{16}$`, first)
}

// TestEnsureAndAttachRepairsExistingSessionBootstrapWithoutConstructing pins
// the forward-repair path: an already-existing session (e.g. one an external
// tool created bare with new-session) must have the safe bootstrap subset
// re-applied — default-command plus the remove-markers, including names read
// from the server-global and session-local environment tables — but no
// construction or pane commands, so future windows behave consistently
// without disturbing the running session.
func TestEnsureAndAttachRepairsExistingSessionBootstrapWithoutConstructing(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession: true,
		globalEnv:  "KWT_GITHUB_TOKEN=secret\nSTARSHIP_SESSION_KEY=abc\nHOME=/home/u",
		sessionEnv: "VSCODE_GIT_IPC_HANDLE=/tmp/ipc\nPATH=/bin",
	}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "tiled", Panes: []string{"codex"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, false)
	require.NoError(t, err)

	want := [][]string{
		expectedBootstrapCommand("s",
			[]string{"KWT_GITHUB_TOKEN", "STARSHIP_SESSION_KEY"},
			[]string{"VSCODE_GIT_IPC_HANDLE"}),
	}
	assert.Equal(t, want, m.calls, "existing session must be repaired but not re-created")
	assert.Equal(t, 0, m.outputCalls, "repair issues no pane-capturing commands")
	assert.Equal(t, "s", m.attachedTo)
	assert.Empty(t, m.switchedTo)
}

func TestEnsureAndAttachStripsConfiguredCredentialCaseVariants(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession: true,
		globalEnv:  "custom_fleet_token=server-secret",
		sessionEnv: "Custom_Fleet_Token=session-secret",
	}
	r := NewWorkspaceRunner(m, []string{"CUSTOM_FLEET_TOKEN"})

	err := r.EnsureAndAttach(
		context.Background(),
		"s",
		"/wt",
		models.Layout{Arrange: "tiled", Panes: []string{""}},
		false,
	)

	require.NoError(t, err)
	require.Len(t, m.calls, 1)
	assert.Contains(t, m.calls[0], "CUSTOM_FLEET_TOKEN")
	assert.Contains(t, m.calls[0], "custom_fleet_token")
	assert.Contains(t, m.calls[0], "Custom_Fleet_Token")
}

// TestEnsureAndAttachRepairSurvivesEnvReadFailures pins the graceful
// degradation of the strip-name derivation: a show-environment failure on
// either table drops that source but never fails the attach.
func TestEnsureAndAttachRepairSurvivesEnvReadFailures(t *testing.T) {
	m := &mockWorkspaceTmux{
		hasSession:    true,
		globalEnvErr:  errors.New("no server"),
		sessionEnvErr: errors.New("no session"),
	}
	r := NewWorkspaceRunner(m, nil)

	err := r.EnsureAndAttach(context.Background(), "s", "/wt",
		models.Layout{Arrange: "tiled", Panes: []string{""}}, false)
	require.NoError(t, err)

	want := [][]string{expectedBootstrapCommand("s")}
	assert.Equal(t, want, m.calls,
		"env-table read failures fall back to the launcher and exact sources")
}

func TestEnsureAndAttachSwitchesClientInsideTmux(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "tiled", Panes: []string{"codex"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, true)
	require.NoError(t, err)

	assert.Equal(t, "s", m.switchedTo)
	assert.Empty(t, m.attachedTo)
}

func TestEnsureAndAttachReturnsErrorOnCaptureFailure(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false, outputErr: errors.New("boom")}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "tiled", Panes: []string{"codex"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "new-session")
	assert.False(t, m.killSessionCalled,
		"new-session itself failed, so no session was created to kill")
}

// TestEnsureAndAttachKillsPartialSessionOnCreateFailure pins the other side
// of the created boundary: once new-session has succeeded, a later
// construction command failing must kill the now-partially-built session so
// a subsequent EnsureAndAttach rebuilds instead of attaching to it broken.
func TestEnsureAndAttachKillsPartialSessionOnCreateFailure(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false, failOutputOnCall: 4}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "even-horizontal", Panes: []string{"codex", "vim"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "split-window")
	assert.True(t, m.killSessionCalled, "must kill the partially built session")
	assert.Equal(t, "s", m.killedSession)
	assert.Empty(t, m.attachedTo, "must not attach to a killed session")
	assert.Empty(t, m.switchedTo, "must not switch to a killed session")
}

// TestEnsureAndAttachKillsPartialSessionOnRespawnFailure covers cleanup when
// the first-pane respawn (RunCommandContext call 2, after the bootstrap's
// set-option default-command) fails: the session exists by then, so it must be
// killed rather than left half-built.
func TestEnsureAndAttachKillsPartialSessionOnRespawnFailure(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false, failRunOnCall: 2}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "even-horizontal", Panes: []string{"codex", "vim"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "respawn-pane")
	assert.True(t, m.killSessionCalled, "must kill the partially built session")
	assert.Equal(t, "s", m.killedSession)
	assert.Empty(t, m.attachedTo, "must not attach to a killed session")
	assert.Empty(t, m.switchedTo, "must not switch to a killed session")
}

// TestEnsureAndAttachKillsPartialSessionOnLayoutFailure covers cleanup when a
// RunCommandContext call fails at select-layout. It is the third
// RunCommandContext call: call 1 is the bootstrap's set-option default-command
// (applied right after new-session, before the split-windows), call 2 is the
// first-pane respawn, and the split-windows are RunCommandOutputContext. The
// RunCommandOutputContext branch is covered by
// TestEnsureAndAttachKillsPartialSessionOnCreateFailure, and the
// BuildPaneCommandSequence branch by the pane-command test below.
func TestEnsureAndAttachKillsPartialSessionOnLayoutFailure(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false, failRunOnCall: 3}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "even-horizontal", Panes: []string{"codex", "vim"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "select-layout")
	assert.True(t, m.killSessionCalled, "must kill the partially built session")
	assert.Equal(t, "s", m.killedSession)
	assert.Empty(t, m.attachedTo, "must not attach to a killed session")
	assert.Empty(t, m.switchedTo, "must not switch to a killed session")
}

// TestEnsureAndAttachKillsPartialSessionOnPaneCommandFailure covers the last
// cleanup branch: a BuildPaneCommandSequence send-keys failing after the panes
// exist and the bootstrap has applied. failRunOnCall 4 targets the first
// send-keys (call 1 is the bootstrap's set-option default-command, call 2 is
// the first-pane respawn, call 3 is select-layout).
func TestEnsureAndAttachKillsPartialSessionOnPaneCommandFailure(t *testing.T) {
	m := &mockWorkspaceTmux{hasSession: false, failRunOnCall: 4}
	r := NewWorkspaceRunner(m, nil)
	layout := models.Layout{Arrange: "even-horizontal", Panes: []string{"codex", "vim"}}

	err := r.EnsureAndAttach(context.Background(), "s", "/wt", layout, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "send-keys")
	assert.True(t, m.killSessionCalled, "must kill the partially built session")
	assert.Equal(t, "s", m.killedSession)
	assert.Empty(t, m.attachedTo, "must not attach to a killed session")
	assert.Empty(t, m.switchedTo, "must not switch to a killed session")
}
