package status

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

// StatusCollectorOptions contains optional parameters for StatusCollector.
type StatusCollectorOptions struct {
	FetchRemote    bool
	StaleThreshold time.Duration
	BaseDir        string
}

// StatusCollector collects status information for worktrees.
type StatusCollector struct {
	fetchRemote    bool
	staleThreshold time.Duration
	basedir        string
}

type porcelainStatus struct {
	GitStatus models.GitStatus
	Paths     []string
	Head      string
	Upstream  string
}

// NewStatusCollectorWithOptions creates a new status collector with custom options.
func NewStatusCollectorWithOptions(opts StatusCollectorOptions) *StatusCollector {
	// Default stale threshold to 14 days if not specified
	if opts.StaleThreshold == 0 {
		opts.StaleThreshold = 14 * 24 * time.Hour
	}

	return &StatusCollector{
		fetchRemote:    opts.FetchRemote,
		staleThreshold: opts.StaleThreshold,
		basedir:        opts.BaseDir,
	}
}

// CollectAll collects status for all provided worktrees in parallel.
func (c *StatusCollector) CollectAll(ctx context.Context, worktrees []*models.Worktree) ([]*models.WorktreeStatus, error) {
	statuses := make([]*models.WorktreeStatus, len(worktrees))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	currentPath, _ := os.Getwd()

	for i, wt := range worktrees {
		wg.Add(1)
		go func(idx int, worktree *models.Worktree) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			default:
			}

			status, err := c.collectOne(ctx, worktree)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}

			if utils.IsSameOrChildPath(currentPath, worktree.Path) {
				status.IsCurrent = true
			}

			statuses[idx] = status
		}(i, wt)
	}

	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	var validStatuses []*models.WorktreeStatus
	for _, s := range statuses {
		if s != nil {
			validStatuses = append(validStatuses, s)
		}
	}

	return validStatuses, nil
}

func (c *StatusCollector) collectOne(ctx context.Context, worktree *models.Worktree) (*models.WorktreeStatus, error) {
	repository := strings.TrimSpace(worktree.Repository)
	g := git.New(worktree.Path)
	if repository == "" {
		repository = c.repositoryIdentity(g, worktree.Path)
	}
	status := &models.WorktreeStatus{
		Path:       worktree.Path,
		Branch:     worktree.Branch,
		Repository: repository,
		Status:     models.WorktreeStatusClean,
	}

	gitStatus, err := c.collectGitStatus(ctx, g)
	if err != nil {
		// Log error but continue with minimal status
		// fmt.Fprintf(os.Stderr, "Warning: Failed to collect git status for %s: %v\n", worktree.Path, err)
		status.GitStatus = models.GitStatus{}
		status.Status = models.WorktreeStatusUnknown
	} else {
		status.GitStatus = *gitStatus
		status.Status = c.determineWorktreeState(gitStatus)
	}

	lastActivity, err := c.getLastActivity(ctx, worktree.Path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	} else {
		status.LastActivity = lastActivity
		if time.Since(lastActivity) > c.staleThreshold {
			status.Status = models.WorktreeStatusStale
		}
	}

	return status, nil
}

func (c *StatusCollector) repositoryIdentity(g *git.Git, path string) string {
	repoURL, err := g.GetRepositoryURL()
	if err == nil {
		// The remote-derived bar keeps a relative dotless filesystem remote
		// ("cache/team/repo.git") from surfacing as a shareable identity,
		// matching the bar kwt list and kwt projects apply.
		if info, ok := url.CanonicalRepositoryInfoFromRemote(repoURL); ok {
			return repositoryFullPathIdentity(info)
		}
	}
	return c.extractRepository(path)
}

func repositoryFullPathIdentity(info *url.RepositoryInfo) string {
	if info == nil {
		return ""
	}
	return strings.ReplaceAll(filepath.ToSlash(info.FullPath), "\\", "/")
}

func (c *StatusCollector) collectGitStatus(ctx context.Context, g *git.Git) (*models.GitStatus, error) {
	parsed, err := c.collectPorcelain(ctx, g)
	if err != nil {
		return nil, err
	}
	if !c.fetchRemote {
		parsed.GitStatus.Ahead = 0
		parsed.GitStatus.Behind = 0
	}
	return &parsed.GitStatus, nil
}

func (c *StatusCollector) collectPorcelain(ctx context.Context, g *git.Git) (porcelainStatus, error) {
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := g.RunWithContext(gitCtx, "status", "--porcelain=v2", "--branch", "-uall", "-z")
	if err != nil {
		return porcelainStatus{}, err
	}
	return parsePorcelainV2(output)
}

func parsePorcelainV2(output string) (porcelainStatus, error) {
	var result porcelainStatus
	records := strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
	for index := 0; index < len(records); index++ {
		record := records[index]
		if record == "" {
			continue
		}
		switch {
		case strings.HasPrefix(record, "# branch.oid "):
			result.Head = strings.TrimPrefix(record, "# branch.oid ")
			continue
		case strings.HasPrefix(record, "# branch.upstream "):
			result.Upstream = strings.TrimPrefix(record, "# branch.upstream ")
			continue
		case strings.HasPrefix(record, "# branch.ab "):
			fields := strings.Fields(strings.TrimPrefix(record, "# branch.ab "))
			if len(fields) != 2 {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 branch.ab %q: expected ahead and behind", record)
			}
			ahead, err := strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
			if err != nil {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 ahead count %q: %w", fields[0], err)
			}
			behind, err := strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
			if err != nil {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 behind count %q: %w", fields[1], err)
			}
			result.GitStatus.Ahead = ahead
			result.GitStatus.Behind = behind
			continue
		case record[0] == '#':
			continue
		}

		var xy, path string
		switch record[0] {
		case '1':
			fields := strings.SplitN(record, " ", 9)
			if len(fields) != 9 || len(fields[1]) != 2 {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 ordinary record %q: expected 9 fields", record)
			}
			xy, path = fields[1], fields[8]
		case '2':
			fields := strings.SplitN(record, " ", 10)
			if len(fields) != 10 || len(fields[1]) != 2 {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 rename record %q: expected 10 fields", record)
			}
			xy, path = fields[1], fields[9]
			if index+1 >= len(records) {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 rename record %q: missing original path", record)
			}
			index++
		case 'u':
			fields := strings.SplitN(record, " ", 11)
			if len(fields) != 11 || len(fields[1]) != 2 {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 unmerged record %q: expected 11 fields", record)
			}
			xy, path = fields[1], fields[10]
			result.GitStatus.Conflicts++
		case '?':
			if !strings.HasPrefix(record, "? ") {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 untracked record %q: missing path", record)
			}
			path = strings.TrimPrefix(record, "? ")
			result.GitStatus.Untracked++
		case '!':
			continue
		default:
			return porcelainStatus{}, fmt.Errorf("parse porcelain v2 record %q: unknown kind", record)
		}

		if xy != "" {
			if xy[0] != '.' {
				result.GitStatus.Staged++
			}
			switch xy[1] {
			case 'M':
				result.GitStatus.Modified++
			case 'A':
				result.GitStatus.Added++
			case 'D':
				result.GitStatus.Deleted++
			case 'U':
				if record[0] != 'u' {
					result.GitStatus.Conflicts++
				}
			}
		}
		if path == "" {
			return porcelainStatus{}, fmt.Errorf("parse porcelain v2 record %q: empty path", record)
		}
		result.Paths = append(result.Paths, path)
	}
	return result, nil
}

func (c *StatusCollector) determineWorktreeState(status *models.GitStatus) models.WorktreeState {
	if status.Conflicts > 0 {
		return models.WorktreeStatusConflict
	}
	if status.Staged > 0 {
		return models.WorktreeStatusStaged
	}
	if status.Modified > 0 || status.Added > 0 || status.Deleted > 0 || status.Untracked > 0 {
		return models.WorktreeStatusModified
	}
	return models.WorktreeStatusClean
}

func (c *StatusCollector) getLastActivity(ctx context.Context, path string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	// Use git ls-files to get tracked files efficiently
	// This approach respects .gitignore patterns automatically and is much faster
	// than walking the entire directory tree
	g := git.New(path)

	latestTime, err := c.getLastActivityFromTrackedFiles(ctx, g, path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return time.Time{}, ctxErr
		}
		// Fallback to directory walk if git command fails
		return c.getLastActivityFallback(ctx, path)
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}

	// Also check untracked files that are not ignored
	untrackedTime, err := c.getLastActivityFromUntrackedFiles(ctx, g, path)
	if err != nil {
		return time.Time{}, err
	}
	if untrackedTime.After(latestTime) {
		latestTime = untrackedTime
	}

	if latestTime.IsZero() {
		// If no files found, use the directory's own modification time
		info, err := os.Stat(path)
		if err == nil {
			latestTime = info.ModTime()
		}
	}

	return latestTime, nil
}

// getLastActivityFromTrackedFiles gets the latest modification time from tracked files
func (c *StatusCollector) getLastActivityFromTrackedFiles(ctx context.Context, g *git.Git, path string) (time.Time, error) {
	// Get list of tracked files
	// Using -z for null-terminated output to handle filenames with spaces
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := g.RunWithContext(gitCtx, "ls-files", "-z")
	if err != nil {
		return time.Time{}, err
	}

	var latestTime time.Time
	files := strings.SplitSeq(strings.TrimRight(output, "\x00"), "\x00")

	for file := range files {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		if file == "" {
			continue
		}

		fullPath := filepath.Join(path, file)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue // Skip files we can't stat
		}

		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
		}
	}

	return latestTime, nil
}

// getLastActivityFromUntrackedFiles gets the latest modification time from untracked files
func (c *StatusCollector) getLastActivityFromUntrackedFiles(ctx context.Context, g *git.Git, path string) (time.Time, error) {
	var latestTime time.Time

	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	untrackedOutput, err := g.RunWithContext(gitCtx, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return time.Time{}, ctxErr
		}
		return latestTime, nil
	}

	untrackedFiles := strings.SplitSeq(strings.TrimRight(untrackedOutput, "\x00"), "\x00")
	for file := range untrackedFiles {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		if file == "" {
			continue
		}

		fullPath := filepath.Join(path, file)
		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			continue
		}

		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
		}
	}

	return latestTime, nil
}

// getLastActivityFallback is the fallback method when git commands fail
func (c *StatusCollector) getLastActivityFallback(ctx context.Context, path string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	var latestTime time.Time

	// Common large directories to skip
	skipDirs := map[string]bool{
		".git":          true,
		"node_modules":  true,
		"vendor":        true,
		".next":         true,
		"dist":          true,
		"build":         true,
		"target":        true,
		".cache":        true,
		"coverage":      true,
		"__pycache__":   true,
		".pytest_cache": true,
		".venv":         true,
		"venv":          true,
		".idea":         true,
		".vscode":       true,
	}

	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil // Continue even if we can't access a file
		}

		// Skip directories
		if info.IsDir() {
			dirName := filepath.Base(p)
			if skipDirs[dirName] {
				return filepath.SkipDir
			}
			// Also skip hidden directories (except the root)
			if dirName != "." && strings.HasPrefix(dirName, ".") && p != path {
				return filepath.SkipDir
			}
		}

		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
		}

		return nil
	})

	if err != nil {
		return time.Time{}, err
	}

	return latestTime, nil
}

func (c *StatusCollector) extractRepository(path string) string {
	// Return basename if basedir is not set
	if c.basedir == "" {
		return filepath.Base(path)
	}

	baseDir := filepath.Clean(c.basedir)
	cleanPath := filepath.Clean(path)

	// Check if the path is under the base directory
	if !strings.HasPrefix(cleanPath, baseDir) {
		// Path is not under base directory, return basename
		return filepath.Base(path)
	}

	rel, err := filepath.Rel(baseDir, cleanPath)
	if err != nil {
		// Failed to get relative path, fallback to basename
		return filepath.Base(path)
	}

	// Split the relative path into components
	parts := strings.Split(rel, string(filepath.Separator))

	// Expected structure: host/owner/repository/branch
	// Return the first 3 components if available
	if len(parts) >= 3 {
		return filepath.Join(parts[0], parts[1], parts[2])
	}

	// If we don't have enough parts, return what we have or the basename
	if len(parts) > 0 {
		return rel
	}

	return filepath.Base(path)
}
