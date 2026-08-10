package cmd

import (
	"context"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/finder"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/utils"
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

type removalGenerationCondition struct {
	generation string
	specified  bool
}

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
	generationCondition, err := requestedRemovalGeneration(cmd)
	if err != nil {
		return err
	}
	commandContext := cmd.Context()
	if commandContext == nil {
		commandContext = context.Background()
	}
	return ExecuteWithArgs(false, func(ctx *CommandContext, cmd *cobra.Command, args []string) error {
		// Try to get git context, but don't fail if we're not in a git repo
		gitCtx, gitErr := NewGitCommandContext()
		if gitErr == nil {
			ctx = gitCtx
		}

		return ctx.WithGlobalLocalSupport(
			removeGlobal,
			func(ctx *CommandContext) error {
				removed, err := removeLocalWorktree(
					commandContext,
					ctx,
					args,
					generationCondition,
				)
				if removed > 0 {
					publishFleetBestEffortForCommand(cmd, ctx.Config)
				}
				return err
			},
			func(ctx *CommandContext) error {
				removed, err := removeGlobalWorktree(
					commandContext,
					ctx,
					args,
					generationCondition,
				)
				if removed > 0 {
					publishFleetBestEffortForCommand(cmd, ctx.Config)
				}
				return err
			},
		)
	})(cmd, args)
}

func requestedRemovalGeneration(
	cmd *cobra.Command,
) (removalGenerationCondition, error) {
	if !cmd.Flags().Changed("if-generation") {
		return removalGenerationCondition{}, nil
	}
	if err := git.ValidateWorktreeGeneration(removeIfGeneration); err != nil {
		return removalGenerationCondition{}, fmt.Errorf(
			"--if-generation must be a 32-character hexadecimal value",
		)
	}
	return removalGenerationCondition{
		generation: removeIfGeneration,
		specified:  true,
	}, nil
}

func removeLocalWorktree(
	commandContext context.Context,
	ctx *CommandContext,
	args []string,
	generationCondition removalGenerationCondition,
) (int, error) {
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
	if generationCondition.specified && len(toRemove) != 1 {
		return 0, fmt.Errorf(
			"--if-generation requires exactly one worktree",
		)
	}

	removed := 0
	repositoryPath, err := ctx.Git.GetMainRepositoryPath()
	if err != nil {
		return 0, fmt.Errorf("failed to resolve main repository: %w", err)
	}
	for _, wt := range toRemove {
		generation := wt.Generation
		if generationCondition.specified {
			generation = generationCondition.generation
		}
		result, removalErr := removeDaemonWorktree(commandContext, kwt.RemovalRequest{
			RepositoryPath: repositoryPath,
			Path:           wt.Path, ExpectedGeneration: generation,
			Force: removeForce, DeleteBranch: deleteBranch,
			ForceDeleteBranch: forceDeleteBranch,
		})
		if result.WorktreeRemoved || daemonMutationRequiresRefresh(removalErr) {
			removed++
		}
		if removalErr != nil {
			if generationCondition.specified {
				return removed, removalErr
			}
			ctx.Printer.PrintError(fmt.Errorf("failed to remove %s: %v", wt.Branch, removalErr))
			continue
		}
		ctx.Printer.PrintSuccess(fmt.Sprintf("Removed worktree: %s", wt.Branch))
		if result.BranchDeleted {
			ctx.Printer.PrintSuccess(fmt.Sprintf("Deleted branch: %s", result.Branch))
		}
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

func removeGlobalWorktree(
	commandContext context.Context,
	ctx *CommandContext,
	args []string,
	generationCondition removalGenerationCondition,
) (int, error) {
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
		matches := matchGlobalRemovalEntries(nonMainEntries, args[0])

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
	if generationCondition.specified && len(toRemove) != 1 {
		return 0, fmt.Errorf(
			"--if-generation requires exactly one worktree",
		)
	}

	removed := 0
	for _, entry := range toRemove {
		repoName := "unknown"
		if entry.RepositoryInfo != nil {
			repoName = entry.RepositoryInfo.Repository
		}
		repoPath, resolveErr := git.NewWithContext(commandContext, entry.Path).GetMainRepositoryPath()
		if resolveErr != nil {
			ctx.Printer.PrintError(fmt.Errorf(
				"failed to resolve repository for %s:%s: %v",
				repoName,
				entry.Branch,
				resolveErr,
			))
			continue
		}
		generation := entry.Generation
		if generationCondition.specified {
			generation = generationCondition.generation
		}
		result, removalErr := removeDaemonWorktree(commandContext, kwt.RemovalRequest{
			RepositoryPath: repoPath,
			Path:           entry.Path, ExpectedGeneration: generation,
			Force: removeForce, DeleteBranch: deleteBranch,
			ForceDeleteBranch: forceDeleteBranch,
		})
		if result.WorktreeRemoved || daemonMutationRequiresRefresh(removalErr) {
			removed++
		}
		if removalErr != nil {
			if generationCondition.specified {
				return removed, removalErr
			}
			ctx.Printer.PrintError(fmt.Errorf("failed to remove %s:%s: %v", repoName, entry.Branch, removalErr))
			continue
		}
		ctx.Printer.PrintSuccess(fmt.Sprintf("Removed worktree: %s:%s", repoName, entry.Branch))
		if result.BranchDeleted {
			ctx.Printer.PrintSuccess(fmt.Sprintf("Deleted branch: %s", result.Branch))
		}
	}

	return removed, nil
}

func matchGlobalRemovalEntries(
	entries []*discovery.GlobalWorktreeEntry,
	pattern string,
) []*discovery.GlobalWorktreeEntry {
	if patternPath, ok := globalRemovalPathKey(pattern); ok {
		var exactPathMatches []*discovery.GlobalWorktreeEntry
		for _, entry := range entries {
			entryPath, entryOK := globalRemovalPathKey(entry.Path)
			if entryOK && entryPath == patternPath {
				exactPathMatches = append(exactPathMatches, entry)
			}
		}
		if len(exactPathMatches) > 0 {
			return exactPathMatches
		}
	}

	lowerPattern := globalRemovalSearchKey(pattern)
	var matches []*discovery.GlobalWorktreeEntry
	for _, entry := range entries {
		branchLower := strings.ToLower(entry.Branch)
		pathLower := globalRemovalSearchKey(entry.Path)
		var repoName string
		if entry.RepositoryInfo != nil {
			repoName = strings.ToLower(entry.RepositoryInfo.Repository)
		}

		if strings.Contains(branchLower, lowerPattern) ||
			strings.Contains(pathLower, lowerPattern) ||
			strings.Contains(repoName, lowerPattern) ||
			strings.Contains(repoName+":"+branchLower, lowerPattern) {
			matches = append(matches, entry)
		}
	}
	return matches
}

func globalRemovalPathKey(rawPath string) (string, bool) {
	windowsPath := windowsStyleRemovalPath(rawPath)
	if !filepath.IsAbs(rawPath) && !windowsPath {
		return "", false
	}
	if !windowsPath {
		return utils.PathKey(rawPath), true
	}
	key := pathpkg.Clean(
		strings.ReplaceAll(filepath.ToSlash(rawPath), `\`, "/"),
	)
	return strings.ToLower(key), true
}

func globalRemovalSearchKey(rawPath string) string {
	key := filepath.ToSlash(rawPath)
	if runtime.GOOS == "windows" || windowsStyleRemovalPath(rawPath) {
		key = strings.ReplaceAll(key, `\`, "/")
	}
	return strings.ToLower(key)
}

func windowsStyleRemovalPath(rawPath string) bool {
	return (len(rawPath) >= 2 && rawPath[1] == ':') ||
		strings.HasPrefix(rawPath, `\\`) ||
		strings.HasPrefix(rawPath, "//")
}
