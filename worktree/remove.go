package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	managed "go.kenn.io/kit/git/managed"
)

type RemovalAuthority uint8

const (
	ExactRegisteredPath RemovalAuthority = iota
	MatchingIdentity
)

type CleanupDisposition uint8

const (
	CleanupWorktree CleanupDisposition = iota
	PreserveSymlinkTarget
	MissingArtifacts
)

type RemovalRequest struct {
	Path                                                             string
	Identity                                                         IdentityPolicy
	Authority                                                        RemovalAuthority
	Force, IncludeIgnored, DeleteObservedBranch, ForceObservedBranch bool
	// DeferNativePreflight lets Git enforce its ordinary dirty/submodule rules
	// during cleanup, after Claim, instead of before admission. Explicit
	// RequireClean conditions still run before admission; Force is unchanged.
	DeferNativePreflight bool
	Conditions           *RemovalConditions
	Branches             []managed.BranchRemoval
	// Claim wraps repeatable preflight and at-most-once removal. Both closures
	// expire when Claim returns, even if its outer repository scope remains held.
	Claim func(context.Context, func() error, func() (RemovalResult, error)) (bool, error)
}

type RemovalResult struct {
	managed.RemoveWorktreeResult
	Branch      string
	Claimed     bool
	Disposition CleanupDisposition
}

type RemovalCheck struct {
	Dirty       bool
	Disposition CleanupDisposition
	Entry       Entry
}

func (r *Repository) InspectRemoval(ctx context.Context, req RemovalRequest) (check RemovalCheck, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { check, err = s.InspectRemoval(ctx, req); return err })
	return check, err
}

// InspectRemoval reports dirty state separately from identity/structural errors.
// Force skips ordinary dirty inspection; an explicit RequireClean still applies.
func (s *Scope) InspectRemoval(ctx context.Context, req RemovalRequest) (RemovalCheck, error) {
	check := RemovalCheck{Disposition: CleanupWorktree}
	if err := s.check(ctx); err != nil {
		return check, err
	}
	if !filepath.IsAbs(req.Path) || req.Authority > MatchingIdentity || req.DeleteObservedBranch && len(req.Branches) != 0 {
		return check, errors.New("invalid worktree removal request")
	}
	if err := s.rejectCreation(); err != nil {
		return check, err
	}
	if req.Authority == MatchingIdentity {
		if err := req.Identity.validate(); err != nil {
			return check, err
		}
		if req.Identity.FileName == "" || req.Identity.Generate {
			return check, errors.New("removal requires an explicit identity")
		}
	}
	if req.Conditions != nil && (req.Conditions.RepositoryIdentity != "" || req.Conditions.UpstreamRepository != "") && req.Conditions.MatchRepositoryIdentity == nil {
		return check, errors.New("repository identity conditions require a comparison policy")
	}
	info, statErr := os.Lstat(req.Path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return check, statErr
	}
	inventory, err := s.inspect(ctx, false)
	if err != nil {
		return check, err
	}
	found := false
	for _, entry := range inventory.Entries {
		if comparableWorktreePath(entry.Path) != comparableWorktreePath(req.Path) {
			continue
		}
		if found {
			return check, errors.New("multiple worktree registrations match removal path")
		}
		found = true
		check.Entry = entry
	}
	if !found {
		if errors.Is(statErr, os.ErrNotExist) && req.Authority == ExactRegisteredPath && req.Conditions == nil {
			check.Disposition = MissingArtifacts
			return check, nil
		}
		return check, ErrWorktreeNotFound
	}
	entry := check.Entry
	if entry.IsMain {
		return check, &ConditionError{Reason: ReasonMainWorktree, Path: req.Path}
	}
	if entry.Locked {
		return check, &ConditionError{Reason: ReasonLocked, Path: req.Path}
	}
	if req.Conditions != nil {
		if err := s.validateRemovalConditions(ctx, req.Path, entry, *req.Conditions); err != nil {
			return check, err
		}
	}
	if entry.GitDirError != "" {
		return check, &IncompleteInventoryError{Path: req.Path, Err: entry.registrationError}
	}
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		if req.Authority != ExactRegisteredPath {
			return check, errors.New("worktree identity does not authorize a symlink")
		}
		check.Disposition = PreserveSymlinkTarget
	} else if statErr == nil && !info.IsDir() {
		return check, errors.New("worktree path is not a directory")
	}
	if req.Authority == MatchingIdentity {
		value, err := s.ReadIdentity(ctx, req.Path, req.Identity.FileName)
		if err != nil || value != req.Identity.Value {
			return check, &ConditionError{Reason: ReasonGenerationChanged, Path: req.Path}
		}
	}
	if entry.Exists {
		// A registration alone does not authorize a directory now owned by
		// another checkout, even when a stale generation file still exists.
		actual, err := s.repo.run(ctx, req.Path, "rev-parse", "--absolute-git-dir")
		if err != nil {
			return check, err
		}
		if pathKey(strings.TrimSpace(string(actual))) != pathKey(entry.GitDir) {
			return check, &ConditionError{Reason: ReasonBacklinkChanged, Path: req.Path}
		}
		if !req.Force || req.Conditions != nil && req.Conditions.RequireClean {
			args := []string{"status", "--porcelain", "--untracked-files=normal"}
			if req.IncludeIgnored || req.Conditions != nil && req.Conditions.IncludeIgnored {
				args = append(args, "--ignored")
			}
			status, err := s.repo.run(ctx, req.Path, args...)
			if err != nil {
				return check, err
			}
			check.Dirty = strings.TrimSpace(string(status)) != ""
		}
	}
	return check, nil
}

func (s *Scope) validateRemovalConditions(ctx context.Context, path string, entry Entry, conditions RemovalConditions) error {
	if conditions.ExpectedGitDir != "" && (pathKey(entry.DotGitTarget) != pathKey(conditions.ExpectedGitDir) || pathKey(entry.GitDir) != pathKey(conditions.ExpectedGitDir)) {
		return &ConditionError{Reason: ReasonBacklinkChanged, Path: path}
	}
	if conditions.Generation != "" && (ValidateWorktreeGeneration(conditions.Generation) != nil || entry.GenerationStatus != GenerationValid || entry.Generation != conditions.Generation) {
		return &ConditionError{Reason: ReasonGenerationChanged, Path: path}
	}
	if conditions.Head != "" && entry.Head != conditions.Head {
		return &ConditionError{Reason: ReasonHeadChanged, Path: path}
	}
	if conditions.Branch != "" && entry.Branch != conditions.Branch {
		return &ConditionError{Reason: ReasonBranchChanged, Path: path}
	}
	if conditions.RepositoryIdentity != "" {
		remote, err := s.repo.run(ctx, path, "remote", "get-url", "origin")
		if err != nil || !conditions.MatchRepositoryIdentity(strings.TrimSpace(string(remote)), conditions.RepositoryIdentity) {
			return &ConditionError{Reason: ReasonRepositoryChanged, Path: path}
		}
	}
	if conditions.UpstreamRepository != "" || conditions.UpstreamBranch != "" {
		remote, err := s.repo.run(ctx, path, "config", "--get", "branch."+entry.Branch+".remote")
		if err != nil {
			return &ConditionError{Reason: ReasonUpstreamRepositoryChanged, Path: path}
		}
		merge, err := s.repo.run(ctx, path, "config", "--get", "branch."+entry.Branch+".merge")
		if err != nil {
			return &ConditionError{Reason: ReasonUpstreamRepositoryChanged, Path: path}
		}
		branch, valid := strings.CutPrefix(strings.TrimSpace(string(merge)), "refs/heads/")
		if !valid || branch == "" {
			return &ConditionError{Reason: ReasonUpstreamRepositoryChanged, Path: path}
		}
		if conditions.UpstreamRepository != "" {
			url, err := s.repo.run(ctx, path, "remote", "get-url", strings.TrimSpace(string(remote)))
			if err != nil || !conditions.MatchRepositoryIdentity(strings.TrimSpace(string(url)), conditions.UpstreamRepository) {
				return &ConditionError{Reason: ReasonUpstreamRepositoryChanged, Path: path}
			}
		}
		if conditions.UpstreamBranch != "" && branch != conditions.UpstreamBranch {
			return &ConditionError{Reason: ReasonUpstreamBranchChanged, Path: path}
		}
	}
	return nil
}

func (r *Repository) Remove(ctx context.Context, req RemovalRequest) (result RemovalResult, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { result, err = s.Remove(ctx, req); return err })
	return result, err
}

func (s *Scope) Remove(ctx context.Context, req RemovalRequest) (result RemovalResult, err error) {
	if req.Conditions != nil {
		copied := *req.Conditions
		req.Conditions = &copied
	}
	req.Branches = slices.Clone(req.Branches)
	check, err := s.removalPreflight(ctx, req)
	result.Disposition = check.Disposition
	result.Branch = check.Entry.Branch
	result.Remaining = managed.RollbackResult{Path: req.Path, Registration: check.Entry.GitDir, Unverified: err != nil}
	for _, branch := range req.Branches {
		result.BranchesRemaining = append(result.BranchesRemaining, branch.Name)
	}
	if req.DeleteObservedBranch && check.Entry.Branch != "" && !check.Entry.Detached {
		result.BranchesRemaining = append(result.BranchesRemaining, check.Entry.Branch)
	}
	if len(result.BranchesRemaining) > 0 {
		result.Remaining.Branch = result.BranchesRemaining[0]
	}
	if err != nil {
		return result, err
	}
	active, invoked := true, false
	defer func() { active = false }()
	preflight := func() error {
		if !active {
			return errors.New("worktree removal claim has expired")
		}
		_, err := s.removalPreflight(ctx, req)
		return err
	}
	remove := func() error {
		if !active || invoked {
			return errors.New("worktree removal may run only once inside its claim")
		}
		invoked = true
		check, err := s.removalPreflight(ctx, req)
		if err != nil {
			result.Remaining.Unverified = true
			return err
		}
		result.Disposition = check.Disposition
		result.Branch = check.Entry.Branch
		if check.Disposition == PreserveSymlinkTarget {
			return nil
		}
		if check.Disposition == MissingArtifacts {
			result.CheckoutRemoved, result.RegistrationRemoved = true, true
			result.Remaining.Path, result.Remaining.Registration = "", ""
			return s.removeRequestedBranches(ctx, req.Branches, &result)
		}
		branch := check.Entry.Branch
		if check.Entry.Detached {
			branch = ""
		}
		branches := req.Branches
		if req.DeleteObservedBranch && branch != "" {
			branches = []managed.BranchRemoval{{Name: branch, ExpectedOID: check.Entry.Head, Force: req.ForceObservedBranch}}
		}
		result.RemoveWorktreeResult, err = managed.RemoveWorktreeFromDisk(ctx, managed.RemoveWorktreeOptions{ProjectRoot: s.repo.path, Path: req.Path, Branch: branch, Force: req.Force, Branches: branches, Runner: s.repo.runner, RunGit: s.repo.runGit})
		if err != nil && result.RegistrationRemoved {
			if !result.CheckoutRemoved {
				return fmt.Errorf("worktree removed, but files remain at %s: %w", req.Path, err)
			}
			if len(result.BranchesRemaining) > 0 {
				return fmt.Errorf("worktree removed but failed to delete branch: %w", err)
			}
		}
		return err
	}
	if req.Claim != nil {
		result.Claimed, err = req.Claim(ctx, preflight, func() (RemovalResult, error) {
			cleanupErr := remove()
			return result, cleanupErr
		})
	} else {
		result.Claimed = true
		err = remove()
	}
	return result, err
}

// removeRequestedBranches applies requested branch cleanup when the checkout
// and its registration were already gone, so Git's removal never ran.
func (s *Scope) removeRequestedBranches(ctx context.Context, branches []managed.BranchRemoval, result *RemovalResult) error {
	for _, branch := range branches {
		absent, err := s.RemoveBranch(ctx, branch)
		if absent {
			result.BranchesRemoved = append(result.BranchesRemoved, branch.Name)
			result.BranchesRemaining = slices.DeleteFunc(result.BranchesRemaining, func(name string) bool { return name == branch.Name })
			result.Remaining.Branch = ""
			if len(result.BranchesRemaining) > 0 {
				result.Remaining.Branch = result.BranchesRemaining[0]
			}
		}
		if err != nil {
			return fmt.Errorf("worktree already removed but failed to delete branch %s: %w", branch.Name, err)
		}
	}
	return nil
}

func (s *Scope) removalPreflight(ctx context.Context, req RemovalRequest) (RemovalCheck, error) {
	preflightReq := req
	if req.DeferNativePreflight {
		preflightReq.Force = true
	}
	check, err := s.InspectRemoval(ctx, preflightReq)
	if err != nil {
		return check, err
	}
	if check.Dirty {
		return check, &ConditionError{Reason: ReasonDirty, Path: req.Path}
	}
	if !req.DeferNativePreflight && !req.Force && check.Entry.Exists && check.Disposition == CleanupWorktree {
		out, err := s.repo.run(ctx, req.Path, "submodule", "status", "--recursive")
		if err != nil {
			return check, fmt.Errorf("inspect worktree submodules: %w", err)
		}
		for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
			if line != "" && line[0] != '-' {
				return check, &ConditionError{Reason: ReasonInitializedSubmodule, Path: req.Path}
			}
		}
	}
	return check, nil
}
