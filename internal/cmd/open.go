package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

var (
	openLayout       string
	openSelectLayout bool
	openStartSession bool

	newOpenWorkspaceRunner = func() openWorkspaceRunner {
		return tmux.NewWorkspaceRunner(tmux.NewTmuxCommand(""))
	}
)

type openWorkspaceRunner interface {
	Ensure(context.Context, string, string, models.Layout) error
	EnsureAndAttach(
		context.Context,
		string,
		string,
		models.Layout,
		bool,
	) error
}

var openCmd = &cobra.Command{
	Use:   "open [pattern]",
	Short: "Open a worktree workspace from any repository",
	Long: `Fuzzy-pick a worktree across all repositories in the configured base
directory and attach to its tmux workspace, creating the workspace with the
resolved layout if it does not yet exist. A pattern filters the worktree list.
An exact worktree path resolves directly even when it is outside the configured
base directory. Add --start-session to create or repair the workspace without
attaching.`,
	Example: `  # Pick a worktree and open its workspace
  kwt open

  # Open one exact worktree path directly
  kwt open /path/to/worktree

  # Ensure an exact worktree workspace exists without attaching
  kwt open /path/to/worktree --start-session

  # Force a specific layout
  kwt open --layout focus

  # Fuzzy-pick the layout too
  kwt open --select-layout`,
	// Isolation: open must not merge the caller's cwd .kwt.toml. Skipping the
	// root cwd merge keeps the global config pristine while still
	// propagating global config initialization failures.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return requireConfigInitialization() },
	Args:              cobra.MaximumNArgs(1),
	RunE:              runOpen,
}

func init() {
	rootCmd.AddCommand(openCmd)
	openCmd.Flags().StringVar(&openLayout, "layout", "", "Workspace layout preset to launch (\"none\" = blank session)")
	openCmd.Flags().BoolVarP(&openSelectLayout, "select-layout", "L", false,
		"Fuzzy-pick a workspace layout")
	openCmd.Flags().BoolVar(
		&openStartSession,
		"start-session",
		false,
		"ensure an exact worktree's workspace exists without attaching",
	)
	openCmd.MarkFlagsMutuallyExclusive("layout", "select-layout")
	openCmd.MarkFlagsMutuallyExclusive("start-session", "select-layout")
}

func runOpen(cmd *cobra.Command, args []string) error {
	return ExecuteWithContext(false, func(ctx *CommandContext) error {
		if err := tmux.ValidateLayouts(ctx.Config.Layouts, ctx.Config.Agents); err != nil {
			return err
		}
		if openStartSession && len(args) != 1 {
			return fmt.Errorf("--start-session requires an exact worktree path")
		}

		entry, requestedPath, err := resolveOpenWorktree(
			ctx,
			args,
			openStartSession,
		)
		if err != nil {
			return err
		}
		if entry == nil {
			return nil
		}
		if entry.RepositoryInfo == nil {
			return fmt.Errorf(
				"could not resolve worktree %s",
				requestedPath,
			)
		}
		finder := ctx.GetGlobalFinder()

		return openSelectedWorktree(
			cmd.Context(),
			ctx,
			entry,
			func(ls []models.Layout) (models.Layout, error) {
				sel, err := finder.SelectLayout(ls)
				if err != nil {
					return models.Layout{}, err
				}
				return *sel, nil
			},
			openStartSession,
			config.StdinInteractive(),
		)
	})(cmd, args)
}

func resolveOpenWorktree(
	ctx *CommandContext,
	args []string,
	startSession bool,
) (*discovery.GlobalWorktreeEntry, string, error) {
	requestedPath := ""
	if startSession {
		requestedPath = args[0]
		entry, err := discovery.DiscoverWorktree(
			requestedPath,
			ctx.Config.Projects,
		)
		if err != nil {
			return nil, requestedPath, fmt.Errorf(
				"could not resolve worktree %s: %w",
				requestedPath,
				err,
			)
		}
		return entry, requestedPath, nil
	}

	if len(args) == 1 {
		requestedPath = args[0]
		if entry, err := discovery.DiscoverWorktree(
			requestedPath,
			ctx.Config.Projects,
		); err == nil {
			return entry, requestedPath, nil
		}
	}

	entries, err := discovery.DiscoverGlobalWorktrees(
		ctx.Config.Worktree.BaseDir,
		ctx.Config.Projects,
	)
	if err != nil {
		return nil, requestedPath, fmt.Errorf(
			"failed to discover worktrees: %w",
			err,
		)
	}
	if len(entries) == 0 {
		if len(args) == 1 {
			return nil, requestedPath, fmt.Errorf(
				"could not resolve worktree %s",
				args[0],
			)
		}
		ctx.Printer.PrintInfo(
			"No worktrees found in " + ctx.Config.Worktree.BaseDir,
		)
		return nil, requestedPath, nil
	}

	var entry *discovery.GlobalWorktreeEntry
	finder := ctx.GetGlobalFinder()
	if len(args) == 1 {
		matches := discovery.FilterGlobalWorktrees(entries, requestedPath)
		switch len(matches) {
		case 0:
		case 1:
			entry = matches[0]
		default:
			selected, selectErr := finder.SelectWorktree(
				discovery.ConvertToWorktreeModels(matches, true),
			)
			if selectErr != nil {
				return nil, requestedPath, fmt.Errorf(
					"selection cancelled: %w",
					selectErr,
				)
			}
			entry = findEntryByPath(matches, selected.Path)
		}
	} else {
		worktrees := discovery.ConvertToWorktreeModels(entries, false)
		selected, selectErr := finder.SelectWorktree(worktrees)
		if selectErr != nil {
			return nil, requestedPath, fmt.Errorf(
				"selection cancelled: %w",
				selectErr,
			)
		}
		requestedPath = selected.Path
		entry = findEntryByPath(entries, requestedPath)
	}

	if entry == nil || entry.RepositoryInfo == nil {
		return nil, requestedPath, fmt.Errorf(
			"could not resolve worktree %s",
			requestedPath,
		)
	}
	return entry, requestedPath, nil
}

func openSelectedWorktree(
	commandCtx context.Context,
	ctx *CommandContext,
	entry *discovery.GlobalWorktreeEntry,
	selectLayout func([]models.Layout) (models.Layout, error),
	startSession bool,
	stdinInteractive bool,
) error {
	if err := rejectProtectedWorkspaceOpen(commandCtx, entry.Path); err != nil {
		return err
	}

	// Only resolve the target repo's default layout (which requires
	// finding its root and loading, and possibly trust-prompting for, its
	// .kwt.toml) when neither flag already determines the layout. This
	// avoids trust-prompting on, or failing over, a malformed target
	// config whose default would be ignored anyway.
	targetDefault := ""
	if shouldLoadTargetDefault(openLayout, openSelectLayout) {
		repoRoot, err := git.New(entry.Path).GetMainRepositoryPath()
		if err != nil {
			return fmt.Errorf("failed to find repository root: %w", err)
		}
		targetDefault, err = config.LoadRepoLayoutDefault(
			repoRoot,
			stdinInteractive && !startSession,
		)
		if err != nil {
			return err
		}
	}

	layout, err := tmux.ResolveLayout(
		ctx.Config.Layouts,
		openLayout,
		openSelectLayout,
		targetDefault,
		selectLayout,
	)
	if err != nil {
		return err
	}
	layout, err = tmux.ResolvePaneCommands(layout, ctx.Config.Agents)
	if err != nil {
		return err
	}

	session := tmux.WorkspaceSessionName(entry.RepositoryInfo, entry.Branch, entry.Path)
	runner := newOpenWorkspaceRunner()
	if startSession {
		return runner.Ensure(commandCtx, session, entry.Path, layout)
	}
	return runner.EnsureAndAttach(
		commandCtx, session, entry.Path, layout, os.Getenv("TMUX") != "",
	)
}

// shouldLoadTargetDefault reports whether kwt open must consult the target
// repo's .kwt.toml layouts.default: only when no explicit layout flag was
// given (an explicit --layout/--select-layout overrides the target default
// anyway).
func shouldLoadTargetDefault(layoutFlag string, selectLayout bool) bool {
	return layoutFlag == "" && !selectLayout
}

// findEntryByPath returns the discovery entry whose Path matches, or nil.
func findEntryByPath(
	entries []*discovery.GlobalWorktreeEntry, path string,
) *discovery.GlobalWorktreeEntry {
	path = utils.CanonicalPath(path)
	for _, e := range entries {
		if utils.CanonicalPath(e.Path) == path {
			return e
		}
	}
	return nil
}
