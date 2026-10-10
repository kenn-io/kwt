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

type RecoveryMode uint8

const (
	AdoptExistingOnly RecoveryMode = iota
	ReconstructRegistered
)

type RecoveryRequest struct {
	Path, Branch, ExpectedHead string
	Mode                       RecoveryMode
	Identity                   IdentityPolicy
}

func (r *Repository) Recover(ctx context.Context, req RecoveryRequest) (result CreateResult, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { result, err = s.Recover(ctx, req); return err })
	return result, err
}

// Recover never falls back to creating a checkout or branch. The caller's Branch
// is persisted ownership, not a name inferred from the worktree's current HEAD.
func (s *Scope) Recover(ctx context.Context, req RecoveryRequest) (result CreateResult, err error) {
	if err = s.check(ctx); err != nil {
		return result, err
	}
	if !filepath.IsAbs(req.Path) || req.Mode > ReconstructRegistered {
		return result, errors.New("invalid worktree recovery request")
	}
	if err = req.Identity.validate(); err != nil {
		return result, err
	}
	if err = s.rejectCreation(); err != nil {
		return result, err
	}
	info, statErr := os.Lstat(req.Path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return result, statErr
	}
	if statErr == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return result, errors.New("recovery requires an exact worktree directory")
	}
	if statErr != nil && req.Mode == AdoptExistingOnly {
		return result, ErrWorktreeNotFound
	}
	metadata, err := s.repo.registration(req.Path)
	if err != nil {
		return result, err
	}
	if pathKey(metadata) == pathKey(s.repo.commonDir) {
		return result, errors.New("cannot adopt the primary worktree")
	}
	head, err := fslink.ReadFile(filepath.Join(metadata, "HEAD"))
	if err != nil {
		return result, err
	}
	branch := strings.TrimPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
	if branch == strings.TrimSpace(string(head)) {
		branch = ""
	}
	if req.Branch != "" && branch != req.Branch {
		return result, fmt.Errorf("registered workspace HEAD does not match managed branch %q", req.Branch)
	}
	if req.ExpectedHead != "" {
		oid, readErr := s.repo.run(ctx, s.repo.path, "--git-dir="+metadata, "rev-parse", "--verify", "HEAD")
		if readErr != nil {
			return result, readErr
		}
		if strings.TrimSpace(string(oid)) != req.ExpectedHead {
			return result, &ConditionError{Reason: ReasonHeadChanged, Path: req.Path}
		}
	}
	result = CreateResult{Path: req.Path, CommonDir: s.repo.commonDir, Branch: branch, OwnedBranch: req.Branch, Disposition: Adopted}
	if statErr == nil {
		direct, readErr := ReadWorktreeBacklink(ctx, req.Path)
		if readErr != nil {
			return result, readErr
		}
		if pathKey(direct) != pathKey(metadata) {
			return result, ErrWorktreeRepositoryMismatch
		}
		actual, readErr := s.RunGit(ctx, req.Path, "rev-parse", "--show-toplevel")
		if readErr != nil {
			return result, readErr
		}
		if pathKey(strings.TrimSpace(string(actual))) != pathKey(req.Path) {
			return result, ErrWorktreeRepositoryMismatch
		}
	} else {
		if err = os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
			return result, err
		}
		if err = os.Mkdir(req.Path, 0o755); err != nil {
			return result, err
		}
		// Preserve the saved HEAD, index and reflog when materialization fails.
		// Only this attempt's newly created directory is eligible for removal.
		created, statErr := os.Lstat(req.Path)
		if statErr != nil {
			return result, statErr
		}
		// Windows defers loading identity until SameFile; capture before Git runs.
		if !os.SameFile(created, created) {
			return result, errors.New("cannot capture reconstructed directory identity")
		}
		defer func() {
			if err != nil {
				if current, e := os.Lstat(req.Path); e == nil && os.SameFile(created, current) {
					err = errors.Join(err, os.RemoveAll(req.Path))
				}
			}
		}()
		if err = os.WriteFile(filepath.Join(req.Path, ".git"), []byte("gitdir: "+metadata+"\n"), 0o644); err != nil {
			return result, err
		}
		if _, err = s.repo.run(ctx, req.Path, "checkout-index", "--all"); err != nil {
			return result, fmt.Errorf("restore missing workspace files: %w", err)
		}
		result.Disposition = Recovered
	}
	if req.Identity.FileName != "" {
		_, err = s.EnsureIdentity(ctx, req.Path, req.Identity)
	}
	return result, err
}
