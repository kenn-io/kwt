package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var branchesJSON bool

var branchesCmd = &cobra.Command{
	Use:   "branches",
	Short: "List branches available for a new worktree",
	Long: `List local and remote branches that are not already checked out.

Remote candidates use the local branch name a worktree will receive while
retaining the exact remote source ref. Use --json for a stable machine-readable
surface.`,
	Args: cobra.NoArgs,
	RunE: runBranches,
}

func init() {
	rootCmd.AddCommand(branchesCmd)
	branchesCmd.Flags().BoolVar(&branchesJSON, "json", false, "Output in JSON format")
}

func runBranches(cmd *cobra.Command, _ []string) error {
	ctx, err := NewGitCommandContext()
	if err != nil {
		return err
	}
	branches, err := ctx.Git.ListAvailableBranches()
	if err != nil {
		return err
	}
	if branchesJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(branches)
	}
	if len(branches) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no available branches")
		return nil
	}
	for _, branch := range branches {
		if branch.IsRemote {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", branch.Name, branch.Source)
		} else {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), branch.Name)
		}
	}
	return nil
}
