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
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/duration"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

var (
	addBranch               bool
	addInteractive          bool
	addForce                bool
	addExpires              string
	addLayout               string
	addSelectLayout         bool
	addNoLaunch             bool
	addFrom                 string
	addExpectedRepository   string
	addExpectedRegistration string

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
	addCmd.Flags().StringVar(
		&addExpectedRepository,
		"expected-repository",
		"",
		"Require the exact credential-free registered repository identity",
	)
	addCmd.Flags().StringVar(
		&addExpectedRegistration,
		"expected-registration",
		"",
		"Require the exact observed project registration fingerprint",
	)
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
	commandContext := cmd.Context()
	if commandContext == nil {
		commandContext = context.Background()
	}
	return ExecuteWithArgs(true, func(ctx *CommandContext, cmd *cobra.Command, args []string) error {
		hasExpectedRepository := commandFlagChanged(cmd, "expected-repository")
		hasExpectedRegistration := commandFlagChanged(cmd, "expected-registration")
		guardedAdd := hasExpectedRepository || hasExpectedRegistration
		if hasExpectedRepository != hasExpectedRegistration ||
			(guardedAdd && (addExpectedRepository == "" || addExpectedRegistration == "")) {
			return service.NewError(
				service.InvalidRequest,
				"expected repository identity and registration fingerprint are required together",
				false, nil, nil,
			)
		}
		if guardedAdd && !lifecycle.EqualProjectIdentity(
			addExpectedRepository,
			addExpectedRepository,
		) {
			return service.NewError(
				service.InvalidRequest,
				"expected repository identity is invalid",
				false, nil, nil,
			)
		}
		if guardedAdd && !config.ValidProjectRegistrationFingerprint(
			addExpectedRegistration,
		) {
			return service.NewError(
				service.InvalidRequest,
				"expected project registration fingerprint is invalid",
				false, nil, nil,
			)
		}

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
		// The launch name is derived again from the created worktree under its
		// lifecycle guard because setup may change the checked-out branch.
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
		if launch {
			layout, _, err = prepareLaunch(ctx)
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
		}
		mainPath, err := ctx.Git.GetMainRepositoryPath()
		if err != nil {
			return fmt.Errorf("identify main repository: %w", err)
		}
		home, err := config.CanonicalHome()
		if err != nil {
			return err
		}
		expansion, err := kwt.CaptureExpansionContext()
		if err != nil {
			return err
		}
		var guard *guardedProjectOperation
		if guardedAdd {
			guard, err = observeExpectedGuardedProjectOperation(
				commandContext,
				home,
				mainPath,
				expansion,
				addExpectedRepository,
				addExpectedRegistration,
			)
		} else {
			guard, err = observeGuardedProjectOperation(
				commandContext, home, mainPath, expansion,
			)
		}
		if err != nil {
			return err
		}

		var expiresAt *time.Time
		var launchRunner openWorkspaceRunner
		var launchSession string
		var protectedNames []string
		if launch {
			protectedNames = credentials.ProtectedNames(ctx.Config)
			launchRunner = newAddWorkspaceRunner(protectedNames)
		}
		err = guard.run(commandContext, func() error {
			var mutationErr error
			if remoteSource != "" {
				if addExpires != "" || launch {
					worktreePath, worktreeGeneration, mutationErr =
						ctx.WorktreeManager.AddTrackingWithGeneration(
							branch,
							remoteSource,
							path,
						)
				} else {
					worktreePath, mutationErr = ctx.WorktreeManager.AddTracking(
						branch,
						remoteSource,
						path,
					)
				}
			} else {
				if addExpires != "" || launch {
					worktreePath, worktreeGeneration, mutationErr =
						ctx.WorktreeManager.AddWithGeneration(
							branch,
							path,
							addBranch,
							worktree.AddOptions{},
						)
				} else {
					worktreePath, mutationErr = ctx.WorktreeManager.Add(
						branch,
						path,
						addBranch,
					)
				}
			}
			if mutationErr != nil {
				return mutationErr
			}

			if addExpires != "" {
				reg, registryErr := registry.New()
				if registryErr != nil {
					return fmt.Errorf("failed to open registry: %w", registryErr)
				}

				t := time.Now().Add(expiresDuration)
				expiresAt = &t

				if err := registerWorktreeExpiration(
					ctx.Git,
					reg,
					worktreePath,
					worktreeGeneration,
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
				projects := []models.Project(nil)
				if guard.claim != nil {
					projects = []models.Project{guard.claim.Registration.Effective}
				}
				launchSession, mutationErr = withCurrentWorktreeSession(
					commandContext,
					mainPath,
					worktreePath,
					worktreeGeneration,
					projects,
					protectedNames,
					func(sessionName string) error {
						return launchRunner.EnsureWithGeneration(
							commandContext,
							sessionName,
							worktreePath,
							worktreeGeneration,
							layout,
						)
					},
				)
				return mutationErr
			}
			return nil
		})
		if err != nil {
			return err
		}

		if launch {
			return launchRunner.Attach(launchSession, os.Getenv("TMUX") != "")
		}
		return nil
	})(cmd, args)
}

func registerWorktreeExpiration(
	g *git.Git,
	reg *registry.Registry,
	worktreePath string,
	generation string,
	branch string,
	expiresAt *time.Time,
) error {
	remote, _ := git.New(worktreePath).GetRepositoryURL()
	repository, _ := url.CanonicalRepositoryIdentityFromRemote(remote)
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
// layout to use and proves that repository identity is resolvable. It performs
// no side effects, so runAdd calls it before creating the
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
