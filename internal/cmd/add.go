package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/duration"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

var (
	addBranch       bool
	addInteractive  bool
	addForce        bool
	addExpires      string
	addLayout       string
	addSelectLayout bool
	addNoLaunch     bool
	addFrom         string

	newAddWorkspaceRunner = func(names []string) openWorkspaceRunner {
		tmuxCommand := tmux.NewTmuxCommandWithStripNames("", names)
		return tmux.NewWorkspaceRunner(tmuxCommand, names)
	}
)

// addCmd represents the add command.
var addCmd = &cobra.Command{
	Use:   "add [branch] [path]",
	Short: "Create a new worktree",
	Long: `Create a new worktree for the specified branch.

If no path is provided, it will be generated based on the configuration template.
Use -i flag to interactively select a branch using fuzzy finder.

When -b creates a branch, kwt fetches origin and starts from its default branch.
If that remote base is unavailable, it falls back to local main, then master,
then the branch checked out in the primary worktree.

Use --from with a fetched remote ref to create a local tracking branch and its
worktree. Shorthand such as origin/topic is verified and resolved to its full
refs/remotes/... identity before creation. Existing-branch worktrees skip
repository setup and workspace launch until you inspect the checkout and
explicitly open it.`,
	Example: `  # Create worktree from existing branch
  kwt add feature/new-ui

  # Create at specific path
  kwt add feature/new-ui ~/projects/myapp-feature

  # Create new branch and worktree
  kwt add -b feature/api-v2

  # Create a tracking worktree from a remote branch
  kwt add --from origin/feature/review feature/review

  # Interactive branch selection
  kwt add -i

  # Create worktree expiring in 7 days
  kwt add --expires 7d feature/experiment

  # Create worktree expiring in 1 hour
  kwt add --expires 1h hotfix/quick-test`,
	RunE:              runAdd,
	ValidArgsFunction: getBranchCompletions,
}

func init() {
	rootCmd.AddCommand(addCmd)

	addCmd.Flags().BoolVarP(&addBranch, "branch", "b", false, "Create new branch")
	addCmd.Flags().BoolVarP(&addInteractive, "interactive", "i", false, "Select branch using fuzzy finder")
	addCmd.Flags().BoolVarP(&addForce, "force", "f", false, "Overwrite existing directory")
	addCmd.Flags().StringVar(&addExpires, "expires", "", "Set expiration (e.g., 1d, 7d, 1h)")
	addCmd.Flags().StringVar(&addLayout, "layout", "", "Workspace layout preset to launch (\"none\" = blank session)")
	addCmd.Flags().BoolVarP(&addSelectLayout, "select-layout", "L", false,
		"Fuzzy-pick a workspace layout")
	addCmd.Flags().BoolVar(&addNoLaunch, "no-launch", false,
		"Create the worktree without launching a workspace")
	addCmd.Flags().StringVar(&addFrom, "from", "",
		"Create a local tracking branch from this remote ref")
	if err := addCmd.RegisterFlagCompletionFunc(
		"from",
		getRemoteBranchCompletions,
	); err != nil {
		panic(err)
	}
	addCmd.MarkFlagsMutuallyExclusive("layout", "select-layout")
	addCmd.MarkFlagsMutuallyExclusive("branch", "from", "interactive")
}

func runAdd(cmd *cobra.Command, args []string) error {
	return ExecuteWithArgs(true, func(ctx *CommandContext, cmd *cobra.Command, args []string) error {
		var branch string
		var path string
		remoteSource := addFrom

		if addInteractive {
			if len(args) > 0 {
				return fmt.Errorf("cannot specify branch name with -i flag")
			}

			branches, err := ctx.Git.ListAvailableBranches()
			if err != nil {
				return fmt.Errorf("failed to list branches: %w", err)
			}

			selectedBranch, err := ctx.GetFinder().SelectBranch(branches)
			if err != nil {
				return fmt.Errorf("branch selection cancelled")
			}

			branch = selectedBranch.Name
			if selectedBranch.IsRemote {
				remoteSource = selectedBranch.Source
			}
		} else {
			if len(args) < 1 {
				return fmt.Errorf("branch name is required")
			}
			branch = args[0]
			if len(args) > 1 {
				path = args[1]
			}
		}

		if path != "" && !addForce {
			if err := ctx.WorktreeManager.ValidateWorktreePath(path); err != nil {
				return err
			}
		}

		// Validate --expires duration before creating the worktree so an
		// invalid value does not leave a stray worktree behind. The actual
		// ExpiresAt is computed after creation so the effective lifetime
		// isn't shortened by setup time (e.g. repository_settings hooks).
		var expiresDuration time.Duration
		if addExpires != "" {
			d, err := duration.Parse(addExpires)
			if err != nil {
				return fmt.Errorf("invalid --expires duration %q: %w", addExpires, err)
			}
			expiresDuration = d
		}

		// Resolve the launch decision and, if launching, validate tmux, the
		// layout config, and the repository identity, and pick a layout — all
		// before the worktree is created, so a rejected flag combo, missing
		// tmux, invalid layouts config, unknown --layout name, or a
		// repository identity failure never leaves a stray worktree behind.
		// Only worktree creation and the tmux attach below are side-effecting,
		// so they run after this point.
		layoutRequested := addLayout != "" || addSelectLayout
		unreviewedSource := !addBranch
		if unreviewedSource && layoutRequested {
			return fmt.Errorf(
				"existing branches cannot be combined with --layout/--select-layout; " +
					"inspect the checkout, then run kwt open",
			)
		}
		launch, err := shouldLaunch(
			ctx.Config.Layouts.AutoLaunchOnAdd,
			layoutRequested,
			addNoLaunch,
		)
		if err != nil {
			return err
		}
		if unreviewedSource {
			launch = false
		}
		var layout models.Layout
		var info *url.RepositoryInfo
		if launch {
			layout, info, err = prepareLaunch(ctx)
			if err != nil {
				return err
			}
		}

		var (
			worktreePath       string
			worktreeGeneration string
		)
		if remoteSource != "" {
			branches, listErr := ctx.Git.ListBranches(true)
			if listErr != nil {
				return fmt.Errorf("list fetched remote refs: %w", listErr)
			}
			remoteSource, err = resolveRemoteBranchSource(
				branches,
				remoteSource,
			)
			if err != nil {
				return err
			}
			if addExpires != "" {
				worktreePath, worktreeGeneration, err =
					ctx.WorktreeManager.AddTrackingWithGeneration(
						branch,
						remoteSource,
						path,
					)
			} else {
				worktreePath, err = ctx.WorktreeManager.AddTracking(
					branch,
					remoteSource,
					path,
				)
			}
		} else {
			if addExpires != "" {
				worktreePath, worktreeGeneration, err =
					ctx.WorktreeManager.AddWithGeneration(
						branch,
						path,
						addBranch,
						worktree.AddOptions{},
					)
			} else {
				worktreePath, err = ctx.WorktreeManager.Add(
					branch,
					path,
					addBranch,
				)
			}
		}
		if err != nil {
			return err
		}

		var expiresAt *time.Time
		if addExpires != "" {
			reg, err := registry.New()
			if err != nil {
				return fmt.Errorf("failed to open registry: %w", err)
			}

			repoURL, _ := ctx.Git.GetRepositoryURL()

			t := time.Now().Add(expiresDuration)
			expiresAt = &t

			if err := registerWorktreeExpiration(
				ctx.Git,
				reg,
				worktreePath,
				worktreeGeneration,
				repoURL,
				branch,
				expiresAt,
			); err != nil {
				return fmt.Errorf("failed to register worktree: %w", err)
			}
		}

		printAddResult(os.Stdout, branch, expiresAt)
		if unreviewedSource {
			_, _ = fmt.Fprintln(os.Stdout, existingBranchReviewMessage())
		}
		publishFleetBestEffortForCommand(cmd, ctx.Config)

		if launch {
			return attachWorkspace(
				info,
				branch,
				worktreePath,
				layout,
				ctx.Config,
			)
		}
		return nil
	})(cmd, args)
}

func registerWorktreeExpiration(
	g *git.Git,
	reg *registry.Registry,
	worktreePath string,
	generation string,
	repository string,
	branch string,
	expiresAt *time.Time,
) error {
	return g.WithWorktreeGeneration(
		worktreePath,
		generation,
		func() error {
			updated, err := reg.SetExpirationIfGeneration(
				worktreePath,
				generation,
				repository,
				branch,
				expiresAt,
			)
			if err != nil {
				return err
			}
			if !updated {
				if observed, ok := reg.Get(worktreePath); ok &&
					observed.CreationToken != "" {
					return fmt.Errorf(
						"worktree creation in progress for %s",
						worktreePath,
					)
				}
				return fmt.Errorf(
					"registry ownership changed for %s",
					worktreePath,
				)
			}
			return nil
		},
	)
}

func resolveRemoteBranchSource(
	branches []models.Branch,
	requested string,
) (string, error) {
	var match string
	for _, branch := range branches {
		if !branch.IsRemote {
			continue
		}
		shortSource := strings.TrimPrefix(
			branch.Source,
			"refs/remotes/",
		)
		if requested != branch.Source && requested != shortSource {
			continue
		}
		if match != "" && match != branch.Source {
			return "", fmt.Errorf(
				"remote ref %q is ambiguous; use a full refs/remotes/... ref",
				requested,
			)
		}
		match = branch.Source
	}
	if match == "" {
		return "", fmt.Errorf(
			"remote ref %q was not found; fetch it first",
			requested,
		)
	}
	return match, nil
}

// printAddResult reports a successful worktree creation.
func printAddResult(w io.Writer, branch string, expiresAt *time.Time) {
	_, _ = fmt.Fprintf(w, "Created worktree for branch '%s'\n", branch)
	if expiresAt != nil {
		_, _ = fmt.Fprintf(w, "Worktree expires at %s\n", expiresAt.Format(time.RFC3339))
	}
}

func existingBranchReviewMessage() string {
	return "Review the existing-branch checkout, then run 'kwt open' to select it and start its workspace."
}

// shouldLaunch implements the add launch-decision rule.
func shouldLaunch(autoLaunch, layoutFlagPassed, noLaunch bool) (bool, error) {
	if noLaunch && layoutFlagPassed {
		return false, fmt.Errorf("--no-launch cannot be combined with --layout/--select-layout")
	}
	if noLaunch {
		return false, nil
	}
	return autoLaunch || layoutFlagPassed, nil
}

// prepareLaunch validates that a workspace can be launched and resolves the
// layout to use and the repository info needed to name its session. It
// performs no side effects, so runAdd calls it before creating the
// worktree: a missing tmux binary, invalid layouts config, an unknown
// --layout/--select-layout pick, or a repository identity failure is caught
// here rather than after a worktree already exists.
func prepareLaunch(ctx *CommandContext) (models.Layout, *url.RepositoryInfo, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return models.Layout{}, nil, fmt.Errorf("tmux not found; install tmux or pass --no-launch")
	}
	if err := tmux.ValidateLayouts(ctx.Config.Layouts, ctx.Config.Agents); err != nil {
		return models.Layout{}, nil, err
	}

	finder := ctx.GetFinder()
	layout, err := tmux.ResolveLayout(ctx.Config.Layouts, addLayout, addSelectLayout, "",
		func(ls []models.Layout) (models.Layout, error) {
			sel, err := finder.SelectLayout(ls)
			if err != nil {
				return models.Layout{}, err
			}
			return *sel, nil
		})
	if err != nil {
		return models.Layout{}, nil, err
	}
	layout, err = tmux.ResolvePaneCommands(layout, ctx.Config.Agents)
	if err != nil {
		return models.Layout{}, nil, err
	}

	info, err := worktree.RepositoryInfoWithProjects(ctx.Git, ctx.Config.Projects)
	if err != nil {
		return models.Layout{}, nil, fmt.Errorf("failed to determine repository identity: %w", err)
	}

	return layout, info, nil
}

// attachWorkspace computes the just-created worktree's session name from the
// already-parsed repository info and attaches to its tmux workspace,
// creating the session first if needed. Called only after prepareLaunch has
// already validated tmux, resolved the layout, and parsed the repository
// URL, and after the worktree exists.
func attachWorkspace(
	info *url.RepositoryInfo,
	branch, worktreePath string,
	layout models.Layout,
	cfg *models.Config,
) error {
	session := tmux.WorkspaceSessionName(info, branch, worktreePath)
	runner := newAddWorkspaceRunner(credentials.ProtectedNames(cfg))
	return runner.EnsureAndAttach(
		context.Background(), session, worktreePath, layout, os.Getenv("TMUX") != "",
	)
}
