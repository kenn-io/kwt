package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/fslink"
)

// Registration describes the exact recorded path without adopting a replacement
// checkout. Symlink paths never authorize removal of their target or metadata.
type Registration struct {
	GitDir, Identity      string
	Exists, Live, Symlink bool
}

func (s *Scope) InspectRegistration(ctx context.Context, path string) (Registration, error) {
	if err := s.check(ctx); err != nil {
		return Registration{}, err
	}
	return s.repo.ObserveRegistration(ctx, path, "")
}

// ObserveRegistration reads an exact linked registration and an optional identity
// marker without locking or writing. It supports repository selection before a
// Scope is acquired; mutations must revalidate through the held Scope.
// Missing or malformed markers have an empty Identity.
func (r *Repository) ObserveRegistration(ctx context.Context, path, identityFile string) (Registration, error) {
	var state Registration
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if identityFile != "" && !validFileName(identityFile) {
		return state, errors.New("identity filename is invalid")
	}
	if !filepath.IsAbs(path) {
		return state, errors.New("registration path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	state.Exists = err == nil
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		state.Symlink = true
		return state, nil
	}
	matches, err := r.linkedRegistrations(path)
	if err != nil {
		return state, err
	}
	if len(matches) > 1 {
		return state, errors.New("multiple worktree registrations match path")
	}
	if len(matches) == 0 {
		return state, nil
	}
	state.GitDir = matches[0]
	if identityFile != "" {
		marker := filepath.Join(state.GitDir, identityFile)
		info, err := os.Lstat(marker)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return state, err
		}
		if err == nil && info.Mode().IsRegular() && info.Size() <= 256 {
			data, err := fslink.ReadFile(marker)
			if err != nil {
				return state, err
			}
			state.Identity = strings.TrimSpace(string(data))
		}
	}
	if info == nil || !info.IsDir() {
		return state, nil
	}
	backlink, err := ReadWorktreeBacklink(ctx, path)
	if err != nil {
		return state, err
	}
	if backlink == "" || pathKey(backlink) != pathKey(state.GitDir) {
		return state, nil
	}
	inside, err := r.run(ctx, path, "rev-parse", "--is-inside-work-tree")
	if IsCheckoutAbsent(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if strings.TrimSpace(string(inside)) != "true" {
		return state, nil
	}
	out, err := r.run(ctx, path, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if IsCheckoutAbsent(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	state.Live = len(paths) == 3 && pathKey(paths[0]) == pathKey(path) && pathKey(paths[1]) == pathKey(state.GitDir) && pathKey(paths[2]) == pathKey(r.commonDir)
	return state, nil
}

// PruneRegistration removes only a stale administrative directory for path.
// It never removes checkout files, branches, or other missing registrations.
func (s *Scope) PruneRegistration(ctx context.Context, path string) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	if err := s.rejectCreation(); err != nil {
		return false, err
	}
	state, err := s.InspectRegistration(ctx, path)
	if err != nil {
		return false, err
	}
	if state.Live {
		return false, errors.New("cannot prune a live worktree registration")
	}
	if state.GitDir == "" {
		return false, nil
	}
	if _, err := os.Lstat(filepath.Join(state.GitDir, "locked")); err == nil {
		return false, &ConditionError{Reason: ReasonLocked, Path: path}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.RemoveAll(state.GitDir); err != nil {
		return false, fmt.Errorf("remove stale worktree registration: %w", err)
	}
	return true, nil
}
