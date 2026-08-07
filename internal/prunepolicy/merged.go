package prunepolicy

import (
	"context"
	"errors"
	"strconv"
	"strings"

	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/utils"
)

// MergedProvider supplies provider evidence without owning prune policy.
type MergedProvider interface {
	ResolveRepository(context.Context, pullrequest.Repository) (pullrequest.Repository, error)
	Get(context.Context, pullrequest.Repository, int) (pullrequest.PullRequest, error)
	ListForCommit(context.Context, pullrequest.Repository, string) ([]pullrequest.PullRequest, error)
	ListByHead(context.Context, pullrequest.Repository, string, string) ([]pullrequest.PullRequest, error)
}

// MergedCandidate is the complete local snapshot used for provider preflight.
type MergedCandidate struct {
	Path               string
	Branch             string
	Head               string
	Generation         string
	MainRepositoryPath string
	ProjectRepository  string
	LiveRepository     string
	SourceRepository   string
	SourceBranch       string
	Dirty              bool
	IsMain             bool
	Provenance         *pullrequest.Provenance
}

// EvaluateMerged classifies candidates without changing local or provider state.
func EvaluateMerged(
	ctx context.Context, provider MergedProvider, candidates []MergedCandidate,
) []Outcome {
	outcomes := make([]Outcome, 0, len(candidates))
	for _, candidate := range candidates {
		outcomes = append(outcomes, evaluateMergedCandidate(ctx, provider, candidate))
	}
	return outcomes
}

func evaluateMergedCandidate(
	ctx context.Context, provider MergedProvider, candidate MergedCandidate,
) Outcome {
	switch {
	case candidate.IsMain:
		return mergedOutcome(candidate, MainWorktree, "main worktree is never pruned")
	case gitadapter.ValidateWorktreeGeneration(candidate.Generation) != nil:
		return mergedOutcome(candidate, MissingGeneration, "worktree has no valid generation")
	case candidate.Dirty:
		return mergedOutcome(candidate, DirtyWorktree, "worktree has uncommitted changes")
	case strings.TrimSpace(candidate.LiveRepository) == "":
		return mergedOutcome(candidate, RepositoryChanged, "worktree origin repository identity is unavailable")
	}
	requestedBase, ok := githubRepository(candidate.ProjectRepository)
	if !ok {
		return mergedOutcome(candidate, DoctorRequired, "project repository identity is unavailable")
	}
	requestedSource, ok := githubRepository(candidate.SourceRepository)
	if !ok || strings.TrimSpace(candidate.SourceBranch) == "" {
		return mergedOutcome(candidate, SourceRepositoryUnavailable, "configured upstream does not identify a source repository and branch")
	}
	observedProjectRepository := candidate.ProjectRepository
	observedSourceRepository := candidate.SourceRepository
	resolvedBase, err := provider.ResolveRepository(ctx, requestedBase)
	if err != nil {
		return providerFailure(candidate, err)
	}
	base, ok := githubRepository(resolvedBase.Identity)
	if !ok {
		return mergedOutcome(candidate, RepositoryChanged, "resolved project repository identity is unavailable")
	}
	resolvedSource, err := provider.ResolveRepository(ctx, requestedSource)
	if err != nil {
		return providerFailure(candidate, err)
	}
	source, ok := githubRepository(resolvedSource.Identity)
	if !ok {
		return mergedOutcome(candidate, SourceRepositoryUnavailable, "resolved upstream repository identity is unavailable")
	}
	candidate.ProjectRepository = base.Identity
	candidate.SourceRepository = source.Identity
	if candidate.Provenance != nil {
		return evaluateImportedMerged(
			ctx,
			provider,
			candidate,
			base,
			observedProjectRepository,
			observedSourceRepository,
		)
	}
	prs, err := provider.ListForCommit(ctx, base, candidate.Head)
	if err != nil {
		return providerFailure(candidate, err)
	}
	exact, mismatch := exactOrdinaryMatches(candidate, prs)
	switch len(exact) {
	case 1:
		return pullRequestOutcome(candidate, exact[0])
	case 0:
		if mismatch != "" {
			return mergedOutcome(candidate, mismatch, mismatchMessage(mismatch))
		}
	default:
		return ambiguousOutcome(candidate, exact)
	}
	prs, err = provider.ListByHead(ctx, base, source.Owner, candidate.SourceBranch)
	if err != nil {
		return providerFailure(candidate, err)
	}
	return diagnoseSourceHead(candidate, prs)
}

func evaluateImportedMerged(
	ctx context.Context,
	provider MergedProvider,
	candidate MergedCandidate,
	base pullrequest.Repository,
	observedProjectRepository string,
	observedSourceRepository string,
) Outcome {
	record := *candidate.Provenance
	if !strings.EqualFold(record.Provider, "github") || record.Number <= 0 ||
		!provenancePullRequestIDMatches(record) ||
		!provenanceRepositoryMatches(record, observedProjectRepository) ||
		!provenanceRepositoryMatches(record, record.Project.Identity) ||
		!provenanceRepositoryMatches(record, record.Workspace.Repository) ||
		strings.TrimSpace(candidate.MainRepositoryPath) == "" ||
		strings.TrimSpace(record.Project.Path) == "" ||
		utils.PathKey(record.Project.Path) != utils.PathKey(candidate.MainRepositoryPath) ||
		utils.PathKey(record.Workspace.Path) != utils.PathKey(candidate.Path) ||
		record.Workspace.Branch != candidate.Branch ||
		(record.Workspace.Generation != "" &&
			record.Workspace.Generation != candidate.Generation) {
		return mergedOutcome(candidate, DoctorRequired, "pull-request provenance does not match the live workspace")
	}
	if !provenanceSourceRepositoryMatches(record, observedSourceRepository) {
		return mergedOutcome(candidate, SourceRepositoryMismatch, mismatchMessage(SourceRepositoryMismatch))
	}
	if record.SourceBranch != candidate.SourceBranch {
		return mergedOutcome(candidate, SourceBranchMismatch, mismatchMessage(SourceBranchMismatch))
	}
	if record.HeadSHA != candidate.Head {
		return mergedOutcome(candidate, HeadAdvancedAfterPR, mismatchMessage(HeadAdvancedAfterPR))
	}
	pr, err := provider.Get(ctx, base, record.Number)
	if err != nil {
		return providerFailure(candidate, err)
	}
	if pr.Number != record.Number ||
		!pullrequest.EqualRepositoryIdentity(pr.Repository.Identity, candidate.ProjectRepository) {
		return mergedOutcome(candidate, RepositoryChanged, "provider pull request does not match the recorded repository")
	}
	if !strings.EqualFold(pr.Provider, record.Provider) {
		return mergedOutcome(candidate, DoctorRequired, "provider pull request does not match recorded provenance")
	}
	if !pullrequest.EqualRepositoryIdentity(
		candidate.SourceRepository,
		pr.Source.Repository.Identity,
	) {
		return mergedOutcome(candidate, SourceRepositoryMismatch, mismatchMessage(SourceRepositoryMismatch))
	}
	if pr.Source.Name != record.SourceBranch {
		return mergedOutcome(candidate, SourceBranchMismatch, mismatchMessage(SourceBranchMismatch))
	}
	if pr.HeadSHA != record.HeadSHA {
		return mergedOutcome(candidate, HeadAdvancedAfterPR, mismatchMessage(HeadAdvancedAfterPR))
	}
	return pullRequestOutcome(candidate, pr)
}

func exactOrdinaryMatches(
	candidate MergedCandidate, prs []pullrequest.PullRequest,
) ([]pullrequest.PullRequest, Reason) {
	var exact []pullrequest.PullRequest
	var mismatch Reason
	for _, pr := range prs {
		if !pullrequest.EqualRepositoryIdentity(pr.Repository.Identity, candidate.ProjectRepository) ||
			pr.HeadSHA != candidate.Head {
			continue
		}
		if !pullrequest.EqualRepositoryIdentity(pr.Source.Repository.Identity, candidate.SourceRepository) {
			mismatch = SourceRepositoryMismatch
			continue
		}
		if pr.Source.Name != candidate.SourceBranch {
			if mismatch == "" {
				mismatch = SourceBranchMismatch
			}
			continue
		}
		exact = append(exact, pr)
	}
	return exact, mismatch
}

func diagnoseSourceHead(candidate MergedCandidate, prs []pullrequest.PullRequest) Outcome {
	var matches []pullrequest.PullRequest
	var mismatch Reason
	for _, pr := range prs {
		if !pullrequest.EqualRepositoryIdentity(pr.Repository.Identity, candidate.ProjectRepository) {
			continue
		}
		if !pullrequest.EqualRepositoryIdentity(pr.Source.Repository.Identity, candidate.SourceRepository) {
			mismatch = SourceRepositoryMismatch
			continue
		}
		if pr.Source.Name != candidate.SourceBranch {
			if mismatch == "" {
				mismatch = SourceBranchMismatch
			}
			continue
		}
		matches = append(matches, pr)
	}
	if len(matches) > 1 {
		return ambiguousOutcome(candidate, matches)
	}
	if len(matches) == 1 {
		if matches[0].MergedAt == nil {
			return pullRequestOutcome(candidate, matches[0])
		}
		outcome := mergedOutcome(candidate, HeadAdvancedAfterPR, mismatchMessage(HeadAdvancedAfterPR))
		addPullRequestEvidence(&outcome, matches[0])
		return outcome
	}
	if mismatch != "" {
		return mergedOutcome(candidate, mismatch, mismatchMessage(mismatch))
	}
	return mergedOutcome(candidate, NoAssociatedPR, "no pull request is associated with the worktree HEAD or source branch")
}

func pullRequestOutcome(candidate MergedCandidate, pr pullrequest.PullRequest) Outcome {
	reason := EligibleMerged
	message := "worktree has one explicitly merged pull request"
	if pr.MergedAt == nil {
		reason = PRNotMerged
		message = "associated pull request is not merged"
	}
	outcome := mergedOutcome(candidate, reason, message)
	addPullRequestEvidence(&outcome, pr)
	return outcome
}

func ambiguousOutcome(candidate MergedCandidate, prs []pullrequest.PullRequest) Outcome {
	outcome := mergedOutcome(candidate, AmbiguousPRMatch, "multiple pull requests match the worktree identity")
	outcome.Evidence["match_count"] = strconv.Itoa(len(prs))
	return outcome
}

func providerFailure(candidate MergedCandidate, err error) Outcome {
	reason := NetworkFailure
	code := pullrequest.CodeNetwork
	retryable := false
	var typed *pullrequest.Error
	if errors.As(err, &typed) {
		code = typed.Code
		retryable = typed.Retryable
		switch typed.Code {
		case pullrequest.CodeAuthentication:
			reason = AuthenticationFailed
		case pullrequest.CodeNetwork:
			reason = NetworkFailure
		default:
			reason = ProviderFailure
		}
	}
	message := "pull-request provider could not classify the candidate"
	remediation := "Check network connectivity and retry merged pruning."
	switch reason {
	case AuthenticationFailed:
		message = "pull-request provider authentication failed"
		remediation = "Refresh GitHub authentication and retry merged pruning."
	case ProviderFailure:
		message = "pull-request provider returned an unusable result"
		remediation = "Review the provider error code, resolve the provider issue, and retry merged pruning."
	}
	outcome := mergedOutcome(candidate, reason, message)
	outcome.Remediation = remediation
	outcome.Evidence["provider_error_code"] = string(code)
	outcome.Evidence["retryable"] = strconv.FormatBool(retryable)
	return outcome
}

func mergedOutcome(candidate MergedCandidate, reason Reason, message string) Outcome {
	return Outcome{
		Path: candidate.Path, Branch: candidate.Branch, Reason: reason, Message: message,
		Evidence: map[string]string{"head_sha": candidate.Head},
	}
}

func addPullRequestEvidence(outcome *Outcome, pr pullrequest.PullRequest) {
	outcome.Evidence["pr_number"] = strconv.Itoa(pr.Number)
	outcome.Evidence["pr_url"] = pr.URL
	outcome.Evidence["pr_head_sha"] = pr.HeadSHA
}

func mismatchMessage(reason Reason) string {
	switch reason {
	case HeadAdvancedAfterPR:
		return "source branch has advanced after the associated pull request head"
	case SourceRepositoryMismatch:
		return "associated pull request source repository does not match the configured upstream"
	case SourceBranchMismatch:
		return "associated pull request source branch does not match the configured upstream"
	default:
		return string(reason)
	}
}

func provenanceRepositoryMatches(record pullrequest.Provenance, identity string) bool {
	if pullrequest.EqualRepositoryIdentity(record.Repository, identity) {
		return true
	}
	for _, alias := range record.RepositoryAliases {
		if pullrequest.EqualRepositoryIdentity(alias, identity) {
			return true
		}
	}
	return false
}

func provenancePullRequestIDMatches(record pullrequest.Provenance) bool {
	if record.PullRequestID == pullrequest.OpaqueID(record.Repository, record.Number) {
		return true
	}
	for _, alias := range record.RepositoryAliases {
		if record.PullRequestID == pullrequest.OpaqueID(alias, record.Number) {
			return true
		}
	}
	return false
}

func provenanceSourceRepositoryMatches(record pullrequest.Provenance, identity string) bool {
	if pullrequest.EqualRepositoryIdentity(record.SourceRepo, identity) {
		return true
	}
	return provenanceRepositoryMatches(record, record.SourceRepo) &&
		provenanceRepositoryMatches(record, identity)
}

func githubRepository(identity string) (pullrequest.Repository, bool) {
	identity = pullrequest.NormalizeRepositoryIdentity(identity)
	const prefix = "github.com/"
	if !strings.HasPrefix(identity, prefix) {
		return pullrequest.Repository{}, false
	}
	owner, name, ok := strings.Cut(strings.TrimPrefix(identity, prefix), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return pullrequest.Repository{}, false
	}
	return pullrequest.Repository{
		Provider: "github", Identity: identity, Host: "github.com", Owner: owner, Name: name,
	}, true
}
