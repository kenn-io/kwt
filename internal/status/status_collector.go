package status

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	Workers        int
	ProtectedNames []string
}

// StatusCollector collects status information for worktrees.
type StatusCollector struct {
	fetchRemote     bool
	staleThreshold  time.Duration
	basedir         string
	workers         int
	protectedNames  []string
	activityTimeout time.Duration
	runHead         func(context.Context, string) (string, error)
}

type Diagnostic struct {
	Path string
	Err  error
}

type Collection struct {
	Statuses    []*models.WorktreeStatus
	Diagnostics []Diagnostic
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
	if opts.Workers <= 0 {
		opts.Workers = min(runtime.GOMAXPROCS(0), 8)
	}
	if opts.Workers < 1 {
		opts.Workers = 1
	}

	collector := &StatusCollector{
		fetchRemote:     opts.FetchRemote,
		staleThreshold:  opts.StaleThreshold,
		basedir:         opts.BaseDir,
		workers:         opts.Workers,
		protectedNames:  append([]string(nil), opts.ProtectedNames...),
		activityTimeout: 5 * time.Second,
	}
	collector.runHead = func(ctx context.Context, path string) (string, error) {
		return git.NewForInventory(ctx, path, collector.protectedNames).
			RunWithContext(ctx, "show", "-s", "--format=%ct", "HEAD")
	}
	return collector
}

// CollectAll collects status for all provided worktrees with bounded concurrency.
func (c *StatusCollector) CollectAll(ctx context.Context, worktrees []*models.Worktree) (Collection, error) {
	result, err := collectWorktrees(ctx, c.workers, worktrees, c.collectOne)
	if err != nil {
		return Collection{}, err
	}
	currentPath, _ := os.Getwd()
	for index, worktreeStatus := range result.Statuses {
		if worktreeStatus != nil && utils.IsSameOrChildPath(currentPath, worktrees[index].Path) {
			worktreeStatus.IsCurrent = true
		}
	}
	return result, nil
}

type statusJob struct {
	index    int
	worktree *models.Worktree
}

func collectWorktrees(
	ctx context.Context,
	workers int,
	worktrees []*models.Worktree,
	collect func(context.Context, *models.Worktree) (*models.WorktreeStatus, error),
) (Collection, error) {
	if err := ctx.Err(); err != nil {
		return Collection{}, err
	}
	if workers < 1 {
		workers = 1
	}
	workers = min(workers, max(len(worktrees), 1))
	result := Collection{Statuses: make([]*models.WorktreeStatus, len(worktrees))}
	diagnostics := make([]error, len(worktrees))
	jobs := make(chan statusJob)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					continue
				}
				worktreeStatus, err := collect(ctx, job.worktree)
				if err != nil {
					diagnostics[job.index] = err
					worktreeStatus = &models.WorktreeStatus{
						Path:       job.worktree.Path,
						Branch:     job.worktree.Branch,
						Repository: job.worktree.Repository,
						Status:     models.WorktreeStatusUnknown,
					}
				}
				result.Statuses[job.index] = worktreeStatus
			}
		}()
	}

submit:
	for index, worktree := range worktrees {
		select {
		case jobs <- statusJob{index: index, worktree: worktree}:
		case <-ctx.Done():
			break submit
		}
	}
	close(jobs)
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return Collection{}, err
	}
	for index, err := range diagnostics {
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Path: worktrees[index].Path, Err: err})
		}
	}

	return result, nil
}

func (c *StatusCollector) collectOne(ctx context.Context, worktree *models.Worktree) (*models.WorktreeStatus, error) {
	repository := strings.TrimSpace(worktree.Repository)
	g := git.NewForInventory(ctx, worktree.Path, c.protectedNames)
	if repository == "" {
		repository = c.repositoryIdentity(g, worktree.Path)
	}
	status := &models.WorktreeStatus{
		Path:       worktree.Path,
		Branch:     worktree.Branch,
		Repository: repository,
		Status:     models.WorktreeStatusClean,
	}

	porcelain, err := c.collectPorcelain(ctx, g)
	if err != nil {
		return nil, err
	}
	if !c.fetchRemote {
		porcelain.GitStatus.Ahead = 0
		porcelain.GitStatus.Behind = 0
	}
	status.GitStatus = porcelain.GitStatus
	status.Status = c.determineWorktreeState(&porcelain.GitStatus)

	lastActivity, err := c.lastActivity(ctx, worktree.Path, porcelain.Paths)
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

func (c *StatusCollector) collectPorcelain(ctx context.Context, g *git.Git) (porcelainStatus, error) {
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"status", "--porcelain=v2", "--branch", "-uall", "-z"}
	if !c.fetchRemote {
		args = append(args, "--no-ahead-behind")
	}
	output, err := g.RunWithContext(gitCtx, args...)
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
			aheadText := strings.TrimPrefix(fields[0], "+")
			behindText := strings.TrimPrefix(fields[1], "-")
			if aheadText == "?" && behindText == "?" {
				continue
			}
			ahead, err := strconv.Atoi(aheadText)
			if err != nil {
				return porcelainStatus{}, fmt.Errorf("parse porcelain v2 ahead count %q: %w", fields[0], err)
			}
			behind, err := strconv.Atoi(behindText)
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
			case 'M', 'R', 'C', 'T':
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

func (c *StatusCollector) lastActivity(ctx context.Context, path string, changed []string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	root, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	latest := root.ModTime().UTC()

	gitCtx, cancel := context.WithTimeout(ctx, c.activityTimeout)
	head, err := c.runHead(gitCtx, path)
	cancel()
	if err == nil {
		seconds, parseErr := strconv.ParseInt(strings.TrimSpace(head), 10, 64)
		if parseErr == nil {
			headTime := time.Unix(seconds, 0)
			if headTime.After(latest) {
				latest = headTime
			}
		}
	}
	for _, relative := range changed {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		info, statErr := os.Stat(filepath.Join(path, filepath.FromSlash(relative)))
		if statErr == nil && info.ModTime().After(latest) {
			latest = info.ModTime().UTC()
		} else if statErr != nil {
			parentTime := nearestExistingParentTime(path, relative)
			if parentTime.After(latest) {
				latest = parentTime
			}
		}
	}
	return latest, ctx.Err()
}

func nearestExistingParentTime(root, relative string) time.Time {
	candidate := filepath.Dir(filepath.Join(root, filepath.FromSlash(relative)))
	root = filepath.Clean(root)
	for utils.IsSameOrChildPath(candidate, root) {
		if info, err := os.Stat(candidate); err == nil {
			return info.ModTime().UTC()
		}
		if candidate == root {
			break
		}
		candidate = filepath.Dir(candidate)
	}
	return time.Time{}
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
