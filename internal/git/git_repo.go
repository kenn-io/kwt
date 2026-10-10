package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	gitworktree "go.kenn.io/kit/git/worktree"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

// GetRepositoryName returns the name of the repository.
func (g *Git) GetRepositoryName() (string, error) {
	rootDir, err := g.getRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Base(rootDir), nil
}

// GetRepositoryPath returns the root path of the git repository.
func (g *Git) GetRepositoryPath() (string, error) {
	return g.getRootDir()
}

// GetMainRepositoryPath returns the root path of the main repository.
// Unlike GetRepositoryPath, this always returns the main repo root even when
// called from inside a worktree. A bare repository returns its bare root,
// or the main checkout beside a conventional .bare control directory.
func (g *Git) GetMainRepositoryPath() (string, error) {
	return g.getMainRepoRoot()
}

// GetBareContainerPath returns the parent directory shared by a conventional
// bare-container repository's worktrees. It returns an empty path for other
// repository layouts.
func (g *Git) GetBareContainerPath() (string, error) {
	commonDirOutput, err := g.run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}
	commonDir := strings.TrimSpace(commonDirOutput)
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(g.workDir, commonDir)
	}
	commonDir = utils.CanonicalPath(commonDir)

	output, err := g.run("worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("failed to list repository worktrees: %w", err)
	}
	entries := gitworktree.ParsePorcelain(output)
	if _, ok := bareContainerAnchor(entries, commonDir); !ok {
		return "", nil
	}
	return utils.CanonicalPath(filepath.Dir(commonDir)), nil
}

// GetMainRepositoryPathWithoutCredentials resolves the main repository while
// removing the named credentials from Git's environment.
func (g *Git) GetMainRepositoryPathWithoutCredentials(
	protectedNames []string,
) (string, error) {
	return g.getMainRepoRootWithoutCredentials(protectedNames)
}

// GetRepositoryURL returns the remote origin URL of the repository.
func (g *Git) GetRepositoryURL() (string, error) {
	output, err := g.run("remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("failed to get repository URL: %w", err)
	}
	return strings.TrimSpace(output), nil
}

// GetRecentCommits returns recent commits for a specific path.
func (g *Git) GetRecentCommits(path string, limit int) ([]models.CommitInfo, error) {
	oldWorkDir := g.workDir
	g.workDir = path
	defer func() { g.workDir = oldWorkDir }()

	args := []string{"log", fmt.Sprintf("-%d", limit), "--pretty=format:%H|%s|%an|%ai"}
	output, err := g.run(args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get recent commits: %w", err)
	}

	var commits []models.CommitInfo
	lines := strings.SplitSeq(strings.TrimSpace(output), "\n")

	for line := range lines {
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}

		date, _ := time.Parse("2006-01-02 15:04:05 -0700", parts[3])

		commits = append(commits, models.CommitInfo{
			Hash:    parts[0],
			Message: parts[1],
			Author:  parts[2],
			Date:    date,
		})
	}

	return commits, nil
}

// getMainRepoRoot returns the main repository root directory using git-common-dir.
// This works correctly from both the main repo and worktrees.
func (g *Git) getMainRepoRoot() (string, error) {
	return g.getMainRepoRootWithoutCredentials(nil)
}

func (g *Git) getMainRepoRootWithoutCredentials(
	protectedNames []string,
) (string, error) {
	ctx := g.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	repo, err := g.WorktreeRepository(ctx, protectedNames)
	if err != nil {
		return "", err
	}
	return repo.PrimaryPath(ctx)
}

func bareContainerAnchor(
	entries []gitworktree.PorcelainEntry,
	commonDir string,
) (string, bool) {
	if filepath.Base(commonDir) != ".bare" ||
		len(entries) == 0 || !entries[0].Bare ||
		utils.PathKey(entries[0].Path) != utils.PathKey(commonDir) {
		return "", false
	}
	anchor := utils.CanonicalPath(filepath.Join(filepath.Dir(commonDir), "main"))
	for _, entry := range entries[1:] {
		if !entry.Bare && utils.PathKey(entry.Path) == utils.PathKey(anchor) {
			return anchor, true
		}
	}
	return "", false
}

// getRootDir returns the repository root directory.
func (g *Git) getRootDir() (string, error) {
	output, err := g.run("rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("failed to get repository root: %w", err)
	}
	return strings.TrimSpace(output), nil
}
