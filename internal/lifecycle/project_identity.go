package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	internalworktree "go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

func resolveProjectIdentity(
	ctx context.Context,
	registration config.ProjectRegistration,
	protectedNames ...string,
) (string, error) {
	if identity, ok := repositoryurl.CanonicalRepositoryIdentity(
		registration.Persisted.Repository,
	); ok {
		return identity, nil
	}
	g := internalworktree.NewCachedIdentityGit(
		git.NewForInventory(ctx, registration.Effective.Path, protectedNames),
	)
	mainPath, mainErr := g.GetMainRepositoryPath()
	if mainErr == nil &&
		utils.PathKey(mainPath) == utils.PathKey(registration.Effective.Path) {
		if info, infoErr := internalworktree.RepositoryInfoWithProjects(
			g, []models.Project{registration.Effective},
		); infoErr == nil {
			if identity, ok := repositoryurl.CanonicalRepositoryIdentity(info.FullPath); ok {
				return identity, nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return stableProjectIdentity(registration)
}

// ResolveProjectRegistrationIdentity applies the same credential-free
// identity contract used by project inventory, lifecycle claims, and guarded
// removal.
func ResolveProjectRegistrationIdentity(
	ctx context.Context,
	registration config.ProjectRegistration,
	protectedNames ...string,
) (string, error) {
	return resolveProjectIdentity(ctx, registration, protectedNames...)
}

func stableProjectIdentity(registration config.ProjectRegistration) (string, error) {
	if identity, ok := repositoryurl.CanonicalRepositoryIdentity(
		registration.Persisted.Repository,
	); ok {
		return identity, nil
	}
	info, err := internalworktree.RepositoryInfoFromExactLocalPath(
		registration.Effective.Path,
	)
	if err != nil || info == nil || !repositoryurl.IsLocalFallbackIdentity(info.FullPath) {
		return "", fmt.Errorf("derive local project identity")
	}
	return info.FullPath, nil
}

func validateStableProjectIdentity(identity string) (string, error) {
	if identity == "" || identity != strings.TrimSpace(identity) {
		return "", fmt.Errorf("expected repository identity is invalid")
	}
	if canonical, ok := repositoryurl.CanonicalRepositoryIdentity(identity); ok && canonical == identity {
		return identity, nil
	}
	if repositoryurl.IsLocalFallbackIdentity(identity) && identity != "local" {
		return identity, nil
	}
	return "", fmt.Errorf("expected repository identity is invalid")
}
