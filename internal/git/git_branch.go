package git

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kwt/pkg/models"
)

// ListBranches returns a list of all branches.
func (g *Git) ListBranches(includeRemote bool) ([]models.Branch, error) {
	args := []string{
		"for-each-ref",
		"--format=%(refname)%00%(HEAD)%00%(committerdate:iso)%00%(objectname)%00%(subject)%00%(authorname)",
		"refs/heads",
	}
	if includeRemote {
		args = append(args, "refs/remotes")
	}

	output, err := g.run(args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	var branches []models.Branch
	lines := strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n")

	for line := range lines {
		if line == "" {
			continue
		}

		parts := strings.Split(line, "\x00")
		if len(parts) < 6 {
			continue
		}

		fullRef := parts[0]
		var name, source string
		var isRemote bool
		switch {
		case strings.HasPrefix(fullRef, "refs/heads/"):
			name = strings.TrimPrefix(fullRef, "refs/heads/")
			source = name
		case strings.HasPrefix(fullRef, "refs/remotes/"):
			name = strings.TrimPrefix(fullRef, "refs/remotes/")
			source = fullRef
			isRemote = true
		default:
			continue
		}
		isCurrent := parts[1] == "*"
		dateStr := parts[2]
		hash := parts[3]
		message := parts[4]
		author := parts[5]

		date, _ := time.Parse("2006-01-02 15:04:05 -0700", dateStr)

		branches = append(branches, models.Branch{
			Name:      name,
			Source:    source,
			IsCurrent: isCurrent,
			IsRemote:  isRemote,
			LastCommit: models.CommitInfo{
				Hash:    hash,
				Message: message,
				Author:  author,
				Date:    date,
			},
		})
	}

	return branches, nil
}

// ListAvailableBranches returns branches that are not already checked out in
// any worktree. Remote refs are normalized to the local branch name that a new
// tracking worktree will use, while Source retains the exact remote ref.
func (g *Git) ListAvailableBranches() ([]models.Branch, error) {
	branches, err := g.ListBranches(true)
	if err != nil {
		return nil, err
	}
	remotes, err := g.remoteNames()
	if err != nil {
		return nil, err
	}
	worktrees, err := g.ListWorktrees()
	if err != nil {
		return nil, fmt.Errorf("failed to list checked out branches: %w", err)
	}

	checkedOut := make(map[string]bool, len(worktrees))
	for _, worktree := range worktrees {
		if worktree.Branch != "" && worktree.Branch != "HEAD" {
			checkedOut[worktree.Branch] = true
		}
	}

	local := make(map[string]bool)
	available := make([]models.Branch, 0, len(branches))
	for _, branch := range branches {
		if branch.IsRemote {
			continue
		}
		local[branch.Name] = true
		if checkedOut[branch.Name] {
			continue
		}
		branch.Source = branch.Name
		available = append(available, branch)
	}
	for _, branch := range branches {
		if !branch.IsRemote {
			continue
		}
		name, ok := remoteBranchName(branch.Source, remotes)
		if !ok || name == "HEAD" ||
			local[name] || checkedOut[name] {
			continue
		}
		branch.Name = name
		available = append(available, branch)
	}
	return available, nil
}

func (g *Git) remoteNames() ([]string, error) {
	output, err := g.run("remote")
	if err != nil {
		return nil, fmt.Errorf("failed to list remotes: %w", err)
	}
	var remotes []string
	for remote := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		remote = strings.TrimSpace(remote)
		if remote != "" {
			remotes = append(remotes, remote)
		}
	}
	sort.Slice(remotes, func(i, j int) bool {
		return len(remotes[i]) > len(remotes[j])
	})
	return remotes, nil
}

func remoteBranchName(source string, remotes []string) (string, bool) {
	const prefix = "refs/remotes/"
	ref, ok := strings.CutPrefix(source, prefix)
	if !ok {
		return "", false
	}
	for _, remote := range remotes {
		if name, found := strings.CutPrefix(ref, remote+"/"); found && name != "" {
			return name, true
		}
	}
	return "", false
}

// DeleteBranch deletes a branch.
func (g *Git) DeleteBranch(branch string, force bool) error {
	args := []string{"branch"}
	if force {
		args = append(args, "-D")
	} else {
		args = append(args, "-d")
	}
	args = append(args, branch)

	if _, err := g.run(args...); err != nil {
		return fmt.Errorf("failed to delete branch %s: %w", branch, err)
	}

	return nil
}

// getCurrentBranch returns the current branch name for a specific worktree.
func (g *Git) getCurrentBranch(worktreePath string) string {
	oldWorkDir := g.workDir
	g.workDir = worktreePath
	defer func() { g.workDir = oldWorkDir }()

	output, err := g.run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}
