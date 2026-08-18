package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"

	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

type removalProtectedSessionTarget struct {
	branch     string
	session    string
	socketName string
}

func observeRemovalProtectedSessionTarget(
	ctx context.Context,
	home string,
	worktreePath string,
	generation string,
	claim *ProjectClaim,
) (*removalProtectedSessionTarget, error) {
	var target *removalProtectedSessionTarget
	err := pullrequest.NewFileStore(filepath.Join(home, "pull-requests.json")).View(
		ctx,
		func(records map[string]pullrequest.Provenance) error {
			for _, record := range records {
				workspace := record.Workspace
				generationMatch := workspace.Generation != "" &&
					workspace.Generation == generation
				legacyPathMatch := workspace.Generation == "" &&
					utils.PathKey(workspace.Path) == utils.PathKey(worktreePath)
				if !generationMatch && !legacyPathMatch {
					continue
				}
				if claim == nil || !pullrequest.ProvenanceHasRepositoryIdentity(
					record, claim.Identity,
				) || !validProvenanceSession(record) {
					return changedRemovalSessionTarget()
				}
				candidate := &removalProtectedSessionTarget{
					branch:     workspace.Branch,
					session:    workspace.SessionName,
					socketName: tmux.ProtectedWorkspaceSocketName(workspace.SessionName, workspace.Path),
				}
				if target != nil && *target != *candidate {
					return changedRemovalSessionTarget()
				}
				target = candidate
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("resolve protected tmux session authority: %w", err)
	}
	return target, nil
}

func validProvenanceSession(record pullrequest.Provenance) bool {
	workspace := record.Workspace
	if workspace.Branch == "" || workspace.SessionName == "" {
		return false
	}
	for _, identity := range pullrequest.ProvenanceRepositoryIdentities(record) {
		info, ok := repositoryurl.CanonicalRepositoryInfo(identity)
		if ok && tmux.MatchesWorkspaceSessionName(
			workspace.SessionName,
			info,
			workspace.Branch,
			workspace.Path,
		) {
			return true
		}
	}
	return false
}

func validateCurrentRemovalSessionTarget(
	ctx context.Context,
	worktreePath string,
	claim *ProjectClaim,
	protected *removalProtectedSessionTarget,
	expansion ExpansionContext,
	condition RemovalSessionCondition,
) error {
	projects := []models.Project(nil)
	if claim != nil && claim.Registered {
		projects = []models.Project{claim.Registration.Effective}
	}
	expectedSession, branch, err := ResolveCurrentWorktreeSessionIdentity(
		ctx,
		worktreePath,
		projects,
		nil,
	)
	if err != nil {
		return err
	}
	// compat(kag1): default-server adoption
	expectedSocketDirectory := removalSocketDirectory(expansion)
	if condition.SocketDirectory != expectedSocketDirectory {
		return changedRemovalSessionTarget()
	}
	if protected == nil {
		// compat(kag1): default-server adoption
		if condition.SessionName != expectedSession ||
			(condition.SocketName != "" && condition.SocketName != tmux.KWTServerSocketName) {
			return changedRemovalSessionTarget()
		}
		return nil
	}
	if protected.branch != branch {
		return changedRemovalSessionTarget()
	}
	if condition.SessionName != protected.session ||
		condition.SocketName != protected.socketName {
		return changedRemovalSessionTarget()
	}
	return nil
}

// compat(kag1): default-server adoption
func removalSocketDirectory(expansion ExpansionContext) string {
	return expansion.Environment[normalizedEnvironmentName("TMUX_TMPDIR")]
}

func changedRemovalSessionTarget() error {
	return &RemovalSessionConditionError{
		Reason: "worktree tmux session identity changed",
	}
}
