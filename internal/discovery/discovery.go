// Package discovery provides filesystem-based global worktree discovery.
package discovery

import (
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

// DiscoverGlobalWorktrees finds all worktrees in the configured base
// directory. projects carries the registered projects so repository identity
// follows the single registered-identity precedence (a registered canonical
// identity wins over a fork origin; see worktree.RepositoryInfoWithProjects);
// pass nil when no registry is available.
func DiscoverGlobalWorktrees(baseDir string, projects []models.Project) ([]*GlobalWorktreeEntry, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("base directory not configured")
	}

	// Expand path (handles ~, env vars, and relative paths)
	expandedPath, err := utils.ExpandPath(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to expand base directory path: %w", err)
	}
	baseDir = expandedPath

	// Check if base directory exists
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		return []*GlobalWorktreeEntry{}, nil
	}

	var candidates []worktreeCandidate

	err = filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
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
		gitInfo, err := os.Stat(gitPath)
		if err != nil {
			return nil // No .git entry, continue
		}

		if gitInfo.IsDir() {
			// Main worktree (.git is a directory)
			candidates = append(candidates, worktreeCandidate{path: path, isMain: true})
			return filepath.SkipDir // Don't descend into the repo
		}

		// Linked worktree (.git is a file)
		gitContent, err := os.ReadFile(gitPath)
		if err != nil {
			return nil
		}

		gitContentStr := strings.TrimSpace(string(gitContent))
		if !strings.HasPrefix(gitContentStr, "gitdir: ") {
			return nil
		}

		// Skip submodules — their gitdir points to .git/modules/...
		gitDir := strings.TrimPrefix(gitContentStr, "gitdir: ")
		if isSubmoduleGitDir(gitDir) {
			return nil
		}

		candidates = append(candidates, worktreeCandidate{path: path})
		return filepath.SkipDir
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	snapshots, err := snapshotCandidateWorktrees(candidates)
	if err != nil {
		return nil, err
	}
	return extractWorktreeCandidates(
		candidates,
		projects,
		func(path string, projects []models.Project) (*GlobalWorktreeEntry, error) {
			snapshot, ok := snapshots[utils.PathKey(path)]
			if !ok {
				return nil, fmt.Errorf("worktree disappeared during discovery")
			}
			return extractWorktreeInfoFromSnapshot(path, projects, snapshot)
		},
	), nil
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
	candidates []worktreeCandidate,
	projects []models.Project,
	extract func(string, []models.Project) (*GlobalWorktreeEntry, error),
) []*GlobalWorktreeEntry {
	if len(candidates) == 0 {
		return []*GlobalWorktreeEntry{}
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
				entry, err := extract(candidates[index].path, projects)
				if err != nil {
					continue
				}
				entry.IsMain = candidates[index].isMain
				results[index] = entry
			}
		}()
	}
	for index := range candidates {
		jobs <- index
	}
	close(jobs)
	workers.Wait()

	entries := make([]*GlobalWorktreeEntry, 0, len(results))
	for _, entry := range results {
		if entry != nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

func snapshotCandidateWorktrees(
	candidates []worktreeCandidate,
) (map[string]models.Worktree, error) {
	repositories := make(map[string]string)
	for _, candidate := range candidates {
		mainRoot, err := git.New(candidate.path).GetMainRepositoryPath()
		if err != nil {
			continue
		}
		repositories[utils.PathKey(mainRoot)] = mainRoot
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
				worktrees, err := git.New(repositoryRoot).ListWorktrees()
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
	for _, repositoryRoot := range repositories {
		jobs <- repositoryRoot
	}
	close(jobs)
	workers.Wait()

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
	return extractWorktreeInfoFromSnapshot(worktreePath, projects, *snapshot)
}

func extractWorktreeInfoFromSnapshot(
	worktreePath string,
	projects []models.Project,
	snapshot models.Worktree,
) (*GlobalWorktreeEntry, error) {
	// The cached wrapper keeps the entry's recorded remote URL and the
	// resolver's own reads to one subprocess per call kind.
	g := worktree.NewCachedIdentityGit(git.New(worktreePath))
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

// isSubmoduleGitDir checks whether a gitdir path points to a submodule
// rather than a linked worktree. Submodule gitdirs always contain a
// "/modules/" segment — either under .git/modules/ (submodules in the main
// worktree) or under .git/worktrees/<name>/modules/ (submodules in a linked
// worktree). Linked worktree gitdirs point to .git/worktrees/<name> with no
// trailing /modules/ path.
func isSubmoduleGitDir(gitDir string) bool {
	normalized := filepath.ToSlash(gitDir)
	return strings.Contains(normalized, "/modules/")
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
