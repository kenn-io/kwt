package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

var (
	listVerbose        bool
	listJSON           bool
	listGlobal         bool
	queryListInventory = queryCLIInventory
)

// listCmd represents the list command.
var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "Display worktree list",
	Long: `Display a list of worktrees.

When run inside a git repository, shows worktrees for the current repository.
When run outside a git repository, shows all worktrees in the configured base directory.
Use -g flag to always show all worktrees from the base directory.
Use -v flag for detailed information including commit hashes and creation times.
Use --json flag to output in JSON format for scripting.`,
	Example: `  # Simple list
  kwt list

  # Using the ls alias
  kwt ls

  # Detailed information
  kwt list -v

  # JSON format for scripting
  kwt list --json

  # Show all worktrees from base directory (from anywhere)
  kwt list -g`,
	PersistentPreRunE: globalOnlyPreRun,
	RunE:              runList,
}

func init() {
	rootCmd.AddCommand(listCmd)

	listCmd.Flags().BoolVarP(&listVerbose, "verbose", "v", false, "Show detailed information")
	listCmd.Flags().BoolVar(&listJSON, "json", false, "Output in JSON format")
	listCmd.Flags().BoolVarP(&listGlobal, "global", "g", false, "Show all worktrees from the configured base directory")
}

func runList(cmd *cobra.Command, args []string) error {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return writeListFailure(cmd, err, err)
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return writeListFailure(cmd, err, err)
	}
	result, err := queryListInventory(
		cmd.Context(),
		kwt.Request{
			View: kwt.ViewRepository, WorkingDirectory: workingDirectory,
			ForceGlobal: listGlobal, RequireCurrent: true,
			IncludeProtectedSockets: listJSON,
		},
		config.StdinInteractive(),
		cmd.ErrOrStderr(),
	)
	if err != nil {
		return writeListFailure(cmd, err, fmt.Errorf("failed to list worktrees: %w", err))
	}
	ctx, err := NewCommandContext()
	if err != nil {
		return writeListFailure(cmd, err, err)
	}
	worktrees := make([]models.Worktree, len(result.Snapshot.Entries))
	for index, entry := range result.Snapshot.Entries {
		worktrees[index] = listedWorktree(entry)
	}
	if len(worktrees) == 0 && !listJSON && listGlobal {
		ctx.Printer.PrintInfo("No worktrees found in " + ctx.Config.Worktree.BaseDir)
		return nil
	}
	if listJSON {
		return ctx.Printer.PrintWorktreesJSON(worktrees)
	}
	ctx.Printer.PrintWorktrees(worktrees, listVerbose)
	return nil
}

func writeListFailure(cmd *cobra.Command, err, humanError error) error {
	if !listJSON {
		return humanError
	}
	return writeCommandFailure(
		cmd,
		service.AsError(err).Descriptor,
		1,
		true,
		"list",
	)
}

func listedWorktree(entry kwt.Entry) models.Worktree {
	return models.Worktree{
		Path: entry.Path, Branch: entry.Branch, CommitHash: entry.CommitHash,
		IsMain: entry.IsMain, CreatedAt: entry.CreatedAt, Generation: entry.Generation,
		Repository: entry.Repository.FullPath, SessionName: entry.SessionName,
		TmuxSocketName: entry.TmuxSocketName, TmuxAttachMode: entry.TmuxAttachMode,
	}
}
