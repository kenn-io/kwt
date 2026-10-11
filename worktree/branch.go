package worktree

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	managed "go.kenn.io/kit/git/managed"
)

type BranchNameRequest struct {
	Branch   string
	Suffixes []string
	Start    int
}

type BranchNameResult struct {
	Branch string
	Next   int
}

// SelectBranchName selects without creating a ref; the application persists its
// own reservation before asynchronous setup.
func (r *Repository) SelectBranchName(ctx context.Context, req BranchNameRequest) (result BranchNameResult, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { result, err = s.SelectBranchName(ctx, req); return err })
	return result, err
}

func (s *Scope) SelectBranchName(ctx context.Context, req BranchNameRequest) (BranchNameResult, error) {
	if err := s.check(ctx); err != nil {
		return BranchNameResult{}, err
	}
	return s.repo.selectBranch(ctx, req)
}

func (r *Repository) branchRefs(ctx context.Context) ([]string, error) {
	out, err := r.run(ctx, r.path, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

func branchAvailable(refs []string, branch string) (available, ancestorConflict bool) {
	want := "refs/heads/" + branch
	for _, ref := range refs {
		if strings.HasPrefix(want, ref+"/") {
			return false, true
		}
		if ref == want || strings.HasPrefix(ref, want+"/") {
			return false, false
		}
	}
	return true, false
}

func (r *Repository) selectBranch(ctx context.Context, req BranchNameRequest) (BranchNameResult, error) {
	if req.Start < 0 || req.Start >= len(req.Suffixes) {
		return BranchNameResult{}, errors.New("branch suffix start is out of range")
	}
	if _, err := r.run(ctx, r.path, "check-ref-format", "--branch", req.Branch); err != nil {
		return BranchNameResult{}, errors.Join(managed.ErrInvalidBranchName, err)
	}
	refs, err := r.branchRefs(ctx)
	if err != nil {
		return BranchNameResult{}, err
	}
	_, ancestor := branchAvailable(refs, req.Branch)
	for index := req.Start; index < len(req.Suffixes); index++ {
		if err := ctx.Err(); err != nil {
			return BranchNameResult{}, err
		}
		suffix := req.Suffixes[index]
		parts := strings.Split(req.Branch, "/")
		candidates := []string{req.Branch + "-" + suffix}
		if ancestor {
			for i := len(parts) - 2; i >= 0; i-- {
				candidate := slices.Clone(parts)
				candidate[i] += "-" + suffix
				candidates = append(candidates, strings.Join(candidate, "/"))
			}
		}
		for _, candidate := range candidates {
			if available, _ := branchAvailable(refs, candidate); available {
				if _, err := r.run(ctx, r.path, "check-ref-format", "--branch", candidate); err != nil {
					return BranchNameResult{}, errors.Join(managed.ErrInvalidBranchName, err)
				}
				return BranchNameResult{Branch: candidate, Next: index + 1}, nil
			}
		}
	}
	return BranchNameResult{}, fmt.Errorf("%w: no available name derived from %q", managed.ErrBranchAlreadyExists, req.Branch)
}

func (r *Repository) selectNumberedBranch(ctx context.Context, branch string) (string, error) {
	suffixes := make([]string, 998)
	for i := range suffixes {
		suffixes[i] = strconv.Itoa(i + 2)
	}
	result, err := r.selectBranch(ctx, BranchNameRequest{Branch: branch, Suffixes: suffixes})
	return result.Branch, err
}

// BranchNameAvailable checks both exact names and parent/child ref conflicts.
func (s *Scope) BranchNameAvailable(ctx context.Context, branch string) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	refs, err := s.repo.branchRefs(ctx)
	if err != nil {
		return false, err
	}
	available, _ := branchAvailable(refs, branch)
	return available, nil
}

// RemoveBranch uses the caller's explicit branch authority after registration
// cleanup, while respecting active creation reservations and Git's branch rules.
func (s *Scope) RemoveBranch(ctx context.Context, branch managed.BranchRemoval) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	if err := s.rejectCreation(); err != nil {
		return false, err
	}
	return managed.RemoveBranch(ctx, managed.RemoveBranchOptions{ProjectRoot: s.repo.path, Branch: branch, Runner: s.repo.runner, RunGit: s.repo.runGit})
}
