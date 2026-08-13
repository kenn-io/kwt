package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
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

// ResolveProspectiveProjectIdentity returns the same canonical identity that
// project inventory will publish after registering project. Resolution occurs
// before mutation so a failure cannot leave an unacknowledged registration.
func ResolveProspectiveProjectIdentity(
	ctx context.Context,
	home string,
	expansion ExpansionContext,
	project models.Project,
) (string, error) {
	if err := expansion.validate(); err != nil {
		return "", err
	}
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(
		home, expansion.expandPath,
	)
	if err != nil {
		return "", err
	}
	registration := config.ProjectRegistration{
		Persisted: project,
		Effective: project,
	}
	effectivePath, err := expansion.expandPath(project.Path)
	if err != nil {
		return "", err
	}
	registration.Effective.Path = effectivePath
	return resolveProjectIdentity(
		ctx,
		registration,
		credentials.ProtectedNames(snapshot.Config)...,
	)
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
	if identity == "" {
		return "", fmt.Errorf("expected repository identity is invalid")
	}
	if isExactLocalProjectIdentity(identity) {
		return identity, nil
	}
	if identity != strings.TrimSpace(identity) {
		return "", fmt.Errorf("expected repository identity is invalid")
	}
	if canonical, ok := repositoryurl.CanonicalRepositoryIdentity(identity); ok && canonical == identity {
		return identity, nil
	}
	return "", fmt.Errorf("expected repository identity is invalid")
}

func isExactLocalProjectIdentity(identity string) bool {
	return strings.HasPrefix(identity, "local/") && len(identity) > len("local/")
}

// EqualProjectIdentity reports whether two credential-free project identities
// name the same repository under the canonical host's case rules.
func EqualProjectIdentity(left, right string) bool {
	left, leftErr := foldProjectIdentity(left)
	right, rightErr := foldProjectIdentity(right)
	return leftErr == nil && rightErr == nil && left == right
}

func foldProjectIdentity(identity string) (string, error) {
	identity, err := validateStableProjectIdentity(identity)
	if err != nil {
		return "", err
	}
	if isExactLocalProjectIdentity(identity) {
		return identity, nil
	}
	return repositoryurl.FoldRepositoryIdentity(identity), nil
}
