package git

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kwt/pkg/models"
)

// ListBranches returns a list of all branches.
func (g *Git) ListBranches(includeRemote bool) ([]models.Branch, error) {
	args := []string{"branch", "-v", "--format=%(refname)|%(refname:short)|%(HEAD)|%(committerdate:iso)|%(objectname)|%(subject)|%(authorname)"}
	if includeRemote {
		args = append(args, "-a")
	}

	output, err := g.run(args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	var branches []models.Branch
	lines := strings.SplitSeq(strings.TrimSpace(output), "\n")

	for line := range lines {
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 7 {
			continue
		}

		fullRef := parts[0]
		name := parts[1]
		isCurrent := parts[2] == "*"
		dateStr := parts[3]
		hash := parts[4]
		message := parts[5]
		author := parts[6]

		isRemote := strings.HasPrefix(fullRef, "refs/remotes/")

		date, _ := time.Parse("2006-01-02 15:04:05 -0700", dateStr)

		branches = append(branches, models.Branch{
			Name:      name,
			Source:    name,
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
		remote, name, ok := strings.Cut(branch.Name, "/")
		if !ok || remote == "" || name == "" || name == "HEAD" ||
			local[name] || checkedOut[name] {
			continue
		}
		branch.Name = name
		branch.Source = remote + "/" + name
		available = append(available, branch)
	}
	return available, nil
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
