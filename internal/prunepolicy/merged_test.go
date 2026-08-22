package prunepolicy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/pullrequest"
)

const (
	mergedHead     = "0123456789abcdef0123456789abcdef01234567"
	advancedHead   = "89abcdef0123456789abcdef0123456789abcdef"
	baseRepository = "github.com/acme/widget"
	forkRepository = "github.com/octocat/widget"
)

type fakeMergedProvider struct {
	resolve      func(pullrequest.Repository) (pullrequest.Repository, error)
	get          func(pullrequest.Repository, int) (pullrequest.PullRequest, error)
	byCommit     func(pullrequest.Repository, string) ([]pullrequest.PullRequest, error)
	byHead       func(pullrequest.Repository, string, string) ([]pullrequest.PullRequest, error)
	getCalls     int
	commitCalls  int
	headCalls    int
	resolveCalls int
}

func (p *fakeMergedProvider) ResolveRepository(_ context.Context, repository pullrequest.Repository) (pullrequest.Repository, error) {
	p.resolveCalls++
	if p.resolve == nil {
		return repository, nil
	}
	return p.resolve(repository)
}

func (p *fakeMergedProvider) Get(_ context.Context, repository pullrequest.Repository, number int) (pullrequest.PullRequest, error) {
	p.getCalls++
	if p.get == nil {
		return pullrequest.PullRequest{}, errors.New("unexpected Get call")
	}
	return p.get(repository, number)
}

func (p *fakeMergedProvider) ListForCommit(_ context.Context, repository pullrequest.Repository, sha string) ([]pullrequest.PullRequest, error) {
	p.commitCalls++
	if p.byCommit == nil {
		return nil, errors.New("unexpected ListForCommit call")
	}
	return p.byCommit(repository, sha)
}

func (p *fakeMergedProvider) ListByHead(_ context.Context, repository pullrequest.Repository, owner, branch string) ([]pullrequest.PullRequest, error) {
	p.headCalls++
	if p.byHead == nil {
		return nil, errors.New("unexpected ListByHead call")
	}
	return p.byHead(repository, owner, branch)
}

func TestEvaluateMerged(t *testing.T) {
	tests := []struct {
		name      string
		candidate MergedCandidate
		provider  *fakeMergedProvider
		want      Reason
	}{
		{name: "imported exact merged", candidate: importedMergedCandidate(), provider: providerForImported(exactMergedPR(forkRepository)), want: EligibleMerged},
		{name: "imported transferred repository aliases", candidate: importedTransferredCandidate(), provider: providerForImported(exactMergedPR(forkRepository)), want: EligibleMerged},
		{name: "imported transferred same repository source", candidate: importedTransferredSourceCandidate(), provider: providerForImported(exactMergedPR(baseRepository)), want: EligibleMerged},
		{name: "imported stale URL after transfer", candidate: importedWithStaleURL(), provider: providerForImported(exactMergedPR(forkRepository)), want: EligibleMerged},
		{name: "imported opaque ID mismatch", candidate: importedWithOpaqueIDMismatch(), provider: &fakeMergedProvider{}, want: DoctorRequired},
		{name: "imported provenance head mismatch", candidate: importedWithHeadMismatch(), provider: &fakeMergedProvider{}, want: HeadAdvancedAfterPR},
		{name: "imported provenance workspace mismatch", candidate: importedWithWorkspaceMismatch(), provider: &fakeMergedProvider{}, want: DoctorRequired},
		{name: "imported provenance generation mismatch", candidate: importedWithGenerationMismatch(), provider: &fakeMergedProvider{}, want: DoctorRequired},
		{name: "imported provenance project path mismatch", candidate: importedWithProjectPathMismatch(), provider: &fakeMergedProvider{}, want: DoctorRequired},
		{name: "ordinary same repo", candidate: ordinaryCandidate(baseRepository), provider: providerForCommit(exactMergedPR(baseRepository)), want: EligibleMerged},
		{name: "ordinary fork", candidate: ordinaryCandidate(forkRepository), provider: providerForCommit(exactMergedPR(forkRepository)), want: EligibleMerged},
		{name: "ordinary fork origin with configured base", candidate: withForkOrigin(ordinaryCandidate(forkRepository)), provider: providerForCommit(exactMergedPR(forkRepository)), want: EligibleMerged},
		{name: "imported fork origin with configured base", candidate: withForkOrigin(importedMergedCandidate()), provider: providerForImported(exactMergedPR(forkRepository)), want: EligibleMerged},
		{name: "missing generation", candidate: withoutGeneration(), provider: &fakeMergedProvider{}, want: MissingGeneration},
		{name: "invalid generation", candidate: withInvalidGeneration(), provider: &fakeMergedProvider{}, want: MissingGeneration},
		{name: "dirty", candidate: dirtyMerged(), provider: providerForCommit(exactMergedPR(forkRepository)), want: EligibleMerged},
		{name: "main worktree", candidate: mainWorktree(), provider: &fakeMergedProvider{}, want: MainWorktree},
		{name: "head advanced", candidate: advancedCandidate(), provider: providerForAdvancedBranch(), want: HeadAdvancedAfterPR},
		{name: "no PR", candidate: ordinaryCandidate(forkRepository), provider: providerEmpty(), want: NoAssociatedPR},
		{name: "closed not merged", candidate: ordinaryCandidate(forkRepository), provider: providerForCommit(closedPR(forkRepository)), want: PRNotMerged},
		{name: "source unavailable", candidate: noUpstream(), provider: &fakeMergedProvider{}, want: SourceRepositoryUnavailable},
		{name: "source repo mismatch", candidate: ordinaryCandidate(forkRepository), provider: providerForCommit(exactMergedPR("github.com/hubot/widget")), want: SourceRepositoryMismatch},
		{name: "branch mismatch", candidate: ordinaryCandidate(forkRepository), provider: providerForCommit(prWithBranch(exactMergedPR(forkRepository), "other")), want: SourceBranchMismatch},
		{name: "ambiguous", candidate: ordinaryCandidate(forkRepository), provider: providerForCommit(exactMergedPR(forkRepository), prWithNumber(exactMergedPR(forkRepository), 18)), want: AmbiguousPRMatch},
		{name: "authentication", candidate: ordinaryCandidate(forkRepository), provider: providerWithError(pullrequest.NewError(pullrequest.CodeAuthentication, "auth failed", false, nil)), want: AuthenticationFailed},
		{name: "network", candidate: ordinaryCandidate(forkRepository), provider: providerWithError(pullrequest.NewError(pullrequest.CodeNetwork, "network failed", true, nil)), want: NetworkFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcomes := EvaluateMerged(context.Background(), tc.provider, []MergedCandidate{tc.candidate})

			require.Len(t, outcomes, 1)
			assert.Equal(t, tc.want, outcomes[0].Reason)
			assert.Equal(t, tc.candidate.Path, outcomes[0].Path)
			assert.Equal(t, tc.candidate.Branch, outcomes[0].Branch)
			if tc.want == EligibleMerged {
				assert.Equal(t, "17", outcomes[0].Evidence["pr_number"])
				assert.Equal(t, "https://github.com/acme/widget/pull/17", outcomes[0].Evidence["pr_url"])
				assert.Equal(t, mergedHead, outcomes[0].Evidence["pr_head_sha"])
			}
			if tc.want == AuthenticationFailed || tc.want == NetworkFailure {
				assert.NotEmpty(t, outcomes[0].Evidence["provider_error_code"])
			}
			if tc.want == NetworkFailure {
				assert.Equal(t, "true", outcomes[0].Evidence["retryable"])
			}
		})
	}
}

func TestEvaluateMergedIgnoresDirtyStateUntilAfterProviderProof(t *testing.T) {
	provider := providerEmpty()
	candidate := dirtyMerged()

	outcome := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})[0]

	assert.Equal(t, NoAssociatedPR, outcome.Reason)
	assert.NotZero(t, provider.resolveCalls)
	assert.NotZero(t, provider.commitCalls)
}

func TestEvaluateMergedReturnsEligibleForDirtyConfirmedMergedCandidate(t *testing.T) {
	provider := providerForCommit(exactMergedPR(forkRepository))
	candidate := dirtyMerged()

	outcome := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})[0]

	assert.Equal(t, EligibleMerged, outcome.Reason)
}

func TestEvaluateMergedKeepsAdvancedDirtyHeadAsHardStop(t *testing.T) {
	candidate := advancedCandidate()
	candidate.Dirty = true

	outcome := EvaluateMerged(context.Background(), providerForAdvancedBranch(), []MergedCandidate{candidate})[0]

	assert.Equal(t, HeadAdvancedAfterPR, outcome.Reason)
}

func TestEvaluateMergedMapsProviderFailuresToStableReasons(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantReason   Reason
		wantEvidence pullrequest.ErrorCode
	}{
		{
			name: "authentication",
			err: pullrequest.NewError(
				pullrequest.CodeAuthentication, "authentication failed", false, nil,
			),
			wantReason: AuthenticationFailed, wantEvidence: pullrequest.CodeAuthentication,
		},
		{
			name: "network",
			err: pullrequest.NewError(
				pullrequest.CodeNetwork, "network failed", true, nil,
			),
			wantReason: NetworkFailure, wantEvidence: pullrequest.CodeNetwork,
		},
		{
			name: "not found",
			err: pullrequest.NewError(
				pullrequest.CodeNotFound, "pull request not found", false, nil,
			),
			wantReason: ProviderFailure, wantEvidence: pullrequest.CodeNotFound,
		},
		{
			name: "malformed response",
			err: pullrequest.NewError(
				pullrequest.CodeMalformedResponse, "malformed response", false, nil,
			),
			wantReason: ProviderFailure, wantEvidence: pullrequest.CodeMalformedResponse,
		},
		{
			name: "untyped failure", err: errors.New("provider unavailable"),
			wantReason: NetworkFailure, wantEvidence: pullrequest.CodeNetwork,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcomes := EvaluateMerged(
				context.Background(),
				providerWithError(tt.err),
				[]MergedCandidate{ordinaryCandidate(forkRepository)},
			)

			require.Len(t, outcomes, 1)
			assert.Equal(t, tt.wantReason, outcomes[0].Reason)
			assert.Equal(
				t, string(tt.wantEvidence), outcomes[0].Evidence["provider_error_code"],
			)
			assert.NotEmpty(t, outcomes[0].Remediation)
		})
	}
}

func TestEvaluateMergedContinuesAfterProviderFailure(t *testing.T) {
	candidates := []MergedCandidate{
		ordinaryCandidate(forkRepository),
		func() MergedCandidate {
			candidate := ordinaryCandidate(forkRepository)
			candidate.Path = "/worktrees/confirmed"
			candidate.Head = advancedHead
			return candidate
		}(),
	}
	provider := &fakeMergedProvider{
		byCommit: func(_ pullrequest.Repository, sha string) ([]pullrequest.PullRequest, error) {
			if sha == mergedHead {
				return nil, pullrequest.NewError(pullrequest.CodeNetwork, "temporary", true, nil)
			}
			pr := exactMergedPR(forkRepository)
			pr.HeadSHA = advancedHead
			return []pullrequest.PullRequest{pr}, nil
		},
	}

	outcomes := EvaluateMerged(context.Background(), provider, candidates)

	require.Len(t, outcomes, 2)
	assert.Equal(t, NetworkFailure, outcomes[0].Reason)
	assert.Equal(t, EligibleMerged, outcomes[1].Reason)
}

func TestEvaluateMergedResolvesTransferredRepositoryBeforeProvenanceValidation(t *testing.T) {
	const legacyRepository = "github.com/legacy/widget"
	candidate := importedTransferredCandidate()
	candidate.ProjectRepository = legacyRepository
	candidate.LiveRepository = legacyRepository
	provider := providerForImported(exactMergedPR(forkRepository))
	provider.resolve = func(requested pullrequest.Repository) (pullrequest.Repository, error) {
		switch requested.Identity {
		case legacyRepository:
			return repository(baseRepository), nil
		case forkRepository:
			return repository(forkRepository), nil
		default:
			return pullrequest.Repository{}, fmt.Errorf("unexpected repository resolution: %s", requested.Identity)
		}
	}

	outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

	require.Len(t, outcomes, 1)
	assert.Equal(t, EligibleMerged, outcomes[0].Reason)
	assert.Equal(t, 2, provider.resolveCalls)
	assert.Equal(t, 1, provider.getCalls)
}

func TestEvaluateMergedAcceptsLegacyProvenanceAfterRepositoryTransfer(t *testing.T) {
	const legacyRepository = "github.com/legacy/widget"
	candidate := importedMergedCandidate()
	candidate.ProjectRepository = legacyRepository
	candidate.LiveRepository = legacyRepository
	candidate.Provenance.PullRequestID = pullrequest.OpaqueID(legacyRepository, 17)
	candidate.Provenance.Repository = legacyRepository
	candidate.Provenance.RepositoryAliases = nil
	candidate.Provenance.Project.Identity = legacyRepository
	candidate.Provenance.Workspace.Repository = legacyRepository
	provider := providerForImported(exactMergedPR(forkRepository))
	provider.resolve = func(requested pullrequest.Repository) (pullrequest.Repository, error) {
		switch requested.Identity {
		case legacyRepository:
			return repository(baseRepository), nil
		case forkRepository:
			return repository(forkRepository), nil
		default:
			return pullrequest.Repository{}, fmt.Errorf(
				"unexpected repository resolution: %s", requested.Identity,
			)
		}
	}

	outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

	require.Len(t, outcomes, 1)
	assert.Equal(t, EligibleMerged, outcomes[0].Reason)
	assert.Equal(t, 1, provider.getCalls)
}

func TestEvaluateMergedAcceptsSymlinkEquivalentImportedWorkspacePath(t *testing.T) {
	realPath := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(realPath, 0o755))
	aliasPath := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	candidate := importedMergedCandidate()
	candidate.Path = aliasPath
	candidate.Provenance.Workspace.Path = realPath
	provider := providerForImported(exactMergedPR(forkRepository))

	outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

	require.Len(t, outcomes, 1)
	assert.Equal(t, EligibleMerged, outcomes[0].Reason)
}

func TestEvaluateMergedResolvesTransferredSourceRepository(t *testing.T) {
	const (
		legacySource   = "github.com/legacy-fork/widget"
		resolvedSource = "github.com/new-owner/widget"
	)
	resolve := func(requested pullrequest.Repository) (pullrequest.Repository, error) {
		switch requested.Identity {
		case baseRepository:
			return repository(baseRepository), nil
		case legacySource:
			return repository(resolvedSource), nil
		default:
			return pullrequest.Repository{}, fmt.Errorf("unexpected repository resolution: %s", requested.Identity)
		}
	}

	t.Run("commit match", func(t *testing.T) {
		candidate := ordinaryCandidate(legacySource)
		provider := providerForCommit(exactMergedPR(resolvedSource))
		provider.resolve = resolve

		outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

		require.Len(t, outcomes, 1)
		assert.Equal(t, EligibleMerged, outcomes[0].Reason)
		assert.Equal(t, 2, provider.resolveCalls)
	})

	t.Run("diagnostic head lookup", func(t *testing.T) {
		candidate := ordinaryCandidate(legacySource)
		candidate.Head = advancedHead
		provider := &fakeMergedProvider{
			resolve: resolve,
			byCommit: func(pullrequest.Repository, string) ([]pullrequest.PullRequest, error) {
				return nil, nil
			},
			byHead: func(_ pullrequest.Repository, owner, branch string) ([]pullrequest.PullRequest, error) {
				assert.Equal(t, "new-owner", owner)
				assert.Equal(t, "topic", branch)
				return []pullrequest.PullRequest{exactMergedPR(resolvedSource)}, nil
			},
		}

		outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

		require.Len(t, outcomes, 1)
		assert.Equal(t, HeadAdvancedAfterPR, outcomes[0].Reason)
		assert.Equal(t, 2, provider.resolveCalls)
	})

	t.Run("imported provenance", func(t *testing.T) {
		candidate := importedMergedCandidate()
		candidate.SourceRepository = legacySource
		candidate.Provenance.SourceRepo = legacySource
		provider := providerForImported(exactMergedPR(resolvedSource))
		provider.resolve = resolve

		outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

		require.Len(t, outcomes, 1)
		assert.Equal(t, EligibleMerged, outcomes[0].Reason)
		assert.Equal(t, 2, provider.resolveCalls)
	})
}

func TestEvaluateMergedRejectsMissingLiveRepositoryWithoutProvider(t *testing.T) {
	candidate := ordinaryCandidate(forkRepository)
	candidate.LiveRepository = ""
	provider := providerForCommit(exactMergedPR(forkRepository))

	outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

	require.Len(t, outcomes, 1)
	assert.Equal(t, RepositoryChanged, outcomes[0].Reason)
	assert.Zero(t, provider.commitCalls)
	assert.Zero(t, provider.headCalls)
}

func TestEvaluateMergedRejectsMissingUpstreamWithoutProvider(t *testing.T) {
	candidate := noUpstream()
	provider := &fakeMergedProvider{}

	outcomes := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})

	require.Len(t, outcomes, 1)
	assert.Equal(t, SourceRepositoryUnavailable, outcomes[0].Reason)
	assert.Zero(t, provider.resolveCalls)
	assert.Zero(t, provider.commitCalls)
	assert.Zero(t, provider.headCalls)
}

func TestMergedReportExitCodeTreatsNormalNonCandidatesAsSuccess(t *testing.T) {
	for _, reason := range []Reason{NoAssociatedPR, PRNotMerged} {
		report := Report{Outcomes: []Outcome{{Reason: reason}}}
		assert.Equal(t, 0, report.ExitCode(), string(reason))
	}
	for _, reason := range []Reason{MissingGeneration, DirtyWorktree, AmbiguousPRMatch, NetworkFailure} {
		report := Report{Outcomes: []Outcome{{Reason: reason}}}
		assert.Equal(t, 1, report.ExitCode(), string(reason))
	}
}

func ordinaryCandidate(sourceRepository string) MergedCandidate {
	return MergedCandidate{
		Path: "/worktrees/topic", Branch: "topic-local", Head: mergedHead, Generation: "0123456789abcdef0123456789abcdef",
		ProjectRepository: baseRepository, LiveRepository: baseRepository,
		SourceRepository: sourceRepository, SourceBranch: "topic",
	}
}

func withForkOrigin(candidate MergedCandidate) MergedCandidate {
	candidate.LiveRepository = forkRepository
	return candidate
}

func importedMergedCandidate() MergedCandidate {
	candidate := ordinaryCandidate(forkRepository)
	candidate.MainRepositoryPath = "/projects/widget"
	candidate.Provenance = &pullrequest.Provenance{
		PullRequestID: pullrequest.OpaqueID(baseRepository, 17), Provider: "github",
		Repository: baseRepository, Number: 17, URL: "https://github.com/acme/widget/pull/17",
		HeadSHA: mergedHead, SourceRepo: forkRepository, SourceBranch: "topic",
		Project: pullrequest.Project{Identity: baseRepository, Path: "/projects/widget"},
		Workspace: pullrequest.Workspace{
			ID: "workspace-17", Repository: baseRepository, Branch: candidate.Branch,
			Path: candidate.Path, State: "ready", SessionName: "topic",
		},
	}
	return candidate
}

func importedWithProjectPathMismatch() MergedCandidate {
	candidate := importedMergedCandidate()
	candidate.MainRepositoryPath = "/projects/replacement"
	return candidate
}

func importedWithHeadMismatch() MergedCandidate {
	candidate := importedMergedCandidate()
	candidate.Head = advancedHead
	return candidate
}

func importedTransferredCandidate() MergedCandidate {
	candidate := importedMergedCandidate()
	const legacyRepository = "github.com/legacy/widget"
	candidate.Provenance.PullRequestID = pullrequest.OpaqueID(legacyRepository, 17)
	candidate.Provenance.Repository = legacyRepository
	candidate.Provenance.RepositoryAliases = []string{legacyRepository, baseRepository}
	candidate.Provenance.Project.Identity = legacyRepository
	candidate.Provenance.Workspace.Repository = legacyRepository
	return candidate
}

func importedWithOpaqueIDMismatch() MergedCandidate {
	candidate := importedMergedCandidate()
	candidate.Provenance.PullRequestID = pullrequest.OpaqueID(baseRepository, 99)
	return candidate
}

func importedTransferredSourceCandidate() MergedCandidate {
	candidate := importedTransferredCandidate()
	const legacyRepository = "github.com/legacy/widget"
	candidate.SourceRepository = baseRepository
	candidate.Provenance.SourceRepo = legacyRepository
	return candidate
}

func importedWithStaleURL() MergedCandidate {
	candidate := importedTransferredCandidate()
	candidate.Provenance.URL = "https://github.com/legacy/widget/pull/17"
	return candidate
}

func importedWithWorkspaceMismatch() MergedCandidate {
	candidate := importedMergedCandidate()
	candidate.Provenance.Workspace.Path = "/worktrees/other"
	return candidate
}

func importedWithGenerationMismatch() MergedCandidate {
	candidate := importedMergedCandidate()
	candidate.Generation = "0123456789abcdef0123456789abcdef"
	candidate.Provenance.Workspace.Generation = "fedcba9876543210fedcba9876543210"
	return candidate
}

func withoutGeneration() MergedCandidate {
	candidate := ordinaryCandidate(forkRepository)
	candidate.Generation = ""
	return candidate
}

func withInvalidGeneration() MergedCandidate {
	candidate := ordinaryCandidate(forkRepository)
	candidate.Generation = "not-a-generation"
	return candidate
}

func dirtyMerged() MergedCandidate {
	candidate := ordinaryCandidate(forkRepository)
	candidate.Dirty = true
	return candidate
}

func mainWorktree() MergedCandidate {
	candidate := ordinaryCandidate(forkRepository)
	candidate.IsMain = true
	return candidate
}

func advancedCandidate() MergedCandidate {
	candidate := ordinaryCandidate(forkRepository)
	candidate.Head = advancedHead
	return candidate
}

func noUpstream() MergedCandidate {
	candidate := ordinaryCandidate("")
	return candidate
}

func providerForImported(pr pullrequest.PullRequest) *fakeMergedProvider {
	return &fakeMergedProvider{get: func(repository pullrequest.Repository, number int) (pullrequest.PullRequest, error) {
		if repository.Identity != baseRepository || number != 17 {
			return pullrequest.PullRequest{}, fmt.Errorf("unexpected imported selector %s#%d", repository.Identity, number)
		}
		return pr, nil
	}}
}

func providerForCommit(prs ...pullrequest.PullRequest) *fakeMergedProvider {
	return &fakeMergedProvider{
		byCommit: func(_ pullrequest.Repository, _ string) ([]pullrequest.PullRequest, error) { return prs, nil },
		byHead:   func(_ pullrequest.Repository, _, _ string) ([]pullrequest.PullRequest, error) { return nil, nil },
	}
}

func providerForAdvancedBranch() *fakeMergedProvider {
	return &fakeMergedProvider{
		byCommit: func(_ pullrequest.Repository, _ string) ([]pullrequest.PullRequest, error) { return nil, nil },
		byHead: func(_ pullrequest.Repository, owner, branch string) ([]pullrequest.PullRequest, error) {
			if owner != "octocat" || branch != "topic" {
				return nil, fmt.Errorf("unexpected head %s:%s", owner, branch)
			}
			return []pullrequest.PullRequest{exactMergedPR(forkRepository)}, nil
		},
	}
}

func providerEmpty() *fakeMergedProvider {
	return &fakeMergedProvider{
		byCommit: func(_ pullrequest.Repository, _ string) ([]pullrequest.PullRequest, error) { return nil, nil },
		byHead:   func(_ pullrequest.Repository, _, _ string) ([]pullrequest.PullRequest, error) { return nil, nil },
	}
}

func providerWithError(err error) *fakeMergedProvider {
	return &fakeMergedProvider{byCommit: func(_ pullrequest.Repository, _ string) ([]pullrequest.PullRequest, error) {
		return nil, err
	}}
}

func exactMergedPR(sourceRepository string) pullrequest.PullRequest {
	mergedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return pullrequest.PullRequest{
		ID: pullrequest.OpaqueID(baseRepository, 17), Provider: "github",
		Repository: repository(baseRepository), Number: 17,
		URL: "https://github.com/acme/widget/pull/17", Title: "Topic", Author: "octocat",
		Source: pullrequest.Branch{Name: "topic", Repository: repository(sourceRepository)},
		Target: pullrequest.Branch{Name: "main", Repository: repository(baseRepository)},
		State:  "closed", HeadSHA: mergedHead, MergedAt: &mergedAt,
	}
}

func closedPR(sourceRepository string) pullrequest.PullRequest {
	pr := exactMergedPR(sourceRepository)
	pr.MergedAt = nil
	return pr
}

func prWithBranch(pr pullrequest.PullRequest, branch string) pullrequest.PullRequest {
	pr.Source.Name = branch
	return pr
}

func prWithNumber(pr pullrequest.PullRequest, number int) pullrequest.PullRequest {
	pr.Number = number
	pr.ID = pullrequest.OpaqueID(baseRepository, number)
	pr.URL = fmt.Sprintf("https://github.com/acme/widget/pull/%d", number)
	return pr
}

func repository(identity string) pullrequest.Repository {
	trimmed := identity
	if len(trimmed) > len("github.com/") && trimmed[:len("github.com/")] == "github.com/" {
		trimmed = trimmed[len("github.com/"):]
	}
	owner, name := "", ""
	for index, value := range trimmed {
		if value == '/' {
			owner, name = trimmed[:index], trimmed[index+1:]
			break
		}
	}
	return pullrequest.Repository{Provider: "github", Host: "github.com", Owner: owner, Name: name, Identity: identity}
}
