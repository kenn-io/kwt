package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kwt/pkg/models"
)

func TestGlobalEnvironmentReadsFreshNamedServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-envtest-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { killPrivateTmuxServer(socket) })
	t.Setenv("KWT_FRESH_SERVER_CANARY", "visible")
	t.Setenv("KWT_GITHUB_TOKEN", "must-not-reach-server")

	command := NewTmuxCommandForSocketWithStripNames(
		"tmux",
		socket,
		[]string{"KWT_GITHUB_TOKEN"},
	)
	globalEnvironment, err := command.GlobalEnvironment()
	if err != nil {
		t.Fatalf("read fresh server environment: %v", err)
	}
	if !strings.Contains(
		globalEnvironment,
		"KWT_FRESH_SERVER_CANARY=visible",
	) {
		t.Errorf(
			"fresh server environment omitted the launcher canary; got:\n%s",
			globalEnvironment,
		)
	}
	if strings.Contains(globalEnvironment, "KWT_GITHUB_TOKEN=") {
		t.Errorf(
			"fresh server environment leaked the configured credential; got:\n%s",
			globalEnvironment,
		)
	}
}

// TestStripEnvMasksGlobalEnvForSessionOnly exercises the real session
// bootstrap sequence against a private tmux server and confirms that a
// variable present in the server-global environment at server-start time is
// absent from a pane spawned later in a session that stripped it via
// set-environment -r (the session-scoped remove-marker; see
// BuildSessionBootstrapCommands). It never touches the default tmux server:
// every invocation targets a private, uniquely-named socket via -L, and only
// -d (detached) session/window creation is used — nothing attaches.
func TestStripEnvMasksGlobalEnvForSessionOnly(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-striptest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-striptest-session"
	const canaryName = "KWT_INTEGRATION_TEST_CANARY"
	const canaryValue = "should-not-leak-into-panes"
	// STARSHIP_SHELL exercises the "STARSHIP_" prefix, part of the shared
	// canonical strip list (see canonicalStripPrefixes in bootstrap.go) that
	// StripEnvNames and SanitizedEnviron both derive from — a variable this
	// test's canary set did not cover before that list existed.
	const canary2Name = "STARSHIP_SHELL"
	const canary2Value = "zsh"

	var socketPath string
	t.Cleanup(func() {
		killPrivateTmuxServer(socket)
		if socketPath != "" {
			_ = os.Remove(socketPath)
		}
	})

	worktreeDir := t.TempDir()
	// The server inherits this environment at start time, so the canaries
	// become part of the server-global environment table — exactly what
	// -r (as opposed to -u) must keep out of this session's panes.
	serverEnv := append(append([]string(nil), os.Environ()...),
		canaryName+"="+canaryValue, canary2Name+"="+canary2Value, "SHELL=/bin/sh")

	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	if _, err := runTmuxSocket(t, socket, serverEnv, createCmd...); err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}
	socketPathOut, err := runTmuxSocket(t, socket, nil, "display-message", "-p", "#{socket_path}")
	if err != nil {
		t.Fatalf("resolve private tmux socket path: %v", err)
	}
	socketPath = strings.TrimSpace(socketPathOut)

	bootCmd := BuildSessionBootstrapCommand(session, []string{canaryName, canary2Name})
	if _, err := runTmuxSocket(t, socket, nil, bootCmd...); err != nil {
		t.Fatalf("tmux %v: %v", bootCmd, err)
	}
	for _, args := range BuildRemainingPaneSequence(session, worktreeDir, BlankLayout()) {
		if _, err := runTmuxSocket(t, socket, nil, args...); err != nil {
			t.Fatalf("tmux %v: %v", args, err)
		}
	}

	paneEnv := capturePaneEnv(t, socket, session, worktreeDir)
	if strings.Contains(paneEnv, canaryName+"=") {
		t.Errorf("new pane environment leaked %s; got:\n%s", canaryName, paneEnv)
	}
	if strings.Contains(paneEnv, canary2Name+"=") {
		t.Errorf("new pane environment leaked %s; got:\n%s", canary2Name, paneEnv)
	}

	killPrivateTmuxServer(socket)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove private tmux socket %q: %v", socketPath, err)
	}
	if out, err := runTmuxSocket(t, socket, nil, "list-sessions"); err == nil {
		t.Errorf("expected private tmux server %q to be gone after kill-server, got: %s", socket, out)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("private tmux socket %q remains after test: %v", socketPath, err)
	}
}

// TestBootstrapMasksFirstAndLaterPanes drives the phased construction (create
// first pane, apply the session bootstrap, respawn the first pane, then
// split) against a server whose global environment table is dirty at
// server-start time, and confirms every pane starts clean: the second pane
// spawns after the remove-markers are in place, and the first pane — which
// necessarily spawned before the session existed to carry a marker — is
// respawned once the markers exist, so it too starts from the stripped
// session environment instead of retaining a residual. It only ever touches a
// private, uniquely-named socket and uses only detached (-d) session/window
// creation.
func TestBootstrapMasksFirstAndLaterPanes(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-ordertest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-ordertest-session"
	const canaryName = "KWT_ORDER_TEST_CANARY"
	const canaryValue = "should-not-leak-into-later-panes"

	t.Cleanup(func() { killPrivateTmuxServer(socket) })

	worktreeDir := t.TempDir()
	serverEnv := append(append([]string(nil), os.Environ()...),
		canaryName+"="+canaryValue, "SHELL=/bin/sh")

	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	firstOut, err := runTmuxSocket(t, socket, serverEnv, createCmd...)
	if err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}
	firstPane := strings.TrimSpace(firstOut)

	bootCmd := BuildSessionBootstrapCommand(session, []string{canaryName})
	if _, err := runTmuxSocket(t, socket, nil, bootCmd...); err != nil {
		t.Fatalf("tmux %v: %v", bootCmd, err)
	}
	respawnCmd := BuildFirstPaneRespawnCommand(firstPane, worktreeDir, tmuxDefaultShell(t, socket))
	if _, err := runTmuxSocket(t, socket, nil, respawnCmd...); err != nil {
		t.Fatalf("tmux %v: %v", respawnCmd, err)
	}

	layout := models.Layout{Arrange: "even-horizontal", Panes: []string{"", ""}}
	var secondPane string
	for _, args := range BuildRemainingPaneSequence(session, worktreeDir, layout) {
		out, err := runTmuxSocket(t, socket, nil, args...)
		if err != nil {
			t.Fatalf("tmux %v: %v", args, err)
		}
		if args[0] == "split-window" {
			secondPane = strings.TrimSpace(out)
		}
	}
	if secondPane == "" {
		t.Fatal("split-window did not report a second pane ID")
	}

	firstEnv := dumpPaneEnv(t, socket, firstPane, worktreeDir, "first")
	secondEnv := dumpPaneEnv(t, socket, secondPane, worktreeDir, "second")

	if strings.Contains(firstEnv, canaryName+"=") {
		t.Errorf("first pane, respawned after the strips, leaked %s; got:\n%s", canaryName, firstEnv)
	}
	if strings.Contains(secondEnv, canaryName+"=") {
		t.Errorf("second pane, spawned after the strips, leaked %s; got:\n%s", canaryName, secondEnv)
	}
}

// TestFreshServerFirstPaneDoesNotInheritLauncherEditor pins the first-pane
// half of the EDITOR/VISUAL policy on a server kwt itself starts: the
// server's exec environment deliberately keeps EDITOR/VISUAL (tmux reads them
// at server start to pick vi-vs-emacs key mode; see serverStartExclusions),
// and new-session both starts the server and spawns the first pane before any
// remove-marker can exist — so without the respawn step the only pane of the
// default single-pane layout would permanently retain the launcher's values.
// It drives the full create-path sequence (new-session, bootstrap markers,
// first-pane respawn) and confirms the first pane sees neither variable while
// the server's global table still holds EDITOR (the key-mode input must
// survive). Later-pane behavior is pinned separately by
// TestSessionMarkerStripsEditorFromPaneEvenThoughServerExecKeepsIt. It only
// touches a private, uniquely-named socket and never attaches.
func TestFreshServerFirstPaneDoesNotInheritLauncherEditor(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-firstpanetest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-firstpanetest-session"

	t.Cleanup(func() { killPrivateTmuxServer(socket) })

	worktreeDir := t.TempDir()
	serverEnv := append(append([]string(nil), os.Environ()...),
		"EDITOR=launcher-editor", "VISUAL=launcher-visual", "SHELL=/bin/sh")

	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	firstOut, err := runTmuxSocket(t, socket, serverEnv, createCmd...)
	if err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}
	firstPane := strings.TrimSpace(firstOut)

	bootCmd := BuildSessionBootstrapCommand(session, CanonicalStripExactNames())
	if _, err := runTmuxSocket(t, socket, nil, bootCmd...); err != nil {
		t.Fatalf("tmux %v: %v", bootCmd, err)
	}
	respawnCmd := BuildFirstPaneRespawnCommand(firstPane, worktreeDir, tmuxDefaultShell(t, socket))
	if _, err := runTmuxSocket(t, socket, nil, respawnCmd...); err != nil {
		t.Fatalf("tmux %v: %v", respawnCmd, err)
	}

	firstEnv := dumpPaneEnv(t, socket, firstPane, worktreeDir, "first")
	if paneEnvHasVar(firstEnv, "EDITOR") {
		t.Errorf("first pane should not inherit the launcher's EDITOR; got:\n%s", firstEnv)
	}
	if paneEnvHasVar(firstEnv, "VISUAL") {
		t.Errorf("first pane should not inherit the launcher's VISUAL; got:\n%s", firstEnv)
	}

	globalEnv, err := runTmuxSocket(t, socket, nil, "show-environment", "-g")
	if err != nil {
		t.Fatalf("show-environment -g: %v", err)
	}
	if !strings.Contains(globalEnv, "EDITOR=launcher-editor") {
		t.Errorf("server-global table must keep EDITOR (tmux's key-mode input at server start); got:\n%s",
			globalEnv)
	}
}

// TestStripMasksStaleServerGlobalVarNotInLauncherEnv plants a launcher-state
// variable (a VSCODE_* prefix match) directly in a running server's global
// environment table, without it ever being in the test process's env, and
// confirms the server-derived strip path masks it in a new pane. This mirrors
// a pre-existing server an editor started holding VSCODE_* that kwt's own env
// lacks. It only touches a private, uniquely-named socket and creates
// sessions/windows detached.
func TestStripMasksStaleServerGlobalVarNotInLauncherEnv(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-staletest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-staletest-session"
	const staleName = "VSCODE_STALE_SERVER_VAR"
	const staleValue = "should-be-masked"

	t.Cleanup(func() { killPrivateTmuxServer(socket) })

	if _, ok := os.LookupEnv(staleName); ok {
		t.Fatalf("%s must not be present in the test environment", staleName)
	}

	worktreeDir := t.TempDir()

	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	if _, err := runTmuxSocket(t, socket, append(append([]string(nil), os.Environ()...), "SHELL=/bin/sh"),
		createCmd...); err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}

	// Plant the stale variable on the server's global table after the server
	// is up but before deriving the strip set — as if another tool had seeded
	// it. It is not in kwt's process env, so only the server-derived path can
	// find it.
	if _, err := runTmuxSocket(t, socket, nil, "set-environment", "-g", staleName, staleValue); err != nil {
		t.Fatalf("set-environment -g: %v", err)
	}

	globalEnv, err := runTmuxSocket(t, socket, nil, "show-environment", "-g")
	if err != nil {
		t.Fatalf("show-environment -g: %v", err)
	}
	serverDerived := CanonicalStripNames(ParseServerEnvNames(globalEnv))
	if !containsName(serverDerived, staleName) {
		t.Fatalf("server-derived strip set %v should include %s", serverDerived, staleName)
	}

	bootCmd := BuildSessionBootstrapCommand(session, MergeStripNames(CanonicalStripExactNames(), serverDerived))
	if _, err := runTmuxSocket(t, socket, nil, bootCmd...); err != nil {
		t.Fatalf("tmux %v: %v", bootCmd, err)
	}

	paneEnv := capturePaneEnv(t, socket, session, worktreeDir)
	if strings.Contains(paneEnv, staleName+"=") {
		t.Errorf("new pane leaked stale server-global %s; got:\n%s", staleName, paneEnv)
	}
}

// TestSessionLocalPrefixVarMaskedOnRepair pins the forward-repair fix: a bare
// session an external tool created directly (e.g. an editor terminal that ran
// new-session itself) can hold a launcher-state variable in its OWN
// environment table via set-environment -t <session> (no -g), never promoted
// to the server-global table. A strip set derived only from
// show-environment -g cannot see it, so it would keep leaking into every
// future window. This drives the same show-environment -t <session> +
// CanonicalStripNames/ParseServerEnvNames path defaultSessionBootstrap's
// session-derived source uses, then re-applies the bootstrap the way
// WorkspaceRunner.repairBootstrap does (BuildSessionBootstrapCommands against
// an already-existing session, no construction commands) and confirms a new
// window does not inherit the canary. It only touches a private,
// uniquely-named socket and creates/opens windows detached.
func TestSessionLocalPrefixVarMaskedOnRepair(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-sessionlocaltest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-sessionlocaltest-session"
	const canaryName = "VSCODE_TEST_CANARY"
	const canaryValue = "x"

	t.Cleanup(func() { killPrivateTmuxServer(socket) })

	worktreeDir := t.TempDir()

	// Bare session, as if an external tool (e.g. an editor) created it
	// directly with new-session -- kwt's create path never ran, so no
	// bootstrap has been applied yet.
	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	if _, err := runTmuxSocket(t, socket, append(append([]string(nil), os.Environ()...), "SHELL=/bin/sh"),
		createCmd...); err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}

	// Plant the canary in the SESSION's own environment table, not the
	// server-global one -- e.g. an editor terminal that created this
	// session captured it there directly.
	if _, err := runTmuxSocket(t, socket, nil, "set-environment", "-t", session, canaryName, canaryValue); err != nil {
		t.Fatalf("set-environment -t: %v", err)
	}

	sessionEnv, err := runTmuxSocket(t, socket, nil, "show-environment", "-t", session)
	if err != nil {
		t.Fatalf("show-environment -t: %v", err)
	}
	sessionDerived := CanonicalStripNames(ParseServerEnvNames(sessionEnv))
	if !containsName(sessionDerived, canaryName) {
		t.Fatalf("session-derived strip set %v should include %s", sessionDerived, canaryName)
	}

	bootCmd := BuildSessionBootstrapCommand(session, MergeStripNames(CanonicalStripExactNames(), sessionDerived))
	if _, err := runTmuxSocket(t, socket, nil, bootCmd...); err != nil {
		t.Fatalf("tmux %v: %v", bootCmd, err)
	}

	paneEnv := capturePaneEnv(t, socket, session, worktreeDir)
	if strings.Contains(paneEnv, canaryName+"=") {
		t.Errorf("new window leaked session-local %s after repair; got:\n%s", canaryName, paneEnv)
	}
}

// TestResolvedDefaultShellSurvivesUnsetSHELL confirms tmux's configured
// default-shell remains the source of truth when SHELL is absent from the
// launcher environment. The first-pane respawn queries and directly executes
// that resolved shell instead of expanding SHELL in another shell language.
func TestResolvedDefaultShellSurvivesUnsetSHELL(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-noshelltest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-noshelltest-session"

	t.Cleanup(func() { killPrivateTmuxServer(socket) })

	worktreeDir := t.TempDir()
	serverEnv := envWithoutName(os.Environ(), "SHELL")

	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	firstOut, err := runTmuxSocket(t, socket, serverEnv, createCmd...)
	if err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}
	firstPane := strings.TrimSpace(firstOut)

	respawnCmd := BuildFirstPaneRespawnCommand(firstPane, worktreeDir, tmuxDefaultShell(t, socket))
	if _, err := runTmuxSocket(t, socket, nil, respawnCmd...); err != nil {
		t.Fatalf("tmux %v: %v", respawnCmd, err)
	}

	paneEnv := dumpPaneEnv(t, socket, firstPane, worktreeDir, "noshell")
	if !strings.Contains(paneEnv, "PATH=") {
		t.Errorf("pane did not survive with SHELL unset; env dump:\n%s", paneEnv)
	}
}

// TestFirstPaneLoginShellSpawnsOnceAcrossCreate pins the create-path fix for
// double-running login profile scripts: new-session necessarily spawns the
// first pane before the session bootstrap's remove-markers exist, so that pane
// starts as the inert placeholder (FirstPanePlaceholderArgv) and only the
// post-bootstrap respawn starts the real login shell. Starting a login shell
// at new-session time instead would source the user's profile once under the
// launcher-polluted environment, be killed by respawn-pane -k, and source it
// again. Shell spawns are counted hermetically: SHELL points at a wrapper
// script inside t.TempDir() that appends every invocation (whatever its
// arguments) to an invocation log, appends to a count file only when invoked
// with -l (a login-shell spawn), and then execs a plain /bin/sh, so the
// user's real profile files are never touched or sourced. The placeholder
// must never reach the wrapper AT ALL: tmux runs a one-string pane command
// through the default shell as `$SHELL -c`, and even that non-interactive,
// non-login invocation fires user startup hooks (zsh sources .zshenv for
// every invocation; bash honors $BASH_ENV) before the remove-markers exist —
// so the placeholder is passed as separate argv words, which tmux execs
// directly with no shell. Between create and bootstrap the pane must have
// been started with the placeholder argv (asserted via pane_start_command,
// which tmux records unquoted for the multi-word argv form — the one-string
// form reads back quoted — and is deterministic, where the log files alone
// would race respawn-pane -k killing a not-yet-recorded shell); after the
// full create sequence the invocation log must show no shell ever ran the
// placeholder, the login count must be exactly one, and the respawned pane
// must not see the launcher's EDITOR. It only touches a private,
// uniquely-named socket and never attaches.
func TestFirstPaneLoginShellSpawnsOnceAcrossCreate(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-loginoncetest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-loginoncetest-session"

	t.Cleanup(func() { killPrivateTmuxServer(socket) })

	worktreeDir := t.TempDir()
	shellDir := t.TempDir()
	countFile := filepath.Join(shellDir, "login-spawns")
	invocationLog := filepath.Join(shellDir, "all-invocations")
	wrapper := filepath.Join(shellDir, "counting-shell")
	script := fmt.Sprintf(
		"#!/bin/sh\necho \"invoked: $*\" >> %q\nif [ \"$1\" = \"-c\" ]; then\n  exit 64\nfi\nif [ \"$1\" = \"-l\" ]; then\n  echo login >> %q\n  shift\nfi\nexec /bin/sh \"$@\"\n",
		invocationLog, countFile)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("writing counting shell wrapper: %v", err)
	}

	serverEnv := append(append([]string(nil), os.Environ()...),
		"SHELL="+wrapper, "EDITOR=launcher-editor")

	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	firstOut, err := runTmuxSocket(t, socket, serverEnv, createCmd...)
	if err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}
	firstPane := strings.TrimSpace(firstOut)

	// Before any bootstrap, the just-created pane must have been started with
	// the inert placeholder, not the login shell. pane_start_command is the
	// command new-session recorded at pane creation, so this is deterministic
	// and needs no polling — unlike pane_current_command, whose value depends
	// on whether the placeholder execs directly or a shell wraps it. tmux
	// formats the recorded command by quoting words that contain spaces, so
	// the multi-word argv form reads back verbatim and unquoted (`sleep
	// 2147483647`) while the shell-wrapped one-string form would read back
	// quoted (`"sleep 2147483647"`) — the unquoted form therefore also proves
	// tmux received separate argv words and execs them without a shell. A
	// login shell recorded here is exactly the pre-markers profile run the
	// placeholder exists to prevent.
	startOut, err := runTmuxSocket(t, socket, nil,
		"display-message", "-p", "-t", firstPane, "#{pane_start_command}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	wantStart := strings.Join(FirstPanePlaceholderArgv(), " ")
	if got := strings.TrimSpace(startOut); got != wantStart {
		t.Fatalf("first pane start command = %q, want the direct-exec placeholder argv %q",
			got, wantStart)
	}
	if _, err := os.Stat(countFile); err == nil {
		t.Fatal("a login shell spawned before the session bootstrap applied its remove-markers")
	}

	bootCmd := BuildSessionBootstrapCommand(session, CanonicalStripExactNames())
	if _, err := runTmuxSocket(t, socket, nil, bootCmd...); err != nil {
		t.Fatalf("tmux %v: %v", bootCmd, err)
	}
	respawnCmd := BuildFirstPaneRespawnCommand(firstPane, worktreeDir, tmuxDefaultShell(t, socket))
	if _, err := runTmuxSocket(t, socket, nil, respawnCmd...); err != nil {
		t.Fatalf("tmux %v: %v", respawnCmd, err)
	}

	// dumpPaneEnv doubles as the synchronization point: the pane answering the
	// env dump proves the wrapper has already run (it appends before exec'ing
	// the shell that answers), so reading the count file afterwards races with
	// nothing.
	firstEnv := dumpPaneEnv(t, socket, firstPane, worktreeDir, "loginonce")
	if paneEnvHasVar(firstEnv, "EDITOR") {
		t.Errorf("respawned first pane should not inherit the launcher's EDITOR; got:\n%s", firstEnv)
	}

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("pane responded but the login-shell wrapper never recorded a spawn: %v", err)
	}
	if got := strings.Count(string(data), "login"); got != 1 {
		t.Errorf("login shell spawned %d times during session create, want exactly 1; count file:\n%s",
			got, string(data))
	}

	// No shell of ANY kind may ever have run the placeholder — not just no
	// login shell. A `$SHELL -c 'sleep …'` invocation (the one-string pane
	// command form) fires zsh's .zshenv / bash's $BASH_ENV before the
	// remove-markers exist, which is exactly what the placeholder prevents.
	// The wrapper appends to the invocation log at startup, well before the
	// bootstrap round trips complete, so by this point (synchronized by the
	// pane answering the env dump above) any placeholder-wrapping shell would
	// have left its argv here.
	invocations, err := os.ReadFile(invocationLog)
	if err != nil {
		t.Fatalf("respawned login shell ran but the wrapper recorded no invocations: %v", err)
	}
	if got := strings.TrimSpace(string(invocations)); got != "invoked: -l" {
		t.Errorf("login shell invocation = %q, want one direct -l argv invocation", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(invocations)), "\n") {
		if strings.Contains(line, FirstPanePlaceholderArgv()[len(FirstPanePlaceholderArgv())-1]) {
			t.Errorf("a shell ran the placeholder (user startup hooks fired pre-markers): %q; full log:\n%s",
				line, string(invocations))
		}
	}
}

// TestSessionMarkerStripsEditorFromPaneEvenThoughServerExecKeepsIt confirms
// the session-facing half of the split policy (see serverStartExclusions and
// SanitizedEnviron in bootstrap.go): a server started with EDITOR set still
// produces panes that do not see EDITOR, because the session-scoped
// remove-marker strips it regardless of whether the server process itself
// keeps it. It only touches a private, uniquely-named socket and never
// attaches.
func TestSessionMarkerStripsEditorFromPaneEvenThoughServerExecKeepsIt(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	socket := fmt.Sprintf("kwt-editortest-%d-%d", os.Getpid(), time.Now().UnixNano())
	const session = "kwt-editortest-session"

	t.Cleanup(func() { killPrivateTmuxServer(socket) })

	worktreeDir := t.TempDir()
	serverEnv := append(append([]string(nil), os.Environ()...), "EDITOR=vim", "SHELL=/bin/sh")

	createCmd := BuildSessionCreateCommand(session, worktreeDir)
	if _, err := runTmuxSocket(t, socket, serverEnv, createCmd...); err != nil {
		t.Fatalf("tmux %v: %v", createCmd, err)
	}

	bootCmd := BuildSessionBootstrapCommand(session, CanonicalStripExactNames())
	if _, err := runTmuxSocket(t, socket, nil, bootCmd...); err != nil {
		t.Fatalf("tmux %v: %v", bootCmd, err)
	}

	paneEnv := capturePaneEnv(t, socket, session, worktreeDir)
	if paneEnvHasVar(paneEnv, "EDITOR") {
		t.Errorf("new pane should not inherit EDITOR via the session remove-marker; got:\n%s", paneEnv)
	}
}

// paneEnvHasVar reports whether dump (the output of `env` captured from a
// pane) has a line for exactly name, as opposed to merely containing
// "name=" as a substring of some other variable (e.g. "EDITOR=" is a
// substring of "GIT_EDITOR=", which env dumps commonly include and which
// kwt's strip list intentionally leaves alone).
func paneEnvHasVar(dump, name string) bool {
	prefix := name + "="
	for _, line := range strings.Split(dump, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// runTmuxSocket runs a tmux command against the given private socket with a
// bounded context timeout. A nil env leaves the subprocess environment as
// the test process's own.
func runTmuxSocket(t *testing.T, socket string, env []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	full := append([]string{"-L", socket}, args...)
	cmd := exec.CommandContext(ctx, "tmux", full...)
	if env != nil {
		cmd.Env = env
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func tmuxDefaultShell(t *testing.T, socket string) string {
	t.Helper()
	out, err := runTmuxSocket(t, socket, nil, "show-options", "-gv", "default-shell")
	if err != nil {
		t.Fatalf("show-options default-shell: %v", err)
	}
	shell := strings.TrimSpace(out)
	if shell == "" {
		t.Fatal("tmux returned an empty default-shell")
	}
	return shell
}

// capturePaneEnv spawns a new window in session (picking up the session's
// default-command, i.e. the login-shell bootstrap) and has it dump its
// process environment to a file, then reads that file back. It polls the
// filesystem rather than tmux, bounded by an overall deadline, so it never
// blocks indefinitely on the pane's shell starting up.
func capturePaneEnv(t *testing.T, socket, session, worktreeDir string) string {
	t.Helper()
	paneIDOut, err := runTmuxSocket(t, socket, nil,
		"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", session)
	if err != nil {
		t.Fatalf("new-window: %v", err)
	}
	paneID := strings.TrimSpace(paneIDOut)

	envFile := filepath.Join(worktreeDir, "pane-env.out")
	doneFile := envFile + ".done"
	dumpCmd := fmt.Sprintf("env > %q; touch %q", envFile, doneFile)
	if _, err := runTmuxSocket(t, socket, nil, "send-keys", "-t", paneID, "-l", "--", dumpCmd); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	if _, err := runTmuxSocket(t, socket, nil, "send-keys", "-t", paneID, "Enter"); err != nil {
		t.Fatalf("send-keys Enter: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(doneFile); err == nil {
			data, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatalf("reading %s: %v", envFile, err)
			}
			return string(data)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pane to dump its environment to %s", envFile)
	return ""
}

// dumpPaneEnv sends an env-dump command to an existing pane (identified by
// paneID) and reads the result back, polling the filesystem rather than tmux
// so it never blocks indefinitely. tag disambiguates the output files when
// several panes are sampled in one test.
func dumpPaneEnv(t *testing.T, socket, paneID, worktreeDir, tag string) string {
	t.Helper()
	envFile := filepath.Join(worktreeDir, "pane-env-"+tag+".out")
	doneFile := envFile + ".done"
	dumpCmd := fmt.Sprintf("env > %q; touch %q", envFile, doneFile)
	if _, err := runTmuxSocket(t, socket, nil, "send-keys", "-t", paneID, "-l", "--", dumpCmd); err != nil {
		t.Fatalf("send-keys to %s: %v", paneID, err)
	}
	if _, err := runTmuxSocket(t, socket, nil, "send-keys", "-t", paneID, "Enter"); err != nil {
		t.Fatalf("send-keys Enter to %s: %v", paneID, err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(doneFile); err == nil {
			data, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatalf("reading %s: %v", envFile, err)
			}
			return string(data)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pane %s to dump its environment to %s", paneID, envFile)
	return ""
}

// killPrivateTmuxServer kills the private tmux server on socket, ignoring
// errors from an already-dead server (e.g. a prior explicit kill-server call
// earlier in the test body).
func killPrivateTmuxServer(socket string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "tmux", "-L", socket, "kill-server").Run()
}
