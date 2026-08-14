package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

var (
	openLayout               string
	openSelectLayout         bool
	openStartSession         bool
	openExpectedRepository   string
	openExpectedRegistration string
	openExpectedGeneration   string
	openExpectedSession      string

	discoverOpenWorktree = discovery.DiscoverWorktree

	newOpenWorkspaceRunner = func(names []string) openWorkspaceRunner {
		tmuxCommand := tmux.NewTmuxCommandWithStripNames("", names)
		return tmux.NewWorkspaceRunner(tmuxCommand, names)
	}
)

type openWorkspaceRunner interface {
	Ensure(context.Context, string, string, models.Layout) error
	EnsureWithGeneration(context.Context, string, string, string, models.Layout) error
	Attach(string, bool) error
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
	Short: "Open a worktree or registered directory workspace",
	Long: `Fuzzy-pick a worktree across all repositories in the configured base
directory and attach to its tmux workspace, creating the workspace with the
resolved layout if it does not yet exist. A pattern filters the worktree list.
An exact worktree path or registered directory workspace path resolves directly.
Add --start-session to create or repair the workspace without attaching.`,
	Example: `  # Pick a worktree and open its workspace
  kwt open

  # Open one exact worktree path directly
  kwt open /path/to/worktree

  # Ensure an exact worktree workspace exists without attaching
  kwt open /path/to/worktree --start-session

  # Open a registered directory workspace
  kwt open /path/to/directory-workspace

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
		"ensure an exact workspace exists without attaching",
	)
	openCmd.Flags().StringVar(&openExpectedRepository, "expected-repository", "", "require this registered repository identity before opening")
	openCmd.Flags().StringVar(&openExpectedRegistration, "expected-registration", "", "require this project registration fingerprint before opening")
	openCmd.Flags().StringVar(&openExpectedGeneration, "expected-generation", "", "require this exact worktree generation before opening")
	openCmd.Flags().StringVar(&openExpectedSession, "expected-session", "", "require this exact tmux session identity before opening")
	openCmd.MarkFlagsMutuallyExclusive("layout", "select-layout")
	openCmd.MarkFlagsMutuallyExclusive("start-session", "select-layout")
}

func runOpen(cmd *cobra.Command, args []string) error {
	return ExecuteWithContext(false, func(ctx *CommandContext) error {
		return runOpenWithContext(cmd, args, ctx)
	})(cmd, args)
}

func runOpenWithContext(
	cmd *cobra.Command,
	args []string,
	ctx *CommandContext,
) error {
	guardedOpen, err := validateExpectedOpenFlags(cmd, args)
	if err != nil {
		return err
	}
	if err := tmux.ValidateLayouts(ctx.Config.Layouts, ctx.Config.Agents); err != nil {
		return err
	}
	if openStartSession && len(args) != 1 {
		return fmt.Errorf("--start-session requires an exact workspace path")
	}
	finder := ctx.GetGlobalFinder()
	selectLayout := func(layouts []models.Layout) (models.Layout, error) {
		selected, err := finder.SelectLayout(layouts)
		if err != nil {
			return models.Layout{}, err
		}
		return *selected, nil
	}
	if len(args) == 1 {
		if workspace, ok := findRegisteredDirectoryWorkspace(
			ctx.Config.Workspaces,
			args[0],
		); ok {
			if guardedOpen {
				return service.NewError(
					service.InvalidRequest,
					"guarded open applies only to registered Git worktrees",
					false, nil, nil,
				)
			}
			return openSelectedDirectoryWorkspace(
				cmd.Context(),
				ctx,
				workspace,
				selectLayout,
				openStartSession,
				config.StdinInteractive(),
			)
		}
	}

	var entry *discovery.GlobalWorktreeEntry
	requestedPath := ""
	if guardedOpen {
		requestedPath = args[0]
		entry, err = resolveExpectedOpenWorktree(ctx, requestedPath)
	} else {
		entry, requestedPath, err = resolveOpenWorktree(
			ctx,
			args,
			openStartSession,
		)
	}
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
	return openSelectedWorktree(
		cmd.Context(),
		ctx,
		entry,
		selectLayout,
		openStartSession,
		config.StdinInteractive(),
	)
}

func resolveExpectedOpenWorktree(
	ctx *CommandContext,
	path string,
) (*discovery.GlobalWorktreeEntry, error) {
	entry, err := discoverOpenWorktree(path, ctx.Config.Projects)
	if err != nil || entry == nil || entry.RepositoryInfo == nil ||
		utils.PathKey(entry.Path) != utils.PathKey(path) {
		return nil, registrationChangedOpenError(err)
	}
	return entry, nil
}

func validateExpectedOpenFlags(cmd *cobra.Command, args []string) (bool, error) {
	names := []string{
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
	}
	values := []string{
		openExpectedRepository,
		openExpectedRegistration,
		openExpectedGeneration,
		openExpectedSession,
	}
	set := 0
	for _, name := range names {
		if commandFlagChanged(cmd, name) {
			set++
		}
	}
	if set == 0 {
		return false, nil
	}
	if set != len(values) {
		return false, service.NewError(
			service.InvalidRequest,
			"expected repository, registration, generation, and session must be provided together",
			false, nil, nil,
		)
	}
	for _, value := range values {
		if value == "" {
			return false, service.NewError(
				service.InvalidRequest,
				"expected repository, registration, generation, and session must be nonempty",
				false, nil, nil,
			)
		}
	}
	if !lifecycle.EqualProjectIdentity(openExpectedRepository, openExpectedRepository) {
		return false, service.NewError(
			service.InvalidRequest,
			"expected repository identity is invalid",
			false, nil, nil,
		)
	}
	if !config.ValidProjectRegistrationFingerprint(openExpectedRegistration) {
		return false, service.NewError(
			service.InvalidRequest,
			"expected project registration fingerprint is invalid",
			false, nil, nil,
		)
	}
	if len(args) != 1 {
		return false, service.NewError(
			service.InvalidRequest,
			"guarded open requires one exact worktree path",
			false, nil, nil,
		)
	}
	if err := git.ValidateWorktreeGeneration(openExpectedGeneration); err != nil {
		return false, service.NewError(
			service.InvalidRequest,
			"expected worktree generation is invalid",
			false, nil, err,
		)
	}
	return true, nil
}

func resolveOpenWorktree(
	ctx *CommandContext,
	args []string,
	startSession bool,
) (*discovery.GlobalWorktreeEntry, string, error) {
	requestedPath := ""
	if startSession {
		requestedPath = args[0]
		entry, err := discoverOpenWorktree(
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
		if entry, err := discoverOpenWorktree(
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
	if err := rejectProtectedWorkspaceOpen(
		commandCtx,
		entry.Path,
		entry.Generation,
	); err != nil {
		return err
	}
	if openExpectedRepository == "" {
		if err := acknowledgeRemoteSourcePath(entry.Path); err != nil {
			return err
		}
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

	protectedNames := credentials.ProtectedNames(ctx.Config)
	runner := newOpenWorkspaceRunner(protectedNames)
	if openExpectedRepository != "" {
		return openExpectedWorktree(
			commandCtx,
			entry,
			layout,
			runner,
			startSession,
			os.Getenv("TMUX") != "",
			protectedNames,
		)
	}
	session, err := runWorktreeSessionEstablishment(
		commandCtx,
		entry.Path,
		entry.Generation,
		protectedNames,
		func(session string) error {
			return runner.EnsureWithGeneration(
				commandCtx, session, entry.Path, entry.Generation, layout,
			)
		},
	)
	if err != nil || startSession {
		return err
	}
	return runner.Attach(session, os.Getenv("TMUX") != "")
}

func openExpectedWorktree(
	ctx context.Context,
	entry *discovery.GlobalWorktreeEntry,
	layout models.Layout,
	runner openWorkspaceRunner,
	startSession bool,
	insideTmux bool,
	protectedNames []string,
) error {
	mainPath, err := git.New(entry.Path).GetMainRepositoryPath()
	if err != nil {
		return fmt.Errorf("failed to find repository root: %w", err)
	}
	home, err := config.CanonicalHome()
	if err != nil {
		return err
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return err
	}
	guard, err := observeExpectedGuardedProjectOperation(
		ctx,
		home,
		mainPath,
		expansion,
		openExpectedRepository,
		openExpectedRegistration,
	)
	if err != nil {
		return err
	}

	err = guard.run(ctx, func() error {
		current, discoverErr := discoverOpenWorktree(
			entry.Path,
			[]models.Project{guard.claim.Registration.Effective},
		)
		if discoverErr != nil || current == nil || current.RepositoryInfo == nil {
			return registrationChangedOpenError(discoverErr)
		}
		currentSession := tmux.WorkspaceSessionName(
			current.RepositoryInfo,
			current.Branch,
			current.Path,
		)
		if utils.PathKey(current.Path) != utils.PathKey(entry.Path) ||
			!lifecycle.EqualProjectIdentity(current.RepositoryInfo.FullPath, openExpectedRepository) ||
			current.Generation != openExpectedGeneration ||
			currentSession != openExpectedSession {
			return registrationChangedOpenError(nil)
		}
		_, generationErr := withCurrentWorktreeSession(
			ctx,
			mainPath,
			current.Path,
			openExpectedGeneration,
			[]models.Project{guard.claim.Registration.Effective},
			protectedNames,
			func(lockedSession string) error {
				if lockedSession != openExpectedSession {
					return registrationChangedOpenError(nil)
				}
				if err := acknowledgeRemoteSourcePath(current.Path); err != nil {
					return err
				}
				return runner.EnsureWithGeneration(
					ctx,
					openExpectedSession,
					current.Path,
					openExpectedGeneration,
					layout,
				)
			},
		)
		var conditionErr *git.ConditionError
		if errors.As(generationErr, &conditionErr) &&
			conditionErr.Reason == git.ReasonGenerationChanged {
			return registrationChangedOpenError(generationErr)
		}
		return generationErr
	})
	if err != nil || startSession {
		return err
	}
	return runner.Attach(openExpectedSession, insideTmux)
}

func registrationChangedOpenError(cause error) error {
	return service.NewError(
		service.RegistrationChanged,
		"the worktree changed before it was opened",
		true, nil, cause,
	)
}

func openSelectedDirectoryWorkspace(
	commandCtx context.Context,
	ctx *CommandContext,
	workspace models.Workspace,
	selectLayout func([]models.Layout) (models.Layout, error),
	startSession bool,
	stdinInteractive bool,
) error {
	if err := rejectProtectedWorkspaceOpen(
		commandCtx,
		workspace.Path,
		"",
	); err != nil {
		return err
	}
	if err := acknowledgeRemoteSourcePath(workspace.Path); err != nil {
		return err
	}
	sessions, err := listWorkspaceSessions()
	if err != nil {
		return fmt.Errorf("failed to list tmux sessions: %w", err)
	}
	records := directoryWorkspaceRecords(
		[]models.Workspace{workspace},
		sessions,
	)
	if len(records) != 1 {
		return fmt.Errorf("could not resolve directory workspace %s", workspace.Path)
	}
	layout, err := resolveDirectoryWorkspaceLayout(
		ctx.Config,
		workspace,
		openLayout,
		openSelectLayout,
		selectLayout,
		stdinInteractive && !startSession,
	)
	if err != nil {
		return err
	}
	runner := newOpenWorkspaceRunner(credentials.ProtectedNames(ctx.Config))
	if startSession {
		return runner.Ensure(
			commandCtx,
			records[0].SessionName,
			workspace.Path,
			layout,
		)
	}
	return runner.EnsureAndAttach(
		commandCtx,
		records[0].SessionName,
		workspace.Path,
		layout,
		os.Getenv("TMUX") != "",
	)
}

func resolveDirectoryWorkspaceLayout(
	cfg *models.Config,
	workspace models.Workspace,
	layoutName string,
	selectLayout bool,
	chooseLayout func([]models.Layout) (models.Layout, error),
	interactive bool,
) (models.Layout, error) {
	targetDefault := ""
	if shouldLoadTargetDefault(layoutName, selectLayout) {
		var err error
		targetDefault, err = config.LoadRepoLayoutDefault(
			workspace.Path,
			interactive,
		)
		if err != nil {
			return models.Layout{}, err
		}
	}
	layout, err := tmux.ResolveLayout(
		cfg.Layouts,
		layoutName,
		selectLayout,
		targetDefault,
		chooseLayout,
	)
	if err != nil {
		return models.Layout{}, err
	}
	return tmux.ResolvePaneCommands(layout, cfg.Agents)
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
