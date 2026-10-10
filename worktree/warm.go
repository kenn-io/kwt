package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/fslink"
	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
)

// WarmRequest describes an application-selected spare and its existing marker
// and pool-lock names. IdentityFile disqualifies checkouts already assigned to
// an application workspace. Revision is resolved when the spare is prepared.
type WarmRequest struct {
	Path, PoolLockName, MarkerFile, IdentityFile, Revision string
}
type WarmClaimRequest struct {
	Warm   WarmRequest
	Create CreateRequest
}

func (req WarmRequest) validate() error {
	if !filepath.IsAbs(req.Path) || !validFileName(req.PoolLockName) || !validFileName(req.MarkerFile) || req.IdentityFile != "" && (!validFileName(req.IdentityFile) || req.IdentityFile == req.MarkerFile) || req.Revision == "" {
		return errors.New("invalid warm worktree request")
	}
	return nil
}

// PrepareWarm takes the pool lock before the repository lock. Materialization
// holds only the pool lock so foreground operations can continue during reset.
func (r *Repository) PrepareWarm(ctx context.Context, req WarmRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	pool := filepath.Join(filepath.Dir(req.Path), req.PoolLockName)
	if pathKey(pool) == pathKey(filepath.Join(r.lockDir, r.coordinator.policy.FileName)) {
		return errors.New("warm pool lock must differ from repository lock")
	}
	return r.coordinator.withFileLock(ctx, pool, func() error {
		var metadata string
		fill := false
		err := r.WithLock(ctx, func(s *Scope) error {
			if err := s.rejectCreation(); err != nil {
				return err
			}
			if _, err := os.Lstat(req.Path); err == nil {
				state, dir, err := s.warmState(ctx, req)
				if err != nil {
					return err
				}
				if state != "preparing" {
					return nil
				}
				metadata = dir
				if err := os.Remove(filepath.Join(dir, "index.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			} else {
				// Only the exact missing checkout's stale registration is removed.
				dir, err := r.registration(req.Path)
				if err != nil && !errors.Is(err, ErrWorktreeNotFound) {
					return err
				}
				if err == nil {
					if pathKey(dir) == pathKey(r.commonDir) {
						return errors.New("warm path is the primary worktree")
					}
					if err = os.RemoveAll(dir); err != nil {
						return err
					}
				}
				// Finish Git's shared registration write even if shutdown cancels the
				// caller. The lock reason makes interrupted preparation recognizable.
				registrationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, err = s.Create(registrationCtx, CreateRequest{Git: managed.CreateWorktreeOptions{Path: req.Path, Mode: managed.CheckoutDetached, BaseRef: req.Revision, NoCheckout: true, LockReason: req.MarkerFile}})
				cancel()
				if err != nil {
					return err
				}
				if err = ctx.Err(); err != nil {
					return err
				}
				metadata, err = r.registration(req.Path)
				if err != nil {
					return err
				}
			}
			if err := s.configureBareLinked(ctx, req.Path); err != nil {
				return err
			}
			if err := atomicfile.WriteFile(filepath.Join(metadata, req.MarkerFile), []byte("preparing\n"), atomicfile.WithPrivate()); err != nil {
				return err
			}
			reason, err := fslink.ReadFile(filepath.Join(metadata, "locked"))
			if err == nil && strings.TrimSpace(string(reason)) == req.MarkerFile {
				if _, err = r.run(ctx, r.path, "worktree", "unlock", req.Path); err != nil {
					return err
				}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			fill = true
			return nil
		})
		if err != nil || !fill {
			return err
		}
		if _, err = r.run(ctx, req.Path, "-c", "submodule.recurse=false", "reset", "--hard", "HEAD"); err != nil {
			return err
		}
		return r.WithLock(ctx, func(s *Scope) error {
			state, current, err := s.warmState(ctx, req)
			if err != nil {
				return err
			}
			if state != "preparing" || pathKey(current) != pathKey(metadata) {
				return errors.New("warm checkout changed during preparation")
			}
			return atomicfile.WriteFile(filepath.Join(metadata, req.MarkerFile), []byte("ready\n"), atomicfile.WithPrivate())
		})
	})
}

func (s *Scope) warmState(ctx context.Context, req WarmRequest) (string, string, error) {
	if err := s.check(ctx); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(req.Path)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", nil
	}
	metadata, err := s.repo.registration(req.Path)
	if errors.Is(err, ErrWorktreeNotFound) || errors.Is(err, ErrWorktreeRepositoryMismatch) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if pathKey(metadata) == pathKey(s.repo.commonDir) {
		return "", "", nil
	}
	direct, err := ReadWorktreeBacklink(ctx, req.Path)
	if err != nil {
		return "", "", err
	}
	if pathKey(direct) != pathKey(metadata) {
		return "", "", nil
	}
	if req.IdentityFile != "" {
		if _, err := os.Lstat(filepath.Join(metadata, req.IdentityFile)); !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
	}
	head, err := fslink.ReadFile(filepath.Join(metadata, "HEAD"))
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(strings.TrimSpace(string(head)), "ref: ") {
		return "", "", nil
	}
	state, err := fslink.ReadFile(filepath.Join(metadata, req.MarkerFile))
	if errors.Is(err, os.ErrNotExist) {
		reason, err := fslink.ReadFile(filepath.Join(metadata, "locked"))
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		if err != nil || strings.TrimSpace(string(reason)) != req.MarkerFile {
			return "", "", err
		}
		return "preparing", metadata, nil
	}
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(state)), metadata, nil
}

// ClaimWarm holds only the repository lock. It never waits on the pool lock,
// and treats absent, preparing, changed, or unsupported spares as cache misses.
func (r *Repository) ClaimWarm(ctx context.Context, req WarmClaimRequest) (result CreateResult, claimed bool, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { result, claimed, err = s.ClaimWarm(ctx, req); return err })
	return result, claimed, err
}
func (s *Scope) ClaimWarm(ctx context.Context, req WarmClaimRequest) (CreateResult, bool, error) {
	if err := s.check(ctx); err != nil {
		return CreateResult{}, false, err
	}
	if err := req.Warm.validate(); err != nil {
		return CreateResult{}, false, err
	}
	if err := s.rejectCreation(); err != nil {
		return CreateResult{}, false, err
	}
	opts := req.Create.Git
	if err := s.repo.validateCreation(opts.ProjectRoot, opts.SetupScript, req.Create.Identity); err != nil {
		return CreateResult{}, false, err
	}
	if req.Create.Serialization != CreateSerialized || len(req.Create.Candidates) != 0 || opts.Checkout != managed.CheckoutTrusted || opts.NoCheckout || opts.LockReason != "" || (opts.Mode != managed.CheckoutNewBranch && opts.Mode != managed.CheckoutExistingBranch && opts.Mode != managed.CheckoutDetached) {
		return CreateResult{}, false, nil
	}
	if !filepath.IsAbs(opts.Path) || comparableWorktreePath(opts.Path) == comparableWorktreePath(req.Warm.Path) {
		return CreateResult{}, false, errors.New("warm claim requires a distinct absolute destination")
	}
	state, metadata, err := s.warmState(ctx, req.Warm)
	if err != nil || state != "ready" {
		return CreateResult{}, false, err
	}
	dirty, err := s.repo.run(ctx, req.Warm.Path, "status", "--porcelain", "--untracked-files=all", "--ignored")
	if err != nil || strings.TrimSpace(string(dirty)) != "" {
		return CreateResult{}, false, err
	}
	if opts.Mode != managed.CheckoutNewBranch {
		args := []string{"-c", "submodule.recurse=false", "checkout", "--no-guess"}
		if opts.Mode == managed.CheckoutDetached {
			ref := opts.BaseRef
			if ref == "" {
				ref = req.Warm.Revision
			}
			oid, exists, err := s.repo.refOID(ctx, ref)
			if err != nil {
				return CreateResult{}, false, err
			}
			if !exists {
				return CreateResult{}, false, fmt.Errorf("warm checkout revision %q is missing", ref)
			}
			args = append(args, "--detach", oid)
		} else {
			if opts.BaseRef != "" {
				return CreateResult{}, false, managed.ErrInvalidWorktreeOptions
			}
			if _, err := s.repo.run(ctx, s.repo.path, "check-ref-format", "--branch", opts.Branch); err != nil {
				return CreateResult{}, false, errors.Join(managed.ErrInvalidBranchName, err)
			}
			if _, exists, err := s.repo.refOID(ctx, "refs/heads/"+opts.Branch); err != nil {
				return CreateResult{}, false, err
			} else if !exists {
				return CreateResult{}, false, managed.ErrBranchNotFound
			}
			args = append(args, opts.Branch)
		}
		if _, err = s.repo.run(ctx, req.Warm.Path, args...); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			branch, branchErr := s.currentBranch(cleanupCtx, req.Warm.Path)
			if branchErr != nil {
				return s.preservedWarmResult(req.Warm.Path, metadata, ""), true, errors.Join(err, branchErr)
			}
			removed, removeErr := managed.RemoveWorktreeFromDisk(cleanupCtx, managed.RemoveWorktreeOptions{ProjectRoot: s.repo.path, Path: req.Warm.Path, Branch: branch, Force: true, Runner: s.repo.runner, RunGit: s.repo.runGit})
			return s.preservedWarmResult(removed.Remaining.Path, removed.Remaining.Registration, branch), true, errors.Join(err, removeErr)
		}

	}
	move := managed.MoveWorktreeOptions{ProjectRoot: s.repo.path, Source: req.Warm.Path, Path: opts.Path, Runner: s.repo.runner, RunGit: s.repo.runGit}
	if opts.Mode == managed.CheckoutNewBranch {
		move.NewBranch, move.BaseRef = opts.Branch, opts.BaseRef
	}
	moved, moveErr := managed.MoveWorktreeOnDisk(ctx, move)
	if moveErr != nil && moved.Path == "" {
		if opts.Mode == managed.CheckoutNewBranch {
			return CreateResult{}, false, moveErr
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, detachErr := s.repo.run(cleanupCtx, req.Warm.Path, "-c", "submodule.recurse=false", "checkout", "--detach")
		return CreateResult{}, true, errors.Join(moveErr, detachErr)
	}
	result := CreateResult{Path: moved.Path, CommonDir: s.repo.commonDir, Branch: moved.Branch, Disposition: Created, acquired: &acquisition{repo: s.repo, git: moved, identity: req.Create.Identity, ready: req.Create.Identity.FileName == ""}}
	if moved.BranchCreated {
		result.OwnedBranch = moved.Branch
	}
	result.acquired.registration, _ = s.repo.registration(moved.Path)
	if moveErr != nil {
		return result, true, moveErr
	}
	if err = s.SetUpstream(ctx, managed.WorktreeUpstreamOptions{Path: moved.Path, Policy: opts.Upstream}); err != nil {
		return result, true, err
	}
	result, err = s.finalize(ctx, result)
	return result, true, err
}

func (s *Scope) currentBranch(ctx context.Context, path string) (string, error) {
	out, err := s.repo.run(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if gitcmd.IsExitCode(err, 1) {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}

// Bare repositories retain their shared core.bare value. Once worktree config
// is enabled, each linked checkout needs its own override to remain usable.
func (s *Scope) configureBareLinked(ctx context.Context, path string) error {
	for _, key := range []string{"core.bare", "extensions.worktreeConfig"} {
		out, err := s.repo.run(ctx, s.repo.commonDir, "config", "--bool", key)
		if gitcmd.IsExitCode(err, 1) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "true" {
			return nil
		}
	}
	metadata, err := s.repo.registration(path)
	if err != nil {
		return err
	}
	if _, err = s.repo.run(ctx, s.repo.commonDir, "config", "--file", filepath.Join(metadata, "config.worktree"), "core.bare", "false"); err != nil {
		return fmt.Errorf("configure linked checkout: %w", err)
	}
	return nil
}

// Failed warm cleanup is a report, never new authority to remove the spare.
func (s *Scope) preservedWarmResult(path, registration, branch string) CreateResult {
	return CreateResult{Path: path, CommonDir: s.repo.commonDir, Branch: branch, acquired: &acquisition{repo: s.repo, git: managed.CreateWorktreeResult{Path: path, Branch: branch}, registration: registration, preserve: true}}
}
