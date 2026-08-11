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
	identity, err := validateStableProjectIdentity(identity)
	if err != nil {
		return nil, service.NewError(service.InvalidRequest, err.Error(), false, nil, err)
	}
	dir := filepath.Join(home, "project-locks")
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("secure project lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(identity))
	lock := flock.New(
		filepath.Join(dir, hex.EncodeToString(digest[:])+".lock"),
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
	identity, err := stableProjectIdentity(*match)
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
	if claim == nil {
		return func() error { return nil }, nil
	}
	release, err := acquireProjectFence(ctx, home, claim.Identity)
	if err != nil {
		return nil, err
	}
	current, err := config.LoadGlobalSnapshotAtWithExpansion(
		home,
		claim.Expansion.expandPath,
	)
	if err == nil {
		for _, candidate := range current.Projects {
			if !candidate.SamePersistedEntry(claim.Registration) {
				continue
			}
			identity, identityErr := stableProjectIdentity(candidate)
			if identityErr == nil && identity == claim.Identity {
				return release, nil
			}
		}
	}
	_ = release()
	return nil, service.NewError(
		service.RegistrationChanged,
		"the project registration changed before the operation began",
		true, nil, err,
	)
}
