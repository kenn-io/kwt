// Package worktree coordinates Git workspaces without owning application state.
// Callers select lock placement, execution policy, and registration markers.
package worktree

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
)

// LockPolicy preserves the caller's on-disk coordination protocol.
type LockPolicy struct {
	FileName, NonBareRoot, CreationLockName, CreationPathName string
	ResolveCommonDirSymlinks, BareUsesSuppliedPath            bool
}

// Coordinator shares cancellable repository gates among its callers.
type Coordinator struct {
	policy LockPolicy
	mu     sync.Mutex
	locks  map[string]*repositoryLock
}

// RepositoryOptions supplies the repository and all Git execution policy.
// RunGit, when present, takes precedence over Runner.
type RepositoryOptions struct {
	Path   string
	Runner gitcmd.Runner
	RunGit managed.GitRunner
}

// Repository holds repository paths and execution settings, never a context.
type Repository struct {
	coordinator *Coordinator
	path        string
	commonDir   string
	lockDir     string
	bare        bool
	runner      gitcmd.Runner
	runGit      managed.GitRunner
}

// Scope permits synchronous operations under one repository lock. It expires
// when WithLock returns; callers must not retain it or use it concurrently.
type Scope struct {
	repo   *Repository
	active atomic.Bool
}

// NewCoordinator validates lock names before any files are touched.
func NewCoordinator(policy LockPolicy) (*Coordinator, error) {
	if !validFileName(policy.FileName) {
		return nil, errors.New("worktree mutation lock must be a filename")
	}
	if (policy.CreationLockName == "") != (policy.CreationPathName == "") {
		return nil, errors.New("worktree creation lock and path filenames must be provided together")
	}
	if policy.CreationLockName != "" && (!validFileName(policy.CreationLockName) ||
		!validFileName(policy.CreationPathName) || policy.CreationLockName == policy.CreationPathName ||
		policy.CreationLockName == policy.FileName || policy.CreationPathName == policy.FileName) {
		return nil, errors.New("worktree lock and reservation filenames must be distinct")
	}
	return &Coordinator{policy: policy, locks: make(map[string]*repositoryLock)}, nil
}

func validFileName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name &&
		!strings.ContainsAny(name, "/\\\x00")
}

// Open resolves the existing repository without changing its configuration.
func (c *Coordinator) Open(ctx context.Context, opts RepositoryOptions) (*Repository, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("worktree repository path is required")
	}
	r := &Repository{coordinator: c, path: opts.Path, runner: opts.Runner, runGit: opts.RunGit}
	// One rev-parse answers both questions; Git prints them in option order.
	out, err := r.run(ctx, opts.Path, "rev-parse", "--git-common-dir", "--is-bare-repository")
	if err != nil {
		if _, statErr := os.Stat(opts.Path); os.IsNotExist(statErr) {
			return nil, errors.Join(ErrWorktreeNotFound, err)
		}
		return nil, fmt.Errorf("resolve worktree repository: %w", err)
	}
	common, bare, ok := strings.Cut(strings.TrimRight(string(out), "\r\n"), "\n")
	r.commonDir = strings.TrimSpace(common)
	bare = strings.TrimSpace(bare)
	if !ok || r.commonDir == "" || (bare != "true" && bare != "false") {
		return nil, fmt.Errorf("unexpected rev-parse output for %s: %q", opts.Path, out)
	}
	if !filepath.IsAbs(r.commonDir) {
		r.commonDir = filepath.Join(opts.Path, r.commonDir)
	}
	if c.policy.ResolveCommonDirSymlinks {
		if resolved, resolveErr := filepath.EvalSymlinks(r.commonDir); resolveErr == nil {
			r.commonDir = resolved
		}
	}
	r.bare = bare == "true"
	r.lockDir = r.commonDir
	if r.bare && c.policy.BareUsesSuppliedPath {
		r.lockDir = opts.Path
	} else if !r.bare && c.policy.NonBareRoot != "" {
		common, resolveErr := r.revParseAbsolute(ctx, opts.Path, "--git-common-dir")
		if resolveErr != nil {
			return nil, resolveErr
		}
		r.lockDir = filepath.Join(c.policy.NonBareRoot, fmt.Sprintf("%x", sha256.Sum256([]byte(common[0]))))
	}
	return r, nil
}

// revParseAbsolute returns one absolute path per rev-parse option. Git before
// 2.31 echoes --path-format=absolute as an unknown argument and prints relative
// paths, which would otherwise be used as if they were absolute.
func (r *Repository) revParseAbsolute(ctx context.Context, dir string, options ...string) ([]string, error) {
	out, err := r.run(ctx, dir, append([]string{"rev-parse", "--path-format=absolute"}, options...)...)
	if err != nil {
		return nil, err
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(paths) != len(options) {
		return nil, fmt.Errorf("absolute Git paths require Git 2.31 or newer: unexpected rev-parse output %q", out)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("absolute Git paths require Git 2.31 or newer: rev-parse returned %q", path)
		}
	}
	return paths, nil
}

func (r *Repository) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.runGit != nil {
		return r.runGit(ctx, r.runner, dir, args...)
	}
	return r.runner.Output(ctx, dir, args...)
}

func (s *Scope) check(ctx context.Context) error {
	if s == nil || !s.active.Load() {
		return errors.New("worktree scope has expired")
	}
	return ctx.Err()
}

// RunGit runs caller-owned repository configuration while the scope is held.
// An empty directory selects the opened repository. Other directories must
// belong to that same repository.
func (s *Scope) RunGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if dir == "" {
		dir = s.repo.path
	}
	common, err := s.repo.run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(string(common))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if pathKey(path) != pathKey(s.repo.commonDir) {
		return nil, ErrWorktreeRepositoryMismatch
	}
	return s.repo.run(ctx, dir, args...)
}
