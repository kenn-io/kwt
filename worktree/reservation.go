package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
	managed "go.kenn.io/kit/git/managed"
)

type creationReservation struct {
	lock     *flock.Flock
	path     string
	released bool
}

func (s *Scope) reserve(path string) (*creationReservation, error) {
	policy := s.repo.coordinator.policy
	lock := flock.New(filepath.Join(s.repo.lockDir, policy.CreationLockName), flock.SetPermissions(0o600))
	// Waiting here would deadlock a native hook trying to list worktrees.
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("worktree creation already in progress")
	}
	record := filepath.Join(s.repo.lockDir, policy.CreationPathName)
	if err := atomicfile.WriteFile(record, []byte(path+"\n"), atomicfile.WithPrivate()); err != nil {
		return nil, errors.Join(err, lock.Unlock())
	}
	return &creationReservation{lock: lock, path: record}, nil
}

func (r *creationReservation) release() error {
	if r.released {
		return nil
	}
	r.released = true
	err := os.Remove(r.path)
	if os.IsNotExist(err) {
		err = nil
	}
	return errors.Join(err, r.lock.Unlock())
}

func (r *Repository) createReentrant(ctx context.Context, req CreateRequest) (result CreateResult, err error) {
	if err := r.validateCreation(req.Git.ProjectRoot, req.Git.SetupScript, req.Identity); err != nil {
		return result, err
	}
	candidate := CheckoutCandidate{Branch: req.Git.Branch, Mode: req.Git.Mode}
	if len(req.Candidates) == 1 {
		candidate = req.Candidates[0]
	}
	if len(req.Candidates) > 1 || candidate.Numbered || candidate.Mode != managed.CheckoutNewBranch || req.Git.Checkout != managed.CheckoutTrusted || req.Git.Path == "" || r.coordinator.policy.CreationLockName == "" {
		return result, errors.New("hook-reentrant creation requires one trusted new branch, an explicit path, and creation reservation filenames")
	}
	path, err := filepath.Abs(req.Git.Path)
	if err != nil {
		return result, err
	}
	var reservation *creationReservation
	if err := r.WithLock(ctx, func(s *Scope) error { var err error; reservation, err = s.reserve(path); return err }); err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, reservation.release()) }()
	result, err = r.create(ctx, req)
	if err != nil {
		return result, err
	}
	entered := false
	err = r.WithLock(ctx, func(s *Scope) error {
		entered = true
		// Release under mutation exclusion so no other checkout can reserve
		// this path between removing the reservation and writing its identity.
		if err := reservation.release(); err != nil {
			return fmt.Errorf("release creation reservation: %w", err)
		}
		var err error
		result, err = s.finalize(ctx, result)
		return err
	})
	if err != nil && !entered && req.Identity.FileName != "" {
		result.acquired.preserve = true
		err = errors.Join(ErrIdentityUnavailable, err)
	}
	return result, err
}
