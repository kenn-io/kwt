package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/prunepolicy"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type pruneMergedRegistry interface {
	List() []*registry.WorktreeEntry
	AcquireCreation(string) (func() error, bool, error)
	EntryMatches(string, *registry.WorktreeEntry) (bool, error)
	RemoveIfMatchAfter(string, *registry.WorktreeEntry, func() error) (bool, error)
}

type pruneMergedProvenanceStore interface {
	View(context.Context, func(map[string]pullrequest.Provenance) error) error
	RemoveIfMatch(context.Context, string, pullrequest.Provenance) (bool, error)
}

type pruneMergedCandidate struct {
	Policy           prunepolicy.MergedCandidate
	RepositoryRoot   string
	ExpectedGitDir   string
	ProvenanceKey    string
	RegistryExpected bool
	RegistryEntry    *registry.WorktreeEntry
	InitialOutcome   *prunepolicy.Outcome
	ProtectedNames   []string
}

type pruneMergedMutationResult struct {
	worktreeRemoved bool
	cleanupProblems []string
	cleanupMessage  string
}

var (
	loadPruneMergedConfig   = config.Load
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) {
		return registry.New()
	}
	openPruneMergedProvenanceStore = func() pruneMergedProvenanceStore {
		return pullrequest.NewFileStore(prStorePath())
	}
	inspectPruneMergedCandidates = defaultInspectPruneMergedCandidates
	findPruneMergedGlobalPaths   = discovery.FindGlobalWorktreePathsStrict
	newPruneMergedProvider       = func(ctx context.Context) (prunepolicy.MergedProvider, error) {
		return pullrequest.NewAuthenticatedGitHubProvider(ctx)
	}
	validatePruneMergedWorktree      = defaultValidatePruneMergedWorktree
	validatePruneMergedDirtyWorktree = defaultValidatePruneMergedDirtyWorktree
	removePruneMergedWorktree        = defaultRemovePruneMergedWorktree
	inspectPruneMergedDirty          = defaultInspectPruneMergedDirty
	confirmPruneMergedDirty          = defaultConfirmPruneMergedDirty
	probePruneMergedProtectedSession = tmux.ProbeProtectedSession
	runPruneMergedProtectedOperation = defaultRunPruneMergedProtectedOperation
)

func defaultRunPruneMergedProtectedOperation(
	ctx context.Context,
	cfg *models.Config,
	candidate pruneMergedCandidate,
	operation func() error,
) error {
	if candidate.Policy.Provenance == nil {
		return operation()
	}
	record := *candidate.Policy.Provenance
	expected := pullrequest.ProvenanceRepositoryIdentities(record)
	if len(expected) == 0 {
		return service.NewError(
			service.ProtectedEndpointInventoryIncomplete,
			"protected endpoint authority is incomplete",
			false, nil, nil,
		)
	}
	home, err := config.CanonicalHome()
	if err != nil {
		return err
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return err
	}
	guard, err := observeRequiredGuardedProjectOperation(
		ctx, home, candidate.RepositoryRoot, expansion, expected...,
	)
	if err != nil {
		return err
	}
	return guard.run(ctx, func() error {
		workspace := record.Workspace
		if workspace.Path == "" || workspace.SessionName == "" ||
			workspace.Generation == "" {
			return service.NewError(
				service.ProtectedEndpointInventoryIncomplete,
				"protected endpoint authority is incomplete",
				false, nil, nil,
			)
		}
		socketName := tmux.ProtectedWorkspaceSocketName(
			workspace.SessionName, workspace.Path,
		)
		protectedNames := append(
			credentials.ProtectedNames(cfg), candidate.ProtectedNames...,
		)
		state, probeErr := probePruneMergedProtectedSession(
			ctx, socketName, workspace.SessionName, protectedNames,
			os.Getenv("TMUX_TMPDIR"),
		)
		switch state {
		case tmux.ProtectedSessionAbsent:
			if probeErr == nil {
				return operation()
			}
		case tmux.ProtectedSessionLive:
			return service.NewError(
				service.ProtectedSessionLive,
				"a protected project session is live",
				false,
				map[string]any{
					"session_name": workspace.SessionName,
					"socket_name":  socketName,
					"generation":   workspace.Generation,
				},
				nil,
			)
		}
		return service.NewError(
			service.ProtectedEndpointInventoryIncomplete,
			"protected session state could not be verified",
			true, nil, probeErr,
		)
	})
}

func pruneMergedOutcomeForError(
	candidate pruneMergedCandidate,
	err error,
) prunepolicy.Outcome {
	typed := service.AsError(err)
	outcome := prunepolicy.Outcome{
		Path: candidate.Policy.Path, Branch: candidate.Policy.Branch,
		Evidence: map[string]string{"head_sha": candidate.Policy.Head},
	}
	switch typed.Code {
	case service.ProtectedSessionLive:
		outcome.Reason = prunepolicy.ProtectedSessionLive
		outcome.Message = "a protected project session is live"
		outcome.Remediation = "Detach or end the protected session, then retry."
		for _, key := range []string{"session_name", "socket_name", "generation"} {
			if value, ok := typed.Details[key].(string); ok && value != "" {
				outcome.Evidence[key] = value
			}
		}
		return outcome
	case service.ProtectedEndpointInventoryIncomplete:
		outcome.Reason = prunepolicy.ProtectedEndpointIncomplete
		outcome.Message = "protected endpoint authority could not be verified"
		outcome.Remediation = "Restore protected endpoint authority and retry."
		return outcome
	case service.RegistrationChanged:
		outcome.Reason = prunepolicy.RegistrationChanged
		outcome.Message = "the project registration changed before pruning"
		outcome.Remediation = "Refresh project inventory and retry."
		return outcome
	default:
		return pruneOutcomeForError(
			candidate.Policy.Path, candidate.Policy.Branch, err,
		)
	}
}

func mergePruneEvidence(
	outcome *prunepolicy.Outcome,
	evidence map[string]string,
) {
	if outcome.Evidence == nil {
		outcome.Evidence = make(map[string]string, len(evidence))
	}
	for key, value := range evidence {
		outcome.Evidence[key] = value
	}
}

func removePruneMergedCandidate(
	ctx context.Context,
	reg pruneMergedRegistry,
	store pruneMergedProvenanceStore,
	candidate pruneMergedCandidate,
	force bool,
) (pruneMergedMutationResult, error) {
	var result pruneMergedMutationResult
	var residualWarning error
	ownershipErr := withPruneMergedOwnershipGuard(
		ctx, reg, store, candidate,
		func() error {
			removed, err := removePruneMergedWorktree(
				candidate,
				force,
				func(remove func() error) (bool, error) {
					return reg.RemoveIfMatchAfter(
						candidate.Policy.Path,
						candidate.RegistryEntry,
						func() error {
							err := remove()
							if git.WorktreeWasRemoved(err) {
								residualWarning = err
								return nil
							}
							return err
						},
					)
				},
			)
			result.worktreeRemoved = removed
			if err == nil && !removed {
				return fmt.Errorf(
					"worktree ownership changed after inspection for %s",
					candidate.Policy.Path,
				)
			}
			return err
		},
	)
	if ownershipErr == nil && residualWarning != nil {
		ownershipErr = residualWarning
		result.cleanupMessage = residualWarning.Error()
	}
	if ownershipErr != nil && !result.worktreeRemoved {
		return result, ownershipErr
	}
	if ownershipErr != nil {
		result.cleanupProblems = append(
			result.cleanupProblems, ownershipErr.Error(),
		)
	}
	if candidate.Policy.Provenance == nil {
		return result, nil
	}
	removed, cleanupErr := store.RemoveIfMatch(
		ctx, candidate.ProvenanceKey, *candidate.Policy.Provenance,
	)
	if cleanupErr != nil {
		result.cleanupProblems = append(
			result.cleanupProblems, "provenance: "+cleanupErr.Error(),
		)
	} else if !removed {
		result.cleanupProblems = append(
			result.cleanupProblems, "provenance record changed",
		)
	}
	return result, nil
}

func runPruneMerged(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	progress := newMaintenanceProgress(cmd, true)
	defer progress.Close()
	progress.Phase("discover candidates", 0)
	cfg, err := loadPruneMergedConfig()
	if err != nil {
		progress.Pause()
		return mergedPruneExecutionError(cmd, fmt.Sprintf("load global configuration: %v", err))
	}
	reg, err := openPruneMergedRegistry()
	if err != nil {
		progress.Pause()
		return mergedPruneExecutionError(cmd, fmt.Sprintf("open worktree registry: %v", err))
	}
	store := openPruneMergedProvenanceStore()
	records := make(map[string]pullrequest.Provenance)
	if err := store.View(ctx, func(current map[string]pullrequest.Provenance) error {
		for key, value := range current {
			records[key] = value
		}
		return nil
	}); err != nil {
		progress.Pause()
		return mergedPruneExecutionError(cmd, fmt.Sprintf("read pull-request provenance: %v", err))
	}
	candidates, err := inspectPruneMergedCandidates(ctx, cfg, records, reg.List())
	if err != nil {
		progress.Pause()
		return mergedPruneExecutionError(cmd, err.Error())
	}
	sort.Slice(candidates, func(left, right int) bool {
		return utils.PathKey(candidates[left].Policy.Path) < utils.PathKey(candidates[right].Policy.Path)
	})

	policyCandidates := make([]prunepolicy.MergedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.InitialOutcome == nil {
			policyCandidates = append(policyCandidates, candidate.Policy)
		}
	}
	lazyProvider := &lazyPruneMergedProvider{ctx: ctx, open: newPruneMergedProvider}
	progress.Phase("verify pull requests", len(policyCandidates))
	evaluated := make([]prunepolicy.Outcome, 0, len(policyCandidates))
	for index, candidate := range policyCandidates {
		evaluated = append(
			evaluated,
			prunepolicy.EvaluateMerged(ctx, lazyProvider, []prunepolicy.MergedCandidate{candidate})[0],
		)
		progress.Set(index + 1)
	}
	evaluationIndex := 0
	outcomes := make([]prunepolicy.Outcome, len(candidates))
	for index, candidate := range candidates {
		if candidate.InitialOutcome != nil {
			outcomes[index] = *candidate.InitialOutcome
			continue
		}
		outcomes[index] = evaluated[evaluationIndex]
		evaluationIndex++
	}

	removedWorktrees := 0
	eligibleCandidates := 0
	for _, outcome := range outcomes {
		if outcome.Reason == prunepolicy.EligibleMerged {
			eligibleCandidates++
		}
	}
	progress.Phase("remove candidates", eligibleCandidates)
	processedEligible := 0
	for index := range candidates {
		candidate := candidates[index]
		outcome := outcomes[index]
		if outcome.Reason != prunepolicy.EligibleMerged {
			outcomes[index] = withMergedRemediation(outcome)
			continue
		}
		if processedEligible > 0 {
			progress.Set(processedEligible)
		}
		processedEligible++
		dirty, inspectErr := inspectPruneMergedDirty(candidate)
		if inspectErr != nil {
			providerEvidence := outcome.Evidence
			outcome = pruneMergedDirtyInspectionOutcome(candidate, inspectErr)
			mergePruneEvidence(&outcome, providerEvidence)
			outcomes[index] = outcome
			continue
		}
		force := false
		if dirty {
			switch {
			case pruneDryRun:
				if err := runPruneMergedProtectedOperation(ctx, cfg, candidate, func() error {
					return withPruneMergedOwnershipGuard(ctx, reg, store, candidate, func() error {
						return validatePruneMergedDirtyWorktree(candidate)
					})
				}); err != nil {
					providerEvidence := outcome.Evidence
					outcome = pruneMergedOutcomeForError(candidate, err)
					mergePruneEvidence(&outcome, providerEvidence)
					outcomes[index] = outcome
					continue
				}
				outcome.Reason = prunepolicy.WouldRequireConfirmation
				outcome.Message = "merged worktree has local files or changes and would require confirmation"
				outcomes[index] = outcome
				continue
			case pruneJSON || !stdinIsTerminal():
				outcome.Reason = prunepolicy.ConfirmationRequired
				outcome.Message = "merged worktree has local files or changes and requires interactive confirmation"
				outcome.Remediation = "Run kwt prune --merged interactively to review this removal."
				outcomes[index] = outcome
				continue
			default:
				progress.Pause()
				dirty, inspectErr = inspectPruneMergedDirty(candidate)
				approved := false
				if inspectErr == nil && dirty {
					approved, inspectErr = confirmPruneMergedDirty(cmd, candidate)
				}
				progress.Resume()
				if inspectErr != nil {
					providerEvidence := outcome.Evidence
					outcome = pruneMergedDirtyInspectionOutcome(candidate, inspectErr)
					mergePruneEvidence(&outcome, providerEvidence)
					outcomes[index] = outcome
					continue
				}
				if dirty && !approved {
					outcome.Reason = prunepolicy.ConfirmationDeclined
					outcome.Message = "merged worktree removal was declined"
					outcomes[index] = outcome
					continue
				}
				force = dirty
			}
		}
		if pruneDryRun {
			if err := runPruneMergedProtectedOperation(
				ctx, cfg, candidate,
				func() error {
					return withPruneMergedOwnershipGuard(
						ctx, reg, store, candidate,
						func() error {
							return validatePruneMergedWorktree(candidate)
						},
					)
				},
			); err != nil {
				providerEvidence := outcome.Evidence
				outcome = pruneMergedOutcomeForError(candidate, err)
				mergePruneEvidence(&outcome, providerEvidence)
				if outcome.Reason == prunepolicy.DirtyWorktree {
					outcome.Remediation = "Commit or discard the changes, then retry; merged pruning has no force mode."
				}
				outcomes[index] = outcome
				continue
			}
			outcome.Reason = prunepolicy.WouldRemove
			outcome.Message = "worktree satisfies merged pull-request removal preconditions"
			outcomes[index] = outcome
			continue
		}
		var mutation pruneMergedMutationResult
		removeErr := runPruneMergedProtectedOperation(
			ctx, cfg, candidate,
			func() error {
				var err error
				mutation, err = removePruneMergedCandidate(
					ctx, reg, store, candidate, force,
				)
				return err
			},
		)
		if removeErr != nil && !mutation.worktreeRemoved {
			providerEvidence := outcome.Evidence
			outcome = pruneMergedOutcomeForError(candidate, removeErr)
			mergePruneEvidence(&outcome, providerEvidence)
			if outcome.Reason == prunepolicy.DirtyWorktree {
				outcome.Remediation = "Commit or discard the changes, then retry; merged pruning has no force mode."
			}
			outcomes[index] = outcome
			continue
		}
		if removeErr != nil {
			mutation.cleanupProblems = append(
				mutation.cleanupProblems, removeErr.Error(),
			)
		}
		removedWorktrees++
		if len(mutation.cleanupProblems) > 0 {
			outcome.Reason = prunepolicy.CleanupIncomplete
			outcome.Message = "worktree was removed but matching state cleanup did not complete"
			if mutation.cleanupMessage != "" {
				outcome.Message = mutation.cleanupMessage
			} else if removeErr != nil {
				outcome.Message = removeErr.Error()
			}
			outcome.Remediation = "Run kwt doctor --fix and review pull-request provenance before retrying cleanup."
			outcome.Evidence["worktree_removed"] = "true"
			outcome.Evidence["cleanup_error"] = strings.Join(
				mutation.cleanupProblems, "; ",
			)
		} else {
			outcome.Reason = prunepolicy.Removed
			outcome.Message = "worktree for merged pull request removed"
		}
		outcomes[index] = outcome
	}
	progress.Set(processedEligible)

	report := prunepolicy.Report{
		SchemaVersion: prunepolicy.SchemaVersion,
		Command:       "prune",
		Policy:        "merged",
		DryRun:        pruneDryRun,
		Outcomes:      outcomes,
	}
	report.Finalize()
	progress.Pause()
	if err := renderPruneReport(cmd, report, pruneJSON); err != nil {
		return writeMaintenanceError(cmd, "prune", "output_failed", err.Error(), 2, false)
	}
	if removedWorktrees > 0 {
		publishFleetBestEffortForCommand(cmd, cfg)
	}
	if exitCode := report.ExitCode(); exitCode != 0 {
		cmd.Root().SilenceUsage = true
		cmd.Root().SilenceErrors = true
		if !pruneJSON {
			_, _ = fmt.Fprintf(
				cmd.ErrOrStderr(),
				"kwt prune: candidates_skipped: %d candidate(s) require attention\n",
				report.Summary.Skipped,
			)
		}
		return &maintenanceCommandError{
			code: exitCode,
			err:  fmt.Errorf("%d prune candidate(s) require attention", report.Summary.Skipped),
		}
	}
	return nil
}

func mergedPruneExecutionError(cmd *cobra.Command, message string) error {
	return writeMaintenanceError(cmd, "prune", "inspection_failed", message, 2, pruneJSON)
}

func defaultRemovePruneMergedWorktree(
	candidate pruneMergedCandidate,
	force bool,
	claim func(func() error) (bool, error),
) (bool, error) {
	conditions := pruneMergedRemovalConditions(candidate)
	conditions.RequireClean = !force
	return git.New(candidate.RepositoryRoot).RemoveWorktreeCheckedAfterClaim(
		candidate.Policy.Path,
		force,
		conditions,
		claim,
	)
}

func defaultInspectPruneMergedDirty(candidate pruneMergedCandidate) (bool, error) {
	output, err := git.New(candidate.Policy.Path).RunCommandWithoutCredentials(
		candidate.ProtectedNames,
		"status", "--ignored", "--porcelain", "--untracked-files=normal",
	)
	return strings.TrimSpace(output) != "", err
}

func defaultConfirmPruneMergedDirty(
	cmd *cobra.Command,
	candidate pruneMergedCandidate,
) (bool, error) {
	if _, err := fmt.Fprintf(
		cmd.ErrOrStderr(),
		"Pull request merged. Remove %s and all local changes and files in it, including ignored files and files added before removal? [y/N] ",
		strconv.QuoteToGraphic(candidate.Policy.Path),
	); err != nil {
		return false, err
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func pruneMergedDirtyInspectionOutcome(
	candidate pruneMergedCandidate,
	err error,
) prunepolicy.Outcome {
	outcome := pruneMergedOutcomeForError(candidate, err)
	if outcome.Reason == prunepolicy.RemovalFailed {
		outcome.Reason = prunepolicy.PathUnavailable
		outcome.Message = fmt.Sprintf("inspect worktree status: %v", err)
		outcome.Remediation = "Restore filesystem access and retry."
	}
	return outcome
}

func defaultValidatePruneMergedWorktree(candidate pruneMergedCandidate) error {
	return git.New(candidate.RepositoryRoot).ValidateWorktreeRemoval(
		candidate.Policy.Path,
		pruneMergedRemovalConditions(candidate),
	)
}

func defaultValidatePruneMergedDirtyWorktree(candidate pruneMergedCandidate) error {
	conditions := pruneMergedRemovalConditions(candidate)
	conditions.RequireClean = false
	return git.New(candidate.RepositoryRoot).ValidateWorktreeRemoval(candidate.Policy.Path, conditions)
}

func withPruneMergedOwnershipGuard(
	ctx context.Context,
	reg pruneMergedRegistry,
	store pruneMergedProvenanceStore,
	candidate pruneMergedCandidate,
	operation func() error,
) (resultErr error) {
	release, acquired, err := reg.AcquireCreation(candidate.Policy.Path)
	if err != nil {
		return fmt.Errorf("lock worktree ownership: %w", err)
	}
	if !acquired {
		return fmt.Errorf("worktree creation is in progress for %s", candidate.Policy.Path)
	}
	defer func() {
		if err := release(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release worktree ownership lock: %w", err))
		}
	}()

	matches, err := reg.EntryMatches(candidate.Policy.Path, candidate.RegistryEntry)
	if err != nil {
		return fmt.Errorf("revalidate worktree registry ownership: %w", err)
	}
	if !matches {
		return fmt.Errorf("worktree ownership changed after inspection for %s", candidate.Policy.Path)
	}
	provenanceMatches, err := pruneMergedProvenanceMatches(ctx, store, candidate)
	if err != nil {
		return fmt.Errorf("revalidate pull-request provenance ownership: %w", err)
	}
	if !provenanceMatches {
		return fmt.Errorf("worktree ownership changed after inspection for %s", candidate.Policy.Path)
	}
	return operation()
}

func pruneMergedProvenanceMatches(
	ctx context.Context,
	store pruneMergedProvenanceStore,
	candidate pruneMergedCandidate,
) (bool, error) {
	pathMatches := 0
	expectedMatches := false
	err := store.View(ctx, func(records map[string]pullrequest.Provenance) error {
		for key, record := range records {
			if !samePRPath(record.Workspace.Path, candidate.Policy.Path) ||
				!provenanceGenerationMatches(
					record.Workspace.Generation,
					candidate.Policy.Generation,
				) {
				continue
			}
			pathMatches++
			if candidate.Policy.Provenance != nil &&
				key == candidate.ProvenanceKey &&
				reflect.DeepEqual(record, *candidate.Policy.Provenance) {
				expectedMatches = true
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if candidate.Policy.Provenance == nil {
		return pathMatches == 0, nil
	}
	return pathMatches == 1 && expectedMatches, nil
}

func pruneMergedRemovalConditions(candidate pruneMergedCandidate) git.WorktreeRemovalConditions {
	return git.WorktreeRemovalConditions{
		ExpectedGitDir:     candidate.ExpectedGitDir,
		Generation:         candidate.Policy.Generation,
		Head:               candidate.Policy.Head,
		RepositoryIdentity: candidate.Policy.LiveRepository,
		Branch:             candidate.Policy.Branch,
		UpstreamRepository: candidate.Policy.SourceRepository,
		UpstreamBranch:     candidate.Policy.SourceBranch,
		RequireClean:       true,
		IncludeIgnored:     true,
		ProtectedNames:     candidate.ProtectedNames,
	}
}

type lazyPruneMergedProvider struct {
	ctx      context.Context
	open     func(context.Context) (prunepolicy.MergedProvider, error)
	provider prunepolicy.MergedProvider
	err      error
	opened   bool
}

func (p *lazyPruneMergedProvider) ensure() (prunepolicy.MergedProvider, error) {
	if !p.opened {
		p.opened = true
		p.provider, p.err = p.open(p.ctx)
	}
	return p.provider, p.err
}

func (p *lazyPruneMergedProvider) Get(
	ctx context.Context, repository pullrequest.Repository, number int,
) (pullrequest.PullRequest, error) {
	provider, err := p.ensure()
	if err != nil {
		return pullrequest.PullRequest{}, err
	}
	return provider.Get(ctx, repository, number)
}

func (p *lazyPruneMergedProvider) ResolveRepository(
	ctx context.Context,
	repository pullrequest.Repository,
) (pullrequest.Repository, error) {
	provider, err := p.ensure()
	if err != nil {
		return pullrequest.Repository{}, err
	}
	return provider.ResolveRepository(ctx, repository)
}

func (p *lazyPruneMergedProvider) ListForCommit(
	ctx context.Context, repository pullrequest.Repository, sha string,
) ([]pullrequest.PullRequest, error) {
	provider, err := p.ensure()
	if err != nil {
		return nil, err
	}
	return provider.ListForCommit(ctx, repository, sha)
}

func (p *lazyPruneMergedProvider) ListByHead(
	ctx context.Context,
	repository pullrequest.Repository,
	owner string,
	branch string,
) ([]pullrequest.PullRequest, error) {
	provider, err := p.ensure()
	if err != nil {
		return nil, err
	}
	return provider.ListByHead(ctx, repository, owner, branch)
}

func defaultInspectPruneMergedCandidates(
	ctx context.Context,
	cfg *models.Config,
	records map[string]pullrequest.Provenance,
	registryEntries []*registry.WorktreeEntry,
) ([]pruneMergedCandidate, error) {
	if cfg == nil {
		return nil, errors.New("global configuration is unavailable")
	}
	protectedNames := credentials.ProtectedNames(cfg)
	type inventoryRoot struct {
		path         string
		repository   string
		repositories []string
		configured   bool
	}
	roots := make([]inventoryRoot, 0, len(cfg.Projects)+len(registryEntries))
	rootIndexes := make(map[string]int, cap(roots))
	configuredInventoryIncomplete := false
	appendRoot := func(root inventoryRoot) {
		if strings.TrimSpace(root.path) == "" {
			return
		}
		key := utils.PathKey(root.path)
		if index, exists := rootIndexes[key]; exists {
			if root.configured {
				roots[index].configured = true
				roots[index].repositories = append(
					roots[index].repositories,
					root.repository,
				)
			}
			return
		}
		if root.configured {
			root.repositories = []string{root.repository}
		}
		rootIndexes[key] = len(roots)
		roots = append(roots, root)
	}
	configuredRepositoriesByPath := make(map[string][]string)
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.Path) == "" {
			configuredInventoryIncomplete = true
			continue
		}
		pathKey := utils.PathKey(project.Path)
		configuredRepositoriesByPath[pathKey] = append(
			configuredRepositoriesByPath[pathKey],
			project.Repository,
		)
		appendRoot(inventoryRoot{
			path: project.Path, repository: project.Repository, configured: true,
		})
	}
	if strings.TrimSpace(cfg.Worktree.BaseDir) != "" {
		paths, err := findPruneMergedGlobalPaths(cfg.Worktree.BaseDir)
		if err != nil {
			return nil, fmt.Errorf("discover global worktrees: %w", err)
		}
		for _, path := range paths {
			appendRoot(inventoryRoot{path: path})
		}
	}
	for _, entry := range registryEntries {
		if entry == nil || entry.CreationToken != "" {
			continue
		}
		appendRoot(inventoryRoot{path: entry.Path})
	}
	seen := make(map[string]bool)
	var candidates []pruneMergedCandidate
	configuredClaims := make(map[string]map[string]struct{})
	backlinkClaims := make(map[string]map[string]string)
	invalidInventoryRoots := make(map[string][]string)
	configuredClaimKey := func(raw string) string {
		identity, ok := repositoryurl.CanonicalRepositoryIdentity(raw)
		if !ok {
			return "invalid"
		}
		return "identity:" + repositoryurl.FoldRepositoryIdentity(identity)
	}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if target, backlinkErr := git.ReadWorktreeBacklink(root.path); backlinkErr == nil && target != "" {
			targetKey := utils.PathKey(target)
			claimants := backlinkClaims[targetKey]
			if claimants == nil {
				claimants = make(map[string]string)
				backlinkClaims[targetKey] = claimants
			}
			claimants[utils.PathKey(root.path)] = utils.CanonicalPath(root.path)
		}
		g := git.New(root.path)
		inspections, err := g.InspectWorktreesWithoutCredentials(protectedNames)
		if err != nil {
			if root.configured {
				configuredInventoryIncomplete = true
			}
			pathKey := utils.PathKey(root.path)
			if !seen[pathKey] {
				seen[pathKey] = true
				policy := prunepolicy.MergedCandidate{Path: root.path}
				candidates = append(candidates, pruneMergedCandidate{
					Policy:         policy,
					RepositoryRoot: root.path,
					ProtectedNames: append([]string(nil), protectedNames...),
					InitialOutcome: initialMergedOutcome(
						policy,
						prunepolicy.DoctorRequired,
						fmt.Sprintf("worktree path could not be inspected: %v", err),
					),
				})
			}
			continue
		}
		mainRepositoryPath, err := g.GetMainRepositoryPathWithoutCredentials(
			protectedNames,
		)
		if err != nil {
			return nil, fmt.Errorf("inspect main repository path for %s: %w", root.path, err)
		}
		mainRepositoryPath = utils.CanonicalPath(mainRepositoryPath)
		if !git.HasExactWorktreeRoot(inspections, root.path) {
			if root.configured {
				configuredInventoryIncomplete = true
			}
			mainKey := utils.PathKey(mainRepositoryPath)
			invalidInventoryRoots[mainKey] = append(
				invalidInventoryRoots[mainKey],
				utils.CanonicalPath(root.path),
			)
			pathKey := utils.PathKey(root.path)
			if !seen[pathKey] {
				seen[pathKey] = true
				policy := prunepolicy.MergedCandidate{
					Path: root.path, MainRepositoryPath: mainRepositoryPath,
				}
				candidates = append(candidates, pruneMergedCandidate{
					Policy:         policy,
					RepositoryRoot: mainRepositoryPath,
					ProtectedNames: append([]string(nil), protectedNames...),
					InitialOutcome: initialMergedOutcome(
						policy,
						prunepolicy.DoctorRequired,
						"inventory path is not an exact worktree root",
					),
				})
			}
			continue
		}
		if root.configured {
			mainKey := utils.PathKey(mainRepositoryPath)
			claims := configuredClaims[mainKey]
			if claims == nil {
				claims = make(map[string]struct{})
				configuredClaims[mainKey] = claims
			}
			for _, repository := range root.repositories {
				claims[configuredClaimKey(repository)] = struct{}{}
			}
			for _, inspection := range inspections {
				for _, repository := range configuredRepositoriesByPath[utils.PathKey(inspection.Path)] {
					claims[configuredClaimKey(repository)] = struct{}{}
				}
			}
		}
		configuredRepository, hasConfiguredRepository := repositoryurl.CanonicalRepositoryIdentity(root.repository)
		for _, inspection := range inspections {
			if inspection.IsMain {
				continue
			}
			pathKey := utils.PathKey(inspection.Path)
			if seen[pathKey] {
				continue
			}
			seen[pathKey] = true
			backlinkMatches := !inspection.Exists ||
				(inspection.GitDirError == "" &&
					inspection.GitDir != "" &&
					inspection.DotGitTarget != "" &&
					utils.PathKey(inspection.DotGitTarget) ==
						utils.PathKey(inspection.GitDir))
			liveRepository := ""
			if inspection.Exists && backlinkMatches {
				liveRepository = repositoryIdentityFromGit(
					git.New(inspection.Path),
					protectedNames,
				)
			}
			projectRepository := configuredRepository
			if !root.configured {
				projectRepository = liveRepository
			}
			candidate := pruneMergedCandidate{
				RepositoryRoot: mainRepositoryPath,
				ExpectedGitDir: inspection.GitDir,
				ProtectedNames: append([]string(nil), protectedNames...),
				Policy: prunepolicy.MergedCandidate{
					Path: inspection.Path, Branch: inspection.Branch, Head: inspection.Head,
					Generation: inspection.Generation, IsMain: inspection.IsMain,
					MainRepositoryPath: mainRepositoryPath,
					ProjectRepository:  projectRepository, LiveRepository: liveRepository,
				},
			}
			if !backlinkMatches {
				candidate.InitialOutcome = initialMergedOutcome(
					candidate.Policy,
					prunepolicy.DoctorRequired,
					"worktree backlink does not match its verified Git administrative directory",
				)
			}
			if root.configured && !hasConfiguredRepository {
				candidate.InitialOutcome = initialMergedOutcome(
					candidate.Policy,
					prunepolicy.DoctorRequired,
					"configured project repository identity is unavailable",
				)
			}
			if !inspection.Exists {
				candidate.InitialOutcome = initialMergedOutcome(candidate.Policy, prunepolicy.DoctorRequired, "Git records a worktree path that is absent")
			}
			if inspection.Locked && candidate.InitialOutcome == nil {
				candidate.InitialOutcome = initialMergedOutcome(candidate.Policy, prunepolicy.LockedWorktree, "locked worktree requires manual review")
			}
			if !root.configured && configuredInventoryIncomplete &&
				candidate.InitialOutcome == nil {
				candidate.InitialOutcome = initialMergedOutcome(
					candidate.Policy,
					prunepolicy.DoctorRequired,
					"configured project inventory is incomplete; run kwt doctor before pruning unconfigured worktrees",
				)
			}
			attachMergedRegistrySnapshot(&candidate, registryEntries)
			attachMergedProvenance(&candidate, records)
			if inspection.Exists && !inspection.IsMain && backlinkMatches {
				upstream, upstreamErr := git.New(inspection.Path).
					BranchUpstreamWithoutCredentials(inspection.Branch, protectedNames)
				if upstreamErr == nil {
					candidate.Policy.SourceBranch = upstream.Branch
					candidate.Policy.SourceRepository, _ = repositoryurl.CanonicalRepositoryIdentityFromRemote(upstream.RepositoryURL)
				}
			}
			candidates = append(candidates, candidate)
		}
	}
	for index := range candidates {
		candidate := &candidates[index]
		invalidRoots := invalidInventoryRoots[utils.PathKey(candidate.Policy.MainRepositoryPath)]
		if len(invalidRoots) != 0 {
			sort.Strings(invalidRoots)
			candidate.InitialOutcome = initialMergedOutcome(
				candidate.Policy,
				prunepolicy.DoctorRequired,
				"repository inventory includes a path that is not an exact worktree root",
			)
			candidate.InitialOutcome.Evidence["inventory_paths"] =
				strings.Join(invalidRoots, "\n")
		}
		claims := configuredClaims[utils.PathKey(candidate.Policy.MainRepositoryPath)]
		if len(claims) >= 2 {
			candidate.InitialOutcome = initialMergedOutcome(
				candidate.Policy,
				prunepolicy.DoctorRequired,
				"conflicting configured project repository identities require doctor review",
			)
		}
		claimants := backlinkClaims[utils.PathKey(candidate.ExpectedGitDir)]
		conflicts := make([]string, 0, len(claimants))
		candidatePathKey := utils.PathKey(candidate.Policy.Path)
		for claimantKey, claimant := range claimants {
			if claimantKey != candidatePathKey {
				conflicts = append(conflicts, claimant)
			}
		}
		if len(conflicts) == 0 {
			continue
		}
		sort.Strings(conflicts)
		candidate.InitialOutcome = initialMergedOutcome(
			candidate.Policy,
			prunepolicy.DoctorRequired,
			"worktree administrative directory is claimed by another discovered path",
		)
		candidate.InitialOutcome.Evidence["claimants"] = strings.Join(conflicts, "\n")
	}
	return candidates, nil
}

func repositoryIdentityFromGit(g *git.Git, protectedNames []string) string {
	remote, err := g.RunCommandWithoutCredentials(
		protectedNames,
		"remote", "get-url", "origin",
	)
	if err != nil {
		return ""
	}
	identity, _ := repositoryurl.CanonicalRepositoryIdentityFromRemote(strings.TrimSpace(remote))
	return identity
}

func attachMergedRegistrySnapshot(
	candidate *pruneMergedCandidate, entries []*registry.WorktreeEntry,
) {
	matches := make([]*registry.WorktreeEntry, 0, 1)
	for _, entry := range entries {
		if entry == nil || utils.PathKey(entry.Path) != utils.PathKey(candidate.Policy.Path) {
			continue
		}
		matches = append(matches, entry)
	}
	if len(matches) == 0 {
		return
	}
	candidate.RegistryExpected = true
	if len(matches) > 1 {
		if candidate.InitialOutcome == nil {
			candidate.InitialOutcome = initialMergedOutcome(candidate.Policy, prunepolicy.DoctorRequired, "multiple registry entries identify the same worktree path")
		}
		return
	}
	entry := matches[0]
	copy := *entry
	candidate.RegistryEntry = &copy
	if entry.Generation != candidate.Policy.Generation && candidate.InitialOutcome == nil {
		candidate.InitialOutcome = initialMergedOutcome(candidate.Policy, prunepolicy.DoctorRequired, "registry generation does not match the Git worktree")
	}
	if entry.Repository != "" && candidate.InitialOutcome == nil {
		registryRepository, ok :=
			repositoryurl.CanonicalRepositoryIdentity(entry.Repository)
		if !ok || !mergedRepositoryIdentityMatchesAny(
			registryRepository,
			candidate.Policy.ProjectRepository,
			candidate.Policy.LiveRepository,
		) {
			candidate.InitialOutcome = initialMergedOutcome(candidate.Policy, prunepolicy.RepositoryChanged, "registry repository identity does not match the configured project or live worktree origin")
		}
	}
}

func mergedRepositoryIdentityMatchesAny(identity string, candidates ...string) bool {
	folded := repositoryurl.FoldRepositoryIdentity(identity)
	for _, candidate := range candidates {
		if candidate != "" && folded == repositoryurl.FoldRepositoryIdentity(candidate) {
			return true
		}
	}
	return false
}

func attachMergedProvenance(
	candidate *pruneMergedCandidate, records map[string]pullrequest.Provenance,
) {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pathMatches := 0
	for _, key := range keys {
		record := records[key]
		if !samePRPath(record.Workspace.Path, candidate.Policy.Path) {
			continue
		}
		if !provenanceGenerationMatches(
			record.Workspace.Generation,
			candidate.Policy.Generation,
		) {
			continue
		}
		pathMatches++
		if record.Workspace.Branch != candidate.Policy.Branch || candidate.Policy.Provenance != nil {
			continue
		}
		copy := record
		candidate.Policy.Provenance = &copy
		candidate.ProvenanceKey = key
	}
	if pathMatches != 1 || (pathMatches == 1 && candidate.Policy.Provenance == nil) {
		if pathMatches > 0 && candidate.InitialOutcome == nil {
			candidate.InitialOutcome = initialMergedOutcome(candidate.Policy, prunepolicy.DoctorRequired, "pull-request provenance path or branch is ambiguous")
		}
	}
}

func initialMergedOutcome(
	candidate prunepolicy.MergedCandidate, reason prunepolicy.Reason, message string,
) *prunepolicy.Outcome {
	outcome := prunepolicy.Outcome{
		Path: candidate.Path, Branch: candidate.Branch, Reason: reason, Message: message,
		Evidence: map[string]string{"head_sha": candidate.Head},
	}
	return &outcome
}

func withMergedRemediation(outcome prunepolicy.Outcome) prunepolicy.Outcome {
	if outcome.Remediation != "" {
		return outcome
	}
	switch outcome.Reason {
	case prunepolicy.MissingGeneration:
		outcome.Remediation = "Run kwt list from the verified repository to adopt the worktree, then retry."
	case prunepolicy.DirtyWorktree:
		outcome.Remediation = "Commit or discard the changes, then retry; merged pruning has no force mode."
	case prunepolicy.DoctorRequired, prunepolicy.RepositoryChanged:
		outcome.Remediation = "Run kwt doctor and repair the reported structural state before retrying."
	case prunepolicy.LockedWorktree:
		outcome.Remediation = "Unlock the worktree with git worktree unlock, then retry."
	case prunepolicy.HeadAdvancedAfterPR:
		outcome.Remediation = "Review the commits added after the pull request head and remove the worktree manually if appropriate."
	case prunepolicy.SourceRepositoryUnavailable:
		outcome.Remediation = "Configure an exact upstream remote and branch, then retry."
	case prunepolicy.SourceRepositoryMismatch, prunepolicy.SourceBranchMismatch, prunepolicy.AmbiguousPRMatch:
		outcome.Remediation = "Review the worktree upstream and associated pull requests; no worktree was removed."
	case prunepolicy.AuthenticationFailed:
		outcome.Remediation = "Set KWT_GITHUB_TOKEN or run gh auth login, then retry."
	case prunepolicy.NetworkFailure:
		outcome.Remediation = "Restore provider connectivity and retry."
	}
	return outcome
}
