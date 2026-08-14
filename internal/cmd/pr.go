package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	urlutil "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type prService interface {
	List(context.Context, pullrequest.Project, string) ([]pullrequest.PullRequest, error)
	Import(context.Context, pullrequest.Project, string) (pullrequest.ImportResult, error)
}

var (
	prProject      string
	prState        string
	prJSON         bool
	prStartSession bool

	loadPRConfig                     = config.Load
	loadPRTargetConfig               = config.LoadForTarget
	newPRService                     = defaultNewPRService
	newPRGitHubProvider              = pullrequest.NewAuthenticatedGitHubProvider
	validatePRProjectRoot            = defaultValidatePRProjectRoot
	inspectPRProjectClone            = defaultInspectPRProjectClone
	validatePRWorkspaceSessionConfig = defaultValidatePRWorkspaceSessionConfig
	ensurePRWorkspaceSession         = defaultStartPRWorkspaceSession
	attachExistingPRWorkspaceSession = defaultAttachExistingPRWorkspaceSession
	readPRWorkspaceGeneration        = func(path string) (string, error) {
		return gitadapter.New(path).ReadWorktreeGeneration(path)
	}
)

func provenanceGenerationMatches(recorded, live string) bool {
	return recorded == "" || recorded == live
}

var prCmd = &cobra.Command{
	Use:   "pr",
	Short: "Discover and import pull requests as kwt workspaces",
	Args:  prNoArgs,
	// Pull-request commands select an explicit globally registered project.
	// A caller's cwd-local config must never alter a remote/SSH automation call.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := requireConfigInitialization(); err != nil {
			return writePRError(cmd, pullrequest.NewError(
				pullrequest.CodeWorkspaceCreation,
				"failed to initialize configuration",
				false,
				err,
			))
		}
		return nil
	},
}

var prListCmd = &cobra.Command{
	Use:   "list",
	Short: "List importable pull requests as JSON",
	Args:  prNoArgs,
	RunE:  withGracefulSignals(runPRList),
}

var prImportCmd = &cobra.Command{
	Use:   "import <pull-request>",
	Short: "Import a pull request as a configured kwt workspace",
	Args:  prExactArgs(1),
	RunE:  withGracefulSignals(runPRImport),
}

var prAttachCmd = &cobra.Command{
	Use:   "attach <workspace-path>",
	Short: "Attach to a protected imported workspace session",
	Args:  prExactArgs(1),
	RunE:  withGracefulSignals(runPRAttach),
}

func init() {
	rootCmd.AddCommand(prCmd)
	prCmd.AddCommand(prListCmd, prImportCmd, prAttachCmd)
	prCmd.PersistentFlags().StringVar(&prProject, "project", "", "registered project identity, name, or path (defaults to current repository)")
	prCmd.PersistentFlags().BoolVar(&prJSON, "json", true, "emit the stable JSON automation contract")
	prListCmd.Flags().StringVar(&prState, "state", "open", "pull-request state: open, closed, or all")
	prImportCmd.Flags().BoolVar(
		&prStartSession,
		"start-session",
		false,
		"ensure the imported workspace's tmux session exists without attaching",
	)
	prCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writePRError(cmd, pullrequest.NewError(pullrequest.CodeInvalidSelector, err.Error(), false, nil))
	})
}

func runPRList(cmd *cobra.Command, _ []string) error {
	if prState != "open" && prState != "closed" && prState != "all" {
		return writePRError(cmd, pullrequest.NewError(
			pullrequest.CodeInvalidSelector, "state must be open, closed, or all", false, nil))
	}
	project, err := preparePRProject()
	if err != nil {
		return writePRError(cmd, err)
	}
	service, _, project, err := preparePRService(cmd.Context(), project)
	if err != nil {
		return writePRError(cmd, err)
	}
	prs, err := service.List(cmd.Context(), project, prState)
	if err != nil {
		return writePRError(cmd, err)
	}
	return writePRJSON(cmd, struct {
		PullRequests []pullrequest.PullRequest `json:"pull_requests"`
	}{PullRequests: nonNilPullRequests(prs)})
}

func runPRImport(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return prExactArgs(1)(cmd, args)
	}
	project, err := preparePRProject()
	if err != nil {
		return writePRError(cmd, err)
	}
	number, err := pullrequest.ParseSelectorNumber(args[0])
	if err != nil {
		return writePRError(cmd, err)
	}
	registeredIdentity := project.Identity
	service, cfg, project, err := preparePRService(cmd.Context(), project)
	if err != nil {
		return writePRError(cmd, err)
	}
	if _, err := pullrequest.ParseSelector(args[0], registeredIdentity); err != nil {
		if _, resolvedErr := pullrequest.ParseSelector(args[0], project.Identity); resolvedErr != nil {
			return writePRError(cmd, resolvedErr)
		}
	}
	if prStartSession {
		if err := validatePRWorkspaceSessionConfig(cfg); err != nil {
			return writePRError(cmd, pullrequest.NewError(
				pullrequest.CodeWorkspaceCreation,
				"invalid imported workspace session configuration",
				false,
				err,
			))
		}
	}
	home, err := config.CanonicalHome()
	if err != nil {
		return writePRError(cmd, err)
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return writePRError(cmd, err)
	}
	guard, err := observeRequiredGuardedProjectOperation(
		cmd.Context(), home, project.Path, expansion,
		registeredIdentity, project.Identity,
	)
	if err != nil {
		return writePRError(cmd, err)
	}
	var result pullrequest.ImportResult
	err = guard.run(cmd.Context(), func() error {
		var importErr error
		result, importErr = service.Import(
			cmd.Context(), project, fmt.Sprintf("%d", number),
		)
		if importErr != nil {
			return importErr
		}
		result.Workspace.TmuxSocketName = tmux.ProtectedWorkspaceSocketName(
			result.Workspace.SessionName,
			result.Workspace.Path,
		)
		result.PullRequest.Workspace = &result.Workspace
		if !prStartSession {
			return nil
		}
		socketName, startErr := ensurePRWorkspaceSession(
			cmd.Context(), result.Workspace, cfg,
		)
		if startErr != nil {
			message := "failed to start imported workspace session"
			var safetyError *tmux.SessionSafetyError
			if errors.As(startErr, &safetyError) {
				message = safetyError.Error()
			}
			result.SessionStartError = pullrequest.NewError(
				pullrequest.CodeWorkspaceCreation,
				message,
				false,
				startErr,
			)
			return nil
		}
		result.Workspace.TmuxSocketName = socketName
		return nil
	})
	if err != nil {
		return writePRError(cmd, err)
	}
	result.PullRequest.Workspace = &result.Workspace
	return writePRJSON(cmd, result)
}

func runPRAttach(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return prExactArgs(1)(cmd, args)
	}
	record, err := importedWorkspaceProvenance(
		cmd.Context(),
		args[0],
	)
	if err != nil {
		return writePRError(cmd, err)
	}
	cfg, err := loadPRTargetConfig(record.Project.Path, false)
	if err != nil {
		return writePRError(cmd, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation,
			"failed to load imported workspace configuration",
			false,
			err,
		))
	}
	home, err := config.CanonicalHome()
	if err != nil {
		return writePRError(cmd, err)
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return writePRError(cmd, err)
	}
	guard, err := observeRequiredGuardedProjectOperation(
		cmd.Context(), home, record.Project.Path, expansion,
		pullrequest.ProvenanceRepositoryIdentities(record)...,
	)
	if err != nil {
		return writePRError(cmd, err)
	}
	var socketName string
	err = guard.run(cmd.Context(), func() error {
		current, currentErr := importedWorkspaceProvenance(
			cmd.Context(), args[0],
		)
		if currentErr != nil {
			return currentErr
		}
		if !provenanceMatchesProjectClaim(current, guard.claim) {
			return service.NewError(
				service.RegistrationChanged,
				"the imported workspace changed projects before attachment",
				true, nil, nil,
			)
		}
		record = current
		socketName, currentErr = ensurePRWorkspaceSession(
			cmd.Context(), record.Workspace, cfg,
		)
		return currentErr
	})
	if service.IsCode(err, service.RegistrationChanged) {
		return writePRError(cmd, err)
	}
	if err == nil {
		err = attachExistingPRWorkspaceSession(
			cmd.Context(), record.Workspace, cfg, socketName,
		)
	}
	if err != nil {
		message := "failed to attach imported workspace session"
		var safetyError *tmux.SessionSafetyError
		if errors.As(err, &safetyError) {
			message = safetyError.Error()
		}
		return writePRError(cmd, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation,
			message,
			false,
			err,
		))
	}
	return nil
}

func provenanceMatchesProjectClaim(
	record pullrequest.Provenance,
	claim *lifecycle.ProjectClaim,
) bool {
	if claim == nil || utils.PathKey(record.Project.Path) !=
		utils.PathKey(claim.Registration.Effective.Path) {
		return false
	}
	return projectClaimHasExpectedIdentity(
		claim, pullrequest.ProvenanceRepositoryIdentities(record),
	)
}

func importedWorkspaceProvenance(
	ctx context.Context,
	workspacePath string,
) (pullrequest.Provenance, error) {
	path := utils.CanonicalPath(workspacePath)
	var pathMatches []pullrequest.Provenance
	err := pullrequest.NewFileStore(prStorePath()).View(
		ctx,
		func(records map[string]pullrequest.Provenance) error {
			for _, record := range records {
				if utils.CanonicalPath(record.Workspace.Path) == path {
					pathMatches = append(pathMatches, record)
				}
			}
			return nil
		},
	)
	if err != nil {
		return pullrequest.Provenance{}, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation,
			"failed to read pull-request provenance",
			false,
			err,
		)
	}
	liveGeneration := ""
	for _, record := range pathMatches {
		if record.Workspace.Generation == "" {
			continue
		}
		liveGeneration, err = readPRWorkspaceGeneration(workspacePath)
		if err != nil {
			return pullrequest.Provenance{}, pullrequest.NewError(
				pullrequest.CodeWorkspaceCreation,
				"failed to inspect live workspace generation",
				false,
				err,
			)
		}
		break
	}
	matches := make([]pullrequest.Provenance, 0, len(pathMatches))
	for _, record := range pathMatches {
		if provenanceGenerationMatches(
			record.Workspace.Generation,
			liveGeneration,
		) {
			matches = append(matches, record)
		}
	}
	if len(matches) != 1 {
		return pullrequest.Provenance{}, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation,
			"workspace is not a uniquely verified pull-request import",
			false,
			nil,
		)
	}
	record := matches[0]
	liveProject, liveWorkspaces, err := inspectPRProjectClone(
		ctx,
		record,
	)
	if err != nil {
		return pullrequest.Provenance{}, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation,
			"failed to verify imported workspace provenance",
			false,
			err,
		)
	}
	if !samePRProjectClone(record, liveProject) ||
		!containsPRWorkspace(liveWorkspaces, record) {
		return pullrequest.Provenance{}, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation,
			"workspace no longer matches its pull-request provenance",
			false,
			nil,
		)
	}
	return record, nil
}

func rejectProtectedWorkspaceOpen(
	ctx context.Context,
	workspacePath string,
	liveGeneration string,
) error {
	var recordedWorkspaces []pullrequest.Workspace
	err := pullrequest.NewFileStore(prStorePath()).View(
		ctx,
		func(records map[string]pullrequest.Provenance) error {
			for _, record := range records {
				recordedWorkspaces = append(recordedWorkspaces, record.Workspace)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf(
			"failed to verify pull-request protection for workspace: %w",
			err,
		)
	}
	if len(recordedWorkspaces) == 0 {
		return nil
	}
	pathMatched := false
	for _, workspace := range recordedWorkspaces {
		pathMatched = pathMatched || samePRPath(workspace.Path, workspacePath)
	}
	if pathMatched {
		liveGeneration, err = readPRWorkspaceGeneration(workspacePath)
		if err != nil {
			return fmt.Errorf(
				"failed to verify live generation for pull-request workspace: %w",
				err,
			)
		}
	}
	if liveGeneration != "" {
		if err := gitadapter.ValidateWorktreeGeneration(liveGeneration); err != nil {
			return fmt.Errorf(
				"failed to verify live generation for pull-request workspace: %w",
				err,
			)
		}
	}
	for _, workspace := range recordedWorkspaces {
		generationMatch := workspace.Generation != "" &&
			workspace.Generation == liveGeneration
		legacyPathMatch := workspace.Generation == "" &&
			samePRPath(workspace.Path, workspacePath)
		if !generationMatch && !legacyPathMatch {
			continue
		}
		return fmt.Errorf(
			"protected pull-request workspaces must be opened with kwt pr attach %s",
			workspacePath,
		)
	}
	return nil
}

func defaultInspectPRProjectClone(
	ctx context.Context,
	recorded pullrequest.Provenance,
) (pullrequest.Project, []pullrequest.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return pullrequest.Project{}, nil, err
	}
	project, err := validatePRProjectRoot(recorded.Project)
	if err != nil {
		return pullrequest.Project{}, nil, err
	}
	cfg, err := loadPRConfig()
	if err != nil {
		return pullrequest.Project{}, nil, fmt.Errorf(
			"load registered project inventory: %w",
			err,
		)
	}
	registered := make([]models.Project, 0, 1)
	for _, candidate := range cfg.Projects {
		if utils.CanonicalPath(candidate.Path) !=
			utils.CanonicalPath(recorded.Project.Path) {
			continue
		}
		registered = append(registered, candidate)
	}
	if len(registered) > 1 {
		return pullrequest.Project{}, nil, fmt.Errorf(
			"recorded project is not uniquely registered",
		)
	}
	if len(registered) == 1 &&
		!pullrequest.EqualRepositoryIdentity(
			publishableProjectRepository(registered[0]),
			recorded.Project.Identity,
		) &&
		!pullrequest.ProvenanceHasRepositoryIdentity(
			recorded,
			publishableProjectRepository(registered[0]),
		) {
		return pullrequest.Project{}, nil, fmt.Errorf(
			"registered project conflicts with recorded identity",
		)
	}
	g := gitadapter.New(project.Path)
	info, err := worktree.RepositoryInfoWithProjects(
		g,
		registered,
	)
	if err != nil {
		return pullrequest.Project{}, nil, fmt.Errorf(
			"resolve recorded project repository: %w",
			err,
		)
	}
	live, err := worktree.New(g, nil).List()
	if err != nil {
		return pullrequest.Project{}, nil, fmt.Errorf(
			"list recorded project worktrees: %w",
			err,
		)
	}
	if err := ctx.Err(); err != nil {
		return pullrequest.Project{}, nil, err
	}
	project.Identity = pullrequest.NormalizeRepositoryIdentity(info.FullPath)
	if !pullrequest.EqualRepositoryIdentity(
		recorded.Project.Identity,
		project.Identity,
	) && !pullrequest.ProvenanceHasRepositoryIdentity(
		recorded,
		project.Identity,
	) {
		return pullrequest.Project{}, nil, fmt.Errorf(
			"live project conflicts with recorded identity",
		)
	}
	return project, livePRWorkspaces(info, project, live), nil
}

func livePRWorkspaces(
	info *urlutil.RepositoryInfo,
	project pullrequest.Project,
	live []models.Worktree,
) []pullrequest.Workspace {
	workspaces := make([]pullrequest.Workspace, 0, len(live))
	for _, candidate := range live {
		if candidate.Prunable || !liveGitWorktreePath(candidate.Path) {
			continue
		}
		workspaces = append(workspaces, pullrequest.Workspace{
			Path:       candidate.Path,
			Branch:     candidate.Branch,
			Repository: project.Identity,
			Generation: candidate.Generation,
			SessionName: tmux.WorkspaceSessionName(
				info,
				candidate.Branch,
				candidate.Path,
			),
		})
	}
	return workspaces
}

func liveGitWorktreePath(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func samePRProjectClone(
	recorded pullrequest.Provenance,
	live pullrequest.Project,
) bool {
	if utils.CanonicalPath(recorded.Project.Path) !=
		utils.CanonicalPath(live.Path) {
		return false
	}
	if pullrequest.EqualRepositoryIdentity(
		recorded.Project.Identity,
		live.Identity,
	) {
		return true
	}
	return pullrequest.ProvenanceHasRepositoryIdentity(
		recorded,
		recorded.Project.Identity,
	) && pullrequest.ProvenanceHasRepositoryIdentity(
		recorded,
		live.Identity,
	)
}

func containsPRWorkspace(
	live []pullrequest.Workspace,
	recorded pullrequest.Provenance,
) bool {
	for _, candidate := range live {
		if utils.CanonicalPath(candidate.Path) ==
			utils.CanonicalPath(recorded.Workspace.Path) &&
			candidate.Branch == recorded.Workspace.Branch &&
			provenanceGenerationMatches(
				recorded.Workspace.Generation,
				candidate.Generation,
			) &&
			prWorkspaceIdentityMatches(candidate, recorded) {
			return true
		}
	}
	return false
}

func prWorkspaceIdentityMatches(
	live pullrequest.Workspace,
	recorded pullrequest.Provenance,
) bool {
	sameRepository := pullrequest.EqualRepositoryIdentity(
		live.Repository,
		recorded.Workspace.Repository,
	)
	if sameRepository && live.SessionName == recorded.Workspace.SessionName {
		return true
	}
	if !sameRepository && (!pullrequest.ProvenanceHasRepositoryIdentity(recorded, live.Repository) ||
		!pullrequest.ProvenanceHasRepositoryIdentity(
			recorded,
			recorded.Workspace.Repository,
		)) {
		return false
	}
	for _, identity := range pullrequest.ProvenanceRepositoryIdentities(recorded) {
		info, ok := urlutil.CanonicalRepositoryInfo(identity)
		if ok && tmux.MatchesWorkspaceSessionName(
			recorded.Workspace.SessionName,
			info,
			recorded.Workspace.Branch,
			recorded.Workspace.Path,
		) {
			return true
		}
	}
	return false
}

func preparePRProject() (pullrequest.Project, error) {
	cfg, err := loadPRConfig()
	if err != nil {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation, "failed to load kwt configuration", false, err)
	}
	project, err := resolvePRProject(cfg, prProject)
	if err != nil {
		return pullrequest.Project{}, err
	}
	return project, nil
}

func preparePRService(
	ctx context.Context,
	project pullrequest.Project,
) (prService, *models.Config, pullrequest.Project, error) {
	project, err := validatePRProjectRoot(project)
	if err != nil {
		return nil, nil, project, err
	}
	cfg, err := loadPRTargetConfig(project.Path, false)
	if err != nil {
		return nil, nil, project, pullrequest.NewError(
			pullrequest.CodeWorkspaceCreation, "failed to load selected project configuration", false, err)
	}
	service, project, err := newPRService(ctx, cfg, project)
	if err != nil {
		return nil, nil, project, err
	}
	return service, cfg, project, nil
}

func defaultStartPRWorkspaceSession(
	ctx context.Context,
	workspace pullrequest.Workspace,
	cfg *models.Config,
) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("kwt configuration is unavailable")
	}
	if strings.TrimSpace(workspace.Path) == "" ||
		strings.TrimSpace(workspace.SessionName) == "" {
		return "", fmt.Errorf("imported workspace has no tmux identity")
	}
	layout, err := preparePRWorkspaceSessionLayout(cfg)
	if err != nil {
		return "", err
	}
	stripNames := credentials.ProtectedNames(cfg)
	socketName := tmux.ProtectedWorkspaceSocketName(
		workspace.SessionName,
		workspace.Path,
	)
	tmuxCommand, _, err := tmux.ResolveProtectedSessionCommand(
		ctx,
		socketName,
		workspace.SessionName,
		stripNames,
		os.Getenv("TMUX_TMPDIR"),
	)
	if err != nil {
		return "", err
	}
	err = tmux.NewProtectedWorkspaceRunner(
		tmuxCommand,
		stripNames,
	).EnsureWithGeneration(
		ctx,
		workspace.SessionName,
		workspace.Path,
		workspace.Generation,
		layout,
	)
	if err != nil {
		return "", err
	}
	return socketName, nil
}

func defaultValidatePRWorkspaceSessionConfig(
	cfg *models.Config,
) error {
	if cfg == nil {
		return fmt.Errorf("kwt configuration is unavailable")
	}
	_, err := preparePRWorkspaceSessionLayout(cfg)
	return err
}

func preparePRWorkspaceSessionLayout(
	cfg *models.Config,
) (models.Layout, error) {
	if cfg == nil {
		return models.Layout{}, fmt.Errorf("kwt configuration is unavailable")
	}
	// Imported contributor-controlled content must never trigger configured
	// layout or agent commands merely because a client imported or opened it.
	// A protected PR workspace starts as one ordinary login shell; the user
	// may explicitly run anything else after inspecting the checkout.
	return tmux.BlankLayout(), nil
}

func defaultAttachExistingPRWorkspaceSession(
	ctx context.Context,
	workspace pullrequest.Workspace,
	cfg *models.Config,
	socketName string,
) error {
	if cfg == nil {
		return fmt.Errorf("kwt configuration is unavailable")
	}
	if strings.TrimSpace(workspace.Path) == "" ||
		strings.TrimSpace(workspace.SessionName) == "" {
		return fmt.Errorf("imported workspace has no tmux identity")
	}
	stripNames := credentials.ProtectedNames(cfg)
	tmuxCommand, _, err := tmux.ResolveProtectedSessionCommand(
		ctx,
		socketName,
		workspace.SessionName,
		stripNames,
		os.Getenv("TMUX_TMPDIR"),
	)
	if err != nil {
		return err
	}
	return tmuxCommand.AttachSessionWithoutEnvironment(
		ctx, workspace.SessionName,
	)
}

func defaultNewPRService(
	ctx context.Context,
	cfg *models.Config,
	project pullrequest.Project,
) (prService, pullrequest.Project, error) {
	requestedRepository, err := pullrequest.RepositoryFromProject(project)
	if err != nil {
		return nil, project, err
	}
	provider, err := newPRGitHubProvider(ctx)
	if err != nil {
		return nil, project, err
	}
	repository, err := provider.ResolveRepository(ctx, requestedRepository)
	if err != nil {
		return nil, project, err
	}
	project.Identity = repository.Identity
	g := gitadapter.New(project.Path)
	manager := worktree.New(g, cfg)
	backend := pullrequest.NewGitBackend(
		g,
		manager,
		project,
		func() (pullrequest.WorktreeCreationGuard, error) {
			return registry.New()
		},
		cfg.Fleet.TokenEnv,
	)
	return pullrequest.NewService(
		provider,
		backend,
		pullrequest.NewFileStore(prStorePath()),
		requestedRepository.Identity,
		repository.Identity,
	), project, nil
}

func resolvePRProject(cfg *models.Config, selector string) (pullrequest.Project, error) {
	if cfg == nil {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "kwt project configuration is unavailable", false, nil)
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		g, err := gitadapter.NewFromCwd()
		if err != nil {
			return pullrequest.Project{}, pullrequest.NewError(
				pullrequest.CodeRepositoryMismatch, "--project is required outside a Git repository", false, err)
		}
		mainPath, err := g.GetMainRepositoryPath()
		if err != nil {
			return pullrequest.Project{}, pullrequest.NewError(
				pullrequest.CodeRepositoryMismatch, "failed to identify the current project", false, err)
		}
		for _, candidate := range cfg.Projects {
			if samePRPath(candidate.Path, mainPath) {
				return prProjectFromModel(candidate)
			}
		}
		info, err := worktree.RepositoryInfoWithProjects(g, cfg.Projects)
		if err != nil {
			return pullrequest.Project{}, pullrequest.NewError(
				pullrequest.CodeRepositoryMismatch, "current repository has no stable provider identity", false, err)
		}
		return validatePRProject(pullrequest.Project{Identity: info.FullPath, Name: info.Repository, Path: mainPath})
	}

	for _, candidate := range cfg.Projects {
		identity := publishableProjectRepository(candidate)
		if pullrequest.EqualRepositoryIdentity(selector, identity) {
			candidate.Repository = identity
			return prProjectFromModel(candidate)
		}
	}
	var nameMatches []models.Project
	for _, candidate := range cfg.Projects {
		if strings.EqualFold(selector, candidate.Name) {
			nameMatches = append(nameMatches, candidate)
		}
	}
	if len(nameMatches) == 1 {
		candidate := nameMatches[0]
		candidate.Repository = publishableProjectRepository(candidate)
		return prProjectFromModel(candidate)
	}
	if len(nameMatches) > 1 {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch,
			fmt.Sprintf("project name %q is ambiguous; select by repository identity or path", selector), false, nil)
	}
	if filepath.IsAbs(selector) {
		canonicalSelector, canonicalErr := canonicalPRPathSelector(selector)
		if canonicalErr != nil {
			return pullrequest.Project{}, pullrequest.NewError(
				pullrequest.CodeRepositoryMismatch,
				"project path selectors must be absolute, canonical paths", false, canonicalErr)
		}
		for _, candidate := range cfg.Projects {
			if samePRPath(canonicalSelector, candidate.Path) {
				candidate.Repository = publishableProjectRepository(candidate)
				return prProjectFromModel(candidate)
			}
		}
	}
	return pullrequest.Project{}, pullrequest.NewError(
		pullrequest.CodeRepositoryMismatch, fmt.Sprintf("no kwt-managed project matches %q", selector), false, nil)
}

func canonicalPRPathSelector(path string) (string, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) || cleaned != path {
		return "", fmt.Errorf("path is not absolute and clean")
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != cleaned {
		return "", fmt.Errorf("path contains symlink components")
	}
	return cleaned, nil
}

func prProjectFromModel(project models.Project) (pullrequest.Project, error) {
	identity := publishableProjectRepository(project)
	return validatePRProject(pullrequest.Project{Identity: identity, Name: project.Name, Path: project.Path})
}

func validatePRProject(project pullrequest.Project) (pullrequest.Project, error) {
	info, ok := urlutil.CanonicalRepositoryInfo(project.Identity)
	if !ok || !strings.EqualFold(info.Host, "github.com") {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeUnsupportedProvider,
			fmt.Sprintf("project %q is not a supported github.com repository", project.Identity), false, nil)
	}
	project.Identity = pullrequest.NormalizeRepositoryIdentity(info.FullPath)
	if strings.TrimSpace(project.Path) == "" {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project has no repository path", false, nil)
	}
	if strings.TrimSpace(project.Name) == "" {
		project.Name = info.Repository
	}
	return project, nil
}

func defaultValidatePRProjectRoot(project pullrequest.Project) (pullrequest.Project, error) {
	path := strings.TrimSpace(project.Path)
	if path == "" {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project has no repository path", false, nil)
	}
	if !filepath.IsAbs(path) {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project path must be absolute", false, nil)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project path is invalid", false, err)
	}
	canonicalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project path is unavailable", false, err)
	}
	info, err := os.Stat(canonicalPath)
	if err != nil || !info.IsDir() {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project path is not a directory", false, err)
	}
	mainPath, err := gitadapter.New(canonicalPath).GetMainRepositoryPath()
	if err != nil {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project path is not a Git repository", false, err)
	}
	canonicalMain, err := filepath.EvalSymlinks(mainPath)
	if err != nil {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch, "selected project main repository is unavailable", false, err)
	}
	if filepath.Clean(canonicalPath) != filepath.Clean(canonicalMain) {
		return pullrequest.Project{}, pullrequest.NewError(
			pullrequest.CodeRepositoryMismatch,
			"selected project path is not the main repository root", false, nil)
	}
	project.Path = canonicalPath
	return project, nil
}

func prNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return writePRError(cmd, pullrequest.NewError(
		pullrequest.CodeInvalidSelector, "this command does not accept positional arguments", false, nil))
}

func prExactArgs(count int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == count {
			return nil
		}
		return writePRError(cmd, pullrequest.NewError(
			pullrequest.CodeInvalidSelector, fmt.Sprintf("expected %d pull-request selector, received %d", count, len(args)), false, nil))
	}
}

func samePRPath(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return utils.PathKey(left) == utils.PathKey(right)
	}
	if resolved, err := filepath.EvalSymlinks(leftAbs); err == nil {
		leftAbs = resolved
	}
	if resolved, err := filepath.EvalSymlinks(rightAbs); err == nil {
		rightAbs = resolved
	}
	return utils.PathKey(leftAbs) == utils.PathKey(rightAbs)
}

func prStorePath() string {
	if kwtHome := strings.TrimSpace(os.Getenv("KWT_HOME")); kwtHome != "" {
		if expanded, err := utils.ExpandPath(kwtHome); err == nil {
			return filepath.Join(expanded, "pull-requests.json")
		}
		return filepath.Join(kwtHome, "pull-requests.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "kwt", "pull-requests.json")
	}
	return filepath.Join(home, ".config", "kwt", "pull-requests.json")
}

func nonNilPullRequests(prs []pullrequest.PullRequest) []pullrequest.PullRequest {
	if prs == nil {
		return []pullrequest.PullRequest{}
	}
	return prs
}

func writePRJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

type prCommandError struct {
	err *pullrequest.Error
}

func (e *prCommandError) Error() string { return e.err.Error() }
func (e *prCommandError) Unwrap() error { return e.err }
func (e *prCommandError) ExitCode() int { return prExitCode(e.err.Code) }

func writePRError(cmd *cobra.Command, err error) error {
	var serviceErr *service.Error
	if errors.As(err, &serviceErr) {
		return writeCommandFailure(
			cmd, serviceErr.Descriptor, 1, true, "pr",
		)
	}
	typed := pullrequest.AsError(err, pullrequest.CodeWorkspaceCreation, "pull-request operation failed")
	cmd.Root().SilenceUsage = true
	cmd.Root().SilenceErrors = true
	_ = writePRJSON(cmd, pullrequest.ErrorEnvelope{Error: typed})
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "kwt pr: %s: %s\n", typed.Code, typed.Message)
	return &prCommandError{err: typed}
}

func prExitCode(code pullrequest.ErrorCode) int {
	switch code {
	case pullrequest.CodeAuthentication:
		return 3
	case pullrequest.CodeRepositoryMismatch, pullrequest.CodeUnsupportedProvider:
		return 4
	case pullrequest.CodeInvalidSelector:
		return 2
	case pullrequest.CodeNotFound:
		return 5
	case pullrequest.CodeInaccessibleHead:
		return 6
	case pullrequest.CodeNamingConflict:
		return 7
	case pullrequest.CodeNetwork:
		return 8
	case pullrequest.CodeWorkspaceCreation:
		return 9
	case pullrequest.CodeMalformedResponse:
		return 10
	case pullrequest.CodeConflict:
		return 11
	case pullrequest.CodeUnsupportedGitVersion:
		return 12
	default:
		return 1
	}
}
