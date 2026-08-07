package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

var (
	listVerbose bool
	listJSON    bool
	listGlobal  bool
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
	RunE: runList,
}

func init() {
	rootCmd.AddCommand(listCmd)

	listCmd.Flags().BoolVarP(&listVerbose, "verbose", "v", false, "Show detailed information")
	listCmd.Flags().BoolVar(&listJSON, "json", false, "Output in JSON format")
	listCmd.Flags().BoolVarP(&listGlobal, "global", "g", false, "Show all worktrees from the configured base directory")
}

func runList(cmd *cobra.Command, args []string) error {
	// Try git context first, fall back to non-git if needed
	ctx, err := NewGitCommandContext()
	if err != nil {
		// If git initialization fails, create non-git context for global mode
		ctx, err = NewCommandContext()
		if err != nil {
			return err
		}
	}

	return ctx.WithGlobalLocalSupport(
		listGlobal,
		func(ctx *CommandContext) error {
			// Local mode - show worktrees from current repository
			worktrees, err := ctx.WorktreeManager.List()
			if err != nil {
				return fmt.Errorf("failed to list worktrees: %w", err)
			}

			if listJSON {
				enrichWorktreeIdentity(ctx.Git, ctx.Config.Projects, worktrees)
				if err := enrichProtectedSocketIdentity(
					cmd.Context(),
					worktrees,
				); err != nil {
					return err
				}
				return ctx.Printer.PrintWorktreesJSON(worktrees)
			}

			ctx.Printer.PrintWorktrees(worktrees, listVerbose)
			return nil
		},
		func(ctx *CommandContext) error {
			// Global mode - show all worktrees from base directory
			return showGlobalWorktrees(ctx)
		},
	)
}

func showGlobalWorktrees(ctx *CommandContext) error {
	worktreePointers, err := ctx.DiscoverGlobalWorktrees()
	if err != nil {
		return fmt.Errorf("failed to discover worktrees: %w", err)
	}

	if len(worktreePointers) == 0 {
		// JSON mode always emits a JSON array, empty included, so scripts can
		// parse the output unconditionally; the prose message is non-JSON only.
		if listJSON {
			return ctx.Printer.PrintWorktreesJSON([]models.Worktree{})
		}
		ctx.Printer.PrintInfo("No worktrees found in " + ctx.Config.Worktree.BaseDir)
		return nil
	}

	// Convert from []*models.Worktree to []models.Worktree for printer
	var worktrees []models.Worktree
	for _, w := range worktreePointers {
		worktrees = append(worktrees, *w)
	}

	if listJSON {
		if err := enrichProtectedSocketIdentity(
			context.Background(),
			worktrees,
		); err != nil {
			return err
		}
		return ctx.Printer.PrintWorktreesJSON(worktrees)
	}

	ctx.Printer.PrintWorktrees(worktrees, listVerbose)
	return nil
}

func enrichProtectedSocketIdentity(
	ctx context.Context,
	worktrees []models.Worktree,
) error {
	records := make(map[string]pullrequest.Provenance)
	if err := pullrequest.NewFileStore(prStorePath()).View(
		ctx,
		func(current map[string]pullrequest.Provenance) error {
			for key, record := range current {
				records[key] = record
			}
			return nil
		},
	); err != nil {
		return fmt.Errorf("failed to read pull-request provenance: %w", err)
	}
	annotateProtectedSocketIdentity(worktrees, records)
	return nil
}

func annotateProtectedSocketIdentity(
	worktrees []models.Worktree,
	records map[string]pullrequest.Provenance,
) {
	type workspaceIdentity struct {
		path       string
		branch     string
		session    string
		repository string
		generation string
	}
	socketByIdentity := make(map[workspaceIdentity]string, len(records))
	for _, record := range records {
		workspace := record.Workspace
		repository := workspace.Repository
		if repository == "" {
			repository = record.Project.Identity
		}
		if workspace.Path == "" ||
			workspace.Branch == "" ||
			workspace.SessionName == "" ||
			repository == "" {
			continue
		}
		key := workspaceIdentity{
			path:       utils.CanonicalPath(workspace.Path),
			branch:     workspace.Branch,
			session:    workspace.SessionName,
			repository: pullrequest.NormalizeRepositoryIdentity(repository),
			generation: workspace.Generation,
		}
		socketByIdentity[key] =
			tmux.ProtectedWorkspaceSocketName(
				workspace.SessionName,
				workspace.Path,
			)
	}
	for i := range worktrees {
		key := workspaceIdentity{
			path:    utils.CanonicalPath(worktrees[i].Path),
			branch:  worktrees[i].Branch,
			session: worktrees[i].SessionName,
			repository: pullrequest.NormalizeRepositoryIdentity(
				worktrees[i].Repository,
			),
			generation: worktrees[i].Generation,
		}
		worktrees[i].TmuxSocketName = socketByIdentity[key]
		if worktrees[i].TmuxSocketName == "" {
			key.generation = ""
			worktrees[i].TmuxSocketName = socketByIdentity[key]
		}
	}
}

// enrichWorktreeIdentity fills the repository slug and session name for each
// local worktree so JSON surfaces carry the same identity fields as global
// (-g) mode. Identity follows the single registered-identity precedence: a
// registered project's canonical identity wins over a fork origin, so these
// surfaces join with `kwt projects` and derive the same session names as
// every other path. Best effort: when repository identity cannot be resolved
// the fields stay empty.
func enrichWorktreeIdentity(g worktree.RepoIdentityGit, projects []models.Project, worktrees []models.Worktree) {
	info, err := worktree.RepositoryInfoWithProjects(g, projects)
	if err != nil {
		return
	}
	for i := range worktrees {
		worktrees[i].Repository = info.FullPath
		worktrees[i].SessionName = tmux.WorkspaceSessionName(info, worktrees[i].Branch, worktrees[i].Path)
	}
}
