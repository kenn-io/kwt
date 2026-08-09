// Package discovery provides filesystem-based global worktree discovery.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

// GlobalWorktreeEntry represents a discovered worktree.
type GlobalWorktreeEntry struct {
	RepositoryURL  string              // Full repository URL
	RepositoryInfo *url.RepositoryInfo // Parsed repository information
	Branch         string
	Path           string
	CommitHash     string
	IsMain         bool
	CreatedAt      time.Time // Worktree directory modification time
	Generation     string    // Durable worktree registration identity
}

type worktreeCandidate struct {
	path   string
	isMain bool
}

var (
	walkGlobalWorktreePaths = filepath.Walk
	statGlobalWorktreeGit   = os.Stat
	readGlobalWorktreeGit   = os.ReadFile
)

// DiscoverGlobalWorktrees finds all worktrees in the configured base
// directory. projects carries the registered projects so repository identity
// follows the single registered-identity precedence (a registered canonical
// identity wins over a fork origin; see worktree.RepositoryInfoWithProjects);
// pass nil when no registry is available.
func DiscoverGlobalWorktrees(baseDir string, projects []models.Project) ([]*GlobalWorktreeEntry, error) {
	return DiscoverGlobalWorktreesContext(context.Background(), baseDir, projects)
}

// DiscoverGlobalWorktreesContext finds all worktrees while allowing callers
// to stop filesystem traversal, lock waits, and Git subprocesses.
func DiscoverGlobalWorktreesContext(
	ctx context.Context,
	baseDir string,
	projects []models.Project,
	protectedNames ...string,
) ([]*GlobalWorktreeEntry, error) {
	candidates, err := findGlobalWorktreeCandidates(ctx, baseDir, false)
	if err != nil {
		return nil, err
	}

	snapshots, err := snapshotCandidateWorktrees(ctx, candidates, protectedNames)
	if err != nil {
		return nil, err
	}
	return extractWorktreeCandidates(
		ctx,
		candidates,
		projects,
		func(
			ctx context.Context,
			path string,
			projects []models.Project,
		) (*GlobalWorktreeEntry, error) {
			snapshot, ok := snapshots[utils.PathKey(path)]
			if !ok {
				return nil, fmt.Errorf("worktree disappeared during discovery")
			}
			return extractWorktreeInfoFromSnapshot(
				ctx,
				path,
				projects,
				snapshot,
				protectedNames...,
			)
		},
	)
}

// FindGlobalWorktreePaths returns filesystem worktree candidates without
// snapshotting repositories or initializing durable generations.
func FindGlobalWorktreePaths(baseDir string) ([]string, error) {
	candidates, err := findGlobalWorktreeCandidates(context.Background(), baseDir, false)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.path)
	}
	return paths, nil
}

// FindGlobalWorktreePathsStrict returns filesystem worktree candidates only
// when the complete configured tree can be traversed and inspected.
func FindGlobalWorktreePathsStrict(baseDir string) ([]string, error) {
	candidates, err := findGlobalWorktreeCandidates(context.Background(), baseDir, true)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.path)
	}
	return paths, nil
}

func findGlobalWorktreeCandidates(
	ctx context.Context,
	baseDir string,
	strict bool,
) ([]worktreeCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if baseDir == "" {
		return nil, fmt.Errorf("base directory not configured")
	}

	// Expand path (handles ~, env vars, and relative paths)
	expandedPath, err := utils.ExpandPath(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to expand base directory path: %w", err)
	}
	baseDir = expandedPath

	// Inspect the link itself before resolving the root so strict discovery can
	// distinguish a genuinely absent directory from a dangling configured
	// symlink. Resolve a healthy symlink so Walk enters its target tree.
	baseInfo, err := os.Lstat(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []worktreeCandidate{}, nil
		}
		if strict {
			return nil, fmt.Errorf("inspect base directory %s: %w", baseDir, err)
		}
	}
	if err == nil && baseInfo.Mode()&os.ModeSymlink != 0 {
		resolved, resolveErr := filepath.EvalSymlinks(baseDir)
		if resolveErr != nil {
			if strict {
				return nil, fmt.Errorf("resolve base directory %s: %w", baseDir, resolveErr)
			}
		} else {
			baseDir = resolved
		}
	}

	var candidates []worktreeCandidate

	err = walkGlobalWorktreePaths(baseDir, func(path string, info os.FileInfo, err error) error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if err != nil {
			if strict {
				return err
			}
			return nil // Skip errors and continue walking
		}

		if !info.IsDir() {
			return nil
		}

		// Skip .git directories themselves
		if info.Name() == ".git" {
			return filepath.SkipDir
		}

		gitPath := filepath.Join(path, ".git")
		gitInfo, err := statGlobalWorktreeGit(gitPath)
		if err != nil {
			if strict && os.IsNotExist(err) {
				_, linkErr := os.Lstat(gitPath)
				if linkErr == nil {
					return fmt.Errorf("inspect worktree metadata %s: %w", gitPath, err)
				}
				if !os.IsNotExist(linkErr) {
					return fmt.Errorf("inspect worktree metadata %s: %w", gitPath, linkErr)
				}
			}
			if strict && !os.IsNotExist(err) {
				return fmt.Errorf("inspect worktree metadata %s: %w", gitPath, err)
			}
			return nil // No .git entry, continue
		}

		if gitInfo.IsDir() {
			// Main worktree (.git is a directory)
			candidates = append(candidates, worktreeCandidate{path: path, isMain: true})
			return filepath.SkipDir // Don't descend into the repo
		}

		// Linked worktree (.git is a file)
		gitContent, err := readGlobalWorktreeGit(gitPath)
		if err != nil {
			if strict {
				return fmt.Errorf("read linked worktree metadata %s: %w", gitPath, err)
			}
			return nil
		}

		gitContentStr := strings.TrimSpace(string(gitContent))
		if !strings.HasPrefix(gitContentStr, "gitdir: ") {
			if strict {
				return fmt.Errorf("invalid linked worktree metadata %s", gitPath)
			}
			return nil
		}

		// Skip submodules — their gitdir points to .git/modules/...
		gitDir := strings.TrimSpace(strings.TrimPrefix(gitContentStr, "gitdir: "))
		if gitDir == "" || strings.ContainsAny(gitDir, "\r\n") {
			if strict {
				return fmt.Errorf("invalid linked worktree metadata %s", gitPath)
			}
			return nil
		}
		if isSubmoduleGitDir(gitDir) {
			return nil
		}

		candidates = append(candidates, worktreeCandidate{path: path})
		return filepath.SkipDir
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	return candidates, nil
}

// DiscoverWorktree resolves one exact worktree root independently of the
// configured global base directory. Automation callers already holding a path
// from repository-local inventory can therefore establish that workspace even
// when the linked worktree lives elsewhere.
func DiscoverWorktree(path string, projects []models.Project) (*GlobalWorktreeEntry, error) {
	expanded, err := utils.ExpandPath(path)
	if err != nil {
		return nil, fmt.Errorf("failed to expand worktree path: %w", err)
	}
	repositoryGit := git.New(expanded)
	root, err := repositoryGit.GetRepositoryPath()
	if err != nil {
		return nil, fmt.Errorf("failed to find worktree root: %w", err)
	}
	if utils.PathKey(root) != utils.PathKey(expanded) {
		return nil, fmt.Errorf("path is not a worktree root")
	}
	entry, err := extractWorktreeInfo(root, projects)
	if err != nil {
		return nil, err
	}
	if mainRoot, mainErr := repositoryGit.GetMainRepositoryPath(); mainErr == nil {
		entry.IsMain =
			utils.PathKey(mainRoot) == utils.PathKey(root)
	}
	return entry, nil
}

func extractWorktreeCandidates(
	ctx context.Context,
	candidates []worktreeCandidate,
	projects []models.Project,
	extract func(context.Context, string, []models.Project) (*GlobalWorktreeEntry, error),
) ([]*GlobalWorktreeEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return []*GlobalWorktreeEntry{}, nil
	}

	const maxWorkers = 16
	workerCount := min(len(candidates), maxWorkers)
	results := make([]*GlobalWorktreeEntry, len(candidates))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				entry, err := extract(ctx, candidates[index].path, projects)
				if err != nil {
					continue
				}
				entry.IsMain = candidates[index].isMain
				results[index] = entry
			}
		}()
	}
sendCandidates:
	for index := range candidates {
		select {
		case <-ctx.Done():
			break sendCandidates
		case jobs <- index:
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	entries := make([]*GlobalWorktreeEntry, 0, len(results))
	for _, entry := range results {
		if entry != nil {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func snapshotCandidateWorktrees(
	ctx context.Context,
	candidates []worktreeCandidate,
	protectedNames []string,
) (map[string]models.Worktree, error) {
	repositories := make(map[string]string)
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mainRoot, err := git.NewForInventory(
			ctx,
			candidate.path,
			protectedNames,
		).GetMainRepositoryPath()
		if err != nil {
			continue
		}
		repositories[utils.PathKey(mainRoot)] = mainRoot
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snapshots := make(map[string]models.Worktree)
	if len(repositories) == 0 {
		return snapshots, nil
	}

	const maxWorkers = 16
	workerCount := min(len(repositories), maxWorkers)
	jobs := make(chan string)
	var snapshotsMu sync.Mutex
	var snapshotErrors []error
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for repositoryRoot := range jobs {
				if ctx.Err() != nil {
					return
				}
				worktrees, err := git.NewForInventory(
					ctx,
					repositoryRoot,
					protectedNames,
				).ListWorktrees()
				if err != nil {
					snapshotsMu.Lock()
					snapshotErrors = append(
						snapshotErrors,
						fmt.Errorf(
							"snapshot repository %s: %w",
							repositoryRoot,
							err,
						),
					)
					snapshotsMu.Unlock()
					continue
				}
				snapshotsMu.Lock()
				for _, snapshot := range worktrees {
					snapshots[utils.PathKey(snapshot.Path)] = snapshot
				}
				snapshotsMu.Unlock()
			}
		}()
	}
sendRepositories:
	for _, repositoryRoot := range repositories {
		select {
		case <-ctx.Done():
			break sendRepositories
		case jobs <- repositoryRoot:
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return snapshots, errors.Join(snapshotErrors...)
}

// extractWorktreeInfo extracts worktree information from a worktree directory.
func extractWorktreeInfo(worktreePath string, projects []models.Project) (*GlobalWorktreeEntry, error) {
	repositoryGit := git.New(worktreePath)
	worktrees, err := repositoryGit.ListWorktrees()
	if err != nil {
		return nil, fmt.Errorf("failed to list repository worktrees: %w", err)
	}
	var snapshot *models.Worktree
	canonicalPath := utils.PathKey(worktreePath)
	for i := range worktrees {
		if utils.PathKey(worktrees[i].Path) == canonicalPath {
			snapshot = &worktrees[i]
			break
		}
	}
	if snapshot == nil {
		return nil, fmt.Errorf("worktree disappeared during discovery")
	}
	return extractWorktreeInfoFromSnapshot(context.Background(), worktreePath, projects, *snapshot)
}

func extractWorktreeInfoFromSnapshot(
	ctx context.Context,
	worktreePath string,
	projects []models.Project,
	snapshot models.Worktree,
	protectedNames ...string,
) (*GlobalWorktreeEntry, error) {
	// The cached wrapper keeps the entry's recorded remote URL and the
	// resolver's own reads to one subprocess per call kind.
	g := worktree.NewCachedIdentityGit(
		git.NewForInventory(ctx, worktreePath, protectedNames),
	)
	repoURL := ""
	if gotURL, err := g.GetRepositoryURL(); err == nil {
		repoURL = gotURL
	}
	// Route through the single canonical resolver (with registered-identity
	// precedence) so a no-remote repository gets the same "local/..."
	// identity here as kwt list and project registration report, and a
	// registered project's canonical identity wins over a fork origin,
	// keeping the JSON surfaces joinable. The resolver returns nil info on
	// error; a worktree without resolvable identity still lists.
	repoInfo, _ := worktree.RepositoryInfoWithProjects(g, projects)

	return &GlobalWorktreeEntry{
		RepositoryURL:  repoURL,
		RepositoryInfo: repoInfo,
		Branch:         snapshot.Branch,
		Path:           snapshot.Path,
		CommitHash:     snapshot.CommitHash,
		IsMain:         snapshot.IsMain,
		CreatedAt:      snapshot.CreatedAt,
		Generation:     snapshot.Generation,
	}, nil
}

// getCurrentBranch gets the current branch name for a worktree.
func getCurrentBranch(worktreePath string) (string, error) {
	g := git.New(worktreePath)

	// Use git rev-parse to get the current branch
	output, err := g.RunCommand("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}

	branch := strings.TrimSpace(output)
	if branch == "HEAD" {
		// Detached HEAD state, try to get a more meaningful name
		return "HEAD", nil
	}

	return branch, nil
}

// getCurrentCommitHash gets the current commit hash for a worktree.
func getCurrentCommitHash(worktreePath string) (string, error) {
	g := git.New(worktreePath)

	output, err := g.RunCommand("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(output), nil
}

// isSubmoduleGitDir checks whether a gitdir path uses Git's standard submodule
// administrative layout. Only segments after the final .git directory are
// relevant; an ordinary repository may itself live below a directory named
// modules.
func isSubmoduleGitDir(gitDir string) bool {
	normalized := filepath.ToSlash(gitDir)
	segments := strings.Split(normalized, "/")
	gitIndex := -1
	for index, segment := range segments {
		if strings.EqualFold(segment, ".git") {
			gitIndex = index
		}
	}
	if gitIndex < 0 {
		return false
	}

	adminPath := segments[gitIndex+1:]
	if len(adminPath) >= 2 && strings.EqualFold(adminPath[0], "modules") {
		return true
	}
	return len(adminPath) >= 4 &&
		strings.EqualFold(adminPath[0], "worktrees") &&
		strings.EqualFold(adminPath[2], "modules")
}

// Model converts a discovered entry into a manifest Worktree, carrying the
// repository slug, session name, and creation time alongside the core
// worktree fields. Both identity fields stay empty when the repository could
// not be identified.
func (e *GlobalWorktreeEntry) Model() models.Worktree {
	m := models.Worktree{
		Path:       e.Path,
		Branch:     e.Branch,
		CommitHash: e.CommitHash,
		IsMain:     e.IsMain,
		CreatedAt:  e.CreatedAt,
		Generation: e.Generation,
	}
	if e.RepositoryInfo != nil {
		m.Repository = e.RepositoryInfo.FullPath
		m.SessionName = tmux.WorkspaceSessionName(e.RepositoryInfo, e.Branch, e.Path)
	}
	return m
}

// ConvertToWorktreeModels converts GlobalWorktreeEntry to models.Worktree.
func ConvertToWorktreeModels(entries []*GlobalWorktreeEntry, showRepoName bool) []models.Worktree {
	worktrees := make([]models.Worktree, 0, len(entries))

	for _, entry := range entries {
		wt := entry.Model()
		if showRepoName && entry.RepositoryInfo != nil {
			// Use repository name from parsed URL info
			wt.Branch = fmt.Sprintf("%s:%s", entry.RepositoryInfo.Repository, entry.Branch)
		}
		worktrees = append(worktrees, wt)
	}

	return worktrees
}

// FilterGlobalWorktrees filters worktrees by pattern matching.
func FilterGlobalWorktrees(entries []*GlobalWorktreeEntry, pattern string) []*GlobalWorktreeEntry {
	pattern = strings.ToLower(pattern)
	var matches []*GlobalWorktreeEntry

	for _, entry := range entries {
		branchLower := strings.ToLower(entry.Branch)
		var repoName string
		if entry.RepositoryInfo != nil {
			repoName = strings.ToLower(entry.RepositoryInfo.Repository)
		}

		// Match against branch name, path, repo name, or repo:branch pattern
		if strings.Contains(branchLower, pattern) ||
			strings.Contains(strings.ToLower(entry.Path), pattern) ||
			strings.Contains(repoName, pattern) ||
			strings.Contains(repoName+":"+branchLower, pattern) {
			matches = append(matches, entry)
		}
	}

	return matches
}
