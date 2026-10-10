package worktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	managed "go.kenn.io/kit/git/managed"
)

type CreateSerialization uint8

const (
	CreateSerialized CreateSerialization = iota
	CreateHookReentrant
)

// CheckoutCandidate is attempted only if every earlier candidate failed without
// acquiring artifacts. Numbered chooses one available suffix from 2 through 999.
type CheckoutCandidate struct {
	Branch   string
	Mode     managed.CheckoutMode
	Numbered bool
}

type CreateRequest struct {
	Git           managed.CreateWorktreeOptions
	Candidates    []CheckoutCandidate
	Identity      IdentityPolicy
	Serialization CreateSerialization
}

type ImportRequest struct {
	Git        managed.MergeRequestWorktreeOptions
	Candidates []CheckoutCandidate
	Identity   IdentityPolicy
}

type Disposition uint8

const (
	Created Disposition = iota
	Adopted
	Recovered
)

// CreateResult reports artifacts separately from the private evidence used for
// rollback. Editing these report fields never changes cleanup authority.
type CreateResult struct {
	Path, CommonDir, Branch, OwnedBranch string
	// IdentityValue is the marker value captured before creation synchronization ends.
	IdentityValue string
	Disposition   Disposition
	acquired      *acquisition
}

type acquisition struct {
	repo         *Repository
	git          managed.CreateWorktreeResult
	identity     IdentityPolicy
	ready        bool
	preserve     bool
	registration string
}

func (r *Repository) Create(ctx context.Context, req CreateRequest) (result CreateResult, err error) {
	if req.Serialization == CreateHookReentrant {
		return r.createReentrant(ctx, req)
	}
	err = r.WithLock(ctx, func(s *Scope) error {
		result, err = s.Create(ctx, req)
		return err
	})
	return result, err
}

func (s *Scope) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := s.check(ctx); err != nil {
		return CreateResult{}, err
	}
	if req.Serialization != CreateSerialized {
		return CreateResult{}, errors.New("held worktree scopes require serialized creation")
	}
	if err := s.rejectCreation(); err != nil {
		return CreateResult{}, err
	}
	result, err := s.repo.create(ctx, req)
	if err != nil {
		return result, err
	}
	return s.finalize(ctx, result)
}

func (r *Repository) create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	opts := req.Git
	if err := r.validateCreation(opts.ProjectRoot, opts.SetupScript, req.Identity); err != nil {
		return CreateResult{}, err
	}
	opts.ProjectRoot, opts.Runner, opts.RunGit = r.path, r.runner, r.runGit
	opts.FailureCleanup = managed.CleanupDeferred
	return r.attemptCandidates(ctx, req.Candidates, CheckoutCandidate{Branch: opts.Branch, Mode: opts.Mode}, req.Identity, func(candidate CheckoutCandidate) (managed.CreateWorktreeResult, error) {
		opts.Branch, opts.Mode = candidate.Branch, candidate.Mode
		return managed.CreateWorktreeOnDisk(ctx, opts)
	})
}

func (r *Repository) Import(ctx context.Context, req ImportRequest) (result CreateResult, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { result, err = s.Import(ctx, req); return err })
	return result, err
}

func (s *Scope) Import(ctx context.Context, req ImportRequest) (CreateResult, error) {
	if err := s.check(ctx); err != nil {
		return CreateResult{}, err
	}
	if err := s.rejectCreation(); err != nil {
		return CreateResult{}, err
	}
	opts := req.Git
	if err := s.repo.validateCreation(opts.ProjectRoot, opts.SetupScript, req.Identity); err != nil {
		return CreateResult{}, err
	}
	opts.ProjectRoot, opts.Runner, opts.RunGit = s.repo.path, s.repo.runner, s.repo.runGit
	opts.FailureCleanup = managed.CleanupDeferred
	mode := opts.Mode
	if mode == managed.CheckoutAuto {
		mode = managed.CheckoutNewBranch
	}
	result, err := s.repo.attemptCandidates(ctx, req.Candidates, CheckoutCandidate{Branch: opts.Branch, Mode: mode}, req.Identity, func(candidate CheckoutCandidate) (managed.CreateWorktreeResult, error) {
		opts.Branch, opts.Mode = candidate.Branch, candidate.Mode
		return managed.CreateWorktreeFromMergeRequest(ctx, opts)
	})
	if err != nil {
		return result, err
	}
	return s.finalize(ctx, result)
}

func (r *Repository) validateCreation(root, script string, identity IdentityPolicy) error {
	if root != "" && pathKey(root) != pathKey(r.path) {
		return ErrWorktreeRepositoryMismatch
	}
	if script != "" {
		return errors.New("prepare and run lifecycle scripts outside the worktree scope")
	}
	return identity.validate()
}

func (r *Repository) attemptCandidates(ctx context.Context, candidates []CheckoutCandidate, initial CheckoutCandidate, identity IdentityPolicy, attempt func(CheckoutCandidate) (managed.CreateWorktreeResult, error)) (CreateResult, error) {
	if len(candidates) == 0 {
		candidates = []CheckoutCandidate{initial}
	}
	var last error
	for _, candidate := range candidates {
		if candidate.Numbered {
			name, err := r.selectNumberedBranch(ctx, candidate.Branch)
			if err != nil {
				if errors.Is(err, managed.ErrBranchAlreadyExists) {
					last = err
					continue
				}
				return CreateResult{}, err
			}
			candidate.Branch = name
		}
		if candidate.Mode == managed.CheckoutNewBranch {
			refs, err := r.branchRefs(ctx)
			if err != nil {
				return CreateResult{}, err
			}
			if available, _ := branchAvailable(refs, candidate.Branch); !available {
				last = fmt.Errorf("%w: %s", managed.ErrBranchAlreadyExists, candidate.Branch)
				continue
			}
		}
		created, err := attempt(candidate)
		if err != nil && created.Path == "" && !created.BranchCreated && (errors.Is(err, managed.ErrBranchAlreadyExists) || errors.Is(err, managed.ErrBranchInUse)) {
			last = err
			continue
		}
		result := CreateResult{Path: created.Path, Branch: created.Branch, CommonDir: r.commonDir, Disposition: Created,
			acquired: &acquisition{repo: r, git: created, identity: identity, ready: identity.FileName == ""}}
		if created.Path != "" {
			// This is a report, not authority; Kit retains the acquisition proof.
			result.acquired.registration, _ = r.registration(created.Path)
		}
		if created.BranchCreated {
			result.OwnedBranch = created.Branch
		}
		return result, err
	}
	return CreateResult{}, last
}

func (s *Scope) finalize(ctx context.Context, result CreateResult) (CreateResult, error) {
	if err := s.configureBareLinked(ctx, result.Path); err != nil {
		return result, err
	}
	a := result.acquired
	if a.identity.FileName == "" {
		return result, nil
	}
	value, err := s.EnsureIdentity(ctx, a.git.Path, a.identity)
	if err != nil {
		a.preserve = true
		return result, fmt.Errorf("%w: %w", ErrIdentityUnavailable, err)
	}
	a.identity.Value, a.identity.Generate, a.ready = value, false, true
	result.IdentityValue = value
	return result, nil
}

func (s *Scope) rejectCreation() error {
	reserved, err := s.activeCreation()
	if err != nil {
		return err
	}
	if reserved != "" {
		return errors.New("worktree creation in progress")
	}
	return nil
}

// Rollback takes a fresh scope, including when a caller's registry transaction
// fails after the original creation scope returned.
func (r CreateResult) Rollback(ctx context.Context, policy managed.RollbackPolicy) (remaining managed.RollbackResult, err error) {
	if r.acquired == nil {
		return managed.RollbackResult{Unverified: true}, managed.ErrWorktreeCleanupIncomplete
	}
	remaining = r.remaining()
	err = r.acquired.repo.WithLock(ctx, func(s *Scope) error { remaining, err = s.Rollback(ctx, r, policy); return err })
	return remaining, err
}

func (r CreateResult) remaining() managed.RollbackResult {
	if r.acquired == nil {
		return managed.RollbackResult{Unverified: true}
	}
	a := r.acquired
	remaining := managed.RollbackResult{Path: a.git.Path, Registration: a.registration, Unverified: !a.ready}
	if a.git.BranchCreated {
		remaining.Branch = a.git.Branch
	}
	return remaining
}

func (s *Scope) Rollback(ctx context.Context, result CreateResult, policy managed.RollbackPolicy) (managed.RollbackResult, error) {
	remaining := result.remaining()
	if err := s.check(ctx); err != nil {
		return remaining, err
	}
	a := result.acquired
	if a == nil || pathKey(a.repo.commonDir) != pathKey(s.repo.commonDir) ||
		pathKey(filepath.Join(a.repo.lockDir, a.repo.coordinator.policy.FileName)) != pathKey(filepath.Join(s.repo.lockDir, s.repo.coordinator.policy.FileName)) {
		return remaining, errors.Join(managed.ErrWorktreeCleanupIncomplete, ErrWorktreeRepositoryMismatch)
	}
	if err := s.rejectCreation(); err != nil {
		return remaining, errors.Join(managed.ErrWorktreeCleanupIncomplete, err)
	}
	if a.preserve {
		return remaining, fmt.Errorf("%w: acquired worktree identity was not established", managed.ErrWorktreeCleanupIncomplete)
	}
	if a.ready && a.identity.FileName != "" {
		value, err := s.ReadIdentity(ctx, a.git.Path, a.identity.FileName)
		if err != nil || value != a.identity.Value {
			remaining.Unverified = true
			return remaining, errors.Join(managed.ErrWorktreeCleanupIncomplete, errors.New("worktree identity changed"), err)
		}
	}
	return a.git.Rollback(ctx, policy)
}

func (s *Scope) SetUpstream(ctx context.Context, opts managed.WorktreeUpstreamOptions) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if opts.ProjectRoot != "" && pathKey(opts.ProjectRoot) != pathKey(s.repo.path) {
		return ErrWorktreeRepositoryMismatch
	}
	if _, err := s.repo.registration(opts.Path); err != nil {
		return err
	}
	opts.ProjectRoot, opts.Runner, opts.RunGit = s.repo.path, s.repo.runner, s.repo.runGit
	return managed.SetWorktreeUpstream(ctx, opts)
}
