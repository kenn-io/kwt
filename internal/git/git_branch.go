package git

import (
	"fmt"
	"os"
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
			Label:     name,
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
	fetchRefspecs, err := g.remoteFetchRefspecs()
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
		name, ok := remoteBranchName(branch.Source, fetchRefspecs)
		if !ok || name == "HEAD" ||
			local[name] || checkedOut[name] {
			continue
		}
		branch.Name = name
		branch.Label = remoteBranchLabel(name, branch.Source)
		available = append(available, branch)
	}
	return available, nil
}

func remoteBranchLabel(name string, source string) string {
	source = strings.TrimPrefix(source, "refs/remotes/")
	return fmt.Sprintf("%s (%s)", name, source)
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

type remoteFetchRefspec struct {
	source      string
	destination string
}

func (g *Git) remoteFetchRefspecs() ([]remoteFetchRefspec, error) {
	remotes, err := g.remoteNames()
	if err != nil {
		return nil, err
	}
	var refspecs []remoteFetchRefspec
	for _, remote := range remotes {
		output, configErr := g.run(
			"config",
			"--get-all",
			"remote."+remote+".fetch",
		)
		if configErr != nil {
			continue
		}
		for value := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
			value = strings.TrimSpace(value)
			value = strings.TrimPrefix(value, "+")
			if value == "" || strings.HasPrefix(value, "^") {
				continue
			}
			source, destination, ok := strings.Cut(value, ":")
			if !ok || source == "" || destination == "" {
				continue
			}
			refspecs = append(refspecs, remoteFetchRefspec{
				source:      source,
				destination: destination,
			})
		}
	}
	return refspecs, nil
}

func remoteBranchName(
	destination string,
	refspecs []remoteFetchRefspec,
) (string, bool) {
	for _, refspec := range refspecs {
		source, ok := refspec.sourceForDestination(destination)
		if !ok {
			continue
		}
		name, ok := strings.CutPrefix(source, "refs/heads/")
		if ok && name != "" {
			return name, true
		}
	}
	return "", false
}

func (r remoteFetchRefspec) sourceForDestination(
	destination string,
) (string, bool) {
	star := strings.IndexByte(r.destination, '*')
	if star < 0 {
		return r.source, destination == r.destination
	}
	if strings.Count(r.destination, "*") != 1 ||
		strings.Count(r.source, "*") != 1 {
		return "", false
	}
	prefix := r.destination[:star]
	suffix := r.destination[star+1:]
	if !strings.HasPrefix(destination, prefix) ||
		!strings.HasSuffix(destination, suffix) ||
		len(destination) < len(prefix)+len(suffix) {
		return "", false
	}
	match := destination[len(prefix) : len(destination)-len(suffix)]
	return strings.Replace(r.source, "*", match, 1), true
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

// DeleteBranchIsolated force-deletes a branch without exposing protected
// credentials or allowing repository-configured hooks to run.
func (g *Git) DeleteBranchIsolated(
	branch string,
	protectedNames []string,
) error {
	if err := g.validateLocalBranchName(branch, protectedNames); err != nil {
		return err
	}
	hooksDir, err := os.MkdirTemp("", "kwt-empty-hooks-")
	if err != nil {
		return fmt.Errorf("create empty hooks directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(hooksDir) }()

	isolationArgs, err := g.checkoutIsolationArgs(
		protectedNames,
		"",
		hooksDir,
	)
	if err != nil {
		return err
	}
	args := append([]string(nil), isolationArgs...)
	args = append(args, "branch", "-D", "--", branch)
	if _, err := g.runWithoutCredentials(protectedNames, args...); err != nil {
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
