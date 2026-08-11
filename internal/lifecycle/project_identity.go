package lifecycle

import (
	"fmt"
	"strings"

	"go.kenn.io/kwt/internal/config"
	repositoryurl "go.kenn.io/kwt/internal/url"
	internalworktree "go.kenn.io/kwt/internal/worktree"
)

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
