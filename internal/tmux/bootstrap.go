package tmux

import (
	"sort"
	"strings"
)

// FirstPanePlaceholderArgv is the inert command the first pane runs between
// new-session and the post-bootstrap respawn. new-session necessarily spawns
// that pane before the session bootstrap's remove-markers can exist, so
// starting the real login shell there would source the user's profile scripts
// once under the launcher-polluted environment — running any side-effectful
// profile logic (history/session files, agents, one-shot hooks) against
// exactly the state the markers exist to remove — and then AGAIN after
// respawn-pane -k replaces it. It is an argv of separate words, not a single
// string, because the two forms spawn differently: tmux runs a one-string
// pane command through the session's default shell as `$SHELL -c`, and even
// that non-interactive, non-login invocation fires user startup hooks — zsh
// sources .zshenv for EVERY invocation and bash honors $BASH_ENV — before
// the remove-markers exist; with multiple words tmux execs the command
// directly and no shell runs at all. A near-infinite sleep (2147483647
// seconds; a plain integer because POSIX sleep takes only seconds — `sleep
// infinity` is a GNU extension) spawns no user shell, sources no profile,
// and touches no files; BuildFirstPaneRespawnCommand then starts tmux's
// resolved default shell directly exactly once, after the markers are in place.
// It returns a fresh slice per call so callers can append to it freely.
func FirstPanePlaceholderArgv() []string {
	return []string{"sleep", "2147483647"}
}

// canonicalStripExact and canonicalStripPrefixes are the single, shared
// definition of "launcher-state" environment variables kwt's tmux
// integration treats as scoped to the terminal/shell/tool that launched kwt,
// not to the workspace session tmux hosts. Two independent mechanisms
// consume this same definition so they cannot drift apart:
//   - SanitizedEnviron drops these variables from the environment kwt execs
//     tmux with, so a tmux server kwt spawns never captures them into its
//     global environment table in the first place — EXCEPT for
//     serverStartExclusions (EDITOR/VISUAL), which SanitizedEnviron
//     deliberately keeps; see that var's comment.
//   - StripEnvNames selects these variables for session-scoped remove-markers
//     (set-environment -r; see BuildSessionBootstrapCommands), which mask a
//     dirty global table for sessions created against an already-running
//     server. StripEnvNames uses the full set, unmodified by
//     serverStartExclusions: panes must not inherit EDITOR/VISUAL even
//     though the server process itself now keeps them.
//
// PWD/OLDPWD/SHLVL/_ are harmless to include here even though they are not
// terminal-integration variables: worktree directories are passed to tmux
// via -c, and shells re-derive PWD/OLDPWD/SHLVL/_ on their own, so a uniform
// list is preferred over a curated subset that has to be kept in sync by
// hand.
var canonicalStripExact = map[string]bool{
	"__CFBundleIdentifier": true,
	"EDITOR":               true,
	"KWT_FLEET_TOKEN":      true,
	"KWT_GITHUB_TOKEN":     true,
	"OLDPWD":               true,
	"PROMPT":               true,
	"PROMPT_COMMAND":       true,
	"PWD":                  true,
	"RPROMPT":              true,
	"SHLVL":                true,
	"TERM_PROGRAM":         true,
	"TERM_PROGRAM_VERSION": true,
	"VISUAL":               true,
	"WINDOWID":             true,
	"_":                    true,
}

// canonicalStripPrefixes is the prefix-match half of the shared definition
// described on canonicalStripExact.
var canonicalStripPrefixes = []string{
	"ALACRITTY_",
	"CONDA_",
	"FZF_",
	"ITERM",
	"KITTY_",
	"NVM_",
	"PYENV_",
	"STARSHIP_",
	"VIRTUAL_ENV",
	"WEZTERM_",
	"WT_",
	"VSCODE_",
}

// isCanonicalStripName reports whether name is in the shared launcher-state
// definition (canonicalStripExact/canonicalStripPrefixes).
func isCanonicalStripName(name string) bool {
	if canonicalStripExact[name] {
		return true
	}
	for _, prefix := range canonicalStripPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// serverStartExclusions is the explicit, commented carve-out from the
// canonical launcher-state definition for the exec-time SERVER sanitizer
// only (SanitizedEnviron). It is declared as a subtraction from the single
// canonical list — rather than a second, independent list — so the two
// policies (server-exec vs. session/pane marker) cannot silently drift apart
// on everything except this explicit exception.
//
// tmux reads EDITOR/VISUAL once, at server start, to pick its own default key
// mode (status-keys/mode-keys: vi vs. emacs), and a user's tmux.conf may
// branch on them too. Stripping EDITOR/VISUAL from the environment kwt execs
// tmux with would silently flip that behavior for every kwt-started server.
// The pane-facing goal — shells inside kwt sessions must not inherit the
// launcher's EDITOR/VISUAL — is already met by the session-scoped
// remove-markers (StripEnvNames/CanonicalStripExactNames), which still strip
// both; only the server process itself keeps them.
var serverStartExclusions = map[string]bool{
	"EDITOR": true,
	"VISUAL": true,
}

// isServerExecStripName reports whether name should be removed from the
// environment kwt execs tmux with (see SanitizedEnviron): the canonical
// launcher-state set, minus serverStartExclusions.
func isServerExecStripName(name string) bool {
	return isCanonicalStripName(name) && !serverStartExclusions[name]
}

// CanonicalStripExactNames returns the exact launcher-state variable names
// (canonicalStripExact) in sorted order. The session bootstrap installs a
// remove-marker for every one of these unconditionally: a marker for a variable
// that is absent from the server's global table is harmless, and installing
// them all covers stale variables a pre-existing server holds that kwt's own
// process environment lacks (e.g. an editor-launched server's leftovers).
func CanonicalStripExactNames() []string {
	names := make([]string, 0, len(canonicalStripExact))
	for name := range canonicalStripExact {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CanonicalStripNames returns the subset of names that match the shared
// launcher-state definition (canonicalStripExact/canonicalStripPrefixes),
// preserving input order and de-duplicating. It is used to select, from a
// server's global environment table, the prefix-matched variables (e.g.
// VSCODE_*) that a remove-marker must mask but that CanonicalStripExactNames
// cannot enumerate because the exact name is not known ahead of time.
func CanonicalStripNames(names []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		if isCanonicalStripName(name) {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// ParseServerEnvNames extracts variable names from the output of tmux
// show-environment (with or without -g). Each line is either "NAME=VALUE" for a
// set variable or "-NAME" for a variable the session marks removed; both forms
// yield NAME. Blank lines are ignored. It performs no I/O.
func ParseServerEnvNames(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if after, ok := strings.CutPrefix(line, "-"); ok {
			if after != "" {
				names = append(names, after)
			}
			continue
		}
		if name, _, ok := strings.Cut(line, "="); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// setServerEnvNames extracts only variables with values from tmux
// show-environment output. Session removal markers are intentionally ignored.
func setServerEnvNames(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		if name, _, ok := strings.Cut(line, "="); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// MergeStripNames unions several name lists into one, preserving first-seen
// order and de-duplicating. It is the single point where the unconditional
// exact keys, the launcher-derived names, and the server-derived prefix matches
// are combined into the final remove-marker set.
func MergeStripNames(sets ...[]string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, set := range sets {
		for _, name := range set {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// StripEnvNames returns the variable names present in env that the session
// bootstrap removes, preserving input order and de-duplicating. Each env entry
// is "NAME=VALUE"; only NAME is considered.
func StripEnvNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok {
			names = append(names, name)
		}
	}
	return CanonicalStripNames(names)
}

// SanitizedEnviron returns a copy of env with the shared launcher-state
// variables (canonicalStripExact/canonicalStripPrefixes) removed, EXCEPT for
// serverStartExclusions (EDITOR/VISUAL; see that var's comment), preserving
// the relative order of the remaining entries. It is used to exec every tmux
// invocation that can start a server (attach-class commands use
// AttachSanitizedEnviron instead): a tmux server spawned by
// kwt inherits whatever environment kwt execs it with into its GLOBAL
// environment table, which then leaks to every future pane of every session
// on that server, not just the session kwt is currently building — the
// session-scoped remove-markers StripEnvNames drives cannot fix a dirty
// server-global table for other or future sessions, only mask it per
// session. A tmux server started by kwt should not capture the transient
// launcher terminal's integration state; sessions outlive the launcher.
// EDITOR/VISUAL are the one exception: tmux itself consults them at server
// start, so they must survive into the server's exec environment even though
// panes still never see them (StripEnvNames still strips both).
// Entries without "=" are passed through unchanged (their name cannot be
// determined). It performs no I/O.
func SanitizedEnviron(env []string) []string {
	return filteredEnviron(env, isServerExecStripName)
}

// AttachSanitizedEnviron returns a copy of env with the FULL shared
// launcher-state set (canonicalStripExact/canonicalStripPrefixes) removed —
// no serverStartExclusions carve-out. It is the sanitizer for attach-class
// tmux commands (attach-session, switch-client), which never start a server,
// so tmux's server-start EDITOR/VISUAL key-mode detection is irrelevant to
// them. What IS relevant: tmux's update-environment session option copies the
// attaching CLIENT's value of each listed variable into the session
// environment table on attach. If a user configures update-environment to
// include EDITOR or VISUAL, an attach carrying them would override the
// remove-markers the session bootstrap installed and later panes would
// inherit the launcher's value again. A fully stripped client environment
// leaves nothing for update-environment to import. It performs no I/O.
func AttachSanitizedEnviron(env []string) []string {
	return filteredEnviron(env, isCanonicalStripName)
}

// filteredEnviron copies env, dropping entries whose name matches strip.
// Entries without "=" are passed through unchanged (their name cannot be
// determined). Relative order of the remaining entries is preserved.
func filteredEnviron(env []string, strip func(string) bool) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strip(name) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// BuildSessionBootstrapCommand returns the single tmux invocation that applies
// the session-level bootstrap: default-command for future windows and a
// session-scoped remove-marker for each stripped environment variable.
// set-environment -r masks the global/server value for this session only
// (unlike -u, which clears just the session table and lets the global value
// leak back through for new panes; -g would wrongly mutate the whole
// server). The sub-commands are joined with tmux's ";" argv separator so the
// whole bootstrap costs one subprocess instead of one per variable — the
// bootstrap runs on every attach, so ~20 sequential spawns would be user-visible
// latency. It performs no I/O.
func BuildSessionBootstrapCommand(session string, stripNames []string) []string {
	// An empty default-command tells tmux to start its configured
	// default-shell natively as a login shell. A non-empty string would be
	// interpreted by that shell with -c.
	cmd := []string{"set-option", "-t", session, "default-command", ""}
	for _, name := range stripNames {
		cmd = append(cmd, ";", "set-environment", "-t", session, "-r", name)
	}
	return cmd
}

const workspacePathOption = "@kwt-workspace-path"

// buildProtectedSessionBootstrapCommand records the workspace identity in the
// same pre-shell bootstrap transaction as the session environment policy.
func buildProtectedSessionBootstrapCommand(
	session, worktreeDir string,
	stripNames []string,
	updateEnvironment string,
) []string {
	cmd := BuildSessionBootstrapCommand(session, stripNames)
	cmd = append(
		cmd,
		";", "set-option", "-t", session, workspacePathOption, worktreeDir,
	)
	return append(
		cmd,
		";", "set-option", "-t", session, "update-environment", updateEnvironment,
	)
}
