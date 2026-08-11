package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/service"
)

type ProjectClaim struct {
	Registration config.ProjectRegistration
	Identity     string
	Expansion    ExpansionContext
}

func acquireProjectFence(
	ctx context.Context,
	home string,
	identity string,
) (func() error, error) {
	identity, err := foldProjectIdentity(identity)
	if err != nil {
		return nil, service.NewError(service.InvalidRequest, err.Error(), false, nil, err)
	}
	digest := sha256.Sum256([]byte(identity))
	return acquireProjectLock(
		ctx,
		home,
		hex.EncodeToString(digest[:])+".lock",
	)
}

func acquireProjectTransitionFence(
	ctx context.Context,
	home string,
) (func() error, error) {
	return acquireProjectLock(ctx, home, "registry.lock")
}

func acquireProjectLock(
	ctx context.Context,
	home string,
	name string,
) (func() error, error) {
	dir := filepath.Join(home, "project-locks")
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("secure project lock directory: %w", err)
	}
	lock := flock.New(
		filepath.Join(dir, name),
		flock.SetPermissions(0o600),
	)
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	if !locked {
		return nil, errors.Join(fmt.Errorf("project lifecycle lock unavailable"), ctx.Err())
	}
	return lock.Unlock, nil
}

func ObserveProjectClaim(
	ctx context.Context,
	home string,
	effectiveMainPath string,
	expansion ExpansionContext,
) (*ProjectClaim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := expansion.validate(); err != nil {
		return nil, err
	}
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(home, expansion.expandPath)
	if err != nil {
		return nil, err
	}
	var match *config.ProjectRegistration
	for index := range snapshot.Projects {
		candidate := &snapshot.Projects[index]
		if utils.PathKey(candidate.Effective.Path) != utils.PathKey(effectiveMainPath) {
			continue
		}
		if match != nil {
			return nil, service.NewError(
				service.UnregistrationFailed,
				"multiple project registrations own the repository",
				false, nil, nil,
			)
		}
		match = candidate
	}
	if match == nil {
		return nil, nil
	}
	identity, err := resolveProjectIdentity(
		ctx, *match, credentials.ProtectedNames(snapshot.Config)...,
	)
	if err != nil {
		return nil, service.NewError(service.Internal, "internal failure", false, nil, err)
	}
	return &ProjectClaim{
		Registration: *match,
		Identity:     identity,
		Expansion:    expansion,
	}, nil
}

func AcquireProjectClaim(
	ctx context.Context,
	home string,
	claim *ProjectClaim,
) (func() error, error) {
	return acquireProjectClaim(ctx, home, claim, false)
}

func AcquireRequiredProjectClaim(
	ctx context.Context,
	home string,
	claim *ProjectClaim,
) (func() error, error) {
	return acquireProjectClaim(ctx, home, claim, true)
}

func acquireProjectClaim(
	ctx context.Context,
	home string,
	claim *ProjectClaim,
	required bool,
) (func() error, error) {
	if claim == nil {
		if required {
			return nil, registrationChanged(nil)
		}
		return func() error { return nil }, nil
	}
	releaseTransition, err := acquireProjectTransitionFence(ctx, home)
	if err != nil {
		return nil, err
	}
	matched, matchErr := projectClaimMatches(ctx, home, claim)
	if matchErr != nil || !matched {
		_ = releaseTransition()
		return nil, registrationChanged(matchErr)
	}
	release, err := acquireProjectFence(ctx, home, claim.Identity)
	if err != nil {
		_ = releaseTransition()
		return nil, err
	}
	matched, matchErr = projectClaimMatches(ctx, home, claim)
	transitionErr := releaseTransition()
	if matchErr == nil && matched && transitionErr == nil {
		return release, nil
	}
	_ = release()
	return nil, registrationChanged(errors.Join(matchErr, transitionErr))
}

func projectClaimMatches(
	ctx context.Context,
	home string,
	claim *ProjectClaim,
) (bool, error) {
	current, err := config.LoadGlobalSnapshotAtWithExpansion(
		home,
		claim.Expansion.expandPath,
	)
	if err != nil {
		return false, err
	}
	for _, candidate := range current.Projects {
		if !candidate.SamePersistedEntry(claim.Registration) {
			continue
		}
		identity, identityErr := resolveProjectIdentity(
			ctx, candidate, credentials.ProtectedNames(current.Config)...,
		)
		if identityErr != nil {
			return false, identityErr
		}
		return EqualProjectIdentity(identity, claim.Identity), nil
	}
	return false, nil
}

func registrationChanged(cause error) error {
	return service.NewError(
		service.RegistrationChanged,
		"the project registration changed before the operation began",
		true, nil, cause,
	)
}
