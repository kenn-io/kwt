package tmux

import (
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kwt/pkg/models"
)

// ValidArranges returns the set of tmux select-layout presets kwt accepts.
func ValidArranges() map[string]bool {
	return map[string]bool{
		"even-horizontal": true,
		"even-vertical":   true,
		"tiled":           true,
		"main-vertical":   true,
		"main-horizontal": true,
	}
}

// BlankLayoutName is the reserved layout name for the blank session. It is
// valid anywhere a layout name is accepted and cannot name a preset.
const BlankLayoutName = "none"

// BlankLayout returns the implicit single-pane, plain-login-shell layout used
// when no preset is selected.
func BlankLayout() models.Layout {
	return models.Layout{Name: BlankLayoutName, Panes: []string{""}}
}

// BuildSessionCreateCommand returns the new-session invocation that creates the
// session and its first pane. It prints the pane's stable ID (via -P -F
// '#{pane_id}'), which the runner captures to target the pane by ID. The pane
// runs the inert placeholder, NOT the login-shell bootstrap: it necessarily
// spawns before the session bootstrap's remove-markers can exist, and a login
// shell started here would source the user's profile scripts under the
// launcher-polluted environment only to be killed and re-sourced by the
// post-bootstrap respawn (see FirstPanePlaceholderArgv and
// BuildFirstPaneRespawnCommand). The placeholder's words are appended as
// separate arguments so tmux execs it directly instead of wrapping it in
// `$SHELL -c`, which would fire user startup hooks (zsh .zshenv, bash
// $BASH_ENV) pre-markers. tmux sets TERM_PROGRAM/TERM_PROGRAM_VERSION
// itself in every pane, so kwt does not inject them. It performs no I/O.
//
// It is deliberately separate from BuildRemainingPaneSequence so the runner
// can apply the session bootstrap (default-command + remove-markers) after the
// first pane exists but before the remaining panes are spawned: the strips
// must be in place before split-window creates any further pane, or those
// panes inherit a dirty server-global environment table.
func BuildSessionCreateCommand(session, worktreeDir string) []string {
	return append([]string{
		"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", session, "-c", worktreeDir,
	}, FirstPanePlaceholderArgv()...)
}

// BuildFirstPaneRespawnCommand returns the respawn-pane invocation that
// replaces the first pane's inert placeholder with the real login shell after
// the session bootstrap has applied. The first pane necessarily spawns before
// the session exists to carry remove-markers (new-session creates both at
// once), when the server-global environment table still holds launcher state —
// the launcher's EDITOR/VISUAL on a server kwt just started (kept there for
// tmux's server-start key-mode detection; see serverStartExclusions), or the
// whole dirty table of a pre-existing server — so it starts as the
// FirstPanePlaceholderArgv sleep and the login shell first spawns here, from the
// same stripped session environment as every later pane, its profile scripts
// sourced exactly once. -k kills the placeholder (an inert sleep; nothing has
// been sent to the pane), -c re-anchors the worktree directory, and the pane
// ID is stable across respawn, so the captured ID remains valid for the
// pane-command sequence. The explicit shell argv is load-bearing:
// respawn-pane without a command re-runs the pane's original command — the
// placeholder — not the session's default-command. Separate argv words
// (shell, then -l) make tmux execute the configured shell directly as a login
// shell instead of wrapping a command string in default-shell -c: non-POSIX
// shells such as fish and tcsh never have to interpret POSIX parameter
// expansion, and startup hooks from an extra non-login shell do not run. It
// performs no I/O.
func BuildFirstPaneRespawnCommand(paneID, worktreeDir, shell string) []string {
	return []string{"respawn-pane", "-k", "-c", worktreeDir, "-t", paneID, shell, "-l"}
}

// buildFirstPaneCommandRespawnCommand replaces the inert first-pane
// placeholder with a caller-supplied command after the session bootstrap has
// installed its environment removal markers. command remains one argv value
// so tmux preserves new-session's command-string semantics and runs it through
// the configured default shell.
func buildFirstPaneCommandRespawnCommand(
	paneID, worktreeDir, command string,
) []string {
	return []string{
		"respawn-pane", "-k", "-c", worktreeDir, "-t", paneID, command,
	}
}

// BuildRemainingPaneSequence returns the tmux invocations that create the
// panes after the first (one split-window per extra pane) and arrange them
// (select-layout, emitted only for multi-pane layouts). Each split-window
// prints the new pane's stable ID; select-layout prints nothing. Every pane
// runs the login-shell bootstrap. The runner issues this sequence only after
// BuildSessionCreateCommand and the session bootstrap have run, so the panes
// created here start from an already-stripped session environment. It performs
// no I/O.
func BuildRemainingPaneSequence(session, worktreeDir string, layout models.Layout) [][]string {
	seq := make([][]string, 0, len(layout.Panes))
	for i := 1; i < len(layout.Panes); i++ {
		seq = append(seq,
			[]string{"split-window", "-P", "-F", "#{pane_id}", "-t", session, "-c", worktreeDir})
	}
	if len(layout.Panes) > 1 {
		seq = append(seq, []string{"select-layout", "-t", session, layout.Arrange})
	}
	return seq
}

// BuildPaneCommandSequence returns the tmux invocations that run each pane's
// command and set focus, given the captured pane IDs. paneIDs is in pane
// creation order with one entry per element of panes (len(paneIDs) ==
// len(panes)); paneIDs[i] is the ID of the pane for panes[i]. An empty command
// leaves that pane a plain login shell. It performs no I/O.
func BuildPaneCommandSequence(paneIDs, panes []string) [][]string {
	seq := make([][]string, 0, len(panes)*2+1)
	for i, cmd := range panes {
		if cmd == "" {
			continue
		}
		seq = append(seq,
			[]string{"send-keys", "-t", paneIDs[i], "-l", "--", cmd},
			[]string{"send-keys", "-t", paneIDs[i], "Enter"},
		)
	}
	if len(paneIDs) > 0 {
		seq = append(seq, []string{"select-pane", "-t", paneIDs[0]})
	}
	return seq
}

// ValidateLayouts checks arrange names, non-empty panes, agent references,
// the reserved blank name, and that a non-blank default resolves to a preset.
// Zero presets is valid: the blank session needs no configuration. Called
// before any workspace launch.
func ValidateLayouts(cfg models.LayoutsConfig, agents map[string]string) error {
	valid := ValidArranges()
	names := make(map[string]bool, len(cfg.Presets))
	for _, p := range cfg.Presets {
		if p.Name == BlankLayoutName {
			return fmt.Errorf("layout name %q is reserved for the blank session", BlankLayoutName)
		}
		if !valid[p.Arrange] {
			return fmt.Errorf("layout %q has invalid arrange %q; valid: %s",
				p.Name, p.Arrange, arrangeList())
		}
		if len(p.Panes) == 0 {
			return fmt.Errorf("layout %q has no panes", p.Name)
		}
		for _, pane := range p.Panes {
			if err := validatePaneCommand(p.Name, pane, agents); err != nil {
				return err
			}
		}
		names[p.Name] = true
	}
	if cfg.Default != "" && cfg.Default != BlankLayoutName && !names[cfg.Default] {
		return fmt.Errorf("layouts.default %q is not a defined preset (%s)",
			cfg.Default, presetList(cfg))
	}
	return nil
}

func validatePaneCommand(layoutName string, pane string, agents map[string]string) error {
	agent, ok := agentReference(pane)
	if !ok {
		return nil
	}
	if agent == "" {
		return fmt.Errorf("layout %q has empty agent reference", layoutName)
	}
	command, ok := agents[agent]
	if !ok {
		return fmt.Errorf("layout %q references unknown agent %q", layoutName, agent)
	}
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("layout %q references agent %q with empty command", layoutName, agent)
	}
	return nil
}

// ResolvePaneCommands replaces agent:<name> pane references with the literal
// command configured in [agents]. Literal pane commands and empty shell panes
// are preserved.
func ResolvePaneCommands(layout models.Layout, agents map[string]string) (models.Layout, error) {
	resolved := layout
	resolved.Panes = append([]string(nil), layout.Panes...)
	for i, pane := range resolved.Panes {
		agent, ok := agentReference(pane)
		if !ok {
			continue
		}
		command, ok := agents[agent]
		if !ok {
			return models.Layout{}, fmt.Errorf("layout %q references unknown agent %q", layout.Name, agent)
		}
		if strings.TrimSpace(command) == "" {
			return models.Layout{}, fmt.Errorf("layout %q references agent %q with empty command", layout.Name, agent)
		}
		resolved.Panes[i] = command
	}
	return resolved, nil
}

func agentReference(pane string) (string, bool) {
	agent, ok := strings.CutPrefix(pane, "agent:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(agent), true
}

// FindPreset returns the preset with the given name or an error listing names.
func FindPreset(cfg models.LayoutsConfig, name string) (models.Layout, error) {
	for _, p := range cfg.Presets {
		if p.Name == name {
			p.Panes = append([]string(nil), p.Panes...)
			return p, nil
		}
	}
	return models.Layout{}, fmt.Errorf("unknown layout %q; available: %s", name, presetList(cfg))
}

// ResolveLayout applies the selection precedence: explicit --layout, then
// --select-layout (via selectFn), then the target repo default, then the
// global default. layoutFlag and selectFlag are mutually exclusive. An empty
// resolved name or the reserved name "none" yields the blank single-pane
// layout.
func ResolveLayout(
	cfg models.LayoutsConfig,
	layoutFlag string,
	selectFlag bool,
	targetDefault string,
	selectFn func([]models.Layout) (models.Layout, error),
) (models.Layout, error) {
	if layoutFlag != "" && selectFlag {
		return models.Layout{}, fmt.Errorf("--layout and --select-layout are mutually exclusive")
	}
	if selectFlag {
		return selectFn(append([]models.Layout{BlankLayout()}, cfg.Presets...))
	}
	name := layoutFlag
	if name == "" {
		name = targetDefault
	}
	if name == "" {
		name = cfg.Default
	}
	if name == "" || name == BlankLayoutName {
		return BlankLayout(), nil
	}
	return FindPreset(cfg, name)
}

func arrangeList() string {
	out := make([]string, 0, len(ValidArranges()))
	for a := range ValidArranges() {
		out = append(out, a)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func presetList(cfg models.LayoutsConfig) string {
	if len(cfg.Presets) == 0 {
		return "no presets defined"
	}
	seen := make(map[string]bool, len(cfg.Presets))
	out := make([]string, 0, len(cfg.Presets))
	for _, p := range cfg.Presets {
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
