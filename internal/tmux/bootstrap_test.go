package tmux

import (
	"reflect"
	"strings"
	"testing"

	"go.kenn.io/kwt/pkg/models"
)

func TestStripEnvNamesSelectsExactAndPrefix(t *testing.T) {
	env := []string{
		"EDITOR=vim",
		"VISUAL=code",
		"PATH=/usr/bin",
		"TERM_PROGRAM=Apple_Terminal",
		"TERM_PROGRAM_VERSION=447",
		"ITERM2_SESSION=abc",
		"VSCODE_INJECTION=1",
		"HOME=/home/u",
	}

	got := StripEnvNames(env)

	want := []string{
		"EDITOR",
		"VISUAL",
		"TERM_PROGRAM",
		"TERM_PROGRAM_VERSION",
		"ITERM2_SESSION",
		"VSCODE_INJECTION",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StripEnvNames() = %v, want %v", got, want)
	}
}

// TestStripEnvNamesCoversFullCanonicalSet pins StripEnvNames to the full
// canonical exact-keys+prefixes definition (canonicalStripExact/
// canonicalStripPrefixes in bootstrap.go), unmodified by
// serverStartExclusions: the session-table remove-markers must still strip
// EDITOR/VISUAL even though SanitizedEnviron (the exec-time sanitizer) now
// deliberately keeps them for the server process itself.
func TestStripEnvNamesCoversFullCanonicalSet(t *testing.T) {
	env := []string{
		"__CFBundleIdentifier=com.example.Terminal",
		"KWT_FLEET_TOKEN=secret",
		"KWT_GITHUB_TOKEN=secret",
		"OLDPWD=/old",
		"PROMPT=$ ",
		"PROMPT_COMMAND=update_prompt",
		"PWD=/here",
		"RPROMPT=%~",
		"SHLVL=3",
		"WINDOWID=42",
		"_=/usr/bin/env",
		"ALACRITTY_SOCKET=/tmp/sock",
		"CONDA_PREFIX=/opt/conda",
		"FZF_DEFAULT_OPTS=--height 40%",
		"ITERM_PROFILE=Default",
		"NVM_DIR=/home/u/.nvm",
		"PYENV_VERSION=3.13",
		"STARSHIP_SHELL=zsh",
		"VIRTUAL_ENV=/home/u/.venv",
		"WT_SESSION=abc-123",
		"PATH=/usr/bin",
		"LANG=en_US.UTF-8",
	}

	got := StripEnvNames(env)

	want := []string{
		"__CFBundleIdentifier",
		"KWT_FLEET_TOKEN",
		"KWT_GITHUB_TOKEN",
		"OLDPWD",
		"PROMPT",
		"PROMPT_COMMAND",
		"PWD",
		"RPROMPT",
		"SHLVL",
		"WINDOWID",
		"_",
		"ALACRITTY_SOCKET",
		"CONDA_PREFIX",
		"FZF_DEFAULT_OPTS",
		"ITERM_PROFILE",
		"NVM_DIR",
		"PYENV_VERSION",
		"STARSHIP_SHELL",
		"VIRTUAL_ENV",
		"WT_SESSION",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StripEnvNames() = %v, want %v", got, want)
	}
}

func TestCanonicalStripExactNamesIsSortedAndComplete(t *testing.T) {
	got := CanonicalStripExactNames()

	want := []string{
		"EDITOR", "KWT_FLEET_TOKEN", "KWT_GITHUB_TOKEN", "OLDPWD", "PROMPT", "PROMPT_COMMAND",
		"PWD", "RPROMPT", "SHLVL", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "VISUAL",
		"WINDOWID", "_", "__CFBundleIdentifier",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CanonicalStripExactNames() = %v, want %v", got, want)
	}
}

func TestCanonicalStripNamesFiltersToCanonicalAndDedups(t *testing.T) {
	got := CanonicalStripNames([]string{
		"VSCODE_STALE", "PATH", "STARSHIP_SESSION", "HOME", "EDITOR",
		"VSCODE_STALE", "LANG",
	})

	want := []string{"VSCODE_STALE", "STARSHIP_SESSION", "EDITOR"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CanonicalStripNames() = %v, want %v", got, want)
	}
}

func TestParseServerEnvNamesHandlesSetAndRemovedEntries(t *testing.T) {
	output := "PATH=/usr/bin\nVSCODE_STALE=1\n-TERM_PROGRAM\n\n-\nMALFORMED\n"

	got := ParseServerEnvNames(output)

	want := []string{"PATH", "VSCODE_STALE", "TERM_PROGRAM"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseServerEnvNames() = %v, want %v", got, want)
	}
}

func TestSetServerEnvNamesIgnoresRemovalMarkers(t *testing.T) {
	output := "KWT_GITHUB_TOKEN=secret\n-CUSTOM_FLEET_TOKEN\nPATH=/usr/bin\n"

	got := setServerEnvNames(output)

	want := []string{"KWT_GITHUB_TOKEN", "PATH"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("setServerEnvNames() = %v, want %v", got, want)
	}
}

func TestMergeStripNamesUnionsPreservingFirstSeenOrder(t *testing.T) {
	got := MergeStripNames(
		[]string{"EDITOR", "PWD"},
		[]string{"PWD", "ITERM2_SESSION"},
		[]string{"VSCODE_STALE", "EDITOR", ""},
	)

	want := []string{"EDITOR", "PWD", "ITERM2_SESSION", "VSCODE_STALE"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeStripNames() = %v, want %v", got, want)
	}
}

func TestStripEnvNamesIgnoresUnmatchedAndMalformed(t *testing.T) {
	got := StripEnvNames([]string{"PATH=/usr/bin", "MALFORMED", "=novalue"})
	if len(got) != 0 {
		t.Errorf("expected no matches, got %v", got)
	}
}

// TestSanitizedEnvironDropsExactKeys pins SanitizedEnviron's exact-match set
// (canonicalStripExact) exhaustively, one key at a time, so a future edit to
// the canonical list is caught by name rather than only in aggregate. EDITOR
// and VISUAL are deliberately excluded from this list: SanitizedEnviron keeps
// them (serverStartExclusions); see TestSanitizedEnvironPreservesEditorAndVisual.
func TestSanitizedEnvironDropsExactKeys(t *testing.T) {
	exactKeys := []string{
		"__CFBundleIdentifier", "KWT_GITHUB_TOKEN", "OLDPWD", "PROMPT", "PROMPT_COMMAND",
		"PWD", "RPROMPT", "SHLVL", "TERM_PROGRAM",
		"TERM_PROGRAM_VERSION", "WINDOWID", "_",
	}
	for _, key := range exactKeys {
		t.Run(key, func(t *testing.T) {
			got := SanitizedEnviron([]string{key + "=value", "PATH=/usr/bin"})
			want := []string{"PATH=/usr/bin"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("SanitizedEnviron() = %v, want %v", got, want)
			}
		})
	}
}

// TestSanitizedEnvironDropsPrefixedKeys pins SanitizedEnviron's prefix-match
// set (canonicalStripPrefixes) exhaustively, one prefix at a time.
func TestSanitizedEnvironDropsPrefixedKeys(t *testing.T) {
	prefixes := []string{
		"ALACRITTY_", "CONDA_", "FZF_", "ITERM", "KITTY_", "NVM_",
		"PYENV_", "STARSHIP_", "VIRTUAL_ENV", "WEZTERM_", "WT_", "VSCODE_",
	}
	for _, prefix := range prefixes {
		t.Run(prefix, func(t *testing.T) {
			entry := prefix + "SUFFIX=value"
			got := SanitizedEnviron([]string{entry, "PATH=/usr/bin"})
			want := []string{"PATH=/usr/bin"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("SanitizedEnviron(%q) = %v, want %v", entry, got, want)
			}
		})
	}
}

func TestSanitizedEnvironPreservesKwtOperationalVariables(t *testing.T) {
	env := []string{"KWT_HOME=/tmp/kwt", "KWT_LOG_LEVEL=debug", "PATH=/usr/bin"}

	got := SanitizedEnviron(env)

	if !reflect.DeepEqual(got, env) {
		t.Errorf("SanitizedEnviron() = %v, want %v", got, env)
	}
}

// TestSanitizedEnvironPreservesEditorAndVisual pins the split-policy fix:
// EDITOR/VISUAL are launcher-state for the session/pane remove-marker set
// (StripEnvNames still strips both; see TestStripEnvNamesCoversFullCanonicalSet),
// but SanitizedEnviron must NOT drop them from the environment kwt execs tmux
// with, because tmux itself reads them at server start to choose its default
// key mode. See serverStartExclusions in bootstrap.go.
func TestSanitizedEnvironPreservesEditorAndVisual(t *testing.T) {
	env := []string{"EDITOR=vim", "VISUAL=code", "PATH=/usr/bin"}

	got := SanitizedEnviron(env)

	if !reflect.DeepEqual(got, env) {
		t.Errorf("SanitizedEnviron() = %v, want %v (EDITOR/VISUAL must survive)", got, env)
	}
}

// TestAttachSanitizedEnvironDropsFullCanonicalSet pins the attach-side
// sanitizer variant: unlike SanitizedEnviron (which keeps EDITOR/VISUAL for
// tmux's own server-start key-mode detection), AttachSanitizedEnviron strips
// the FULL canonical launcher-state set with no exclusions. attach-class
// commands never start a server, and a user-configured update-environment
// containing EDITOR or VISUAL would copy the client's value back into the
// session table on attach, overriding the bootstrap's remove-markers — so
// the attaching client's environment must carry nothing importable.
func TestAttachSanitizedEnvironDropsFullCanonicalSet(t *testing.T) {
	env := []string{
		"EDITOR=vim", "VISUAL=code", "TERM_PROGRAM=Apple_Terminal",
		"VSCODE_INJECTION=1", "PATH=/usr/bin", "HOME=/home/u",
	}

	got := AttachSanitizedEnviron(env)

	want := []string{"PATH=/usr/bin", "HOME=/home/u"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachSanitizedEnviron() = %v, want %v", got, want)
	}
}

// TestAttachSanitizedEnvironMatchesCanonicalDefinition guards against the two
// sanitizer variants drifting: every name the session remove-marker set
// treats as launcher-state must also be dropped by the attach sanitizer.
func TestAttachSanitizedEnvironMatchesCanonicalDefinition(t *testing.T) {
	for _, name := range CanonicalStripExactNames() {
		got := AttachSanitizedEnviron([]string{name + "=value", "PATH=/usr/bin"})
		want := []string{"PATH=/usr/bin"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AttachSanitizedEnviron(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestSanitizedEnvironKeepsUnrelatedVars(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/u", "LANG=en_US.UTF-8", "SHELL=/bin/zsh"}

	got := SanitizedEnviron(env)

	if !reflect.DeepEqual(got, env) {
		t.Errorf("SanitizedEnviron() = %v, want %v (all kept)", got, env)
	}
}

// TestTERMINFOSurvivesSanitization pins the reviewed decision that TERMINFO is
// functional terminal configuration, not transient launcher-integration
// state: users with a custom terminfo database (e.g. a terminal that installs
// its own terminfo entries) need it preserved for tmux attach rendering and
// for pane applications resolving tmux's own TERM. It must not be stripped by
// either the exec-time sanitizer or the session-scoped remove-marker set.
func TestTERMINFOSurvivesSanitization(t *testing.T) {
	env := []string{"TERMINFO=/usr/share/terminfo", "PATH=/usr/bin"}

	if got := SanitizedEnviron(env); !reflect.DeepEqual(got, env) {
		t.Errorf("SanitizedEnviron() = %v, want %v (TERMINFO must survive)", got, env)
	}
	if got := StripEnvNames(env); len(got) != 0 {
		t.Errorf("StripEnvNames() = %v, want empty (TERMINFO must not be a remove-marker candidate)", got)
	}
}

func TestSanitizedEnvironEmptyInput(t *testing.T) {
	if got := SanitizedEnviron(nil); len(got) != 0 {
		t.Errorf("SanitizedEnviron(nil) = %v, want empty", got)
	}
	if got := SanitizedEnviron([]string{}); len(got) != 0 {
		t.Errorf("SanitizedEnviron([]string{}) = %v, want empty", got)
	}
}

// TestSanitizedEnvironKeepsMalformedEntries documents that entries without an
// "=" (which StripEnvNames simply ignores when collecting names) are passed
// through unchanged rather than dropped: SanitizedEnviron cannot determine
// their name, and os/exec.Cmd.Env tolerates malformed entries the same way
// os.Environ() does.
func TestSanitizedEnvironKeepsMalformedEntries(t *testing.T) {
	env := []string{"MALFORMED", "PATH=/usr/bin"}

	got := SanitizedEnviron(env)

	if !reflect.DeepEqual(got, env) {
		t.Errorf("SanitizedEnviron() = %v, want %v", got, env)
	}
}

// A resolved non-POSIX shell must be passed as separate argv words. tmux runs
// a single command string through default-shell -c, which would ask fish or
// tcsh to interpret shell-specific syntax and would run an extra startup hook.
func TestBuildFirstPaneRespawnCommandUsesResolvedNonPOSIXShellDirectly(t *testing.T) {
	got := BuildFirstPaneRespawnCommand("%1", "/wt", "/usr/bin/fish")
	want := []string{"respawn-pane", "-k", "-c", "/wt", "-t", "%1", "/usr/bin/fish", "-l"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildFirstPaneRespawnCommand() = %q, want %q", got, want)
	}
}

// envWithoutName returns a copy of env with every entry named name removed,
// preserving order. It performs no I/O.
func envWithoutName(env []string, name string) []string {
	out := make([]string, 0, len(env))
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func TestBuildSessionBootstrapCommand(t *testing.T) {
	got := BuildSessionBootstrapCommand("s", []string{"EDITOR", "ITERM2_SESSION"})

	want := []string{
		"set-option", "-t", "s", "default-command", "",
		";", "set-environment", "-t", "s", "-r", "EDITOR",
		";", "set-environment", "-t", "s", "-r", "ITERM2_SESSION",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildSessionBootstrapCommand() = %v, want %v", got, want)
	}
}

// TestBuildSessionCreateCommandUsesInertPlaceholder pins that the first pane
// spawns with the inert placeholder, NOT the login shell: new-session runs
// before the session bootstrap's remove-markers can exist, so a login shell
// here would source the user's profile scripts under the launcher-polluted
// environment and then again after the post-bootstrap respawn. The
// placeholder words must be separate trailing arguments — collapsing them
// into one string makes tmux wrap the placeholder in `$SHELL -c`, firing
// user startup hooks (zsh .zshenv, bash $BASH_ENV) pre-markers.
func TestBuildSessionCreateCommandUsesInertPlaceholder(t *testing.T) {
	got := BuildSessionCreateCommand("s", "/wt")

	want := []string{
		"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "s", "-c", "/wt", "sleep", "2147483647",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildSessionCreateCommand() = %v, want %v", got, want)
	}
}

// TestFirstPanePlaceholderArgvIsPortablyInert pins the placeholder's form:
// plain POSIX sleep with integer seconds (`sleep infinity` is a GNU
// extension), split into separate argv words so tmux execs it directly — no
// shell, no startup-hook sourcing, no file I/O.
func TestFirstPanePlaceholderArgvIsPortablyInert(t *testing.T) {
	want := []string{"sleep", "2147483647"}
	if !reflect.DeepEqual(FirstPanePlaceholderArgv(), want) {
		t.Errorf("FirstPanePlaceholderArgv() = %v, want %v",
			FirstPanePlaceholderArgv(), want)
	}
}

func TestBuildRemainingPaneSequenceSinglePaneIsEmpty(t *testing.T) {
	layout := models.Layout{Name: "none", Panes: []string{""}}

	got := BuildRemainingPaneSequence("s", "/wt", layout)

	if len(got) != 0 {
		t.Errorf("BuildRemainingPaneSequence() single-pane = %v, want empty", got)
	}
}

func TestBuildRemainingPaneSequenceSplitsThenArranges(t *testing.T) {
	layout := models.Layout{Arrange: "even-horizontal", Panes: []string{"a", "b", "c"}}

	got := BuildRemainingPaneSequence("s", "/wt", layout)

	want := [][]string{
		{"split-window", "-P", "-F", "#{pane_id}", "-t", "s", "-c", "/wt"},
		{"split-window", "-P", "-F", "#{pane_id}", "-t", "s", "-c", "/wt"},
		{"select-layout", "-t", "s", "even-horizontal"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildRemainingPaneSequence() = %v, want %v", got, want)
	}
}
