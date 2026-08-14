package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
)

// TestNewCmdSanitizesEnvironment pins the exec seam every TmuxCommand method
// funnels through (newCmd): the *exec.Cmd it builds carries a sanitized copy
// of the process environment, not the raw os.Environ(), so a tmux server
// spawned by kwt does not inherit launcher-state variables into its global
// environment table. It sets a canonical-strip variable via t.Setenv (which
// os.Environ() picks up) and confirms newCmd's Env omits it while keeping an
// unrelated variable. It uses TERM_PROGRAM rather than EDITOR/VISUAL, which
// are the one deliberate exception to exec-time stripping (serverStartExclusions
// in bootstrap.go; see TestNewCmdPreservesEditorAndVisual).
func TestNewCmdSanitizesEnvironment(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv("KWT_GITHUB_TOKEN", "secret")
	t.Setenv("KWT_HOME", "/tmp/kwt")
	t.Setenv("UNRELATED_VAR", "keep-me")

	tc := NewTmuxCommand("tmux")
	cmd := tc.newCmd(context.Background(), []string{"has-session", "-t", "x"})

	for _, entry := range cmd.Env {
		if hasEnvName(entry, "TERM_PROGRAM") {
			t.Errorf("newCmd().Env leaked TERM_PROGRAM: %v", cmd.Env)
		}
		if hasEnvName(entry, "KWT_GITHUB_TOKEN") {
			t.Errorf("newCmd().Env leaked GitHub credential: %v", cmd.Env)
		}
	}
	foundUnrelated, foundKwtHome := false, false
	for _, entry := range cmd.Env {
		if hasEnvName(entry, "UNRELATED_VAR") {
			foundUnrelated = true
		}
		if hasEnvName(entry, "KWT_HOME") {
			foundKwtHome = true
		}
	}
	if !foundUnrelated {
		t.Errorf("newCmd().Env dropped unrelated var: %v", cmd.Env)
	}
	if !foundKwtHome {
		t.Errorf("newCmd().Env dropped KWT_HOME: %v", cmd.Env)
	}
}

func TestNewCmdStripsConfiguredSensitiveEnvironmentName(t *testing.T) {
	t.Setenv("Custom_Fleet_Token", "secret")
	t.Setenv("UNRELATED_VAR", "keep-me")

	tc := NewTmuxCommandWithStripNames(
		"tmux",
		[]string{"custom_fleet_token"},
	)
	cmd := tc.newCmd(
		context.Background(),
		[]string{"has-session", "-t", "x"},
	)

	for _, entry := range cmd.Env {
		if hasEnvName(entry, "Custom_Fleet_Token") {
			t.Errorf("newCmd().Env leaked configured credential: %v", cmd.Env)
		}
	}
}

func TestProtectedSessionProbeCommandStripsConfiguredCredential(t *testing.T) {
	t.Setenv("GHOSTHUB_AUTH", "secret")

	command := newProtectedSessionProbeCommand(
		"kwt-pr-0123456789abcdef", []string{"GHOSTHUB_AUTH"},
	).newCmd(context.Background(), []string{"list-sessions"})

	for _, entry := range command.Env {
		if hasEnvName(entry, "GHOSTHUB_AUTH") {
			t.Fatalf("protected-session probe leaked configured credential: %v", command.Env)
		}
	}
}

func TestSocketCommandPrefixesEveryInvocationWithSocketName(t *testing.T) {
	tc := NewTmuxCommandForSocketWithStripNames(
		"tmux",
		"kwt-pr-0123456789abcdef",
		[]string{"KWT_GITHUB_TOKEN"},
	)

	cmd := tc.newCmd(
		context.Background(),
		[]string{"has-session", "-t", "workspace"},
	)
	attach := tc.newAttachCmd(
		context.Background(),
		[]string{"attach-session", "-t", "workspace"},
	)

	wantPrefix := []string{"tmux", "-L", "kwt-pr-0123456789abcdef"}
	if len(cmd.Args) < len(wantPrefix) || !slices.Equal(cmd.Args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("newCmd args = %v, want prefix %v", cmd.Args, wantPrefix)
	}
	if len(attach.Args) < len(wantPrefix) || !slices.Equal(attach.Args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("newAttachCmd args = %v, want prefix %v", attach.Args, wantPrefix)
	}
}

func TestDefaultSocketCommandUsesExplicitTempDirectory(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/ambient/tmux")
	t.Setenv("TMUX", "/ambient/tmux/default,123,0")
	tc := NewTmuxCommandInTempDir("tmux", "/chosen/tmux")

	cmd := tc.newCmd(context.Background(), []string{"list-sessions"})

	if !slices.Contains(cmd.Env, "TMUX_TMPDIR=/chosen/tmux") {
		t.Fatalf("newCmd environment = %v, want explicit TMUX_TMPDIR", cmd.Env)
	}
	for _, entry := range cmd.Env {
		if entry == "TMUX_TMPDIR=/ambient/tmux" {
			t.Fatalf("newCmd environment retained ambient TMUX_TMPDIR: %v", cmd.Env)
		}
		if hasEnvName(entry, "TMUX") {
			t.Fatalf("newCmd environment retained ambient TMUX: %v", cmd.Env)
		}
	}
}

func TestUnqualifiedRemovalGuardUsesCanonicalDefaultSocket(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/ambient/tmux")
	t.Setenv("TMUX", "/ambient/tmux/named,123,0")
	command := newRemovalTmuxCommand("tmux", RemovalSessionCondition{
		SessionName: "workspace",
		Absent:      true,
	})

	cmd := command.newCmd(context.Background(), []string{"list-sessions"})
	for _, entry := range cmd.Env {
		if hasEnvName(entry, "TMUX") || hasEnvName(entry, "TMUX_TMPDIR") {
			t.Fatalf("unqualified removal inherited ambient socket selector: %v", cmd.Env)
		}
	}
}

func TestRemovalCommandsStripRequestProtectedNames(t *testing.T) {
	t.Setenv("CUSTOM_FLEET_TOKEN", "secret")
	condition := RemovalSessionCondition{
		SessionName:    "workspace",
		Absent:         true,
		ProtectedNames: []string{"CUSTOM_FLEET_TOKEN"},
	}

	for _, test := range []struct {
		name      string
		condition RemovalSessionCondition
	}{
		{name: "default socket", condition: condition},
		{
			name: "named socket",
			condition: func() RemovalSessionCondition {
				value := condition
				value.SocketName = "protected"
				return value
			}(),
		},
		{
			name: "named socket in explicit directory",
			condition: func() RemovalSessionCondition {
				value := condition
				value.SocketName = "protected"
				value.SocketDirectory = "/run/user/1000/tmux"
				return value
			}(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := newRemovalTmuxCommand("tmux", test.condition)
			cmd := command.newCmd(context.Background(), []string{"list-sessions"})
			for _, entry := range cmd.Env {
				if hasEnvName(entry, "CUSTOM_FLEET_TOKEN") {
					t.Fatalf("removal command retained configured credential: %v", cmd.Env)
				}
			}
		})
	}
}

// TestNewCmdBackgroundContextSanitizes confirms newCmd sanitizes the
// environment for the context-free call sites (runCommand, runCommandOutput),
// which pass context.Background(). It uses VSCODE_INJECTION
// (a prefix match) rather than EDITOR/VISUAL, which are the one deliberate
// exception to exec-time stripping (serverStartExclusions in bootstrap.go;
// see TestNewCmdPreservesEditorAndVisual).
func TestNewCmdBackgroundContextSanitizes(t *testing.T) {
	t.Setenv("VSCODE_INJECTION", "1")

	tc := NewTmuxCommand("tmux")
	cmd := tc.newCmd(context.Background(), []string{"has-session", "-t", "x"})

	if cmd == nil {
		t.Fatal("newCmd(...) returned nil cmd")
	}
	for _, entry := range cmd.Env {
		if hasEnvName(entry, "VSCODE_INJECTION") {
			t.Errorf("newCmd(...).Env leaked VSCODE_INJECTION: %v", cmd.Env)
		}
	}
}

// TestNewCmdPreservesEditorAndVisual pins the split-policy fix at the exec
// seam: EDITOR/VISUAL must survive into the environment kwt execs tmux with
// (serverStartExclusions in bootstrap.go), because tmux itself reads them at
// server start to pick its default key mode. The pane-facing goal — shells
// inside kwt sessions do not inherit them — is covered separately by the
// session-scoped remove-markers (see BuildSessionBootstrapCommands), not by
// this exec-time sanitizer.
func TestNewCmdPreservesEditorAndVisual(t *testing.T) {
	t.Setenv("EDITOR", "vim")
	t.Setenv("VISUAL", "code")

	tc := NewTmuxCommand("tmux")
	cmd := tc.newCmd(context.Background(), []string{"has-session", "-t", "x"})

	foundEditor, foundVisual := false, false
	for _, entry := range cmd.Env {
		if hasEnvName(entry, "EDITOR") {
			foundEditor = true
		}
		if hasEnvName(entry, "VISUAL") {
			foundVisual = true
		}
	}
	if !foundEditor {
		t.Errorf("newCmd().Env dropped EDITOR: %v", cmd.Env)
	}
	if !foundVisual {
		t.Errorf("newCmd().Env dropped VISUAL: %v", cmd.Env)
	}
}

// TestNewAttachCmdStripsEditorAndVisual pins the attach-side exec seam: the
// environment kwt execs attach-class tmux commands with must carry NO
// launcher-state variables at all — including EDITOR/VISUAL, which the
// server-start sanitizer deliberately keeps. tmux's update-environment
// session option, when a user configures it to include EDITOR or VISUAL,
// copies the attaching CLIENT's value into the session environment table on
// attach-session, overriding the remove-marker the bootstrap installed. A
// fully stripped client environment leaves nothing to import.
func TestNewAttachCmdStripsEditorAndVisual(t *testing.T) {
	t.Setenv("EDITOR", "vim")
	t.Setenv("VISUAL", "code")
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv("UNRELATED_VAR", "keep-me")

	tc := NewTmuxCommand("tmux")
	cmd := tc.newAttachCmd(context.Background(), []string{"attach-session", "-t", "x"})

	foundUnrelated := false
	for _, entry := range cmd.Env {
		if hasEnvName(entry, "EDITOR") {
			t.Errorf("newAttachCmd().Env leaked EDITOR: %v", cmd.Env)
		}
		if hasEnvName(entry, "VISUAL") {
			t.Errorf("newAttachCmd().Env leaked VISUAL: %v", cmd.Env)
		}
		if hasEnvName(entry, "TERM_PROGRAM") {
			t.Errorf("newAttachCmd().Env leaked TERM_PROGRAM: %v", cmd.Env)
		}
		if hasEnvName(entry, "UNRELATED_VAR") {
			foundUnrelated = true
		}
	}
	if !foundUnrelated {
		t.Errorf("newAttachCmd().Env dropped unrelated var: %v", cmd.Env)
	}
}

// TestAttachPathsUseFullStripSanitizer pins the two attach-class code paths
// (AttachSession, SwitchClient) to the full-strip exec seam: the commands
// they build must carry no EDITOR/VISUAL, unlike server-start commands.
// This is the seam-level substitute for a real attach round-trip, which
// would require a tty.
func TestAttachPathsUseFullStripSanitizer(t *testing.T) {
	t.Setenv("EDITOR", "vim")
	t.Setenv("VISUAL", "code")

	tc := NewTmuxCommand("tmux")
	cmds := map[string][]string{
		"attach-session": tc.attachSessionCmd("x").Args,
		"switch-client":  tc.switchClientCmd("x").Args,
	}
	envs := map[string][]string{
		"attach-session": tc.attachSessionCmd("x").Env,
		"switch-client":  tc.switchClientCmd("x").Env,
	}
	for name, args := range cmds {
		wantArgs := []string{"tmux", name, "-t", "x"}
		if len(args) != len(wantArgs) || args[1] != name || args[2] != "-t" || args[3] != "x" {
			t.Errorf("%s cmd args = %v, want %v", name, args, wantArgs)
		}
		for _, entry := range envs[name] {
			if hasEnvName(entry, "EDITOR") || hasEnvName(entry, "VISUAL") {
				t.Errorf("%s cmd env leaked EDITOR/VISUAL: %v", name, envs[name])
			}
		}
	}
}

func TestProtectedAttachDisablesTmuxEnvironmentUpdate(t *testing.T) {
	tc := NewTmuxCommandForSocketWithStripNames(
		"tmux",
		"kwt-pr-0123456789abcdef",
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
	)

	cmd := tc.attachSessionWithoutEnvironmentCmd(
		context.Background(),
		"workspace",
	)

	want := []string{
		"tmux", "-L", "kwt-pr-0123456789abcdef",
		"attach-session", "-E", "-t", "workspace",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("protected attach args = %v, want %v", cmd.Args, want)
	}
}

func TestProtectedAttachStripsParentTmuxIdentity(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")
	t.Setenv("TMUX_PANE", "%7")
	t.Setenv("UNRELATED_VAR", "keep-me")

	tc := NewTmuxCommandForSocketWithStripNames(
		"tmux",
		"kwt-pr-0123456789abcdef",
		nil,
	)
	cmd := tc.attachSessionWithoutEnvironmentCmd(
		context.Background(),
		"workspace",
	)

	foundUnrelated := false
	for _, entry := range cmd.Env {
		if hasEnvName(entry, "TMUX") {
			t.Error("protected attach leaked TMUX")
		}
		if hasEnvName(entry, "TMUX_PANE") {
			t.Error("protected attach leaked TMUX_PANE")
		}
		if hasEnvName(entry, "UNRELATED_VAR") {
			foundUnrelated = true
		}
	}
	if !foundUnrelated {
		t.Error("protected attach dropped UNRELATED_VAR")
	}
}

func TestNamedSocketCommandsIgnoreAmbientTmuxTempDirectory(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/tmp/caller-specific-tmux")

	tc := NewTmuxCommandForSocketWithStripNames(
		"tmux",
		"kwt-pr-0123456789abcdef",
		nil,
	)
	commands := []*exec.Cmd{
		tc.newCmd(context.Background(), []string{"has-session", "-t", "workspace"}),
		tc.newAttachCmd(context.Background(), []string{"attach-session", "-t", "workspace"}),
	}
	for _, command := range commands {
		for _, entry := range command.Env {
			if hasEnvName(entry, "TMUX_TMPDIR") {
				t.Fatalf("named socket command inherited TMUX_TMPDIR")
			}
		}
	}
}

func TestLegacyNamedSocketCommandUsesExplicitTmuxTempDirectory(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/tmp/unrelated")
	tempDir := "/tmp/legacy-tmux"

	tc := NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux", "kwt-pr-0123456789abcdef", tempDir, nil,
	)
	command := tc.newCmd(context.Background(), []string{"list-sessions"})

	found := false
	for _, entry := range command.Env {
		if entry == "TMUX_TMPDIR="+tempDir {
			found = true
		}
		if entry == "TMUX_TMPDIR=/tmp/unrelated" {
			t.Fatalf("legacy socket command inherited ambient TMUX_TMPDIR: %v", command.Env)
		}
	}
	if !found {
		t.Fatalf("legacy socket command omitted explicit TMUX_TMPDIR: %v", command.Env)
	}
}

func TestExternalAttachReplacesCurrentProcess(t *testing.T) {
	t.Setenv("EDITOR", "vim")
	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")
	t.Setenv("TMUX_PANE", "%7")
	t.Setenv("KWT_GITHUB_TOKEN", "secret")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	replacementErr := errors.New("replacement attempted")

	tests := []struct {
		name     string
		attach   func(*TmuxCommand) error
		wantArgs []string
		wantGone []string
	}{
		{
			name: "ordinary",
			attach: func(tc *TmuxCommand) error {
				return tc.AttachSession("workspace")
			},
			wantArgs: []string{
				executable, "-L", "kwt-test-socket",
				"attach-session", "-t", "workspace",
			},
			wantGone: []string{"EDITOR", "KWT_GITHUB_TOKEN"},
		},
		{
			name: "protected",
			attach: func(tc *TmuxCommand) error {
				return tc.AttachSessionWithoutEnvironment(
					context.Background(),
					"workspace",
				)
			},
			wantArgs: []string{
				executable, "-L", "kwt-test-socket",
				"attach-session", "-E", "-t", "workspace",
			},
			wantGone: []string{
				"EDITOR", "TMUX", "TMUX_PANE", "KWT_GITHUB_TOKEN",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := NewTmuxCommandForSocketWithStripNames(
				executable,
				"kwt-test-socket",
				[]string{"KWT_GITHUB_TOKEN"},
			)
			var got *exec.Cmd
			tc.attachProcess = func(cmd *exec.Cmd) error {
				got = cmd
				return replacementErr
			}

			err := tt.attach(tc)
			if !errors.Is(err, replacementErr) {
				t.Fatalf(
					"attach error = %v, want replacement error",
					err,
				)
			}
			if got == nil {
				t.Fatal("attachment did not attempt process replacement")
			}
			if !slices.Equal(got.Args, tt.wantArgs) {
				t.Errorf(
					"replacement args = %v, want %v",
					got.Args,
					tt.wantArgs,
				)
			}
			for _, name := range tt.wantGone {
				for _, entry := range got.Env {
					if hasEnvName(entry, name) {
						t.Errorf(
							"replacement environment leaked %s",
							name,
						)
					}
				}
			}
		})
	}
}

func TestExternalAttachReturnsLookupErrorBeforeReplacement(t *testing.T) {
	tc := NewTmuxCommand("kwt-test-missing-tmux-executable")
	called := false
	tc.attachProcess = func(*exec.Cmd) error {
		called = true
		return nil
	}

	err := tc.AttachSession("workspace")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("attach error = %v, want executable-not-found", err)
	}
	if called {
		t.Fatal("process replacement called after command lookup failed")
	}
}

func TestProtectedExternalAttachReturnsCanceledContextBeforeReplacement(
	t *testing.T,
) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tc := NewTmuxCommand(executable)
	called := false
	tc.attachProcess = func(*exec.Cmd) error {
		called = true
		return nil
	}

	err = tc.AttachSessionWithoutEnvironment(ctx, "workspace")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("attach error = %v, want context canceled", err)
	}
	if called {
		t.Fatal("process replacement called after context cancellation")
	}
}

func hasEnvName(entry, name string) bool {
	return len(entry) > len(name) && entry[:len(name)] == name && entry[len(name)] == '='
}
