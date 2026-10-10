package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/fslink"
	"golang.org/x/sync/semaphore"
)

type repositoryLock struct {
	local *semaphore.Weighted
	file  *flock.Flock
}

func (c *Coordinator) lock(path string) *repositoryLock {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lock := c.locks[path]; lock != nil {
		return lock
	}
	lock := &repositoryLock{local: semaphore.NewWeighted(1), file: flock.New(path, flock.SetPermissions(0o600))}
	c.locks[path] = lock
	return lock
}

// WithLock holds the existing on-disk mutation lock until fn returns.
func (r *Repository) WithLock(ctx context.Context, fn func(*Scope) error) error {
	if fn == nil {
		return errors.New("worktree scope callback is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.coordinator.withFileLock(ctx, filepath.Join(r.lockDir, r.coordinator.policy.FileName), func() error {
		scope := &Scope{repo: r}
		scope.active.Store(true)
		defer scope.active.Store(false)
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(scope)
	})
}

// activeCreation never waits: a checkout hook can be waiting for this scope.
func (s *Scope) activeCreation() (string, error) {
	policy := s.repo.coordinator.policy
	if policy.CreationLockName == "" {
		return "", nil
	}
	lock := flock.New(filepath.Join(s.repo.lockDir, policy.CreationLockName), flock.SetPermissions(0o600))
	available, err := lock.TryLock()
	if err != nil {
		return "", fmt.Errorf("inspect worktree creation: %w", err)
	}
	record := filepath.Join(s.repo.lockDir, policy.CreationPathName)
	if available {
		removeErr := os.Remove(record)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return "", errors.Join(removeErr, lock.Unlock())
	}
	data, err := fslink.ReadFile(record)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("worktree creation in progress: %w", errors.Join(errors.New("reservation unavailable"), err))
	}
	return strings.TrimSpace(string(data)), nil
}

func (c *Coordinator) withFileLock(ctx context.Context, path string, fn func() error) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock := c.lock(path)
	if err = lock.local.Acquire(ctx, 1); err != nil {
		return err
	}
	defer lock.local.Release(1)
	locked, err := lock.file.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock worktree mutations: %w", err)
	}
	if !locked {
		return fmt.Errorf("lock worktree mutations: %w", ctx.Err())
	}
	defer func() { err = errors.Join(err, lock.file.Unlock()) }()
	if err = ctx.Err(); err != nil {
		return err
	}
	return fn()
}
