package discovery

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

// TestRepository creates a test git repository (copy from git package for testing)
type TestRepository struct {
	Path string
}

// NewTestRepository creates a new test repository
func NewTestRepository(t *testing.T) *TestRepository {
	t.Helper()

	tmpDir := t.TempDir()
	repo := &TestRepository{Path: tmpDir}

	// Set environment variables for git if needed in CI
	t.Setenv("GIT_AUTHOR_NAME", "Test User")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test User")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	// Initialize repository with main as default branch
	if err := repo.run("init", "-b", "main"); err != nil {
		t.Fatalf("Failed to init repository: %v", err)
	}

	// Configure git user for commits
	if err := repo.run("config", "user.name", "Test User"); err != nil {
		t.Fatalf("Failed to set user.name: %v", err)
	}
	if err := repo.run("config", "user.email", "test@example.com"); err != nil {
		t.Fatalf("Failed to set user.email: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "README.md")
	if err := os.WriteFile(testFile, []byte("# Test Repository\n"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	if err := repo.run("add", "."); err != nil {
		t.Fatalf("Failed to add files: %v", err)
	}
	if err := repo.run("commit", "-m", "Initial commit"); err != nil {
		t.Fatalf("Failed to create initial commit: %v", err)
	}

	return repo
}

// run executes a git command in the test repository
func (r *TestRepository) run(args ...string) error {
	g := git.New(r.Path)
	_, err := g.RunCommand(args...)
	return err
}

// CreateBranch creates a new branch in the test repository
func (r *TestRepository) CreateBranch(t *testing.T, name string) {
	t.Helper()
	if err := r.run("checkout", "-b", name); err != nil {
		t.Fatalf("Failed to create branch %s: %v", name, err)
	}
}

// CreateWorktree creates a worktree in the test repository
func (r *TestRepository) CreateWorktree(t *testing.T, path, branch string) {
	t.Helper()
	// First check if branch exists in current worktree, if so switch away
	currentBranch, _ := r.getCurrentBranch()
	if currentBranch == branch {
		// Try to switch to main branch first
		if err := r.run("checkout", "main"); err != nil {
			// If main doesn't exist or we're already on it, create a temporary branch
			if err := r.run("checkout", "-b", "temp-branch-"+branch); err != nil {
				t.Fatalf("Failed to switch away from branch: %v", err)
			}
		}
	}

	if err := r.run("worktree", "add", path, branch); err != nil {
		t.Fatalf("Failed to create worktree: %v", err)
	}
}

func (r *TestRepository) getCurrentBranch() (string, error) {
	g := git.New(r.Path)
	output, err := g.RunCommand("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// AddRemote adds a remote to the repository
func (r *TestRepository) AddRemote(t *testing.T, name, url string) {
	t.Helper()
	if err := r.run("remote", "add", name, url); err != nil {
		t.Fatalf("Failed to add remote %s: %v", name, err)
	}
}

func TestDiscoverGlobalWorktrees_EmptyBaseDir(t *testing.T) {
	entries, err := DiscoverGlobalWorktrees("", nil)
	if err == nil {
		t.Error("Expected error for empty base directory")
	}
	if entries != nil {
		t.Error("Expected nil entries for empty base directory")
	}
}

func TestDiscoverGlobalWorktrees_NonExistentBaseDir(t *testing.T) {
	entries, err := DiscoverGlobalWorktrees("/nonexistent/path", nil)
	if err != nil {
		t.Errorf("Unexpected error for non-existent directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Expected empty entries for non-existent directory, got %d", len(entries))
	}
}

func TestDiscoverGlobalWorktrees_NoWorktrees(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a directory with no git repositories
	subDir := filepath.Join(tmpDir, "not-a-repo")
	err := os.MkdirAll(subDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	entries, err := DiscoverGlobalWorktrees(tmpDir, nil)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Expected no entries, got %d", len(entries))
	}
}

// initRepoAt creates and initializes a git repository at the given directory
// with an initial commit and a remote.
func initRepoAt(t *testing.T, dir, remoteURL string) *TestRepository {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("Failed to create repo directory: %v", err)
	}
	repo := &TestRepository{Path: dir}
	if err := repo.run("init", "-b", "main"); err != nil {
		t.Fatalf("Failed to init: %v", err)
	}
	if err := repo.run("config", "user.name", "Test"); err != nil {
		t.Fatalf("Failed to set user.name: %v", err)
	}
	if err := repo.run("config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("Failed to set user.email: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	if err := repo.run("add", "."); err != nil {
		t.Fatalf("Failed to add: %v", err)
	}
	if err := repo.run("commit", "-m", "init"); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}
	repo.AddRemote(t, "origin", remoteURL)
	return repo
}

func initLocalRepoAt(t *testing.T, dir string) *TestRepository {
	t.Helper()
	repo := initRepoAt(t, dir, "https://example.com/temp/repo.git")
	if err := repo.run("remote", "remove", "origin"); err != nil {
		t.Fatalf("Failed to remove origin: %v", err)
	}
	return repo
}

func TestDiscoverGlobalWorktrees_IncludesMainWorktree(t *testing.T) {
	baseDir := t.TempDir()

	repoDir := filepath.Join(baseDir, "github.com", "user", "repo", "main")
	initRepoAt(t, repoDir, "https://github.com/user/repo.git")

	entries, err := DiscoverGlobalWorktrees(baseDir, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}

	if !entries[0].IsMain {
		t.Error("Expected entry to be marked as main worktree")
	}
	if entries[0].Branch != "main" {
		t.Errorf("Expected branch 'main', got '%s'", entries[0].Branch)
	}
}

func TestDiscoverGlobalWorktrees_IncludesLocalOnlyMainWorktree(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "local", "service")
	initLocalRepoAt(t, repoDir)

	entries, err := DiscoverGlobalWorktrees(baseDir, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if !entry.IsMain {
		t.Error("Expected entry to be marked as main worktree")
	}
	if entry.RepositoryURL != "" {
		t.Errorf("Expected no repository URL, got %q", entry.RepositoryURL)
	}
	if entry.RepositoryInfo == nil {
		t.Fatal("Expected fallback repository info")
	}
	if entry.RepositoryInfo.Repository != "service" {
		t.Errorf("Expected fallback repository name service, got %q", entry.RepositoryInfo.Repository)
	}
	// Discovery routes through the single canonical resolver, so a no-remote
	// repository gets the same "local/..." identity that kwt list and project
	// registration report, keeping the JSON surfaces joinable.
	wantInfo, err := worktree.RepositoryInfoFromLocalPath(repoDir)
	if err != nil {
		t.Fatalf("building expected local identity: %v", err)
	}
	if entry.RepositoryInfo.FullPath != wantInfo.FullPath {
		t.Errorf("Expected fallback full path %q, got %q", wantInfo.FullPath, entry.RepositoryInfo.FullPath)
	}
}

// TestDiscoverGlobalWorktrees_RegisteredIdentityWinsForForkOrigin pins the
// registered-identity precedence on the discovery surface: a repository whose
// origin is a fork but whose main path is registered as a project with a
// canonical upstream identity reports the REGISTERED identity (matching the
// registry-backed projects surface and the session names other paths derive),
// while the raw origin URL is still carried alongside.
func TestDiscoverGlobalWorktrees_RegisteredIdentityWinsForForkOrigin(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "github.com", "fork", "repo", "main")
	repo := initRepoAt(t, repoDir, "https://github.com/fork/repo.git")

	repo.CreateBranch(t, "feature")
	worktreePath := filepath.Join(baseDir, "github.com", "fork", "repo", "feature")
	repo.CreateWorktree(t, worktreePath, "feature")

	projects := []models.Project{
		{Repository: "github.com/upstream/repo", Path: repoDir},
	}
	entries, err := DiscoverGlobalWorktrees(baseDir, projects)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(entries))
	}
	for _, entry := range entries {
		if entry.RepositoryInfo == nil {
			t.Fatalf("entry %s: missing repository info", entry.Path)
		}
		if entry.RepositoryInfo.FullPath != "github.com/upstream/repo" {
			t.Errorf("entry %s: FullPath = %q, want registered identity github.com/upstream/repo",
				entry.Path, entry.RepositoryInfo.FullPath)
		}
		if entry.RepositoryURL != "https://github.com/fork/repo.git" {
			t.Errorf("entry %s: RepositoryURL = %q, want raw origin preserved",
				entry.Path, entry.RepositoryURL)
		}
		wantSession := tmux.WorkspaceSessionName(entry.RepositoryInfo, entry.Branch, entry.Path)
		if got := entry.Model().SessionName; got != wantSession {
			t.Errorf("entry %s: SessionName = %q, want %q", entry.Path, got, wantSession)
		}
	}
}

func TestDiscoverWorktreeResolvesLinkedPathOutsideGlobalBase(t *testing.T) {
	mainDir := filepath.Join(t.TempDir(), "registered", "widget")
	repo := initRepoAt(t, mainDir, "https://github.com/fork/widget.git")
	repo.CreateBranch(t, "feature")
	worktreePath := filepath.Join(t.TempDir(), "external", "feature")
	repo.CreateWorktree(t, worktreePath, "feature")

	entry, err := DiscoverWorktree(worktreePath, []models.Project{{
		Repository: "github.com/acme/widget",
		Path:       mainDir,
	}})

	if err != nil {
		t.Fatalf("DiscoverWorktree() error = %v", err)
	}
	if utils.CanonicalPath(entry.Path) != utils.CanonicalPath(worktreePath) {
		t.Errorf("Path = %q, want %q", entry.Path, worktreePath)
	}
	if entry.RepositoryInfo == nil {
		t.Fatal("RepositoryInfo = nil")
	}
	if entry.RepositoryInfo.FullPath != "github.com/acme/widget" {
		t.Errorf(
			"RepositoryInfo.FullPath = %q, want github.com/acme/widget",
			entry.RepositoryInfo.FullPath,
		)
	}
}

func TestDiscoverGlobalWorktrees_MainAndLinkedWorktrees(t *testing.T) {
	baseDir := t.TempDir()

	repoDir := filepath.Join(baseDir, "github.com", "user", "repo", "main")
	repo := initRepoAt(t, repoDir, "https://github.com/user/repo.git")

	// Create a branch and linked worktree
	repo.CreateBranch(t, "feature")
	if err := repo.run("checkout", "main"); err != nil {
		t.Fatalf("Failed to checkout main: %v", err)
	}
	worktreeDir := filepath.Join(baseDir, "github.com", "user", "repo", "feature")
	repo.CreateWorktree(t, worktreeDir, "feature")

	entries, err := DiscoverGlobalWorktrees(baseDir, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(entries))
	}

	var mainCount, linkedCount int
	for _, e := range entries {
		if e.IsMain {
			mainCount++
		} else {
			linkedCount++
		}
	}
	if mainCount != 1 {
		t.Errorf("Expected 1 main worktree, got %d", mainCount)
	}
	if linkedCount != 1 {
		t.Errorf("Expected 1 linked worktree, got %d", linkedCount)
	}
}

func TestDiscoverGlobalWorktreesReportsRepositorySnapshotFailure(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "repo", "main")
	repo := initRepoAt(t, repoDir, "https://github.com/user/repo.git")
	repo.CreateBranch(t, "feature")
	worktreeDir := filepath.Join(baseDir, "repo", "feature")
	repo.CreateWorktree(t, worktreeDir, "feature")

	dotGit, err := os.ReadFile(filepath.Join(worktreeDir, ".git"))
	if err != nil {
		t.Fatalf("read linked worktree .git file: %v", err)
	}
	gitDir := strings.TrimSpace(
		strings.TrimPrefix(strings.TrimSpace(string(dotGit)), "gitdir: "),
	)
	if err := os.Mkdir(
		filepath.Join(gitDir, "kwt-generation"),
		0o700,
	); err != nil {
		t.Fatalf("block worktree generation initialization: %v", err)
	}

	entries, err := DiscoverGlobalWorktrees(baseDir, nil)

	if err == nil {
		t.Fatalf(
			"DiscoverGlobalWorktrees() entries = %v, want snapshot error",
			entries,
		)
	}
}

func TestDiscoverGlobalWorktreesListsEachRepositoryOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell wrapper")
	}

	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "github.com", "user", "repo", "main")
	repo := initRepoAt(t, repoDir, "https://github.com/user/repo.git")
	for _, branch := range []string{"feature-a", "feature-b"} {
		repo.CreateBranch(t, branch)
		repo.CreateWorktree(
			t,
			filepath.Join(baseDir, "github.com", "user", "repo", branch),
			branch,
		)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git executable: %v", err)
	}
	countPath := filepath.Join(t.TempDir(), "worktree-list-count")
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	wrapper := `#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "list" ]; then
	printf '1\n' >> "$WORKTREE_LIST_COUNT"
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0755); err != nil {
		t.Fatalf("write git wrapper: %v", err)
	}
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("WORKTREE_LIST_COUNT", countPath)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	entries, err := DiscoverGlobalWorktrees(baseDir, nil)

	if err != nil {
		t.Fatalf("DiscoverGlobalWorktrees() error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(entries))
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read worktree list count: %v", err)
	}
	if got := strings.Count(string(count), "\n"); got != 1 {
		t.Fatalf("git worktree list calls = %d, want 1", got)
	}
}

func TestDiscoverGlobalWorktrees_DoesNotDescendIntoMainRepo(t *testing.T) {
	baseDir := t.TempDir()

	repoDir := filepath.Join(baseDir, "repo")
	initRepoAt(t, repoDir, "https://github.com/user/repo.git")

	// SkipDir on the main repo means nothing inside it (submodules, nested
	// repos, etc.) is ever visited. Verify only the main worktree is found.
	entries, err := DiscoverGlobalWorktrees(baseDir, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry (main only), got %d", len(entries))
	}
	if !entries[0].IsMain {
		t.Error("Expected entry to be marked as main worktree")
	}
}

func TestDiscoverGlobalWorktrees_DoesNotDescendIntoLinkedWorktree(t *testing.T) {
	baseDir := t.TempDir()
	mainDir := filepath.Join(baseDir, "repo", "main")
	repo := initRepoAt(t, mainDir, "https://github.com/user/repo.git")
	repo.CreateBranch(t, "feature")
	linkedDir := filepath.Join(baseDir, "repo", "feature")
	repo.CreateWorktree(t, linkedDir, "feature")

	// Linked worktrees can contain dependency checkouts or other nested Git
	// repositories. Those implementation details are not kwt worktrees.
	nestedDir := filepath.Join(linkedDir, ".build", "checkouts", "dependency")
	initRepoAt(t, nestedDir, "https://github.com/example/dependency.git")

	entries, err := DiscoverGlobalWorktrees(baseDir, nil)

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Expected main and linked worktrees only, got %d entries", len(entries))
	}
	for _, entry := range entries {
		if entry.Path == nestedDir {
			t.Fatal("discovery descended into a linked worktree")
		}
	}
}

func TestExtractWorktreeCandidatesRunsConcurrentlyAndPreservesOrder(t *testing.T) {
	candidates := []worktreeCandidate{
		{path: "/worktrees/first"},
		{path: "/worktrees/second"},
	}
	started := make(chan string, len(candidates))
	release := make(chan struct{})
	done := make(chan []*GlobalWorktreeEntry, 1)

	go func() {
		done <- extractWorktreeCandidates(
			candidates,
			nil,
			func(path string, _ []models.Project) (*GlobalWorktreeEntry, error) {
				started <- path
				<-release
				return &GlobalWorktreeEntry{Path: path}, nil
			},
		)
	}()

	for range candidates {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("candidate extraction ran serially")
		}
	}
	close(release)

	entries := <-done
	if len(entries) != 2 {
		t.Fatalf("Expected 2 entries, got %d", len(entries))
	}
	if entries[0].Path != candidates[0].path || entries[1].Path != candidates[1].path {
		t.Fatalf("Candidate order changed: %#v", entries)
	}
}

func TestGetCurrentBranch_InvalidPath(t *testing.T) {
	_, err := getCurrentBranch("/nonexistent/path")
	if err == nil {
		t.Error("Expected error for invalid path")
	}
}

func TestGetCurrentCommitHash_InvalidPath(t *testing.T) {
	_, err := getCurrentCommitHash("/nonexistent/path")
	if err == nil {
		t.Error("Expected error for invalid path")
	}
}

func TestConvertToWorktreeModels_BasicConversion(t *testing.T) {
	entries := []*GlobalWorktreeEntry{
		{
			Branch:     "main",
			Path:       "/path/to/main",
			CommitHash: "abc123",
			IsMain:     true,
		},
		{
			Branch:     "feature",
			Path:       "/path/to/feature",
			CommitHash: "def456",
			IsMain:     false,
		},
	}

	worktrees := ConvertToWorktreeModels(entries, false)

	if len(worktrees) != 2 {
		t.Fatalf("Expected 2 worktrees, got %d", len(worktrees))
	}

	if worktrees[0].Branch != "main" {
		t.Errorf("Expected first branch 'main', got '%s'", worktrees[0].Branch)
	}
	if worktrees[1].Branch != "feature" {
		t.Errorf("Expected second branch 'feature', got '%s'", worktrees[1].Branch)
	}
}

func TestConvertToWorktreeModels_WithRepoName(t *testing.T) {
	repoInfo, _ := url.ParseRepositoryURL("https://github.com/testuser/testrepo.git")
	createdAt := time.Date(2026, 7, 17, 12, 30, 0, 0, time.UTC)
	entries := []*GlobalWorktreeEntry{
		{
			RepositoryInfo: repoInfo,
			Branch:         "feature",
			Path:           "/path/to/feature",
			CommitHash:     "abc123",
			IsMain:         false,
			CreatedAt:      createdAt,
		},
	}

	worktrees := ConvertToWorktreeModels(entries, true)

	if len(worktrees) != 1 {
		t.Fatalf("Expected 1 worktree, got %d", len(worktrees))
	}

	expected := "testrepo:feature"
	if worktrees[0].Branch != expected {
		t.Errorf("Expected branch '%s', got '%s'", expected, worktrees[0].Branch)
	}
	if worktrees[0].Repository != repoInfo.FullPath {
		t.Errorf("Repository = %q, want %q", worktrees[0].Repository, repoInfo.FullPath)
	}
	if !worktrees[0].CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", worktrees[0].CreatedAt, createdAt)
	}
	wantSession := tmux.WorkspaceSessionName(repoInfo, entries[0].Branch, entries[0].Path)
	if worktrees[0].SessionName != wantSession {
		t.Errorf("SessionName = %q, want %q", worktrees[0].SessionName, wantSession)
	}
}

func TestFilterGlobalWorktrees_BranchMatch(t *testing.T) {
	entries := []*GlobalWorktreeEntry{
		{Branch: "main", Path: "/path/main"},
		{Branch: "feature-auth", Path: "/path/feature"},
		{Branch: "bugfix-login", Path: "/path/bugfix"},
	}

	matches := FilterGlobalWorktrees(entries, "feature")
	if len(matches) != 1 {
		t.Fatalf("Expected 1 match, got %d", len(matches))
	}
	if matches[0].Branch != "feature-auth" {
		t.Errorf("Expected branch 'feature-auth', got '%s'", matches[0].Branch)
	}
}

func TestFilterGlobalWorktrees_PathMatch(t *testing.T) {
	entries := []*GlobalWorktreeEntry{
		{Branch: "main", Path: "/projects/webapp/main"},
		{Branch: "feature", Path: "/projects/api/feature"},
		{Branch: "test", Path: "/other/test"},
	}

	matches := FilterGlobalWorktrees(entries, "api")
	if len(matches) != 1 {
		t.Fatalf("Expected 1 match, got %d", len(matches))
	}
	if matches[0].Branch != "feature" {
		t.Errorf("Expected branch 'feature', got '%s'", matches[0].Branch)
	}
}

func TestFilterGlobalWorktrees_RepoMatch(t *testing.T) {
	repoInfo1, _ := url.ParseRepositoryURL("https://github.com/user/webapp.git")
	repoInfo2, _ := url.ParseRepositoryURL("https://github.com/user/api.git")

	entries := []*GlobalWorktreeEntry{
		{RepositoryInfo: repoInfo1, Branch: "main", Path: "/path1"},
		{RepositoryInfo: repoInfo2, Branch: "feature", Path: "/path2"},
	}

	matches := FilterGlobalWorktrees(entries, "webapp")
	if len(matches) != 1 {
		t.Fatalf("Expected 1 match, got %d", len(matches))
	}
	if matches[0].Branch != "main" {
		t.Errorf("Expected branch 'main', got '%s'", matches[0].Branch)
	}
}

func TestFilterGlobalWorktrees_RepoColonBranchMatch(t *testing.T) {
	repoInfo, _ := url.ParseRepositoryURL("https://github.com/user/webapp.git")
	entries := []*GlobalWorktreeEntry{
		{RepositoryInfo: repoInfo, Branch: "main", Path: "/path1"},
		{RepositoryInfo: repoInfo, Branch: "feature", Path: "/path2"},
	}

	matches := FilterGlobalWorktrees(entries, "webapp:feature")
	if len(matches) != 1 {
		t.Fatalf("Expected 1 match, got %d", len(matches))
	}
	if matches[0].Branch != "feature" {
		t.Errorf("Expected branch 'feature', got '%s'", matches[0].Branch)
	}
}

func TestFilterGlobalWorktrees_CaseInsensitive(t *testing.T) {
	entries := []*GlobalWorktreeEntry{
		{Branch: "Feature-Auth", Path: "/path"},
	}

	matches := FilterGlobalWorktrees(entries, "FEATURE")
	if len(matches) != 1 {
		t.Fatalf("Expected 1 match for case-insensitive search, got %d", len(matches))
	}
}

func TestFilterGlobalWorktrees_NoMatches(t *testing.T) {
	entries := []*GlobalWorktreeEntry{
		{Branch: "main", Path: "/path"},
		{Branch: "feature", Path: "/other"},
	}

	matches := FilterGlobalWorktrees(entries, "nonexistent")
	if len(matches) != 0 {
		t.Errorf("Expected no matches, got %d", len(matches))
	}
}

func TestFilterGlobalWorktrees_EmptyPattern(t *testing.T) {
	entries := []*GlobalWorktreeEntry{
		{Branch: "main", Path: "/path"},
		{Branch: "feature", Path: "/other"},
	}

	matches := FilterGlobalWorktrees(entries, "")
	if len(matches) != 2 {
		t.Errorf("Expected all entries to match empty pattern, got %d", len(matches))
	}
}

func TestIsSubmoduleGitDir(t *testing.T) {
	tests := []struct {
		name     string
		gitDir   string
		expected bool
	}{
		{
			name:     "worktree gitdir",
			gitDir:   "/path/to/repo/.git/worktrees/feature-branch",
			expected: false,
		},
		{
			name:     "relative worktree gitdir",
			gitDir:   "../../.git/worktrees/feature-branch",
			expected: false,
		},
		{
			name:     "submodule in main worktree",
			gitDir:   "/path/to/repo/.git/modules/my-submodule",
			expected: true,
		},
		{
			name:     "relative submodule in main worktree",
			gitDir:   "../../.git/modules/my-submodule",
			expected: true,
		},
		{
			name:     "submodule in linked worktree",
			gitDir:   "../../../repo/.git/worktrees/feature/modules/cm/lwip",
			expected: true,
		},
		{
			name:     "nested submodule in linked worktree",
			gitDir:   "../../../repo/.git/worktrees/feature/modules/third_party/xgrammar/xgrammar/modules/3rdparty/googletest",
			expected: true,
		},
		{
			name:     "nested submodule gitdir",
			gitDir:   "/path/to/repo/.git/modules/outer/modules/inner",
			expected: true,
		},
		{
			name:     "windows submodule gitdir",
			gitDir:   "C:\\repo\\.git\\modules\\my-submodule",
			expected: filepath.Separator == '\\', // only matches on Windows where ToSlash converts
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSubmoduleGitDir(tt.gitDir)
			if result != tt.expected {
				t.Errorf("isSubmoduleGitDir(%q) = %v, want %v", tt.gitDir, result, tt.expected)
			}
		})
	}
}

func TestDiscoverGlobalWorktrees_SkipsSubmodules(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a directory with a submodule-style .git file
	submoduleDir := filepath.Join(tmpDir, "my-submodule")
	if err := os.MkdirAll(submoduleDir, 0755); err != nil {
		t.Fatalf("Failed to create submodule directory: %v", err)
	}

	gitContent := "gitdir: /path/to/repo/.git/modules/my-submodule"
	if err := os.WriteFile(filepath.Join(submoduleDir, ".git"), []byte(gitContent), 0644); err != nil {
		t.Fatalf("Failed to create .git file: %v", err)
	}

	entries, err := DiscoverGlobalWorktrees(tmpDir, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("Expected no entries (submodule should be skipped), got %d", len(entries))
	}
}

func TestGlobalWorktreeEntryModelCarriesRepositoryAndCreatedAt(t *testing.T) {
	created := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	entry := &GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "wesm",
			Repository: "kwt",
			FullPath:   "github.com/wesm/kwt",
		},
		Branch:     "feature/foo",
		Path:       "/wt/github.com/wesm/kwt/feature-foo",
		CommitHash: "abc123",
		IsMain:     false,
		CreatedAt:  created,
	}

	got := entry.Model()

	if got.Repository != "github.com/wesm/kwt" {
		t.Errorf("Repository = %q, want github.com/wesm/kwt", got.Repository)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}
	if got.Path != entry.Path || got.Branch != entry.Branch ||
		got.CommitHash != entry.CommitHash || got.IsMain != entry.IsMain {
		t.Errorf("Model() did not copy core fields: %+v", got)
	}
}

func TestDiscoverGlobalWorktreesPopulatesCreatedAt(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "github.com", "user", "repo", "main")
	initRepoAt(t, repoDir, "https://github.com/user/repo.git")

	entries, err := DiscoverGlobalWorktrees(baseDir, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}
	if entries[0].CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be populated from directory mtime, got zero")
	}
}

// Benchmark tests
func BenchmarkDiscoverGlobalWorktrees(b *testing.B) {
	// Create a temporary directory with multiple worktrees
	baseDir := b.TempDir()

	// Create multiple repositories and worktrees
	for i := range 10 {
		repo := &TestRepository{Path: filepath.Join(baseDir, fmt.Sprintf("repo%d", i))}
		if err := os.MkdirAll(repo.Path, 0755); err != nil {
			b.Fatalf("Failed to create repo directory: %v", err)
		}

		// Create a simple .git file for worktree simulation
		gitFile := filepath.Join(repo.Path, ".git")
		gitContent := fmt.Sprintf("gitdir: /path/to/main/repo/.git/worktrees/branch%d", i)
		if err := os.WriteFile(gitFile, []byte(gitContent), 0644); err != nil {
			b.Fatalf("Failed to create .git file: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// This will mostly test the filesystem walking since we don't have full git repos
		// It will return errors for the mock .git files, but tests the core discovery logic
		_, _ = DiscoverGlobalWorktrees(baseDir, nil)
	}
}

func BenchmarkFilterGlobalWorktrees(b *testing.B) {
	// Create a large slice of entries
	entries := make([]*GlobalWorktreeEntry, 1000)
	for i := range 1000 {
		entries[i] = &GlobalWorktreeEntry{
			Branch: fmt.Sprintf("branch-%d", i),
			Path:   fmt.Sprintf("/path/to/branch-%d", i),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		FilterGlobalWorktrees(entries, "branch-500")
	}
}

func TestGlobalWorktreeEntryModelExposesSessionName(t *testing.T) {
	info := &url.RepositoryInfo{
		Host:       "github.com",
		Owner:      "wesm",
		Repository: "kwt",
		FullPath:   "github.com/wesm/kwt",
	}
	entry := &GlobalWorktreeEntry{
		RepositoryInfo: info,
		Branch:         "feature/foo",
		Path:           "/wt/github.com/wesm/kwt/feature-foo",
	}

	got := entry.Model()

	want := tmux.WorkspaceSessionName(info, "feature/foo", "/wt/github.com/wesm/kwt/feature-foo")
	if got.SessionName != want {
		t.Errorf("SessionName = %q, want %q", got.SessionName, want)
	}
	if got.SessionName == "" {
		t.Error("SessionName should not be empty for an identified repository")
	}
}
