package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/finder"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

var (
	removeForce        bool
	removeDryRun       bool
	removeGlobal       bool
	removeIfGeneration string
	deleteBranch       bool
	forceDeleteBranch  bool
)

// removeCmd represents the remove command.
var removeCmd = &cobra.Command{
	Use:     "remove [pattern]",
	Aliases: []string{"rm"},
	Short:   "Delete worktree",
	Long: `Delete a worktree from the repository.

If no pattern is provided, shows a fuzzy finder to select the worktree.
The pattern can match against branch name or path.

By default, only the worktree directory is removed and the branch is preserved.
Use -b flag to also delete the branch after removing the worktree.

When run inside a git repository, shows worktrees for the current repository.
When run outside a git repository, shows all worktrees from the configured base directory.
Use -g flag to always show all worktrees from the base directory.`,
	Example: `  # Select and delete using fuzzy finder
  kwt remove

  # Delete by pattern matching
  kwt remove feature/old

  # Force delete even if dirty
  kwt remove -f feature/broken

  # Delete worktree and branch
  kwt remove -b feature/completed

  # Force delete branch even if not merged
  kwt remove -b --force-delete-branch feature/abandoned

  # Show what would be deleted
  kwt remove --dry-run feature/old

  # Remove from all worktrees in base directory
  kwt remove -g myapp:feature/old`,
	RunE: runRemove,
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if removeGlobal {
			return getGlobalWorktreeCompletions(cmd, args, toComplete)
		}
		return getRemoveCompletions(cmd, args, toComplete)
	},
}

func init() {
	rootCmd.AddCommand(removeCmd)

	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Force delete even if dirty")
	removeCmd.Flags().BoolVarP(&removeDryRun, "dry-run", "d", false, "Show deletion targets only")
	removeCmd.Flags().BoolVarP(&removeGlobal, "global", "g", false, "Remove from any worktree in the configured base directory")
	removeCmd.Flags().StringVar(&removeIfGeneration, "if-generation", "", "Remove only if the worktree generation matches")
	removeCmd.Flags().BoolVarP(&deleteBranch, "delete-branch", "b", false, "Also delete the branch after removing worktree")
	removeCmd.Flags().BoolVar(&forceDeleteBranch, "force-delete-branch", false, "Force delete the branch even if not merged")
}

func runRemove(cmd *cobra.Command, args []string) error {
	return ExecuteWithArgs(false, func(ctx *CommandContext, cmd *cobra.Command, args []string) error {
		// Try to get git context, but don't fail if we're not in a git repo
		gitCtx, gitErr := NewGitCommandContext()
		if gitErr == nil {
			ctx = gitCtx
		}

		return ctx.WithGlobalLocalSupport(
			removeGlobal,
			func(ctx *CommandContext) error {
				removed, err := removeLocalWorktree(ctx, args)
				if removed > 0 {
					publishFleetBestEffortForCommand(cmd, ctx.Config)
				}
				return err
			},
			func(ctx *CommandContext) error {
				removed, err := removeGlobalWorktree(ctx, args)
				if removed > 0 {
					publishFleetBestEffortForCommand(cmd, ctx.Config)
				}
				return err
			},
		)
	})(cmd, args)
}

func removeLocalWorktree(ctx *CommandContext, args []string) (int, error) {
	expectedGeneration := removeIfGeneration
	worktrees, err := ctx.WorktreeManager.List()
	if err != nil {
		return 0, fmt.Errorf("failed to list worktrees: %w", err)
	}

	nonMainWorktrees := filterNonMainWorktrees(worktrees)
	if len(nonMainWorktrees) == 0 {
		return 0, fmt.Errorf("no removable worktrees found")
	}

	var toRemove []models.Worktree

	if len(args) > 0 {
		// Get all matching worktrees
		matches, err := ctx.WorktreeManager.GetMatchingWorktrees(args[0])
		if err != nil {
			return 0, err
		}

		// Filter out main worktrees
		var nonMainMatches []models.Worktree
		for _, wt := range matches {
			if !wt.IsMain {
				nonMainMatches = append(nonMainMatches, wt)
			}
		}

		if len(nonMainMatches) == 0 {
			return 0, fmt.Errorf("no worktree found matching pattern: %s", args[0])
		} else if len(nonMainMatches) == 1 {
			toRemove = nonMainMatches
		} else {
			// Multiple matches - use fuzzy finder
			selected, err := ctx.GetFinder().SelectMultipleWorktrees(nonMainMatches)
			if err != nil {
				return 0, fmt.Errorf("worktree selection cancelled")
			}
			toRemove = selected
		}
	} else {
		selected, err := ctx.GetFinder().SelectMultipleWorktrees(nonMainWorktrees)
		if err != nil {
			return 0, fmt.Errorf("worktree selection cancelled")
		}
		toRemove = selected
	}

	if removeDryRun {
		fmt.Println("Would remove the following worktrees:")
		for _, wt := range toRemove {
			fmt.Printf("  %s (%s)\n", wt.Branch, wt.Path)
			if deleteBranch {
				fmt.Printf("    - Would delete branch: %s\n", wt.Branch)
			}
		}
		return 0, nil
	}
	if expectedGeneration != "" && len(toRemove) != 1 {
		return 0, fmt.Errorf(
			"--if-generation requires exactly one worktree",
		)
	}

	removed := 0
	for _, wt := range toRemove {
		registryPath := registeredWorktreePath(wt.Path)
		if deleteBranch {
			if err := ctx.WorktreeManager.RemoveWithBranch(
				wt.Path,
				wt.Branch,
				removeForce,
				deleteBranch,
				forceDeleteBranch,
				expectedGeneration,
			); err != nil {
				if worktreePathRemoved(wt.Path) {
					removed++
					unregisterWorktreePath(registryPath)
				}
				if expectedGeneration != "" {
					return removed, err
				}
				ctx.Printer.PrintError(fmt.Errorf("failed to remove %s: %v", wt.Branch, err))
				continue
			}
			ctx.Printer.PrintSuccess(fmt.Sprintf("Removed worktree: %s", wt.Branch))
			if wt.Branch != "" {
				ctx.Printer.PrintSuccess(fmt.Sprintf("Deleted branch: %s", wt.Branch))
			}
		} else {
			if err := ctx.WorktreeManager.Remove(
				wt.Path,
				removeForce,
				expectedGeneration,
			); err != nil {
				if worktreePathRemoved(wt.Path) {
					removed++
					unregisterWorktreePath(registryPath)
				}
				if expectedGeneration != "" {
					return removed, err
				}
				ctx.Printer.PrintError(fmt.Errorf("failed to remove %s: %v", wt.Branch, err))
				continue
			}
			ctx.Printer.PrintSuccess(fmt.Sprintf("Removed worktree: %s", wt.Branch))
		}
		removed++

		// Clean up registry entry after successful removal
		unregisterWorktreePath(registryPath)
	}

	return removed, nil
}

func filterNonMainWorktrees(worktrees []models.Worktree) []models.Worktree {
	var filtered []models.Worktree
	for _, wt := range worktrees {
		if !wt.IsMain {
			filtered = append(filtered, wt)
		}
	}
	return filtered
}

func removeGlobalWorktree(ctx *CommandContext, args []string) (int, error) {
	expectedGeneration := removeIfGeneration
	entries, err := discovery.DiscoverGlobalWorktrees(ctx.Config.Worktree.BaseDir, ctx.Config.Projects)
	if err != nil {
		return 0, fmt.Errorf("failed to discover worktrees: %w", err)
	}

	if len(entries) == 0 {
		return 0, fmt.Errorf("no worktrees found in %s", ctx.Config.Worktree.BaseDir)
	}

	// Filter out main worktrees
	var nonMainEntries []*discovery.GlobalWorktreeEntry
	for _, entry := range entries {
		if !entry.IsMain {
			nonMainEntries = append(nonMainEntries, entry)
		}
	}

	if len(nonMainEntries) == 0 {
		return 0, fmt.Errorf("no removable worktrees found")
	}

	var toRemove []*discovery.GlobalWorktreeEntry

	if len(args) > 0 {
		// Pattern matching
		pattern := strings.ToLower(args[0])
		var matches []*discovery.GlobalWorktreeEntry

		for _, entry := range nonMainEntries {
			branchLower := strings.ToLower(entry.Branch)
			var repoName string
			if entry.RepositoryInfo != nil {
				repoName = strings.ToLower(entry.RepositoryInfo.Repository)
			}

			// Match against branch name, path, repo name, or repo:branch pattern
			if strings.Contains(branchLower, pattern) ||
				strings.Contains(strings.ToLower(entry.Path), pattern) ||
				strings.Contains(repoName, pattern) ||
				strings.Contains(repoName+":"+branchLower, pattern) {
				matches = append(matches, entry)
			}
		}

		if len(matches) == 0 {
			return 0, fmt.Errorf("no worktree matches pattern: %s", args[0])
		} else if len(matches) == 1 {
			toRemove = matches
		} else {
			// Multiple matches - use fuzzy finder
			worktrees := discovery.ConvertToWorktreeModels(matches, true)

			// Create a temporary git instance for finder
			g, _ := git.NewFromCwd()
			if g == nil {
				g = &git.Git{}
			}

			f := finder.NewWithUI(g, &ctx.Config.Finder, &ctx.Config.UI)
			selected, err := f.SelectMultipleWorktrees(worktrees)
			if err != nil {
				return 0, fmt.Errorf("worktree selection cancelled")
			}

			// Map selected worktrees back to entries
			selectedPaths := make(map[string]bool)
			for _, wt := range selected {
				selectedPaths[wt.Path] = true
			}

			for _, entry := range matches {
				if selectedPaths[entry.Path] {
					toRemove = append(toRemove, entry)
				}
			}
		}
	} else {
		// No pattern - show all in fuzzy finder
		worktrees := discovery.ConvertToWorktreeModels(nonMainEntries, true)

		// Use global finder for selection
		f := ctx.GetGlobalFinder()
		selected, err := f.SelectMultipleWorktrees(worktrees)
		if err != nil {
			return 0, fmt.Errorf("worktree selection cancelled")
		}

		// Map selected worktrees back to entries
		selectedPaths := make(map[string]bool)
		for _, wt := range selected {
			selectedPaths[wt.Path] = true
		}

		for _, entry := range nonMainEntries {
			if selectedPaths[entry.Path] {
				toRemove = append(toRemove, entry)
			}
		}
	}

	if removeDryRun {
		fmt.Println("Would remove the following worktrees:")
		for _, entry := range toRemove {
			repoName := "unknown"
			if entry.RepositoryInfo != nil {
				repoName = entry.RepositoryInfo.Repository
			}
			fmt.Printf("  %s:%s (%s)\n", repoName, entry.Branch, entry.Path)
			if deleteBranch {
				fmt.Printf("    - Would delete branch: %s\n", entry.Branch)
			}
		}
		return 0, nil
	}
	if expectedGeneration != "" && len(toRemove) != 1 {
		return 0, fmt.Errorf(
			"--if-generation requires exactly one worktree",
		)
	}

	// Remove each worktree by changing to its repository directory
	removed := 0
	for _, entry := range toRemove {
		registryPath := registeredWorktreePath(entry.Path)
		// Change to the repository directory to run git commands
		originalDir, err := os.Getwd()
		if err != nil {
			ctx.Printer.PrintError(fmt.Errorf("failed to get current directory: %v", err))
			continue
		}

		// Change to repository directory (need to find git repository root from URL)
		repoPath := entry.RepositoryURL
		if entry.RepositoryInfo != nil {
			// Try to find repository in common locations
			g := git.New(entry.Path)
			if repoRootPath, err := g.GetMainRepositoryPath(); err == nil {
				repoPath = repoRootPath
			}
		}

		if err := os.Chdir(repoPath); err != nil {
			ctx.Printer.PrintError(fmt.Errorf("failed to change to repository %s: %v", repoPath, err))
			continue
		}

		// Create git instance for the repository
		g := git.New(repoPath)
		wm := worktree.New(g, ctx.Config)

		if deleteBranch {
			if err := wm.RemoveWithBranch(
				entry.Path,
				entry.Branch,
				removeForce,
				deleteBranch,
				forceDeleteBranch,
				expectedGeneration,
			); err != nil {
				if worktreePathRemoved(entry.Path) {
					removed++
					unregisterWorktreePath(registryPath)
				}
				if expectedGeneration != "" {
					_ = os.Chdir(originalDir)
					return removed, err
				}
				repoName := "unknown"
				if entry.RepositoryInfo != nil {
					repoName = entry.RepositoryInfo.Repository
				}
				ctx.Printer.PrintError(fmt.Errorf("failed to remove %s:%s: %v", repoName, entry.Branch, err))
				_ = os.Chdir(originalDir)
				continue
			}
		} else {
			if err := wm.Remove(
				entry.Path,
				removeForce,
				expectedGeneration,
			); err != nil {
				if worktreePathRemoved(entry.Path) {
					removed++
					unregisterWorktreePath(registryPath)
				}
				if expectedGeneration != "" {
					_ = os.Chdir(originalDir)
					return removed, err
				}
				repoName := "unknown"
				if entry.RepositoryInfo != nil {
					repoName = entry.RepositoryInfo.Repository
				}
				ctx.Printer.PrintError(fmt.Errorf("failed to remove %s:%s: %v", repoName, entry.Branch, err))
				_ = os.Chdir(originalDir)
				continue
			}
		}

		repoName := "unknown"
		if entry.RepositoryInfo != nil {
			repoName = entry.RepositoryInfo.Repository
		}
		ctx.Printer.PrintSuccess(fmt.Sprintf("Removed worktree: %s:%s", repoName, entry.Branch))
		if deleteBranch && entry.Branch != "" {
			ctx.Printer.PrintSuccess(fmt.Sprintf("Deleted branch: %s", entry.Branch))
		}

		// Clean up registry entry after successful removal
		unregisterWorktreePath(registryPath)
		removed++

		// Change back to original directory
		_ = os.Chdir(originalDir)
	}

	return removed, nil
}

func worktreePathRemoved(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func registeredWorktreePath(path string) string {
	reg, err := registry.New()
	if err != nil {
		return path
	}
	entry, ok := reg.Get(path)
	if !ok {
		return path
	}
	return entry.Path
}

func unregisterWorktreePath(path string) {
	if reg, err := registry.New(); err == nil {
		_ = reg.Unregister(path)
	}
}
