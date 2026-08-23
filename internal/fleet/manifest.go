package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/status"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

// ManifestBuilderOptions contains dependencies used by ManifestBuilder.
type ManifestBuilderOptions struct {
	Now                     func() time.Time
	Hostname                func() (string, error)
	DiscoverGlobalWorktrees func(
		baseDir string,
		projects []models.Project,
	) ([]*discovery.GlobalWorktreeEntry, error)
	ListProjectWorktrees func(ctx context.Context, project models.Project) ([]models.Worktree, error)
}

// ManifestBuilder builds local advisory fleet manifests.
type ManifestBuilder struct {
	now                     func() time.Time
	hostname                func() (string, error)
	discoverGlobalWorktrees func(
		baseDir string,
		projects []models.Project,
	) ([]*discovery.GlobalWorktreeEntry, error)
	listProjectWorktrees func(ctx context.Context, project models.Project) ([]models.Worktree, error)
}

// NewManifestBuilder creates a manifest builder with optional dependency hooks.
func NewManifestBuilder(opts ManifestBuilderOptions) *ManifestBuilder {
	builder := &ManifestBuilder{
		now:                     opts.Now,
		hostname:                opts.Hostname,
		discoverGlobalWorktrees: opts.DiscoverGlobalWorktrees,
		listProjectWorktrees:    opts.ListProjectWorktrees,
	}
	if builder.now == nil {
		builder.now = time.Now
	}
	if builder.hostname == nil {
		builder.hostname = os.Hostname
	}
	if builder.discoverGlobalWorktrees == nil {
		builder.discoverGlobalWorktrees = discovery.DiscoverGlobalWorktrees
	}
	if builder.listProjectWorktrees == nil {
		builder.listProjectWorktrees = func(ctx context.Context, project models.Project) ([]models.Worktree, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return git.New(project.Path).ListWorktrees()
		}
	}
	return builder
}

// Build observes configured projects and global worktrees into a fleet manifest.
func (b *ManifestBuilder) Build(ctx context.Context, cfg *models.Config) (*Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		cfg = &models.Config{}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	hostname, hostID, err := b.resolveHost(cfg)
	if err != nil {
		return nil, err
	}

	manifest := &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		HostID:        hostID,
		Host: HostInfo{
			Hostname: hostname,
			Platform: runtime.GOOS + "/" + runtime.GOARCH,
		},
		ObservedAt: b.now().UTC(),
	}

	projectIndexes := make(map[string]int)
	seenWorktreePaths := make(map[string]struct{})

	for _, project := range cfg.Projects {
		if err := b.addConfiguredProject(ctx, cfg, project, manifest, projectIndexes, seenWorktreePaths); err != nil {
			return nil, err
		}
	}
	if err := b.addGlobalWorktrees(ctx, cfg, manifest, projectIndexes, seenWorktreePaths); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sort.Slice(manifest.Projects, func(i, j int) bool {
		if manifest.Projects[i].Identity != manifest.Projects[j].Identity {
			return manifest.Projects[i].Identity < manifest.Projects[j].Identity
		}
		return manifest.Projects[i].LocalRoot < manifest.Projects[j].LocalRoot
	})
	sort.Slice(manifest.Worktrees, func(i, j int) bool {
		left := manifest.Worktrees[i]
		right := manifest.Worktrees[j]
		if left.ProjectIdentity != right.ProjectIdentity {
			return left.ProjectIdentity < right.ProjectIdentity
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Ref != right.Ref {
			return left.Ref < right.Ref
		}
		return left.Path < right.Path
	})

	return manifest, nil
}

func (b *ManifestBuilder) resolveHost(cfg *models.Config) (string, string, error) {
	rawHostID := strings.TrimSpace(cfg.Fleet.HostID)
	if rawHostID != "" {
		hostID, err := NormalizeHostID(rawHostID)
		if err != nil {
			return "", "", err
		}
		hostname, err := b.hostname()
		if err != nil {
			return "", hostID, nil
		}
		return hostname, hostID, nil
	}

	hostname, err := b.hostname()
	if err != nil {
		return "", "", err
	}
	hostID, err := NormalizeHostID(hostname)
	if err != nil {
		return "", "", err
	}
	return hostname, hostID, nil
}

func (b *ManifestBuilder) addConfiguredProject(
	ctx context.Context,
	cfg *models.Config,
	project models.Project,
	manifest *Manifest,
	projectIndexes map[string]int,
	seenWorktreePaths map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	projectPath, ok := observablePath(project.Path)
	if !ok {
		return nil
	}
	project.Path = projectPath

	worktrees, err := b.listProjectWorktrees(ctx, project)
	if err != nil {
		if isContextError(err) || git.IsIncompleteInventory(err) {
			return err
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	identity, ok := configuredProjectIdentity(project.Repository, func() string {
		return remoteURLForPath(projectPath)
	})
	if !ok {
		return nil
	}
	name := projectDisplayName(project.Name, identity, projectPath)
	addProject(manifest, projectIndexes, ProjectManifest{
		Identity:  identity,
		Name:      name,
		LocalRoot: projectPath,
	})

	for _, worktree := range worktrees {
		if _, err := b.addWorktree(ctx, cfg, identity, worktree, manifest, seenWorktreePaths); err != nil {
			return err
		}
	}
	return nil
}

func (b *ManifestBuilder) addGlobalWorktrees(
	ctx context.Context,
	cfg *models.Config,
	manifest *Manifest,
	projectIndexes map[string]int,
	seenWorktreePaths map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	baseDir := strings.TrimSpace(cfg.Worktree.BaseDir)
	if baseDir == "" {
		return nil
	}

	entries, err := b.discoverGlobalWorktrees(
		baseDir,
		cfg.Projects,
	)
	if err != nil {
		return fmt.Errorf("discover global worktrees: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return globalEntryPath(entries[i]) < globalEntryPath(entries[j])
	})

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry == nil {
			continue
		}
		path, ok := observablePath(entry.Path)
		if !ok || hasSeenPath(seenWorktreePaths, path) {
			continue
		}

		identity, ok := globalProjectIdentity(entry)
		if !ok {
			continue
		}
		localRoot := mainRepositoryPath(path)
		name := globalProjectName(entry, identity, localRoot)
		addProject(manifest, projectIndexes, ProjectManifest{
			Identity:  identity,
			Name:      name,
			LocalRoot: localRoot,
		})

		worktree := models.Worktree{
			Path:       path,
			Branch:     entry.Branch,
			CommitHash: entry.CommitHash,
			IsMain:     entry.IsMain,
		}
		if _, err := b.addWorktree(ctx, cfg, identity, worktree, manifest, seenWorktreePaths); err != nil {
			return err
		}
	}
	return nil
}

func (b *ManifestBuilder) addWorktree(
	ctx context.Context,
	cfg *models.Config,
	projectIdentity string,
	worktree models.Worktree,
	manifest *Manifest,
	seenWorktreePaths map[string]struct{},
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path, ok := observablePath(worktree.Path)
	if !ok || hasSeenPath(seenWorktreePaths, path) {
		return false, nil
	}
	worktree.Path = path

	row, ok, err := buildWorktreeManifest(ctx, cfg, projectIdentity, worktree)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	seenWorktreePaths[canonicalPathKey(path)] = struct{}{}
	manifest.Worktrees = append(manifest.Worktrees, row)
	return true, nil
}

func buildWorktreeManifest(ctx context.Context, cfg *models.Config, projectIdentity string, worktree models.Worktree) (WorktreeManifest, bool, error) {
	if err := ctx.Err(); err != nil {
		return WorktreeManifest{}, false, err
	}

	g := git.New(worktree.Path)
	head, err := gitOutput(ctx, g, "rev-parse", "HEAD")
	if err != nil || head == "" {
		if isContextError(err) {
			return WorktreeManifest{}, false, err
		}
		return WorktreeManifest{}, false, nil
	}
	headTime, err := gitTime(ctx, g, "show", "-s", "--format=%cI", "HEAD")
	if err != nil {
		if isContextError(err) {
			return WorktreeManifest{}, false, err
		}
		return WorktreeManifest{}, false, nil
	}

	branch := strings.TrimSpace(worktree.Branch)
	if branch == "" {
		var branchErr error
		branch, branchErr = gitOutput(ctx, g, "rev-parse", "--abbrev-ref", "HEAD")
		if isContextError(branchErr) {
			return WorktreeManifest{}, false, branchErr
		}
	}

	kind := "branch"
	ref := branch
	if branch == "" || branch == "HEAD" {
		kind = "detached"
		ref = head
		branch = ""
	}

	upstream := ""
	ahead := 0
	behind := 0
	if kind == "branch" {
		var upstreamErr error
		upstream, upstreamErr = gitOutput(ctx, g, "rev-parse", "--abbrev-ref", branch+"@{upstream}")
		if isContextError(upstreamErr) {
			return WorktreeManifest{}, false, upstreamErr
		}
		if upstream != "" {
			ahead, err = countRevList(ctx, g, upstream+"..HEAD")
			if err != nil {
				return WorktreeManifest{}, false, err
			}
			behind, err = countRevList(ctx, g, "HEAD.."+upstream)
			if err != nil {
				return WorktreeManifest{}, false, err
			}
		}
	}

	changeStatus, lastActivity, err := collectChangeStatus(ctx, cfg, worktree, branch)
	if err != nil {
		return WorktreeManifest{}, false, err
	}
	return WorktreeManifest{
		ProjectIdentity: projectIdentity,
		Kind:            kind,
		Ref:             ref,
		Branch:          branch,
		Path:            worktree.Path,
		Head:            head,
		HeadTime:        headTime,
		Upstream:        upstream,
		Ahead:           ahead,
		Behind:          behind,
		Status:          changeStatus,
		LastActivity:    lastActivity,
		IsMain:          worktree.IsMain,
	}, true, nil
}

func collectChangeStatus(ctx context.Context, cfg *models.Config, worktree models.Worktree, branch string) (ChangeStatus, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return ChangeStatus{}, time.Time{}, err
	}
	worktree.Branch = branch
	collector := status.NewStatusCollectorWithOptions(status.StatusCollectorOptions{
		FetchRemote:    false,
		BaseDir:        cfg.Worktree.BaseDir,
		ProtectedNames: credentials.ProtectedNames(cfg),
	})
	collection, err := collector.CollectAll(ctx, []*models.Worktree{&worktree})
	if err != nil || len(collection.Statuses) == 0 || collection.Statuses[0] == nil {
		if isContextError(err) {
			return ChangeStatus{}, time.Time{}, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ChangeStatus{}, time.Time{}, ctxErr
		}
		return ChangeStatus{}, time.Time{}, nil
	}
	if len(collection.Diagnostics) > 0 {
		return ChangeStatus{}, time.Time{}, fmt.Errorf(
			"collect status for %s: %w", worktree.Path, collection.Diagnostics[0].Err,
		)
	}
	gitStatus := collection.Statuses[0].GitStatus
	return ChangeStatus{
		Modified:  gitStatus.Modified,
		Added:     gitStatus.Added,
		Deleted:   gitStatus.Deleted,
		Untracked: gitStatus.Untracked,
		Staged:    gitStatus.Staged,
		Conflicts: gitStatus.Conflicts,
	}, collection.Statuses[0].LastActivity, nil
}

// configuredProjectIdentity resolves a publishable identity: a configured
// registry identity that passes the canonical bar is authoritative, otherwise
// the remote must pass the provenance-aware remote bar. remoteURL is a
// thunk so callers whose remote costs a git subprocess only pay it when the
// stored identity does not already qualify.
func configuredProjectIdentity(configuredRepository string, remoteURL func() string) (string, bool) {
	if identity, ok := repositoryurl.CanonicalRepositoryIdentity(configuredRepository); ok {
		return identity, true
	}
	return repositoryurl.CanonicalRepositoryIdentityFromRemote(remoteURL())
}

func globalProjectIdentity(entry *discovery.GlobalWorktreeEntry) (string, bool) {
	configured := ""
	if entry.RepositoryInfo != nil {
		configured = entry.RepositoryInfo.FullPath
	}
	return configuredProjectIdentity(configured, func() string { return entry.RepositoryURL })
}

func projectDisplayName(configuredName, identity, projectPath string) string {
	if strings.TrimSpace(configuredName) != "" {
		return strings.TrimSpace(configuredName)
	}
	if identity != "" {
		parts := strings.Split(filepath.ToSlash(identity), "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1]
		}
	}
	return filepath.Base(projectPath)
}

func globalProjectName(entry *discovery.GlobalWorktreeEntry, identity, localRoot string) string {
	if entry.RepositoryInfo != nil && strings.TrimSpace(entry.RepositoryInfo.Repository) != "" {
		return strings.TrimSpace(entry.RepositoryInfo.Repository)
	}
	return projectDisplayName("", identity, localRoot)
}

func globalEntryPath(entry *discovery.GlobalWorktreeEntry) string {
	if entry == nil {
		return ""
	}
	return entry.Path
}

func addProject(manifest *Manifest, indexes map[string]int, project ProjectManifest) {
	if project.Identity == "" {
		return
	}
	if idx, ok := indexes[project.Identity]; ok {
		existing := &manifest.Projects[idx]
		if existing.Name == "" {
			existing.Name = project.Name
		}
		if existing.LocalRoot == "" {
			existing.LocalRoot = project.LocalRoot
		}
		return
	}
	indexes[project.Identity] = len(manifest.Projects)
	manifest.Projects = append(manifest.Projects, project)
}

func observablePath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	if absPath, err := filepath.Abs(path); err == nil {
		path = absPath
	}
	if resolvedPath, err := filepath.EvalSymlinks(path); err == nil {
		path = resolvedPath
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return filepath.Clean(path), true
}

func hasSeenPath(seen map[string]struct{}, path string) bool {
	_, ok := seen[canonicalPathKey(path)]
	return ok
}

func canonicalPathKey(path string) string {
	return utils.CanonicalPath(path)
}

func remoteURLForPath(path string) string {
	remoteURL, err := git.New(path).GetRepositoryURL()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(remoteURL)
}

func mainRepositoryPath(path string) string {
	root, err := git.New(path).GetMainRepositoryPath()
	if err != nil {
		return path
	}
	if normalized, ok := observablePath(root); ok {
		return normalized
	}
	return filepath.Clean(root)
}

func gitOutput(ctx context.Context, g *git.Git, args ...string) (string, error) {
	output, err := g.RunWithContext(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func gitTime(ctx context.Context, g *git.Git, args ...string) (time.Time, error) {
	output, err := gitOutput(ctx, g, args...)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, output)
}

func countRevList(ctx context.Context, g *git.Git, revRange string) (int, error) {
	output, err := gitOutput(ctx, g, "rev-list", "--count", revRange)
	if err != nil {
		if isContextError(err) {
			return 0, err
		}
		return 0, nil
	}
	count, err := strconv.Atoi(output)
	if err != nil {
		return 0, nil
	}
	return count, nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
