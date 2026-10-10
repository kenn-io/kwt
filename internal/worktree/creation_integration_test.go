package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	managed "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
	shared "go.kenn.io/kwt/worktree"
)

func createForTest(t *testing.T, g *git.Git, opts CreateOptions) error {
	t.Helper()
	cfg := &models.Config{Fleet: models.FleetConfig{TokenEnv: "custom_fleet_token"}}
	_, err := New(g, cfg).Create(t.Context(), opts)
	return err
}

// TestRepository creates a test git repository
type TestRepository struct {
	Path string
}

// NewTestRepository creates a new test repository
func NewTestRepository(t *testing.T) *TestRepository {
	t.Helper()

	t.Setenv("KWT_HOME", t.TempDir())
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
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Path
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s failed: %v\nOutput: %s", strings.Join(args, " "), err, output)
	}
	return nil
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
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = r.Path
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\nOutput: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func commitTestFile(t *testing.T, dir, name, contents, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	gitOutput(t, dir, "add", name)
	gitOutput(t, dir, "commit", "-m", message)
}

func createBranchWithMissingBlob(
	t *testing.T,
	repo *TestRepository,
	branch string,
) string {
	t.Helper()
	repo.CreateBranch(t, branch)
	commitTestFile(t, repo.Path, "missing.txt", "missing\n", "Missing blob")
	commit := gitOutput(t, repo.Path, "rev-parse", "HEAD")
	blob := gitOutput(t, repo.Path, "rev-parse", branch+":missing.txt")
	gitOutput(t, repo.Path, "checkout", "main")
	objectPath := filepath.Join(repo.Path, ".git", "objects", blob[:2], blob[2:])
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove blob object: %v", err)
	}
	return commit
}

func TestManagerCreate(t *testing.T) {
	repo := NewTestRepository(t)
	g := git.New(repo.Path)

	t.Run("ExistingBranch", func(t *testing.T) {
		// Create a branch first
		repo.CreateBranch(t, "existing-branch")
		if err := repo.run("checkout", "main"); err != nil {
			t.Fatalf("Failed to checkout main: %v", err)
		}

		// Add worktree for existing branch
		worktreePath := filepath.Join(t.TempDir(), "existing-wt")
		err := createForTest(t, g, CreateOptions{Path: worktreePath, Branch: "existing-branch", NewBranch: false})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		// Verify worktree was created
		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			t.Error("Worktree directory was not created")
		}
	})

	t.Run("ExistingBranchDoesNotFetch", func(t *testing.T) {
		repo := NewTestRepository(t)
		remoteParent := t.TempDir()
		remotePath := filepath.Join(remoteParent, "origin.git")
		gitOutput(t, remoteParent, "init", "--bare", "-b", "trunk", remotePath)
		if err := repo.run("remote", "add", "origin", remotePath); err != nil {
			t.Fatalf("add origin: %v", err)
		}
		if err := repo.run("push", "origin", "main:trunk"); err != nil {
			t.Fatalf("push initial remote default: %v", err)
		}
		if err := repo.run("fetch", "origin"); err != nil {
			t.Fatalf("fetch initial remote state: %v", err)
		}
		staleRemoteHead := gitOutput(t, repo.Path, "rev-parse", "refs/remotes/origin/trunk")
		if err := repo.run("branch", "existing-branch"); err != nil {
			t.Fatalf("create existing branch: %v", err)
		}

		updaterParent := t.TempDir()
		updaterPath := filepath.Join(updaterParent, "updater")
		gitOutput(t, updaterParent, "clone", remotePath, updaterPath)
		gitOutput(t, updaterPath, "config", "user.name", "Test User")
		gitOutput(t, updaterPath, "config", "user.email", "test@example.com")
		commitTestFile(t, updaterPath, "remote.txt", "remote\n", "Advance remote default")
		gitOutput(t, updaterPath, "push", "origin", "trunk")
		if remoteHead := gitOutput(t, remotePath, "rev-parse", "refs/heads/trunk"); remoteHead == staleRemoteHead {
			t.Fatal("test setup did not advance the remote default")
		}

		worktreePath := filepath.Join(t.TempDir(), "existing-wt")
		if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "existing-branch", NewBranch: false}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got := gitOutput(t, repo.Path, "rev-parse", "refs/remotes/origin/trunk"); got != staleRemoteHead {
			t.Fatalf("existing-branch creation fetched origin: tracking ref = %s, want stale %s", got, staleRemoteHead)
		}
	})

	t.Run("NewBranch", func(t *testing.T) {
		// Add worktree with new branch
		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		err := createForTest(t, g, CreateOptions{Path: worktreePath, Branch: "new-branch", NewBranch: true})
		if err != nil {
			t.Fatalf("Create() with new branch error = %v", err)
		}

		// Verify worktree was created
		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			t.Error("Worktree directory was not created")
		}

		// Verify branch exists
		worktrees, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
		if err != nil {
			t.Fatalf("ListWorktrees() error = %v", err)
		}

		found := false
		for _, wt := range worktrees {
			// Compare resolved paths
			resolvedWtPath, _ := filepath.EvalSymlinks(wt.Path)
			resolvedWorktreePath, _ := filepath.EvalSymlinks(worktreePath)

			if resolvedWtPath == resolvedWorktreePath {
				found = true
				if wt.Branch != "new-branch" {
					t.Errorf("Worktree branch = %s, want new-branch", wt.Branch)
				}
				break
			}
		}
		if !found {
			t.Error("New branch worktree not found")
		}
	})

	t.Run("NewBranchFromRemoteDefault", func(t *testing.T) {
		repo := NewTestRepository(t)
		remoteParent := t.TempDir()
		remotePath := filepath.Join(remoteParent, "origin.git")
		gitOutput(t, remoteParent, "init", "--bare", "-b", "trunk", remotePath)
		if err := repo.run("remote", "add", "origin", remotePath); err != nil {
			t.Fatalf("add origin: %v", err)
		}
		if err := repo.run("push", "origin", "main:trunk"); err != nil {
			t.Fatalf("push initial remote default: %v", err)
		}

		repo.CreateBranch(t, "feature/current")
		commitTestFile(t, repo.Path, "feature.txt", "feature\n", "Feature commit")

		updaterParent := t.TempDir()
		updaterPath := filepath.Join(updaterParent, "updater")
		gitOutput(t, updaterParent, "clone", remotePath, updaterPath)
		gitOutput(t, updaterPath, "config", "user.name", "Test User")
		gitOutput(t, updaterPath, "config", "user.email", "test@example.com")
		commitTestFile(t, updaterPath, "remote.txt", "remote\n", "Advance remote default")
		gitOutput(t, updaterPath, "push", "origin", "trunk")

		if runtime.GOOS != "windows" {
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatalf("find git executable: %v", err)
			}
			wrapperDir := t.TempDir()
			wrapperPath := filepath.Join(wrapperDir, "git")
			wrapper := `#!/bin/sh
if [ "$1" = "ls-remote" ] && [ "$2" = "--symref" ]; then
	exit 129
fi
exec "$REAL_GIT" "$@"
`
			if err := os.WriteFile(wrapperPath, []byte(wrapper), 0755); err != nil {
				t.Fatalf("write git compatibility wrapper: %v", err)
			}
			t.Setenv("REAL_GIT", realGit)
			t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}

		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "new-from-default", NewBranch: true}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		got := gitOutput(t, worktreePath, "rev-parse", "HEAD")
		want := gitOutput(t, remotePath, "rev-parse", "refs/heads/trunk")
		if got != want {
			t.Fatalf("new worktree HEAD = %s, want fetched remote default %s", got, want)
		}
	})

	t.Run("NewBranchFetchesRemoteDefaultOutsideConfiguredRefspec", func(t *testing.T) {
		repo := NewTestRepository(t)
		remoteParent := t.TempDir()
		remotePath := filepath.Join(remoteParent, "origin.git")
		gitOutput(t, remoteParent, "init", "--bare", "-b", "trunk", remotePath)
		if err := repo.run("remote", "add", "origin", remotePath); err != nil {
			t.Fatalf("add origin: %v", err)
		}
		if err := repo.run("push", "origin", "main:trunk", "main:other"); err != nil {
			t.Fatalf("push initial remote branches: %v", err)
		}
		if err := repo.run("fetch", "origin"); err != nil {
			t.Fatalf("fetch initial remote state: %v", err)
		}
		staleRemoteHead := gitOutput(t, repo.Path, "rev-parse", "refs/remotes/origin/trunk")
		if err := repo.run("config", "--unset-all", "remote.origin.fetch"); err != nil {
			t.Fatalf("clear origin fetch refspec: %v", err)
		}
		if err := repo.run("config", "--add", "remote.origin.fetch", "+refs/heads/other:refs/remotes/origin/other"); err != nil {
			t.Fatalf("set restrictive origin fetch refspec: %v", err)
		}

		repo.CreateBranch(t, "feature/current")
		commitTestFile(t, repo.Path, "feature.txt", "feature\n", "Feature commit")

		updaterParent := t.TempDir()
		updaterPath := filepath.Join(updaterParent, "updater")
		gitOutput(t, updaterParent, "clone", remotePath, updaterPath)
		gitOutput(t, updaterPath, "config", "user.name", "Test User")
		gitOutput(t, updaterPath, "config", "user.email", "test@example.com")
		commitTestFile(t, updaterPath, "remote.txt", "remote\n", "Advance remote default")
		gitOutput(t, updaterPath, "push", "origin", "trunk")
		freshRemoteHead := gitOutput(t, remotePath, "rev-parse", "refs/heads/trunk")
		if freshRemoteHead == staleRemoteHead {
			t.Fatal("test setup did not advance the remote default")
		}

		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "new-from-default", NewBranch: true}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got := gitOutput(t, worktreePath, "rev-parse", "HEAD"); got != freshRemoteHead {
			t.Fatalf("new worktree HEAD = %s, want fetched remote default %s", got, freshRemoteHead)
		}
	})

	t.Run("NewBranchFromLocalMain", func(t *testing.T) {
		repo := NewTestRepository(t)
		want := gitOutput(t, repo.Path, "rev-parse", "main")
		repo.CreateBranch(t, "feature/current")
		commitTestFile(t, repo.Path, "feature.txt", "feature\n", "Feature commit")

		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "new-from-main", NewBranch: true}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got := gitOutput(t, worktreePath, "rev-parse", "HEAD"); got != want {
			t.Fatalf("new worktree HEAD = %s, want local main %s", got, want)
		}
	})

	t.Run("NewBranchPrefersLocalMainOverMaster", func(t *testing.T) {
		repo := NewTestRepository(t)
		want := gitOutput(t, repo.Path, "rev-parse", "main")
		repo.CreateBranch(t, "master")
		commitTestFile(t, repo.Path, "master.txt", "master\n", "Advance master")
		repo.CreateBranch(t, "feature/current")

		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "new-from-main", NewBranch: true}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got := gitOutput(t, worktreePath, "rev-parse", "HEAD"); got != want {
			t.Fatalf("new worktree HEAD = %s, want preferred local main %s", got, want)
		}
	})

	t.Run("NewBranchFromLocalMaster", func(t *testing.T) {
		repo := NewTestRepository(t)
		if err := repo.run("branch", "-m", "master"); err != nil {
			t.Fatalf("rename main to master: %v", err)
		}
		want := gitOutput(t, repo.Path, "rev-parse", "master")
		repo.CreateBranch(t, "feature/current")
		commitTestFile(t, repo.Path, "feature.txt", "feature\n", "Feature commit")

		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "new-from-master", NewBranch: true}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got := gitOutput(t, worktreePath, "rev-parse", "HEAD"); got != want {
			t.Fatalf("new worktree HEAD = %s, want local master %s", got, want)
		}
	})

	t.Run("NewBranchFromPrimaryWorktree", func(t *testing.T) {
		repo := NewTestRepository(t)
		if err := repo.run("branch", "-m", "trunk"); err != nil {
			t.Fatalf("rename main to trunk: %v", err)
		}
		want := gitOutput(t, repo.Path, "rev-parse", "trunk")
		if err := repo.run("branch", "feature/current"); err != nil {
			t.Fatalf("create feature branch: %v", err)
		}
		featurePath := filepath.Join(t.TempDir(), "feature-wt")
		if err := repo.run("worktree", "add", featurePath, "feature/current"); err != nil {
			t.Fatalf("create feature worktree: %v", err)
		}
		commitTestFile(t, featurePath, "feature.txt", "feature\n", "Feature commit")

		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		if err := createForTest(t, git.New(featurePath), CreateOptions{Path: worktreePath, Branch: "new-from-primary", NewBranch: true}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if got := gitOutput(t, worktreePath, "rev-parse", "HEAD"); got != want {
			t.Fatalf("new worktree HEAD = %s, want primary worktree branch %s", got, want)
		}
	})

	t.Run("NewBranchFailsWithoutDefaultBase", func(t *testing.T) {
		repo := NewTestRepository(t)
		if err := repo.run("branch", "-m", "trunk"); err != nil {
			t.Fatalf("rename main to trunk: %v", err)
		}
		if err := repo.run("checkout", "--detach"); err != nil {
			t.Fatalf("detach primary worktree: %v", err)
		}

		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "new-without-base", NewBranch: true})
		if err == nil {
			t.Fatal("Create() error = nil, want base resolution error")
		}
		if !strings.Contains(err.Error(), "no local main, master, or primary worktree branch") {
			t.Fatalf("Create() error = %q, want local fallback details", err)
		}
		if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
			t.Fatalf("worktree path created despite resolution failure: stat error = %v", statErr)
		}
	})
}

func TestHookReentrantWorktreeList(t *testing.T) {
	if os.Getenv("KWT_TEST_HOOK_REENTRANT_LIST") != "1" {
		t.Skip("helper process")
	}

	done := make(chan error, 1)
	go func() {
		worktrees, err := openSharedWorktrees(t, git.New(
			os.Getenv("KWT_TEST_HOOK_REPO"),
		)).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
		reservedPath := os.Getenv("KWT_TEST_HOOK_WORKTREE")
		for _, worktree := range worktrees {
			if utils.CanonicalPath(worktree.Path) ==
				utils.CanonicalPath(reservedPath) {
				err = fmt.Errorf(
					"in-progress worktree was visible during checkout",
				)
			}
		}
		if _, generationErr := openSharedWorktrees(t, git.New(
			os.Getenv("KWT_TEST_HOOK_REPO"),
		)).ReadIdentity(t.Context(), reservedPath, "kwt-generation"); generationErr == nil {
			err = fmt.Errorf(
				"in-progress worktree generation was initialized during listing",
			)
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		fmt.Fprintln(os.Stderr, "hook could not re-enter kwt worktree listing")
		os.Exit(2)
	}
}

func TestHookCapableWorktreeAddsAllowHookToListWorktrees(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test hook uses a POSIX shell")
	}

	tests := []struct {
		name string
		add  func(*git.Git, string) error
	}{
		{
			name: "default base",
			add: func(g *git.Git, path string) error {
				return createForTest(t, g, CreateOptions{Path: path, Branch: "hook-default-base", NewBranch: true})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := NewTestRepository(t)
			hooksDir := filepath.Join(repo.Path, ".git", "hooks")
			hookPath := filepath.Join(hooksDir, "post-checkout")
			hook := `#!/bin/sh
"$KWT_TEST_BINARY" -test.run=^TestHookReentrantWorktreeList$
`
			require.NoError(t, os.WriteFile(hookPath, []byte(hook), 0755))
			t.Setenv("KWT_TEST_BINARY", os.Args[0])
			t.Setenv("KWT_TEST_HOOK_REENTRANT_LIST", "1")
			t.Setenv("KWT_TEST_HOOK_REPO", repo.Path)

			worktreePath := filepath.Join(t.TempDir(), "hook-worktree")
			t.Setenv("KWT_TEST_HOOK_WORKTREE", worktreePath)
			require.NoError(t, tt.add(git.New(repo.Path), worktreePath))
			assert.DirExists(t, worktreePath)
		})
	}
}

func TestWorktreeCreationReservationHidesAndProtectsCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test hook uses a POSIX shell")
	}

	repo := NewTestRepository(t)
	startedPath := filepath.Join(t.TempDir(), "hook-started")
	releasePath := filepath.Join(t.TempDir(), "hook-release")
	hookPath := filepath.Join(repo.Path, ".git", "hooks", "post-checkout")
	hook := `#!/bin/sh
touch "$KWT_TEST_HOOK_STARTED"
while [ ! -f "$KWT_TEST_HOOK_RELEASE" ]; do
	sleep 0.01
done
`
	require.NoError(t, os.WriteFile(hookPath, []byte(hook), 0755))
	t.Setenv("KWT_TEST_HOOK_STARTED", startedPath)
	t.Setenv("KWT_TEST_HOOK_RELEASE", releasePath)
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, nil, 0644)
	})

	g := git.New(repo.Path)
	worktreePath := filepath.Join(t.TempDir(), "reserved-worktree")
	addErr := make(chan error, 1)
	go func() {
		addErr <- createForTest(t, g, CreateOptions{Path: worktreePath, Branch: "reserved-worktree", NewBranch: true})
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	worktrees, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	for _, worktree := range worktrees {
		assert.NotEqual(
			t,
			utils.CanonicalPath(worktreePath),
			utils.CanonicalPath(worktree.Path),
		)
	}
	_, removeErr := openSharedWorktrees(t, g).Remove(t.Context(), shared.RemovalRequest{Path: worktreePath})
	require.ErrorContains(t, removeErr, "creation in progress")
	assert.DirExists(t, worktreePath)

	require.NoError(t, os.WriteFile(releasePath, nil, 0644))
	require.NoError(t, <-addErr)
}

func TestHookCapableWorktreeAddRecoversGenerationInitializationFailure(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("test hook uses POSIX permissions")
	}

	tests := []struct {
		name string
		add  func(*git.Git, string) error
	}{
		{
			name: "default base",
			add: func(g *git.Git, path string) error {
				return createForTest(t, g, CreateOptions{Path: path, Branch: "recover-default-base", NewBranch: true})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := NewTestRepository(t)
			lockPath := filepath.Join(
				repo.Path,
				".git",
				"kwt-worktree.lock",
			)
			require.NoError(t, os.WriteFile(lockPath, nil, 0600))
			t.Cleanup(func() { _ = os.Chmod(lockPath, 0600) })
			hookPath := filepath.Join(
				repo.Path,
				".git",
				"hooks",
				"post-checkout",
			)
			hook := `#!/bin/sh
chmod 000 "$KWT_TEST_MUTATION_LOCK"
`
			require.NoError(t, os.WriteFile(hookPath, []byte(hook), 0755))
			t.Setenv("KWT_TEST_MUTATION_LOCK", lockPath)

			worktreePath := filepath.Join(t.TempDir(), "worktree")
			require.NoError(t, tt.add(git.New(repo.Path), worktreePath))
			assert.DirExists(t, worktreePath)

			require.NoError(t, os.Chmod(lockPath, 0600))
			worktrees, err := openSharedWorktrees(t, git.New(repo.Path)).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
			require.NoError(t, err)
			for _, worktree := range worktrees {
				if utils.CanonicalPath(worktree.Path) ==
					utils.CanonicalPath(worktreePath) {
					assert.NotEmpty(t, worktree.Generation)
					return
				}
			}
			t.Fatal("created worktree missing after generation retry")
		})
	}
}

func TestManagerCreateExistingDisablesCheckoutHooks(t *testing.T) {
	repo := NewTestRepository(t)
	repo.CreateBranch(t, "existing-unreviewed")
	if err := os.WriteFile(
		filepath.Join(repo.Path, ".gitattributes"),
		[]byte("branch.txt filter=conditional-attack\n"),
		0644,
	); err != nil {
		t.Fatalf("write attributes: %v", err)
	}
	gitOutput(t, repo.Path, "add", ".gitattributes")
	commitTestFile(t, repo.Path, "branch.txt", "branch\n", "Existing branch")
	gitOutput(t, repo.Path, "checkout", "main")

	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	configuredHookMarker := filepath.Join(t.TempDir(), "configured-hook-ran")
	filterMarker := filepath.Join(t.TempDir(), "conditional-filter-ran")
	hooksDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(hooksDir, "post-checkout"),
		fmt.Appendf(nil, "#!/bin/sh\nprintf hook > %q\n", hookMarker),
		0755,
	); err != nil {
		t.Fatalf("write checkout hook: %v", err)
	}
	gitOutput(t, repo.Path, "config", "core.hooksPath", hooksDir)
	configuredHook := filepath.Join(t.TempDir(), "configured-hook")
	if err := os.WriteFile(
		configuredHook,
		fmt.Appendf(nil, "#!/bin/sh\nprintf hook > %q\n", configuredHookMarker),
		0755,
	); err != nil {
		t.Fatalf("write configured hook: %v", err)
	}
	gitOutput(t, repo.Path, "config", "hook.configured-attack.command", configuredHook)
	gitOutput(t, repo.Path, "config", "--add", "hook.configured-attack.event", "post-checkout")
	gitOutput(t, repo.Path, "config", "--add", "hook.configured-attack.event", "post-index-change")
	configuredHooksSupported := exec.Command(
		"git", "-C", repo.Path, "hook", "list", "post-checkout",
	).Run() == nil

	filterCommand := filepath.Join(t.TempDir(), "conditional-filter")
	if err := os.WriteFile(
		filterCommand,
		fmt.Appendf(nil, "#!/bin/sh\nprintf filter > %q\ncat\n", filterMarker),
		0755,
	); err != nil {
		t.Fatalf("write conditional filter: %v", err)
	}
	includePath := filepath.Join(t.TempDir(), "onbranch.config")
	gitOutput(t, repo.Path, "config", "-f", includePath, "filter.conditional-attack.smudge", filterCommand)
	gitOutput(t, repo.Path, "config", "-f", includePath, "filter.conditional-attack.required", "true")
	gitOutput(
		t,
		repo.Path,
		"config",
		"includeIf.onbranch:existing-unreviewed.path",
		includePath,
	)
	gitOutput(t, repo.Path, "config", "core.autocrlf", "true")

	worktreePath := filepath.Join(t.TempDir(), "existing-unreviewed")
	if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "existing-unreviewed"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Errorf("checkout hook ran against existing branch: stat error = %v", err)
	}
	if configuredHooksSupported {
		if _, err := os.Stat(configuredHookMarker); !os.IsNotExist(err) {
			t.Errorf("configured hook ran against existing branch: stat error = %v", err)
		}
	}
	if _, err := os.Stat(filterMarker); !os.IsNotExist(err) {
		t.Errorf("branch-conditional filter ran against existing branch: stat error = %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(worktreePath, "branch.txt")); err != nil {
		t.Errorf("read checked-out branch file: %v", err)
	} else if strings.ReplaceAll(string(contents), "\r\n", "\n") != "branch\n" {
		t.Errorf("branch.txt = %q, want branch content", contents)
	}
	if got := gitOutput(t, worktreePath, "branch", "--show-current"); got != "existing-unreviewed" {
		t.Errorf("branch = %q, want existing-unreviewed", got)
	}
}

func TestManagerCreateExistingPreservesRefsHeadsPrefixInShortName(t *testing.T) {
	repo := NewTestRepository(t)
	gitOutput(t, repo.Path, "branch", "topic")
	gitOutput(
		t,
		repo.Path,
		"update-ref",
		"refs/heads/refs/heads/topic",
		"HEAD",
	)
	worktreePath := filepath.Join(t.TempDir(), "literal-refs-heads")

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "refs/heads/topic"})

	require.NoError(t, err)
	assert.Equal(
		t,
		"refs/heads/topic",
		gitOutput(t, worktreePath, "branch", "--show-current"),
	)
}

func TestManagerCreateExistingDoesNotRecurseIntoSubmodules(t *testing.T) {
	submodule := NewTestRepository(t)
	if err := os.WriteFile(
		filepath.Join(submodule.Path, ".gitattributes"),
		[]byte("payload.txt filter=submodule-attack\n"),
		0644,
	); err != nil {
		t.Fatalf("write submodule attributes: %v", err)
	}
	gitOutput(t, submodule.Path, "add", ".gitattributes")
	commitTestFile(t, submodule.Path, "payload.txt", "payload\n", "Payload")

	repo := NewTestRepository(t)
	repo.CreateBranch(t, "existing-unreviewed")
	gitOutput(
		t,
		repo.Path,
		"-c",
		"protocol.file.allow=always",
		"submodule",
		"add",
		submodule.Path,
		"dependency",
	)
	gitOutput(t, repo.Path, "commit", "-am", "Add dependency")
	gitOutput(t, repo.Path, "checkout", "main")

	filterMarker := filepath.Join(t.TempDir(), "submodule-filter-ran")
	filterCommand := filepath.Join(t.TempDir(), "submodule-filter")
	if err := os.WriteFile(
		filterCommand,
		fmt.Appendf(nil, "#!/bin/sh\nprintf filter > %q\ncat\n", filterMarker),
		0755,
	); err != nil {
		t.Fatalf("write submodule filter: %v", err)
	}
	gitOutput(
		t,
		filepath.Join(repo.Path, "dependency"),
		"config",
		"filter.submodule-attack.smudge",
		filterCommand,
	)
	gitOutput(t, repo.Path, "config", "submodule.recurse", "true")

	worktreePath := filepath.Join(t.TempDir(), "existing-unreviewed")
	if err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "existing-unreviewed"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := os.Stat(filterMarker); !os.IsNotExist(err) {
		t.Errorf("submodule filter ran before review: stat error = %v", err)
	}
	if _, err := os.Stat(
		filepath.Join(worktreePath, "dependency", "payload.txt"),
	); !os.IsNotExist(err) {
		t.Errorf("submodule content materialized before review: stat error = %v", err)
	}
}

func TestManagerCreateTrackingRemoteBranch(t *testing.T) {
	repo := NewTestRepository(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	gitOutput(t, repo.Path, "remote", "add", "origin", remotePath)

	repo.CreateBranch(t, "remote-only")
	if err := os.WriteFile(
		filepath.Join(repo.Path, ".gitattributes"),
		[]byte(
			"remote.txt filter=smudge-attack\n"+
				"process.txt filter=process-attack\n"+
				"conditional.txt filter=conditional-attack\n",
		),
		0644,
	); err != nil {
		t.Fatalf("write attributes: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(repo.Path, "process.txt"),
		[]byte("process content\n"),
		0644,
	); err != nil {
		t.Fatalf("write process input: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(repo.Path, "conditional.txt"),
		[]byte("conditional content\n"),
		0644,
	); err != nil {
		t.Fatalf("write conditional input: %v", err)
	}
	gitOutput(t, repo.Path, "add", ".gitattributes", "process.txt", "conditional.txt")
	commitTestFile(t, repo.Path, "remote.txt", "remote\n", "Remote branch")
	wantHead := gitOutput(t, repo.Path, "rev-parse", "HEAD")
	gitOutput(t, repo.Path, "push", "origin", "remote-only")
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", "remote-only")
	t.Setenv("KWT_GITHUB_TOKEN", "must-not-reach-remote-checkout")
	t.Setenv("KWT_FLEET_TOKEN", "must-not-reach-remote-checkout")
	t.Setenv("Custom_Fleet_Token", "must-not-reach-remote-checkout")

	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	referenceHookMarker := filepath.Join(t.TempDir(), "reference-hook-ran")
	configuredHookMarker := filepath.Join(t.TempDir(), "configured-hook-ran")
	filterMarker := filepath.Join(t.TempDir(), "filter-ran")
	processMarker := filepath.Join(t.TempDir(), "process-ran")
	conditionalFilterMarker := filepath.Join(t.TempDir(), "conditional-filter-ran")
	hooksDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(hooksDir, "post-checkout"),
		fmt.Appendf(nil, "#!/bin/sh\nprintf hook > %q\n", hookMarker),
		0755,
	); err != nil {
		t.Fatalf("write checkout hook: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(hooksDir, "reference-transaction"),
		fmt.Appendf(nil, "#!/bin/sh\nprintf reference > %q\n", referenceHookMarker),
		0755,
	); err != nil {
		t.Fatalf("write reference transaction hook: %v", err)
	}
	filterDir := t.TempDir()
	smudgePath := filepath.Join(filterDir, "smudge")
	if err := os.WriteFile(
		smudgePath,
		fmt.Appendf(nil, "#!/bin/sh\nprintf filter > %q\ncat\n", filterMarker),
		0755,
	); err != nil {
		t.Fatalf("write smudge filter: %v", err)
	}
	processPath := filepath.Join(filterDir, "process")
	if err := os.WriteFile(
		processPath,
		fmt.Appendf(nil, "#!/bin/sh\nprintf process > %q\nexit 1\n", processMarker),
		0755,
	); err != nil {
		t.Fatalf("write process filter: %v", err)
	}
	gitOutput(t, repo.Path, "config", "core.hooksPath", hooksDir)
	gitOutput(t, repo.Path, "config", "filter.smudge-attack.smudge", smudgePath)
	gitOutput(t, repo.Path, "config", "filter.process-attack.process", processPath)
	gitOutput(t, repo.Path, "config", "filter.process-attack.required", "true")
	configuredHook := filepath.Join(t.TempDir(), "configured-hook")
	if err := os.WriteFile(
		configuredHook,
		fmt.Appendf(nil, "#!/bin/sh\nprintf hook > %q\n", configuredHookMarker),
		0755,
	); err != nil {
		t.Fatalf("write configured hook: %v", err)
	}
	gitOutput(t, repo.Path, "config", "hook.configured-attack.command", configuredHook)
	for _, event := range []string{
		"post-checkout",
		"post-index-change",
		"reference-transaction",
	} {
		gitOutput(t, repo.Path, "config", "--add", "hook.configured-attack.event", event)
	}
	configuredHooksSupported := exec.Command(
		"git", "-C", repo.Path, "hook", "list", "post-checkout",
	).Run() == nil

	conditionalFilter := filepath.Join(t.TempDir(), "conditional-filter")
	if err := os.WriteFile(
		conditionalFilter,
		fmt.Appendf(
			nil,
			"#!/bin/sh\nprintf filter > %q\ncat\n",
			conditionalFilterMarker,
		),
		0755,
	); err != nil {
		t.Fatalf("write conditional filter: %v", err)
	}
	includePath := filepath.Join(t.TempDir(), "gitdir.config")
	gitOutput(t, repo.Path, "config", "-f", includePath, "filter.conditional-attack.smudge", conditionalFilter)
	gitOutput(t, repo.Path, "config", "-f", includePath, "filter.conditional-attack.required", "true")
	gitOutput(
		t,
		repo.Path,
		"config",
		"includeIf.gitdir:**/worktrees/remote-only.path",
		includePath,
	)
	gitOutput(t, repo.Path, "config", "core.autocrlf", "true")

	if runtime.GOOS != "windows" {
		realGit, err := exec.LookPath("git")
		if err != nil {
			t.Fatalf("find git executable: %v", err)
		}
		wrapperDir := t.TempDir()
		wrapperPath := filepath.Join(wrapperDir, "git")
		wrapper := `#!/bin/sh
if [ -n "$KWT_GITHUB_TOKEN" ] || [ -n "$KWT_FLEET_TOKEN" ] || [ -n "$Custom_Fleet_Token" ]; then
	printf '%s\n' 'kwt credential reached remote-source git command' >&2
	exit 88
fi
worktree_add=false
previous=
for arg in "$@"; do
	if [ "$previous" = "worktree" ] && [ "$arg" = "add" ]; then
		worktree_add=true
	fi
	previous=$arg
done
if $worktree_add; then
	for arg in "$@"; do
		if [ "$arg" = "--track" ]; then
			printf '%s\n' 'error: unknown option track' >&2
			exit 129
		fi
	done
fi
exec "$REAL_GIT" "$@"
`
		if err := os.WriteFile(wrapperPath, []byte(wrapper), 0755); err != nil {
			t.Fatalf("write git compatibility wrapper: %v", err)
		}
		t.Setenv("REAL_GIT", realGit)
		t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	worktreePath := filepath.Join(t.TempDir(), "remote-only")
	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "remote-only", Source: "origin/remote-only"})
	t.Setenv("KWT_GITHUB_TOKEN", "")
	t.Setenv("KWT_FLEET_TOKEN", "")
	t.Setenv("Custom_Fleet_Token", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := gitOutput(t, worktreePath, "rev-parse", "HEAD"); got != wantHead {
		t.Errorf("HEAD = %s, want remote branch %s", got, wantHead)
	}
	if got := gitOutput(t, worktreePath, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/remote-only" {
		t.Errorf("upstream = %s, want origin/remote-only", got)
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Errorf("checkout hook ran against remote content: stat error = %v", err)
	}
	if _, err := os.Stat(referenceHookMarker); !os.IsNotExist(err) {
		t.Errorf("reference transaction hook ran during remote creation: stat error = %v", err)
	}
	if configuredHooksSupported {
		if _, err := os.Stat(configuredHookMarker); !os.IsNotExist(err) {
			t.Errorf("configured hook ran during remote creation: stat error = %v", err)
		}
	}
	if _, err := os.Stat(filterMarker); !os.IsNotExist(err) {
		t.Errorf("smudge filter ran against remote content: stat error = %v", err)
	}
	if _, err := os.Stat(processMarker); !os.IsNotExist(err) {
		t.Errorf("process filter ran against remote content: stat error = %v", err)
	}
	if _, err := os.Stat(conditionalFilterMarker); !os.IsNotExist(err) {
		t.Errorf("gitdir-conditional filter ran against remote content: stat error = %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(worktreePath, "conditional.txt")); err != nil {
		t.Errorf("read checked-out remote file: %v", err)
	} else if strings.ReplaceAll(string(contents), "\r\n", "\n") != "conditional content\n" {
		t.Errorf("conditional.txt = %q, want remote content", contents)
	}
}

func TestManagerCreateTrackingRollsBackBranchWhenWorktreeFails(t *testing.T) {
	repo := NewTestRepository(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	gitOutput(t, repo.Path, "remote", "add", "origin", remotePath)

	repo.CreateBranch(t, "remote-only")
	gitOutput(t, repo.Path, "push", "origin", "remote-only")
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", "remote-only")
	referenceHookMarker := filepath.Join(t.TempDir(), "reference-hook-ran")
	hooksDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(hooksDir, "reference-transaction"),
		fmt.Appendf(nil, "#!/bin/sh\nprintf reference > %q\n", referenceHookMarker),
		0755,
	); err != nil {
		t.Fatalf("write reference transaction hook: %v", err)
	}
	gitOutput(t, repo.Path, "config", "core.hooksPath", hooksDir)
	configuredHookMarker := filepath.Join(t.TempDir(), "configured-hook-ran")
	configuredHook := filepath.Join(t.TempDir(), "configured-hook")
	if err := os.WriteFile(
		configuredHook,
		fmt.Appendf(nil, "#!/bin/sh\nprintf hook > %q\n", configuredHookMarker),
		0755,
	); err != nil {
		t.Fatalf("write configured hook: %v", err)
	}
	gitOutput(t, repo.Path, "config", "hook.configured-attack.command", configuredHook)
	gitOutput(t, repo.Path, "config", "--add", "hook.configured-attack.event", "reference-transaction")
	configuredHooksSupported := exec.Command(
		"git", "-C", repo.Path, "hook", "list", "reference-transaction",
	).Run() == nil

	occupiedPath := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(occupiedPath, 0755); err != nil {
		t.Fatalf("create occupied path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(occupiedPath, "keep"), []byte("keep"), 0644); err != nil {
		t.Fatalf("write occupied path: %v", err)
	}

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: occupiedPath, Branch: "remote-only", Source: "refs/remotes/origin/remote-only"})

	if err == nil {
		t.Fatal("Create() expected an error")
	}
	if err := repo.run("show-ref", "--verify", "--quiet", "refs/heads/remote-only"); err == nil {
		t.Error("local tracking branch remained after worktree creation failed")
	}
	if _, err := os.Stat(referenceHookMarker); !os.IsNotExist(err) {
		t.Errorf("reference transaction hook ran during rollback: stat error = %v", err)
	}
	if configuredHooksSupported {
		if _, err := os.Stat(configuredHookMarker); !os.IsNotExist(err) {
			t.Errorf("configured hook ran during rollback: stat error = %v", err)
		}
	}
}

func TestManagerCreateTrackingRejectsOptionLikeBranchName(t *testing.T) {
	repo := NewTestRepository(t)
	gitOutput(
		t,
		repo.Path,
		"update-ref",
		"refs/remotes/origin/-M",
		"HEAD",
	)

	worktreePath := filepath.Join(t.TempDir(), "option-like")
	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "-M", Source: "refs/remotes/origin/-M"})

	require.Error(t, err)
	assert.ErrorIs(t, err, managed.ErrInvalidBranchName)
	assert.Equal(t, "main", gitOutput(t, repo.Path, "branch", "--show-current"))
	assert.NoDirExists(t, worktreePath)
}

func TestManagerCreateTrackingReusesMatchingOrphanBranch(t *testing.T) {
	repo := NewTestRepository(t)
	gitOutput(t, repo.Path, "remote", "add", "origin", repo.Path)
	gitOutput(
		t,
		repo.Path,
		"update-ref",
		"refs/remotes/origin/orphaned",
		"HEAD",
	)
	gitOutput(
		t,
		repo.Path,
		"branch",
		"--track",
		"orphaned",
		"refs/remotes/origin/orphaned",
	)
	worktreePath := filepath.Join(t.TempDir(), "orphaned")

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "orphaned", Source: "refs/remotes/origin/orphaned"})

	require.NoError(t, err)
	assert.DirExists(t, worktreePath)
	assert.Equal(
		t,
		"origin/orphaned",
		gitOutput(
			t,
			worktreePath,
			"rev-parse",
			"--abbrev-ref",
			"@{upstream}",
		),
	)
}

func TestManagerCreateTrackingRejectsDivergentOrphanBranch(t *testing.T) {
	repo := NewTestRepository(t)
	gitOutput(t, repo.Path, "remote", "add", "origin", repo.Path)
	gitOutput(
		t,
		repo.Path,
		"update-ref",
		"refs/remotes/origin/diverged",
		"HEAD",
	)
	gitOutput(
		t,
		repo.Path,
		"branch",
		"--track",
		"diverged",
		"refs/remotes/origin/diverged",
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo.Path, "local-only"),
		[]byte("different content"),
		0o600,
	))
	gitOutput(t, repo.Path, "add", "local-only")
	gitOutput(t, repo.Path, "commit", "-m", "advance local branch source")
	gitOutput(t, repo.Path, "update-ref", "refs/heads/diverged", "HEAD")
	worktreePath := filepath.Join(t.TempDir(), "diverged")

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "diverged", Source: "refs/remotes/origin/diverged"})

	require.ErrorContains(t, err, "points to a different commit")
	assert.NoDirExists(t, worktreePath)
}

func TestManagerCreateExistingRefusesGenerationlessRegisteredWorktree(
	t *testing.T,
) {
	repo := NewTestRepository(t)
	repo.CreateBranch(t, "legacy-existing")
	worktreePath := filepath.Join(t.TempDir(), "legacy-existing")
	repo.CreateWorktree(t, worktreePath, "legacy-existing")
	keepPath := filepath.Join(worktreePath, "keep")
	require.NoError(t, os.WriteFile(keepPath, []byte("preserve me"), 0o600))

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "legacy-existing"})

	require.ErrorIs(t, err, managed.ErrWorktreeDestinationExists)
	data, readErr := os.ReadFile(keepPath)
	require.NoError(t, readErr)
	assert.Equal(t, "preserve me", string(data))
}

func TestManagerCreateExistingRemovesWorktreeAfterCheckoutFailure(t *testing.T) {
	repo := NewTestRepository(t)
	createBranchWithMissingBlob(t, repo, "broken-local")
	worktreePath := filepath.Join(t.TempDir(), "broken-local")

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "broken-local"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing.txt")
	assert.NoDirExists(t, worktreePath)
	worktrees, listErr := openSharedWorktrees(t, git.New(repo.Path)).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, listErr)
	for _, worktree := range worktrees {
		assert.NotEqual(t, worktreePath, worktree.Path)
	}
}

func TestManagerCreateExistingRejectsRemoteOnlyBranch(t *testing.T) {
	repo := NewTestRepository(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(
		t,
		filepath.Dir(remotePath),
		"init",
		"--bare",
		"-b",
		"main",
		remotePath,
	)
	gitOutput(t, repo.Path, "remote", "add", "origin", remotePath)
	repo.CreateBranch(t, "remote-only-local-import")
	gitOutput(t, repo.Path, "push", "-u", "origin", "remote-only-local-import")
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", "remote-only-local-import")
	worktreePath := filepath.Join(t.TempDir(), "remote-only-local-import")

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "remote-only-local-import"})

	require.Error(t, err)
	assert.NoDirExists(t, worktreePath)
	assert.Error(
		t,
		repo.run(
			"show-ref",
			"--verify",
			"--quiet",
			"refs/heads/remote-only-local-import",
		),
	)
}

func TestManagerCreateTrackingRemovesWorktreeAndBranchAfterCheckoutFailure(
	t *testing.T,
) {
	repo := NewTestRepository(t)
	commit := createBranchWithMissingBlob(t, repo, "broken-remote")
	gitOutput(t, repo.Path, "remote", "add", "origin", repo.Path)
	gitOutput(
		t,
		repo.Path,
		"update-ref",
		"refs/remotes/origin/broken-remote",
		commit,
	)
	gitOutput(t, repo.Path, "branch", "-D", "broken-remote")
	worktreePath := filepath.Join(t.TempDir(), "broken-remote")

	err := createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "broken-remote", Source: "refs/remotes/origin/broken-remote"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing.txt")
	assert.NoDirExists(t, worktreePath)
	assert.Error(
		t,
		repo.run(
			"show-ref",
			"--verify",
			"--quiet",
			"refs/heads/broken-remote",
		),
	)
	worktrees, listErr := openSharedWorktrees(t, git.New(repo.Path)).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, listErr)
	for _, worktree := range worktrees {
		assert.NotEqual(t, worktreePath, worktree.Path)
	}
}

func TestManagerCreateTrackingHonorsCustomFetchMapping(t *testing.T) {
	repo := NewTestRepository(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(origin), "init", "--bare", "-b", "main", origin)
	gitOutput(t, repo.Path, "remote", "add", "origin", origin)
	gitOutput(t, repo.Path, "push", "origin", "main:topic")
	gitOutput(t, repo.Path, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/cache/*")
	gitOutput(t, repo.Path, "config", "branch.autoSetupMerge", "false")
	gitOutput(t, repo.Path, "fetch", "origin")
	path := filepath.Join(t.TempDir(), "topic")
	result, err := New(git.New(repo.Path), &models.Config{}).Create(t.Context(), CreateOptions{Branch: "topic", Source: "refs/remotes/cache/topic", Path: path, RequireGeneration: true})
	require.NoError(t, err)
	require.NotEmpty(t, result.IdentityValue)
	require.Equal(t, "origin", gitOutput(t, path, "config", "branch.topic.remote"))
	require.Equal(t, "refs/heads/topic", gitOutput(t, path, "config", "branch.topic.merge"))
	require.Equal(t, "cache/topic", gitOutput(t, path, "rev-parse", "--abbrev-ref", "@{upstream}"))
	_, err = result.Rollback(t.Context(), managed.RollbackFreshOwned)
	require.NoError(t, err)
	require.NoDirExists(t, path)
}

func TestManagerCreateDefaultFetchAllowsHookToListWorktrees(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX hooks")
	}
	repo := NewTestRepository(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(origin), "init", "--bare", "-b", "main", origin)
	gitOutput(t, repo.Path, "remote", "add", "origin", origin)
	gitOutput(t, repo.Path, "push", "origin", "main")
	path := filepath.Join(t.TempDir(), "topic")
	failure := filepath.Join(t.TempDir(), "blocked")
	t.Setenv("KWT_TEST_BINARY", os.Args[0])
	t.Setenv("KWT_TEST_HOOK_REENTRANT_LIST", "1")
	t.Setenv("KWT_TEST_HOOK_REPO", repo.Path)
	t.Setenv("KWT_TEST_HOOK_WORKTREE", path)
	hook := fmt.Sprintf("#!/bin/sh\nif ! \"$KWT_TEST_BINARY\" -test.run=^TestHookReentrantWorktreeList$; then printf blocked > %q; exit 1; fi\n", failure)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path, ".git", "hooks", "reference-transaction"), []byte(hook), 0o755))
	err := createForTest(t, git.New(repo.Path), CreateOptions{Branch: "topic", Path: path, NewBranch: true, RequireGeneration: true})
	require.NoError(t, err)
	require.NoFileExists(t, failure, "the default-branch fetch must permit native hooks to list worktrees")
}

// An existing local branch is unreviewed content, so its checkout must not see
// the credentials kwt protects, just like a remote-source checkout.
func TestManagerCreateExistingBranchWithholdsCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Git wrapper is a POSIX shell script")
	}
	repo := NewTestRepository(t)
	gitOutput(t, repo.Path, "branch", "local-only")
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	wrapper := "#!/bin/sh\n" +
		"if [ -n \"$KWT_GITHUB_TOKEN\" ] || [ -n \"$KWT_FLEET_TOKEN\" ] || [ -n \"$custom_fleet_token\" ]; then\n" +
		"  echo 'kwt credential reached existing-branch git command' >&2\n" +
		"  exit 88\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(wrapper), 0o755))
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KWT_GITHUB_TOKEN", "must-not-reach-existing-checkout")
	t.Setenv("KWT_FLEET_TOKEN", "must-not-reach-existing-checkout")
	t.Setenv("custom_fleet_token", "must-not-reach-existing-checkout")

	worktreePath := filepath.Join(t.TempDir(), "local-only")
	err = createForTest(t, git.New(repo.Path), CreateOptions{Path: worktreePath, Branch: "local-only"})
	t.Setenv("KWT_GITHUB_TOKEN", "")
	t.Setenv("KWT_FLEET_TOKEN", "")
	t.Setenv("custom_fleet_token", "")

	require.NoError(t, err)
	assert.Equal(t, "local-only", gitOutput(t, worktreePath, "branch", "--show-current"))
}
