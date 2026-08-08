package cmd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/prunepolicy"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
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
	validatePruneMergedWorktree = defaultValidatePruneMergedWorktree
	removePruneMergedWorktree   = defaultRemovePruneMergedWorktree
)

func runPruneMerged(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	cfg, err := loadPruneMergedConfig()
	if err != nil {
		return mergedPruneExecutionError(cmd, fmt.Sprintf("load global configuration: %v", err))
	}
	reg, err := openPruneMergedRegistry()
	if err != nil {
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
		return mergedPruneExecutionError(cmd, fmt.Sprintf("read pull-request provenance: %v", err))
	}
	candidates, err := inspectPruneMergedCandidates(ctx, cfg, records, reg.List())
	if err != nil {
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
	evaluated := prunepolicy.EvaluateMerged(ctx, lazyProvider, policyCandidates)
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
	for index := range candidates {
		candidate := candidates[index]
		outcome := outcomes[index]
		if outcome.Reason != prunepolicy.EligibleMerged {
			outcomes[index] = withMergedRemediation(outcome)
			continue
		}
		if pruneDryRun {
			if err := withPruneMergedOwnershipGuard(ctx, reg, store, candidate, func() error {
				return validatePruneMergedWorktree(candidate)
			}); err != nil {
				providerEvidence := outcome.Evidence
				outcome = pruneOutcomeForError(candidate.Policy.Path, candidate.Policy.Branch, err)
				outcome.Evidence = providerEvidence
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
		worktreeRemoved := false
		var residualWarning error
		removeErr := withPruneMergedOwnershipGuard(ctx, reg, store, candidate, func() error {
			removed, err := removePruneMergedWorktree(
				candidate,
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
			worktreeRemoved = removed
			if err == nil && !removed {
				return fmt.Errorf(
					"worktree ownership changed after inspection for %s",
					candidate.Policy.Path,
				)
			}
			return err
		})
		if removeErr == nil && residualWarning != nil {
			removeErr = residualWarning
		}
		if removeErr != nil && !worktreeRemoved {
			providerEvidence := outcome.Evidence
			outcome = pruneOutcomeForError(candidate.Policy.Path, candidate.Policy.Branch, removeErr)
			outcome.Evidence = providerEvidence
			if outcome.Reason == prunepolicy.DirtyWorktree {
				outcome.Remediation = "Commit or discard the changes, then retry; merged pruning has no force mode."
			}
			outcomes[index] = outcome
			continue
		}
		removedWorktrees++
		cleanupProblems := make([]string, 0, 2)
		if removeErr != nil {
			cleanupProblems = append(cleanupProblems, removeErr.Error())
		}
		if candidate.Policy.Provenance != nil {
			removed, cleanupErr := store.RemoveIfMatch(
				ctx, candidate.ProvenanceKey, *candidate.Policy.Provenance,
			)
			if cleanupErr != nil {
				cleanupProblems = append(cleanupProblems, "provenance: "+cleanupErr.Error())
			} else if !removed {
				cleanupProblems = append(cleanupProblems, "provenance record changed")
			}
		}
		if len(cleanupProblems) > 0 {
			outcome.Reason = prunepolicy.CleanupIncomplete
			outcome.Message = "worktree was removed but matching state cleanup did not complete"
			if removeErr != nil {
				outcome.Message = removeErr.Error()
			}
			outcome.Remediation = "Run kwt doctor --fix and review pull-request provenance before retrying cleanup."
			outcome.Evidence["worktree_removed"] = "true"
			outcome.Evidence["cleanup_error"] = strings.Join(cleanupProblems, "; ")
		} else {
			outcome.Reason = prunepolicy.Removed
			outcome.Message = "worktree for merged pull request removed"
		}
		outcomes[index] = outcome
	}

	report := prunepolicy.Report{
		SchemaVersion: prunepolicy.SchemaVersion,
		Command:       "prune",
		Policy:        "merged",
		DryRun:        pruneDryRun,
		Outcomes:      outcomes,
	}
	report.Finalize()
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
	claim func(func() error) (bool, error),
) (bool, error) {
	return git.New(candidate.RepositoryRoot).RemoveWorktreeCheckedAfterClaim(
		candidate.Policy.Path,
		false,
		pruneMergedRemovalConditions(candidate),
		claim,
	)
}

func defaultValidatePruneMergedWorktree(candidate pruneMergedCandidate) error {
	return git.New(candidate.RepositoryRoot).ValidateWorktreeRemoval(
		candidate.Policy.Path,
		pruneMergedRemovalConditions(candidate),
	)
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
				status, statusErr := g.RunCommandWithoutCredentials(
					protectedNames,
					"-C", inspection.Path, "status", "--ignored", "--porcelain", "--untracked-files=normal",
				)
				if statusErr != nil {
					candidate.InitialOutcome = initialMergedOutcome(candidate.Policy, prunepolicy.PathUnavailable, fmt.Sprintf("inspect worktree status: %v", statusErr))
				} else {
					candidate.Policy.Dirty = strings.TrimSpace(status) != ""
				}
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
