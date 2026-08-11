package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

func TransitionProjectRegistration(
	ctx context.Context,
	home string,
	expansion ExpansionContext,
	project models.Project,
	mutation func() error,
) error {
	if mutation == nil {
		return fmt.Errorf("project registration mutation is required")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		releaseTransition, err := acquireProjectTransitionFence(ctx, home)
		if err != nil {
			return err
		}
		identities, err := projectRegistrationTransitionIdentities(
			ctx, home, expansion, project,
		)
		if err != nil {
			_ = releaseTransition()
			return err
		}
		releases, err := acquireProjectIdentitySet(ctx, home, identities)
		if err != nil {
			_ = releaseTransition()
			return err
		}
		current, err := projectRegistrationTransitionIdentities(
			ctx, home, expansion, project,
		)
		if err == nil && slices.Equal(identities, current) {
			mutationErr := mutation()
			return errors.Join(
				mutationErr,
				releaseProjectIdentitySet(releases),
				releaseTransition(),
			)
		}
		releaseErr := errors.Join(
			releaseProjectIdentitySet(releases),
			releaseTransition(),
		)
		if err != nil || releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
	}
}

func projectRegistrationTransitionIdentities(
	ctx context.Context,
	home string,
	expansion ExpansionContext,
	project models.Project,
) ([]string, error) {
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(
		home, expansion.expandPath,
	)
	if err != nil {
		return nil, err
	}
	effectivePath, err := expansion.expandPath(project.Path)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(effectivePath); resolveErr == nil {
		effectivePath = resolved
	}
	next := config.ProjectRegistration{
		Persisted: project,
		Effective: project,
	}
	next.Effective.Path = effectivePath
	protectedNames := credentials.ProtectedNames(snapshot.Config)
	identity, err := resolveProjectIdentity(ctx, next, protectedNames...)
	if err != nil {
		return nil, err
	}
	identity, err = foldProjectIdentity(identity)
	if err != nil {
		return nil, err
	}
	identities := []string{identity}
	for _, registration := range snapshot.Projects {
		if !projectRegistrationUpdateMatches(registration, next) {
			continue
		}
		identity, identityErr := resolveProjectIdentity(
			ctx, registration, protectedNames...,
		)
		if identityErr != nil {
			return nil, identityErr
		}
		identity, identityErr = foldProjectIdentity(identity)
		if identityErr != nil {
			return nil, identityErr
		}
		identities = append(identities, identity)
	}
	slices.Sort(identities)
	return slices.Compact(identities), nil
}

func projectRegistrationUpdateMatches(
	current config.ProjectRegistration,
	next config.ProjectRegistration,
) bool {
	if current.Effective.Path != "" && next.Effective.Path != "" &&
		utils.PathKey(current.Effective.Path) == utils.PathKey(next.Effective.Path) {
		return true
	}
	currentIdentity, currentOK := repositoryurl.CanonicalRepositoryIdentity(
		current.Persisted.Repository,
	)
	nextIdentity, nextOK := repositoryurl.CanonicalRepositoryIdentity(
		next.Persisted.Repository,
	)
	if currentOK && nextOK {
		return repositoryurl.FoldRepositoryIdentity(currentIdentity) ==
			repositoryurl.FoldRepositoryIdentity(nextIdentity)
	}
	return current.Persisted.Repository != "" &&
		current.Persisted.Repository == next.Persisted.Repository
}

func acquireProjectIdentitySet(
	ctx context.Context,
	home string,
	identities []string,
) ([]func() error, error) {
	releases := make([]func() error, 0, len(identities))
	for _, identity := range identities {
		release, err := acquireProjectFence(ctx, home, identity)
		if err != nil {
			return nil, errors.Join(err, releaseProjectIdentitySet(releases))
		}
		releases = append(releases, release)
	}
	return releases, nil
}

func releaseProjectIdentitySet(releases []func() error) error {
	var result error
	for index := len(releases) - 1; index >= 0; index-- {
		result = errors.Join(result, releases[index]())
	}
	return result
}
