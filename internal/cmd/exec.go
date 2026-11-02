package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

var (
	execGlobal bool
	execStay   bool
)

var execCmd = &cobra.Command{
	Use:                "exec [pattern] -- command [args...]",
	Short:              "Execute command in worktree directory",
	DisableFlagParsing: true,
	Long: `Execute a command in a worktree directory without changing the current directory.

The command runs in a subshell with the working directory set to the selected worktree.
Use -- to separate kwt arguments from the command to execute.

If multiple worktrees match the pattern, an interactive fuzzy finder will be shown.
If no pattern is provided, all worktrees will be shown in the fuzzy finder.`,
	Example: `  # Run tests in a feature branch
  kwt exec feature -- npm test
  
  # Pull latest changes in main branch
  kwt exec main -- git pull
  
  # Run multiple commands
  kwt exec feature -- sh -c "git pull && npm install && npm test"
  
  # Stay in the worktree directory after command execution
  kwt exec --stay feature -- npm install
  
  # Execute in global worktree
  kwt exec -g project:feature -- make build`,
	Args: cobra.ArbitraryArgs,
	RunE: runExec,
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Disable file completion after --
		if slices.Contains(args, "--") {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		if len(args) == 0 || (len(args) == 1 && !strings.HasPrefix(args[0], "-")) {
			return getWorktreeCompletions(cmd, args, toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	},
}

func init() {
	rootCmd.AddCommand(execCmd)

	execCmd.Flags().BoolVarP(&execGlobal, "global", "g", false, "Execute in global worktree")
	execCmd.Flags().BoolVarP(&execStay, "stay", "s", false, "Stay in worktree directory after command execution")
}

// execArgs holds parsed execution arguments
type execArgs struct {
	pattern     string
	commandArgs []string
	global      bool
	stay        bool
}

// parseExecArgs manually parses command arguments since DisableFlagParsing is true
func parseExecArgs(cmd *cobra.Command, args []string) (*execArgs, error) {
	result := &execArgs{}
	dashDashIndex := -1

	// Parse flags manually
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			dashDashIndex = i
			break
		}

		switch arg {
		case "-g", "--global":
			result.global = true
			i++
		case "-s", "--stay":
			result.stay = true
			i++
		case "-h", "--help":
			return nil, cmd.Help()
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, fmt.Errorf("unknown flag: %s", arg)
			}
			// This is the pattern
			if result.pattern == "" {
				result.pattern = arg
			}
			i++
		}
	}

	if dashDashIndex == -1 {
		return nil, fmt.Errorf("missing -- separator. Use: kwt exec [pattern] -- command [args...]")
	}

	// Extract command and its arguments
	if dashDashIndex+1 >= len(args) {
		return nil, fmt.Errorf("no command specified after --")
	}
	result.commandArgs = args[dashDashIndex+1:]

	return result, nil
}

func runExec(cmd *cobra.Command, args []string) error {
	parsedArgs, err := parseExecArgs(cmd, args)
	if err != nil {
		return err
	}

	// Check if parsedArgs is nil (e.g., when --help is used)
	if parsedArgs == nil {
		return nil
	}

	// Set global variables for backward compatibility
	execGlobal = parsedArgs.global
	execStay = parsedArgs.stay

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var worktreePath string
	if parsedArgs.global {
		worktreePath, err = getGlobalWorktreePathForExec(cfg, parsedArgs.pattern)
	} else {
		worktreePath, err = getLocalWorktreePathForExec(cfg, parsedArgs.pattern)
	}

	if err != nil {
		return err
	}

	// Execute the command in the worktree directory
	return executeInWorktree(commandContext(cmd), worktreePath, parsedArgs.commandArgs, parsedArgs.stay)
}

func getLocalWorktreePathForExec(cfg *models.Config, pattern string) (string, error) {
	g, err := git.NewFromCwd()
	if err != nil {
		// Not in a git repo, try global
		return getGlobalWorktreePathForExec(cfg, pattern)
	}

	wm := worktree.New(g, cfg)

	if pattern != "" {
		// Get all matching worktrees
		matches, err := wm.GetMatchingWorktrees(pattern)
		if err != nil {
			return "", err
		}

		if len(matches) == 0 {
			return "", fmt.Errorf("no worktree found matching pattern: %s", pattern)
		} else if len(matches) == 1 {
			return matches[0].Path, nil
		} else {
			// Multiple matches - use fuzzy finder
			f := CreateFinder(g, cfg)
			selected, err := f.SelectWorktree(matches)
			if err != nil {
				return "", fmt.Errorf("worktree selection cancelled")
			}
			return selected.Path, nil
		}
	} else {
		// No pattern - show all worktrees
		worktrees, err := wm.List()
		if err != nil {
			return "", err
		}

		if len(worktrees) == 0 {
			return "", fmt.Errorf("no worktrees found")
		}

		if len(worktrees) == 1 {
			return worktrees[0].Path, nil
		}

		f := CreateFinder(g, cfg)
		selected, err := f.SelectWorktree(worktrees)
		if err != nil {
			return "", fmt.Errorf("worktree selection cancelled")
		}
		return selected.Path, nil
	}
}

func getGlobalWorktreePathForExec(cfg *models.Config, pattern string) (string, error) {
	entries, err := discovery.DiscoverGlobalWorktrees(cfg.Worktree.BaseDir, cfg.Projects)
	if err != nil {
		return "", err
	}

	if len(entries) == 0 {
		return "", fmt.Errorf("no worktrees found across all repositories")
	}

	var selected *discovery.GlobalWorktreeEntry

	if pattern != "" {
		// Pattern matching
		matches := discovery.FilterGlobalWorktrees(entries, pattern)

		if len(matches) == 0 {
			return "", fmt.Errorf("no worktree matches pattern: %s", pattern)
		} else if len(matches) == 1 {
			selected = matches[0]
		} else {
			// Multiple matches - use fuzzy finder
			worktrees := discovery.ConvertToWorktreeModels(matches, true)

			f := CreateGlobalFinder(cfg)
			selectedWT, err := f.SelectWorktree(worktrees)
			if err != nil {
				return "", fmt.Errorf("worktree selection cancelled")
			}

			// Find the corresponding entry
			for _, entry := range matches {
				if entry.Path == selectedWT.Path {
					selected = entry
					break
				}
			}
		}
	} else {
		// No pattern - show all in fuzzy finder
		worktrees := discovery.ConvertToWorktreeModels(entries, true)

		f := CreateGlobalFinder(cfg)
		selectedWT, err := f.SelectWorktree(worktrees)
		if err != nil {
			return "", fmt.Errorf("worktree selection cancelled")
		}

		// Find the corresponding entry
		for _, entry := range entries {
			if entry.Path == selectedWT.Path {
				selected = entry
				break
			}
		}
	}

	if selected == nil {
		return "", fmt.Errorf("no worktree selected")
	}

	return selected.Path, nil
}

func executeInWorktree(ctx context.Context, worktreePath string, commandArgs []string, stay bool) error {
	cmd := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)

	cmd.Dir = worktreePath
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()

	if stay {
		// Launch a new shell in the worktree directory after command execution
		// Run the shell regardless of the original command's exit status
		_ = LaunchShell(ctx, worktreePath)
	}

	return err
}
