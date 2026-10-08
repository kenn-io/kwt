package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/tmux"
	dashboard "go.kenn.io/kwt/internal/tui"
	"go.kenn.io/kwt/internal/worktree"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the cross-repo worktree dashboard",
	// Isolation: tui must not merge the caller's cwd .kwt.toml. The dashboard
	// is global, and target repo layout defaults are read separately at attach.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return requireConfigInitialization() },
	RunE:              runTUI,
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}

func runTUI(cmd *cobra.Command, args []string) error {
	if !stdinIsTerminal() || !stdoutIsTerminal() {
		return fmt.Errorf("kwt tui requires an interactive terminal")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if cfg.Worktree.BaseDir == "" {
		return fmt.Errorf("worktree.basedir is not configured")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	if err := tmux.ValidateLayouts(cfg.Layouts, cfg.Agents); err != nil {
		return err
	}

	backend := newTUIBackend(cfg)
	backend.queryInventory = queryCLIInventory
	backend.stderr = os.Stderr
	model := newTUIModel(cmd.Context(), backend)
	final, err := tea.NewProgram(model).Run()
	if err != nil {
		return err
	}

	finalModel, ok := final.(dashboard.Model)
	if !ok {
		return nil
	}
	return executeTUIHandoff(backend, finalModel.Handoff())
}

func newTUIModel(ctx context.Context, backend *tuiBackend) dashboard.Model {
	anchor := launchAnchorPath(backend.launchDir)
	model := dashboard.NewModel(backend, backend.cfg.Worktree.BaseDir).WithInitialAnchor(anchor)
	if anchor == "" {
		return model
	}
	g := git.NewForInventory(ctx, anchor, backend.protectedNames)
	if info, err := worktree.RepositoryInfoWithProjects(g, backend.cfg.Projects); err == nil {
		return model.WithInitialRepository(anchor, info.FullPath)
	}
	return model
}

func executeTUIHandoff(backend *tuiBackend, handoff dashboard.Handoff) error {
	switch handoff.Kind {
	case dashboard.HandoffAttach:
		if handoff.ExistingOnly {
			return backend.AttachExistingOutsideTmux(handoff.Row)
		}
		return backend.AttachOutsideTmux(handoff.Row, handoff.LayoutName)
	case dashboard.HandoffShell:
		return LaunchShell(rowPathForHandoff(handoff.Row))
	default:
		return nil
	}
}

// launchAnchorPath resolves symlinks in the launch directory so the initial
// anchor string matches registered workspace paths, which are stored
// symlink-resolved.
func launchAnchorPath(launchDir string) string {
	if launchDir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(launchDir); err == nil {
		return resolved
	}
	return launchDir
}

func rowPathForHandoff(row dashboard.Row) string {
	if row.Workspace != nil {
		return row.Workspace.Path
	}
	if row.Entry != nil {
		return row.Entry.Path
	}
	if row.Status != nil {
		return row.Status.Path
	}
	return ""
}
