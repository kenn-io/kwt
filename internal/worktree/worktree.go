// Package worktree provides high-level worktree management functionality.
package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/template"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

// GitInterface defines the git operations used by Manager.
type GitInterface interface {
	ListWorktrees() ([]models.Worktree, error)
	AddWorktree(path, branch string, createBranch bool) error
	AddWorktreeExisting(path, branch string, protectedNames []string) error
	AddWorktreeTracking(
		path, branch, remoteBranch string,
		protectedNames []string,
	) error
	AddWorktreeFromBase(path, branch, baseBranch string) error
	RemoveWorktree(path string, force bool) error
	DeleteBranch(branch string, force bool) error
	PruneWorktrees() error
	GetRepositoryName() (string, error)
	GetRecentCommits(path string, limit int) ([]models.CommitInfo, error)
	GetRepositoryURL() (string, error)
	GetMainRepositoryPath() (string, error)
}

// Manager handles worktree operations.
type Manager struct {
	git                   GitInterface
	config                *models.Config
	openRemoteSourceState func() (remoteSourceState, error)
}

type remoteSourceState interface {
	Register(*registry.WorktreeEntry) error
	Unregister(string) error
	Get(string) (*registry.WorktreeEntry, bool)
}

// AddOptions controls optional behavior for creating a worktree.
type AddOptions struct {
	SkipSetup bool
}

// New creates a new worktree Manager.
func New(g GitInterface, config *models.Config) *Manager {
	return &Manager{
		git:    g,
		config: config,
		openRemoteSourceState: func() (remoteSourceState, error) {
			return registry.New()
		},
	}
}

// Add creates a new worktree and returns the path of the created worktree.
func (m *Manager) Add(branch string, customPath string, createBranch bool) (string, error) {
	return m.AddWithOptions(branch, customPath, createBranch, AddOptions{})
}

// AddWithOptions creates a new worktree and returns the path of the created worktree.
func (m *Manager) AddWithOptions(branch string, customPath string, createBranch bool, opts AddOptions) (string, error) {
	if !createBranch {
		return m.addExisting(branch, customPath)
	}

	path, err := m.preparePath(customPath, branch, nil)
	if err != nil {
		return "", err
	}

	if err := m.git.AddWorktree(path, branch, createBranch); err != nil {
		return "", err
	}

	if !opts.SkipSetup {
		m.runPostWorktreeSetup(branch, path)
	}
	return path, nil
}

func (m *Manager) addExisting(branch, customPath string) (string, error) {
	path, err := m.preparePath(customPath, branch, nil)
	if err != nil {
		return "", err
	}
	return m.addUnreviewedSource(path, branch, func() error {
		return m.git.AddWorktreeExisting(
			path,
			branch,
			credentials.ProtectedNames(m.config),
		)
	})
}

// AddTracking creates a worktree on a local branch that tracks remoteBranch.
// Repository setup is intentionally deferred: the remote checkout is
// untrusted until the user has reviewed it.
func (m *Manager) AddTracking(branch, remoteBranch, customPath string) (string, error) {
	path, err := m.preparePath(customPath, branch, nil)
	if err != nil {
		return "", err
	}

	return m.addUnreviewedSource(path, branch, func() error {
		return m.git.AddWorktreeTracking(
			path,
			branch,
			remoteBranch,
			credentials.ProtectedNames(m.config),
		)
	})
}

func (m *Manager) addUnreviewedSource(
	path, branch string,
	add func() error,
) (string, error) {
	state, err := m.openRemoteSourceState()
	if err != nil {
		return "", fmt.Errorf("open remote-source state: %w", err)
	}
	previous, existed := state.Get(path)
	entry := &registry.WorktreeEntry{
		Branch:                 branch,
		Path:                   path,
		UnreviewedRemoteSource: true,
	}
	if err := state.Register(entry); err != nil {
		return "", fmt.Errorf("mark remote-source worktree unreviewed: %w", err)
	}

	if err := add(); err != nil {
		var restoreErr error
		switch {
		case existed:
			restoreErr = state.Register(previous)
		default:
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				restoreErr = state.Unregister(path)
			}
		}
		if restoreErr != nil {
			return "", fmt.Errorf(
				"%w (failed to restore remote-source state: %v)",
				err,
				restoreErr,
			)
		}
		return "", err
	}

	return path, nil
}

// AddFromBase creates a new worktree with a branch from a specific base branch
// and returns the path of the created worktree.
func (m *Manager) AddFromBase(branch string, baseBranch string, customPath string) (string, error) {
	path, err := m.preparePath(customPath, branch, nil)
	if err != nil {
		return "", err
	}

	if err := m.git.AddWorktreeFromBase(path, branch, baseBranch); err != nil {
		return "", err
	}

	m.runPostWorktreeSetup(branch, path)
	return path, nil
}

// Remove deletes a worktree.
func (m *Manager) Remove(path string, force bool) error {
	return m.git.RemoveWorktree(path, force)
}

// RemoveWithBranch deletes a worktree and optionally its branch.
func (m *Manager) RemoveWithBranch(path string, branch string, forceWorktree bool, deleteBranch bool, forceBranch bool) error {
	// First remove the worktree
	if err := m.git.RemoveWorktree(path, forceWorktree); err != nil {
		return err
	}

	// Then delete the branch if requested
	if deleteBranch && branch != "" {
		if err := m.git.DeleteBranch(branch, forceBranch); err != nil {
			// Return error but worktree is already removed
			return fmt.Errorf("worktree removed but failed to delete branch: %w", err)
		}
	}

	return nil
}

// List returns all worktrees.
func (m *Manager) List() ([]models.Worktree, error) {
	return m.git.ListWorktrees()
}

// Prune removes worktree information for deleted directories.
func (m *Manager) Prune() error {
	return m.git.PruneWorktrees()
}

// GetWorktreePath returns the path for a worktree by pattern matching.
func (m *Manager) GetWorktreePath(pattern string) (string, error) {
	worktrees, err := m.List()
	if err != nil {
		return "", err
	}

	pattern = strings.ToLower(pattern)
	for _, wt := range worktrees {
		if strings.Contains(strings.ToLower(wt.Branch), pattern) ||
			strings.Contains(strings.ToLower(wt.Path), pattern) {
			return wt.Path, nil
		}
	}

	return "", fmt.Errorf("no worktree found matching pattern: %s", pattern)
}

// GetMatchingWorktrees returns all worktrees matching the given pattern.
func (m *Manager) GetMatchingWorktrees(pattern string) ([]models.Worktree, error) {
	worktrees, err := m.List()
	if err != nil {
		return nil, err
	}

	var matches []models.Worktree
	pattern = strings.ToLower(pattern)
	for _, wt := range worktrees {
		if strings.Contains(strings.ToLower(wt.Branch), pattern) ||
			strings.Contains(strings.ToLower(wt.Path), pattern) {
			matches = append(matches, wt)
		}
	}

	return matches, nil
}

// ValidateWorktreePath checks if a path can be used for a new worktree.
func (m *Manager) ValidateWorktreePath(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return fmt.Errorf("failed to read directory: %w", err)
			}
			if len(entries) > 0 {
				return fmt.Errorf("directory is not empty: %s", path)
			}
		} else {
			return fmt.Errorf("path exists and is not a directory: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check path: %w", err)
	}

	return nil
}

// PreparePath applies KWT's configured naming and destination policy without
// creating a worktree. Callers can then hand the resolved path to a shared
// lifecycle implementation.
func (m *Manager) PreparePath(customPath, branch string) (string, error) {
	return m.preparePath(customPath, branch, nil)
}

// PreparePathForRepository applies KWT's destination policy using an explicit
// registered repository identity rather than inferring it from the origin
// remote.
func (m *Manager) PreparePathForRepository(
	customPath, branch, repository string,
) (string, error) {
	repoInfo, ok := url.CanonicalRepositoryInfo(repository)
	if !ok {
		return "", fmt.Errorf("invalid repository identity %q", repository)
	}
	return m.preparePath(customPath, branch, repoInfo)
}

// preparePath resolves and prepares the worktree path, creating parent directories if needed.
func (m *Manager) preparePath(
	customPath, branch string, repoInfo *url.RepositoryInfo,
) (string, error) {
	path := customPath
	generated := path == ""
	if generated {
		generatedPath, err := m.generateWorktreePathForRepository(branch, repoInfo)
		if err != nil {
			return "", fmt.Errorf("failed to generate worktree path: %w", err)
		}
		path = generatedPath
	}

	if !generated {
		expandedPath, err := utils.ExpandPath(path)
		if err != nil {
			return "", fmt.Errorf("failed to expand path: %w", err)
		}
		path = expandedPath
	}

	if err := tmux.ValidateStartDirectory(path); err != nil {
		return "", err
	}

	if m.config.Worktree.AutoMkdir {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create directory: %w", err)
		}
	}

	return path, nil
}

// generateWorktreePath generates a path for a new worktree using template configuration.
func (m *Manager) generateWorktreePath(branch string) (string, error) {
	return m.generateWorktreePathForRepository(branch, nil)
}

func (m *Manager) generateWorktreePathForRepository(
	branch string, repoInfo *url.RepositoryInfo,
) (string, error) {
	if repoInfo == nil {
		var err error
		repoInfo, err = m.repositoryInfo()
		if err != nil {
			return "", err
		}
	}
	branch = encodeBranchForWorktreePath(branch)

	// Determine effective base directory: per-repo setting overrides global
	baseDir := m.config.Worktree.BaseDir
	if len(m.config.RepositorySettings) > 0 {
		repoRoot, err := m.git.GetMainRepositoryPath()
		if err != nil {
			return "", fmt.Errorf("failed to get repository path: %w", err)
		}
		if setting := findRepoSetting(m.config.RepositorySettings, repoRoot); setting != nil && setting.BaseDir != "" {
			baseDir = setting.BaseDir
		}
	}
	namingTemplate := m.config.Naming.Template
	expandedBaseDir, err := utils.ExpandPath(baseDir)
	if err != nil {
		return "", fmt.Errorf("failed to expand worktree base: %w", err)
	}
	baseDir = expandedBaseDir

	// Use template if configured, otherwise fall back to default URL hierarchy
	if namingTemplate != "" {
		// Create template processor
		naming := m.config.Naming
		environmentOptions := template.EnvironmentOptions{
			ExpandTemplate:      !naming.TemplateRepositoryLocal,
			ExpandSanitizeChars: !naming.SanitizeCharsRepositoryLocal,
		}
		processor, err := template.NewWithEnvironmentOptions(
			namingTemplate,
			naming.SanitizeChars,
			environmentOptions,
		)
		if err != nil {
			// Fall back to default hierarchy if template is invalid
			return url.GenerateWorktreePath(baseDir, repoInfo, branch), nil
		}

		// Generate path using template
		path, err := processor.GeneratePath(baseDir, repoInfo, branch)
		if err != nil {
			// Fall back to default hierarchy if template execution fails
			path = url.GenerateWorktreePath(baseDir, repoInfo, branch)
			return path, ensurePathWithinBase(baseDir, path)
		}

		return path, ensurePathWithinBase(baseDir, path)
	}

	// Fall back to default URL hierarchy
	path := url.GenerateWorktreePath(baseDir, repoInfo, branch)
	return path, ensurePathWithinBase(baseDir, path)
}

func encodeBranchForWorktreePath(branch string) string {
	return strings.NewReplacer("%", "%25", "#", "%23").Replace(branch)
}

func (m *Manager) repositoryInfo() (*url.RepositoryInfo, error) {
	return RepositoryInfoFromGit(m.git)
}

// RepoIdentityGit is the minimal git surface RepositoryInfoFromGit needs: the
// origin URL and the main-repository path. GitInterface satisfies it, and so
// does *git.Git, so the single canonical resolver serves every surface that
// reports repository identity (kwt list, kwt list -g discovery, and project
// registration) rather than each re-deriving a divergent fallback.
type RepoIdentityGit interface {
	GetRepositoryURL() (string, error)
	GetMainRepositoryPath() (string, error)
}

// CachedIdentityGit memoizes the RepoIdentityGit surface so one identity
// resolution pass costs at most one `git remote get-url` and one
// `git rev-parse` subprocess, however many consumers ask. Discovery, for
// example, records the remote URL on the entry and then resolves identity
// through the same URL; without the cache each read is a fresh subprocess.
type CachedIdentityGit struct {
	g        RepoIdentityGit
	urlOnce  bool
	url      string
	urlErr   error
	mainOnce bool
	main     string
	mainErr  error
}

// NewCachedIdentityGit wraps g with per-call memoization. Not safe for
// concurrent use; create one per resolution pass.
func NewCachedIdentityGit(g RepoIdentityGit) *CachedIdentityGit {
	return &CachedIdentityGit{g: g}
}

func (c *CachedIdentityGit) GetRepositoryURL() (string, error) {
	if !c.urlOnce {
		c.urlOnce = true
		c.url, c.urlErr = c.g.GetRepositoryURL()
	}
	return c.url, c.urlErr
}

func (c *CachedIdentityGit) GetMainRepositoryPath() (string, error) {
	if !c.mainOnce {
		c.mainOnce = true
		c.main, c.mainErr = c.g.GetMainRepositoryPath()
	}
	return c.main, c.mainErr
}

// RepositoryInfoFromGit returns repository identity from origin when the
// origin passes the remote-derived canonical bar
// (url.CanonicalRepositoryInfoFromRemote), falling back to a path-safe local
// identity (a "local/..." full path) otherwise. Raw `git remote get-url`
// output is ambiguous: git accepts a relative filesystem path with no leading
// "./" ("cache/team/repo.git"), which must not launder into a
// shareable-looking identity. This is the single canonical resolver; all
// identity-reporting surfaces route through it so a repository without a
// usable (barred or missing) remote gets the same identity everywhere.
func RepositoryInfoFromGit(g RepoIdentityGit) (*url.RepositoryInfo, error) {
	repoURL, urlErr := g.GetRepositoryURL()
	if urlErr == nil {
		if repoInfo, ok := url.CanonicalRepositoryInfoFromRemote(repoURL); ok {
			return repoInfo, nil
		}
		urlErr = fmt.Errorf(
			"origin %q does not qualify as a canonical repository identity", repoURL)
	} else {
		urlErr = fmt.Errorf("failed to get repository URL: %w", urlErr)
	}

	repoRoot, pathErr := g.GetMainRepositoryPath()
	if pathErr != nil {
		return nil, fmt.Errorf("%v; failed to get repository path for local fallback: %w", urlErr, pathErr)
	}
	repoInfo, pathErr := RepositoryInfoFromLocalPath(repoRoot)
	if pathErr != nil {
		return nil, fmt.Errorf("%v; failed to build local repository identity: %w", urlErr, pathErr)
	}
	return repoInfo, nil
}

// RepositoryInfoWithProjects extends the canonical resolver with the single
// registered-identity precedence policy: when the repository's main path
// matches a registered project whose configured Repository is canonical, the
// REGISTERED identity wins, so a checkout whose origin is a fork still
// reports the project's canonical identity on every surface (list enrichment,
// global discovery, session-name derivation) and joins with the
// registry-backed `projects` surface. For unregistered repositories — or a
// registered project whose identity is not canonical —
// RepositoryInfoFromGit's origin-then-local precedence applies unchanged.
func RepositoryInfoWithProjects(
	g RepoIdentityGit, projects []models.Project,
) (*url.RepositoryInfo, error) {
	if info, ok := registeredProjectIdentity(g, projects); ok {
		return info, nil
	}
	return RepositoryInfoFromGit(g)
}

// registeredProjectIdentity returns the canonical identity pinned by a
// registered project whose Path is the repository's main path, if any.
func registeredProjectIdentity(
	g RepoIdentityGit, projects []models.Project,
) (*url.RepositoryInfo, bool) {
	if len(projects) == 0 {
		return nil, false
	}
	mainPath, err := g.GetMainRepositoryPath()
	if err != nil {
		return nil, false
	}
	mainPath = utils.CanonicalPath(mainPath)
	for _, project := range projects {
		if project.Path == "" || utils.CanonicalPath(project.Path) != mainPath {
			continue
		}
		if info, ok := url.CanonicalRepositoryInfo(project.Repository); ok {
			return info, true
		}
	}
	return nil, false
}

// RepositoryInfoFromLocalPath builds the path-safe local identity ("local/..."
// full path) for a repository root that has no usable remote. It is the raw-path
// entry point to the same canonical local fallback RepositoryInfoFromGit uses,
// for callers that hold a directory path rather than a git surface.
func RepositoryInfoFromLocalPath(repoRoot string) (*url.RepositoryInfo, error) {
	repoRoot = strings.TrimSpace(repoRoot)
	if repoRoot == "" {
		return nil, fmt.Errorf("empty repository path")
	}
	cleanPath := repoRoot
	if absPath, err := filepath.Abs(cleanPath); err == nil {
		cleanPath = absPath
	} else {
		return nil, err
	}
	if resolvedPath, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = resolvedPath
	}
	name := filepath.Base(cleanPath)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = filepath.ToSlash(cleanPath)
	}
	return &url.RepositoryInfo{
		Repository: name,
		FullPath:   localRepositoryFullPath(cleanPath),
	}, nil
}

func localRepositoryFullPath(cleanPath string) string {
	slashPath := slashLocalPath(cleanPath)
	volumeName, slashPath := splitLocalPathVolume(slashPath)
	slashPath = strings.TrimLeft(slashPath, "/")

	parts := []string{"local"}
	if volumeName != "" {
		volumeName = strings.Trim(volumeName, "/")
		if volumeName != "" {
			parts = append(parts, volumeName)
		}
	}
	if slashPath != "" {
		parts = append(parts, slashPath)
	}
	return strings.Join(parts, "/")
}

func slashLocalPath(cleanPath string) string {
	slashPath := filepath.ToSlash(cleanPath)
	if runtime.GOOS == "windows" || isWindowsPathLiteral(cleanPath) {
		return strings.ReplaceAll(slashPath, `\`, "/")
	}
	slashPath = strings.ReplaceAll(slashPath, "%", "%25")
	return strings.ReplaceAll(slashPath, `\`, "%5C")
}

func isWindowsPathLiteral(cleanPath string) bool {
	if len(cleanPath) >= 2 && cleanPath[1] == ':' {
		return true
	}
	return strings.HasPrefix(cleanPath, `\\`) || strings.HasPrefix(cleanPath, "//")
}

func splitLocalPathVolume(slashPath string) (string, string) {
	if len(slashPath) >= 2 && slashPath[1] == ':' {
		return slashPath[:1], slashPath[2:]
	}
	if strings.HasPrefix(slashPath, "//") {
		rest := strings.TrimLeft(slashPath, "/")
		host, rest, ok := strings.Cut(rest, "/")
		if !ok {
			return host, ""
		}
		share, rest, ok := strings.Cut(rest, "/")
		if !ok {
			return host + "/" + share, ""
		}
		return host + "/" + share, rest
	}
	return "", slashPath
}

func ensurePathWithinBase(baseDir, path string) error {
	baseResolved, err := resolveExistingPathPrefix(baseDir)
	if err != nil {
		return fmt.Errorf("failed to resolve worktree base path: %w", err)
	}
	pathResolved, err := resolveExistingPathPrefix(path)
	if err != nil {
		return fmt.Errorf("failed to resolve generated worktree path: %w", err)
	}
	rel, err := filepath.Rel(baseResolved, pathResolved)
	if err != nil {
		return fmt.Errorf("failed to compare generated path with worktree base: %w", err)
	}
	if rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return nil
	}
	return fmt.Errorf("generated worktree path %s is outside worktree base %s", path, baseDir)
}

func resolveExistingPathPrefix(rawPath string) (string, error) {
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		return "", err
	}

	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	current := absPath
	var missingParts []string
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absPath), nil
		}
		missingParts = append(missingParts, filepath.Base(current))
		current = parent

		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missingParts) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missingParts[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
}
