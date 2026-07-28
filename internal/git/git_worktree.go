package git

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gitworktree "go.kenn.io/kit/git/worktree"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

// ListWorktrees returns a list of all worktrees in the repository.
func (g *Git) ListWorktrees() ([]models.Worktree, error) {
	output, err := g.run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	entries := gitworktree.ParsePorcelain(output)
	worktrees := make([]models.Worktree, 0, len(entries))
	for _, entry := range entries {
		worktree := models.Worktree{
			Path: entry.Path, Branch: entry.Branch, CommitHash: entry.Head,
			Prunable: entry.Prunable,
		}
		if worktree.Branch == "" {
			worktree.Branch = g.getCurrentBranch(worktree.Path)
		}
		if info, statErr := os.Stat(worktree.Path); statErr == nil {
			worktree.CreatedAt = info.ModTime()
		}
		worktrees = append(worktrees, worktree)
	}

	if len(worktrees) > 0 {
		mainDir, err := g.getMainRepoRoot()
		if err == nil {
			for i := range worktrees {
				resolvedPath := worktrees[i].Path
				if resolved, err := filepath.EvalSymlinks(resolvedPath); err == nil {
					resolvedPath = resolved
				}
				if resolvedPath == mainDir {
					worktrees[i].IsMain = true
					break
				}
			}
		}
	}

	return worktrees, nil
}

// AddWorktree creates a new worktree.
func (g *Git) AddWorktree(path, branch string, createBranch bool) error {
	args := []string{"worktree", "add"}
	if createBranch {
		base, err := g.defaultWorktreeBase()
		if err != nil {
			return err
		}
		args = append(args, "-b", branch, path, base)
	} else {
		args = append(args, path, branch)
	}
	if _, err := g.run(args...); err != nil {
		return fmt.Errorf("failed to add worktree: %w", err)
	}
	return nil
}

// AddWorktreeExisting checks out an existing branch without allowing the
// checkout to run repository-configured hooks or filters, and without exposing
// protected credentials to Git.
func (g *Git) AddWorktreeExisting(
	path, branch string,
	protectedNames []string,
) error {
	isolationArgs, err := g.remoteCheckoutIsolationArgs(protectedNames)
	if err != nil {
		return err
	}
	hooksDir, err := os.MkdirTemp("", "kwt-empty-hooks-")
	if err != nil {
		return fmt.Errorf("create empty hooks directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(hooksDir) }()

	args := []string{"-c", "core.hooksPath=" + hooksDir}
	args = append(args, isolationArgs...)
	args = append(args, "worktree", "add", path, branch)
	if _, err := g.runWithoutCredentials(protectedNames, args...); err != nil {
		return fmt.Errorf("failed to add existing-branch worktree: %w", err)
	}
	return nil
}

// AddWorktreeTracking creates a local branch and worktree that track a
// specific remote branch.
func (g *Git) AddWorktreeTracking(
	path, branch, remoteBranch string,
	protectedNames []string,
) error {
	isolationArgs, err := g.remoteCheckoutIsolationArgs(protectedNames)
	if err != nil {
		return err
	}
	hooksDir, err := os.MkdirTemp("", "kwt-empty-hooks-")
	if err != nil {
		return fmt.Errorf("create empty hooks directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(hooksDir) }()
	hookIsolation := []string{"-c", "core.hooksPath=" + hooksDir}

	if _, err := g.runWithoutCredentials(
		protectedNames,
		append(
			append([]string(nil), hookIsolation...),
			"branch", "--track", branch, remoteBranch,
		)...,
	); err != nil {
		return fmt.Errorf(
			"failed to create branch tracking %s: %w",
			remoteBranch,
			err,
		)
	}
	worktreeArgs := append(
		append([]string(nil), hookIsolation...),
		isolationArgs...,
	)
	worktreeArgs = append(worktreeArgs, "worktree", "add", path, branch)
	if _, err := g.runWithoutCredentials(
		protectedNames,
		worktreeArgs...,
	); err != nil {
		if _, rollbackErr := g.runWithoutCredentials(
			protectedNames,
			append(
				append([]string(nil), hookIsolation...),
				"branch", "-D", branch,
			)...,
		); rollbackErr != nil {
			return fmt.Errorf(
				"failed to add worktree tracking %s: %w (failed to remove branch %s: %v)",
				remoteBranch,
				err,
				branch,
				rollbackErr,
			)
		}
		return fmt.Errorf(
			"failed to add worktree tracking %s: %w",
			remoteBranch,
			err,
		)
	}
	return nil
}

func (g *Git) remoteCheckoutIsolationArgs(
	protectedNames []string,
) ([]string, error) {
	output, err := g.runWithoutCredentials(
		protectedNames,
		"config", "--null", "--list",
	)
	if err != nil {
		return nil, fmt.Errorf("list configured checkout filters: %w", err)
	}
	drivers := make(map[string]bool)
	for record := range strings.SplitSeq(output, "\x00") {
		key, _, _ := strings.Cut(record, "\n")
		key = strings.TrimSpace(key)
		if !strings.HasPrefix(key, "filter.") {
			continue
		}
		rest := strings.TrimPrefix(key, "filter.")
		propertyAt := strings.LastIndex(rest, ".")
		if propertyAt <= 0 {
			continue
		}
		switch rest[propertyAt+1:] {
		case "smudge", "process", "required":
			drivers[rest[:propertyAt]] = true
		}
	}
	names := make([]string, 0, len(drivers))
	for driver := range drivers {
		names = append(names, driver)
	}
	sort.Strings(names)

	args := make([]string, 0, len(names)*6)
	for _, driver := range names {
		prefix := "filter." + driver + "."
		args = append(
			args,
			"-c", prefix+"smudge=cat",
			"-c", prefix+"process=",
			"-c", prefix+"required=false",
		)
	}
	return args, nil
}

func (g *Git) defaultWorktreeBase() (string, error) {
	remoteBase, remoteErr := g.remoteDefaultWorktreeBase()
	if remoteErr == nil {
		return remoteBase, nil
	}
	for _, branch := range []string{"main", "master"} {
		ref := "refs/heads/" + branch
		if g.refExists(ref) {
			return ref, nil
		}
	}
	root, rootErr := g.getMainRepoRoot()
	if rootErr == nil {
		output, branchErr := g.run(
			"-C", root, "symbolic-ref", "--quiet", "--short", "HEAD",
		)
		if branchErr == nil {
			ref := "refs/heads/" + strings.TrimSpace(output)
			if g.refExists(ref) {
				return ref, nil
			}
		}
	}
	return "", fmt.Errorf(
		"could not resolve default worktree base: remote default unavailable (%v); no local main, master, or primary worktree branch",
		remoteErr,
	)
}

func (g *Git) remoteDefaultWorktreeBase() (string, error) {
	const ref = "refs/kwt/origin/default"
	if _, err := g.run("fetch", "origin", "+HEAD:"+ref); err != nil {
		return "", fmt.Errorf("fetch origin default branch: %w", err)
	}
	if !g.refExists(ref) {
		return "", fmt.Errorf("fetched origin default ref does not exist")
	}
	return ref, nil
}

func (g *Git) refExists(ref string) bool {
	_, err := g.run("show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// AddWorktreeFromBase creates a new worktree with a branch from a specific base branch.
func (g *Git) AddWorktreeFromBase(path, branch, baseBranch string) error {
	args := []string{"worktree", "add", "-b", branch, path}
	if baseBranch != "" {
		args = append(args, baseBranch)
	}
	if _, err := g.run(args...); err != nil {
		return fmt.Errorf("failed to add worktree from base branch %s: %w", baseBranch, err)
	}
	return nil
}

// RemoveWorktree removes a worktree.
func (g *Git) RemoveWorktree(path string, force bool) error {
	canonicalPath := utils.CanonicalPath(path)
	registryGit := g
	if mainRoot, err := g.getMainRepoRoot(); err == nil {
		registryGit = New(mainRoot)
	}
	wasRegistered, _ := registryGit.hasRegisteredWorktree(canonicalPath)

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, err := g.run(args...); err != nil {
		stillRegistered, listErr := registryGit.hasRegisteredWorktree(canonicalPath)
		if wasRegistered && listErr == nil && !stillRegistered {
			if removeErr := os.RemoveAll(path); removeErr != nil {
				return fmt.Errorf(
					"failed to remove worktree: %w (Git deregistered the worktree but directory cleanup failed: %v)",
					err,
					removeErr,
				)
			}
			return nil
		}
		return fmt.Errorf("failed to remove worktree: %w", err)
	}
	return nil
}

func (g *Git) hasRegisteredWorktree(canonicalPath string) (bool, error) {
	output, err := g.run("worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, entry := range gitworktree.ParsePorcelain(output) {
		if utils.CanonicalPath(entry.Path) == canonicalPath {
			return true, nil
		}
	}
	return false, nil
}

// PruneWorktrees removes worktree information for deleted directories.
func (g *Git) PruneWorktrees() error {
	if _, err := g.run("worktree", "prune"); err != nil {
		return fmt.Errorf("failed to prune worktrees: %w", err)
	}
	return nil
}
