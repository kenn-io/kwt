package worktree

import "fmt"

// ConditionReason identifies the observation that invalidated cleanup.
type ConditionReason string

const (
	ReasonBacklinkChanged           ConditionReason = "backlink_changed"
	ReasonGenerationChanged         ConditionReason = "generation_changed"
	ReasonHeadChanged               ConditionReason = "head_changed"
	ReasonRepositoryChanged         ConditionReason = "repository_identity_changed"
	ReasonBranchChanged             ConditionReason = "branch_changed"
	ReasonUpstreamRepositoryChanged ConditionReason = "upstream_repository_changed"
	ReasonUpstreamBranchChanged     ConditionReason = "upstream_branch_changed"
	ReasonDirty                     ConditionReason = "dirty_worktree"
	ReasonInitializedSubmodule      ConditionReason = "initialized_submodule"
	ReasonLocked                    ConditionReason = "locked_worktree"
	ReasonMainWorktree              ConditionReason = "main_worktree"
)

type ConditionError struct {
	Reason ConditionReason
	Path   string
}

func (e *ConditionError) Error() string {
	return fmt.Sprintf("worktree %s for %s", e.Reason, e.Path)
}

// RemovalConditions are rechecked immediately before Git cleanup. The caller
// supplies repository identity comparison because application identity rules
// need not match Git clone-URL normalization. Empty fields impose no condition;
// callers that require generation fencing must supply a validated Generation.
// Generation compares kwt's generation marker; select another application's
// marker with RemovalRequest.Identity and MatchingIdentity instead.
type RemovalConditions struct {
	ExpectedGitDir, Generation, Head, RepositoryIdentity string
	Branch, UpstreamRepository, UpstreamBranch           string
	RequireClean, IncludeIgnored                         bool
	MatchRepositoryIdentity                              func(remoteURL, expected string) bool
}
