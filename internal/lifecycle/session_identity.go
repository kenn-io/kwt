package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/tmux"
	internalworktree "go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

// ResolveCurrentWorktreeSessionIdentity derives the session name from the
// worktree's current repository identity and branch. Callers that grant
// launch or removal authority invoke it inside their project and worktree
// lifecycle guards.
func ResolveCurrentWorktreeSessionIdentity(
	ctx context.Context,
	worktreePath string,
	projects []models.Project,
	protectedNames []string,
) (sessionName string, branch string, err error) {
	currentGit := git.NewForInventory(ctx, worktreePath, protectedNames)
	branchOutput, err := currentGit.RunCommand("symbolic-ref", "--short", "HEAD")
	if err != nil {
		branchOutput, err = currentGit.RunCommand("rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", "", fmt.Errorf("resolve current worktree branch: %w", err)
		}
	}
	branch = strings.TrimSpace(branchOutput)
	if branch == "" {
		return "", "", errors.New("current worktree branch is empty")
	}
	info, err := internalworktree.RepositoryInfoWithProjects(
		internalworktree.NewCachedIdentityGit(currentGit),
		projects,
	)
	if err != nil {
		return "", "", fmt.Errorf("resolve current repository identity: %w", err)
	}
	return tmux.WorkspaceSessionName(info, branch, worktreePath), branch, nil
}
