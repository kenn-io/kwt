package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

// TestRepository creates a test git repository
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

func TestNew(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	if g.workDir != repo.Path {
		t.Errorf("New() workDir = %s, want %s", g.workDir, repo.Path)
	}
}

func TestNewFromCwd(t *testing.T) {
	repo := NewTestRepository(t)

	// Change to test repository directory
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	if err := os.Chdir(repo.Path); err != nil {
		t.Fatalf("Failed to change directory: %v", err)
	}

	g, err := NewFromCwd()
	if err != nil {
		t.Fatalf("NewFromCwd() error = %v", err)
	}

	// macOS may use /private/var symlinks, so resolve paths before comparing
	resolvedWorkDir, _ := filepath.EvalSymlinks(g.workDir)
	resolvedRepoPath, _ := filepath.EvalSymlinks(repo.Path)

	if resolvedWorkDir != resolvedRepoPath {
		t.Errorf("NewFromCwd() workDir = %s, want %s", resolvedWorkDir, resolvedRepoPath)
	}
}

func TestListWorktrees(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Create test branches and worktrees
	repo.CreateBranch(t, "feature/test1")
	worktree1Path := filepath.Join(t.TempDir(), "worktree1")
	repo.CreateWorktree(t, worktree1Path, "feature/test1")

	// Switch back to main branch
	if err := repo.run("checkout", "main"); err != nil {
		t.Fatalf("Failed to checkout main: %v", err)
	}

	repo.CreateBranch(t, "feature/test2")
	worktree2Path := filepath.Join(t.TempDir(), "worktree2")
	repo.CreateWorktree(t, worktree2Path, "feature/test2")

	// List worktrees
	worktrees, err := g.ListWorktrees()
	if err != nil {
		t.Fatalf("ListWorktrees() error = %v", err)
	}

	// Should have 3 worktrees (main + 2 additional)
	if len(worktrees) != 3 {
		t.Errorf("ListWorktrees() returned %d worktrees, want 3", len(worktrees))
	}

	// Verify main worktree
	foundMain := false
	for _, wt := range worktrees {
		if wt.IsMain {
			foundMain = true
			// Compare resolved paths
			resolvedWtPath, _ := filepath.EvalSymlinks(wt.Path)
			resolvedRepoPath, _ := filepath.EvalSymlinks(repo.Path)
			if resolvedWtPath != resolvedRepoPath {
				t.Errorf("Main worktree path = %s, want %s", resolvedWtPath, resolvedRepoPath)
			}
		}
	}
	if !foundMain {
		t.Error("Main worktree not found")
	}

	// Verify additional worktrees
	if !containsWorktreeWithPath(worktrees, worktree1Path) {
		t.Errorf("Worktree 1 not found at path %s", worktree1Path)
	}
	if !containsWorktreeWithPath(worktrees, worktree2Path) {
		t.Errorf("Worktree 2 not found at path %s", worktree2Path)
	}
}

func TestAddWorktree(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	t.Run("ExistingBranch", func(t *testing.T) {
		// Create a branch first
		repo.CreateBranch(t, "existing-branch")
		if err := repo.run("checkout", "main"); err != nil {
			t.Fatalf("Failed to checkout main: %v", err)
		}

		// Add worktree for existing branch
		worktreePath := filepath.Join(t.TempDir(), "existing-wt")
		err := g.AddWorktree(worktreePath, "existing-branch", false)
		if err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
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
		if err := New(repo.Path).AddWorktree(worktreePath, "existing-branch", false); err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
		}
		if got := gitOutput(t, repo.Path, "rev-parse", "refs/remotes/origin/trunk"); got != staleRemoteHead {
			t.Fatalf("existing-branch creation fetched origin: tracking ref = %s, want stale %s", got, staleRemoteHead)
		}
	})

	t.Run("NewBranch", func(t *testing.T) {
		// Add worktree with new branch
		worktreePath := filepath.Join(t.TempDir(), "new-wt")
		err := g.AddWorktree(worktreePath, "new-branch", true)
		if err != nil {
			t.Fatalf("AddWorktree() with new branch error = %v", err)
		}

		// Verify worktree was created
		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			t.Error("Worktree directory was not created")
		}

		// Verify branch exists
		worktrees, err := g.ListWorktrees()
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
		if err := New(repo.Path).AddWorktree(worktreePath, "new-from-default", true); err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
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
		if err := New(repo.Path).AddWorktree(worktreePath, "new-from-default", true); err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
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
		if err := New(repo.Path).AddWorktree(worktreePath, "new-from-main", true); err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
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
		if err := New(repo.Path).AddWorktree(worktreePath, "new-from-main", true); err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
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
		if err := New(repo.Path).AddWorktree(worktreePath, "new-from-master", true); err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
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
		if err := New(featurePath).AddWorktree(worktreePath, "new-from-primary", true); err != nil {
			t.Fatalf("AddWorktree() error = %v", err)
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
		err := New(repo.Path).AddWorktree(worktreePath, "new-without-base", true)
		if err == nil {
			t.Fatal("AddWorktree() error = nil, want base resolution error")
		}
		if !strings.Contains(err.Error(), "no local main, master, or primary worktree branch") {
			t.Fatalf("AddWorktree() error = %q, want local fallback details", err)
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
		worktrees, err := New(
			os.Getenv("KWT_TEST_HOOK_REPO"),
		).ListWorktrees()
		reservedPath := os.Getenv("KWT_TEST_HOOK_WORKTREE")
		for _, worktree := range worktrees {
			if utils.CanonicalPath(worktree.Path) ==
				utils.CanonicalPath(reservedPath) {
				err = fmt.Errorf(
					"in-progress worktree was visible during checkout",
				)
			}
		}
		if _, generationErr := New(
			os.Getenv("KWT_TEST_HOOK_REPO"),
		).readWorktreeGeneration(reservedPath); generationErr == nil {
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
		add  func(*Git, string) error
	}{
		{
			name: "default base",
			add: func(g *Git, path string) error {
				return g.AddWorktree(path, "hook-default-base", true)
			},
		},
		{
			name: "explicit base",
			add: func(g *Git, path string) error {
				return g.AddWorktreeFromBase(path, "hook-explicit-base", "main")
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
			require.NoError(t, tt.add(New(repo.Path), worktreePath))
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

	g := New(repo.Path)
	worktreePath := filepath.Join(t.TempDir(), "reserved-worktree")
	addErr := make(chan error, 1)
	go func() {
		addErr <- g.AddWorktree(worktreePath, "reserved-worktree", true)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	worktrees, err := g.ListWorktrees()
	require.NoError(t, err)
	for _, worktree := range worktrees {
		assert.NotEqual(
			t,
			utils.CanonicalPath(worktreePath),
			utils.CanonicalPath(worktree.Path),
		)
	}
	require.ErrorContains(
		t,
		g.RemoveWorktree(worktreePath, false, ""),
		"creation in progress",
	)
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
		add  func(*Git, string) error
	}{
		{
			name: "default base",
			add: func(g *Git, path string) error {
				return g.AddWorktree(path, "recover-default-base", true)
			},
		},
		{
			name: "explicit base",
			add: func(g *Git, path string) error {
				return g.AddWorktreeFromBase(
					path,
					"recover-explicit-base",
					"main",
				)
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
			require.NoError(t, tt.add(New(repo.Path), worktreePath))
			assert.DirExists(t, worktreePath)

			require.NoError(t, os.Chmod(lockPath, 0600))
			worktrees, err := New(repo.Path).ListWorktrees()
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

func TestAddWorktreeExistingDisablesCheckoutHooks(t *testing.T) {
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
	if err := New(repo.Path).AddWorktreeExisting(
		worktreePath,
		"existing-unreviewed",
		nil,
	); err != nil {
		t.Fatalf("AddWorktreeExisting() error = %v", err)
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

func TestAddWorktreeExistingPreservesRefsHeadsPrefixInShortName(t *testing.T) {
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

	err := New(repo.Path).AddWorktreeExisting(
		worktreePath,
		"refs/heads/topic",
		nil,
	)

	require.NoError(t, err)
	assert.Equal(
		t,
		"refs/heads/topic",
		gitOutput(t, worktreePath, "branch", "--show-current"),
	)
}

func TestAddWorktreeExistingDoesNotRecurseIntoSubmodules(t *testing.T) {
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
	if err := New(repo.Path).AddWorktreeExisting(
		worktreePath,
		"existing-unreviewed",
		nil,
	); err != nil {
		t.Fatalf("AddWorktreeExisting() error = %v", err)
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

func TestRemoveWorktree(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Create a worktree to remove
	repo.CreateBranch(t, "to-remove")
	worktreePath := filepath.Join(t.TempDir(), "remove-wt")
	repo.CreateWorktree(t, worktreePath, "to-remove")

	// Remove the worktree
	err := g.RemoveWorktree(worktreePath, false, "")
	if err != nil {
		t.Fatalf("RemoveWorktree() error = %v", err)
	}

	// Verify worktree is removed from list
	worktrees, _ := g.ListWorktrees()
	for _, wt := range worktrees {
		if wt.Path == worktreePath {
			t.Error("Worktree still exists in list after removal")
		}
	}
}

func TestListWorktreesKeepsGenerationStableWhenDirectoryChanges(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "stable-generation")
	worktreePath := filepath.Join(t.TempDir(), "stable-generation")
	repo.CreateWorktree(t, worktreePath, "stable-generation")

	before, err := g.ListWorktrees()
	require.NoError(t, err)
	var generation string
	for _, worktree := range before {
		if utils.CanonicalPath(worktree.Path) == utils.CanonicalPath(worktreePath) {
			generation = worktree.Generation
		}
	}
	require.NotEmpty(t, generation)

	require.NoError(t, os.WriteFile(
		filepath.Join(worktreePath, "ordinary-change"),
		[]byte("content"),
		0644,
	))

	after, err := g.ListWorktrees()
	require.NoError(t, err)
	for _, worktree := range after {
		if utils.CanonicalPath(worktree.Path) == utils.CanonicalPath(worktreePath) {
			assert.Equal(t, generation, worktree.Generation)
			return
		}
	}
	t.Fatal("worktree missing after ordinary directory change")
}

func TestListWorktreesDoesNotAdoptGenerationFromAnotherRepository(t *testing.T) {
	repoA := NewTestRepository(t)
	repoB := NewTestRepository(t)
	repoA.CreateBranch(t, "repo-a-worktree")
	repoB.CreateBranch(t, "repo-b-worktree")
	worktreePath := filepath.Join(t.TempDir(), "reused-worktree")
	repoA.CreateWorktree(t, worktreePath, "repo-a-worktree")

	before, err := New(repoA.Path).ListWorktrees()
	require.NoError(t, err)
	var repoAGeneration string
	for _, worktree := range before {
		if utils.PathKey(worktree.Path) == utils.PathKey(worktreePath) {
			repoAGeneration = worktree.Generation
			break
		}
	}
	require.NotEmpty(t, repoAGeneration)

	require.NoError(t, os.RemoveAll(worktreePath))
	repoB.CreateWorktree(t, worktreePath, "repo-b-worktree")
	repoBGeneration, err := New(repoB.Path).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	require.NotEqual(t, repoAGeneration, repoBGeneration)

	_, err = New(repoA.Path).ListWorktrees()

	require.ErrorContains(t, err, "belongs to a different repository")
}

func TestWorktreeGenerationRecoversFromRelativeAdministrativeGitDir(
	t *testing.T,
) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "relative-admin-gitdir")
	worktreePath := filepath.Join(t.TempDir(), "relative-admin-gitdir")
	repo.CreateWorktree(t, worktreePath, "relative-admin-gitdir")
	generation, err := g.WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	adminDir, err := g.worktreeGitDir(worktreePath)
	require.NoError(t, err)
	relativeDotGit, err := filepath.Rel(
		adminDir,
		filepath.Join(worktreePath, ".git"),
	)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(adminDir, "gitdir"),
		[]byte(relativeDotGit+"\n"),
		0o600,
	))
	require.NoError(t, os.Remove(filepath.Join(worktreePath, ".git")))
	unrelatedCWD := filepath.Join(
		t.TempDir(),
		strings.Repeat("unrelated/", 20),
	)
	require.NoError(t, os.MkdirAll(unrelatedCWD, 0o755))
	t.Chdir(unrelatedCWD)

	recovered, err := g.WorktreeGeneration(worktreePath)

	require.NoError(t, err)
	assert.Equal(t, generation, recovered)
}

func TestWorktreeGenerationRecoversInterruptedInitialization(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "interrupted-generation")
	worktreePath := filepath.Join(t.TempDir(), "interrupted-generation")
	repo.CreateWorktree(t, worktreePath, "interrupted-generation")
	adminDir, err := g.worktreeGitDir(worktreePath)
	require.NoError(t, err)
	generationPath := filepath.Join(adminDir, "kwt-generation")
	require.NoError(t, os.WriteFile(generationPath, []byte("partial"), 0o600))

	generation, err := g.WorktreeGeneration(worktreePath)

	require.NoError(t, err)
	require.NoError(t, ValidateWorktreeGeneration(generation))
	data, err := os.ReadFile(generationPath)
	require.NoError(t, err)
	assert.Equal(t, generation, strings.TrimSpace(string(data)))
}

func TestListWorktreesReportsGenerationInitializationFailure(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "broken-generation")
	worktreePath := filepath.Join(t.TempDir(), "broken-generation")
	repo.CreateWorktree(t, worktreePath, "broken-generation")
	adminDir, err := g.worktreeGitDir(worktreePath)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(
		filepath.Join(adminDir, "kwt-generation"),
		0700,
	))

	_, err = g.ListWorktrees()

	require.ErrorContains(t, err, "worktree generation")
}

func TestListWorktreesWaitsForConcurrentWorktreeReplacement(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "original")
	worktreePath := filepath.Join(t.TempDir(), "replacement")
	repo.CreateWorktree(t, worktreePath, "original")
	before, err := g.ListWorktrees()
	require.NoError(t, err)
	var originalGeneration string
	for _, worktree := range before {
		if utils.CanonicalPath(worktree.Path) ==
			utils.CanonicalPath(worktreePath) {
			originalGeneration = worktree.Generation
		}
	}
	require.NotEmpty(t, originalGeneration)

	lock := flock.New(
		filepath.Join(repo.Path, ".git", "kwt-worktree.lock"),
		flock.SetPermissions(0600),
	)
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { _ = lock.Unlock() })

	result := make(chan []models.Worktree, 1)
	listErr := make(chan error, 1)
	go func() {
		worktrees, err := g.ListWorktrees()
		if err != nil {
			listErr <- err
			return
		}
		result <- worktrees
	}()

	select {
	case err := <-listErr:
		require.NoError(t, err)
	case <-result:
		t.Fatal("worktree listing completed while replacement lock was held")
	case <-time.After(250 * time.Millisecond):
	}

	gitOutput(t, repo.Path, "worktree", "remove", worktreePath)
	gitOutput(t, repo.Path, "branch", "-D", "original")
	gitOutput(
		t,
		repo.Path,
		"worktree",
		"add",
		"-b",
		"replacement",
		worktreePath,
	)
	require.NoError(t, lock.Unlock())

	select {
	case err := <-listErr:
		require.NoError(t, err)
	case worktrees := <-result:
		for _, worktree := range worktrees {
			if utils.CanonicalPath(worktree.Path) ==
				utils.CanonicalPath(worktreePath) {
				assert.Equal(t, "replacement", worktree.Branch)
				assert.NotEqual(
					t,
					originalGeneration,
					worktree.Generation,
				)
				return
			}
		}
		t.Fatal("replacement worktree missing from locked snapshot")
	case <-time.After(5 * time.Second):
		t.Fatal("worktree listing did not resume after replacement")
	}
}

func TestRemoveWorktreeRejectsChangedGeneration(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "replacement")
	worktreePath := filepath.Join(t.TempDir(), "replacement")
	repo.CreateWorktree(t, worktreePath, "replacement")

	worktrees, err := g.ListWorktrees()
	require.NoError(t, err)
	var generation string
	for _, worktree := range worktrees {
		if utils.CanonicalPath(worktree.Path) == utils.CanonicalPath(worktreePath) {
			generation = worktree.Generation
		}
	}
	require.NotEmpty(t, generation)

	err = g.RemoveWorktree(worktreePath, false, "replacement-generation")

	require.ErrorContains(t, err, "generation changed")
	assert.DirExists(t, worktreePath)
}

func TestRemoveWorktreeCleansDirectoryAfterGitDeregistersWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell wrapper")
	}

	repo := NewTestRepository(t)
	repo.CreateBranch(t, "partially-removed")
	worktreePath := filepath.Join(t.TempDir(), "remove-wt")
	repo.CreateWorktree(t, worktreePath, "partially-removed")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git executable: %v", err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	wrapper := `#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "remove" ]; then
	"$REAL_GIT" "$@" || exit $?
	mkdir -p "$3"
	printf 'created during removal\n' > "$3/residual"
	printf "error: failed to delete '%s': Directory not empty\n" "$3" >&2
	exit 1
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0755); err != nil {
		t.Fatalf("write git wrapper: %v", err)
	}
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = New(worktreePath).RemoveWorktree(worktreePath, false, "")

	if err != nil {
		t.Fatalf("RemoveWorktree() error = %v", err)
	}
	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree path still exists after removal: stat error = %v", statErr)
	}
}

func TestRemoveWorktreeClassifiesResidualCleanupFailureAfterGitDeregistersWorktree(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX permissions and a shell wrapper")
	}

	repo := NewTestRepository(t)
	repo.CreateBranch(t, "partially-removed-cleanup-fails")
	worktreePath := filepath.Join(t.TempDir(), "remove-wt")
	repo.CreateWorktree(t, worktreePath, "partially-removed-cleanup-fails")
	protectedPath := filepath.Join(worktreePath, "protected")
	t.Cleanup(func() {
		_ = os.Chmod(protectedPath, 0700)
		_ = os.RemoveAll(worktreePath)
	})

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	wrapper := `#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "remove" ]; then
	"$REAL_GIT" "$@" || exit $?
	mkdir -p "$3/protected"
	printf 'created during removal\n' > "$3/protected/residual"
	chmod 500 "$3/protected"
	printf "error: failed to delete '%s': Directory not empty\n" "$3" >&2
	exit 1
fi
exec "$REAL_GIT" "$@"
`
	require.NoError(t, os.WriteFile(wrapperPath, []byte(wrapper), 0755))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = New(worktreePath).RemoveWorktree(worktreePath, false, "")

	require.Error(t, err)
	assert.True(t, WorktreeWasRemoved(err))
	assert.DirExists(t, protectedPath)
	cause := errors.Unwrap(err)
	require.Error(t, cause)
	assert.ErrorContains(t, cause, "git worktree remove")
	var pathErr *os.PathError
	require.ErrorAs(t, cause, &pathErr)
}

func TestConditionalRemoveWorktreePreservesDirectoryAfterGitDeregistersWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell wrapper")
	}

	repo := NewTestRepository(t)
	repo.CreateBranch(t, "partially-removed-conditionally")
	worktreePath := filepath.Join(t.TempDir(), "remove-wt")
	repo.CreateWorktree(t, worktreePath, "partially-removed-conditionally")

	g := New(worktreePath)
	worktrees, err := g.ListWorktrees()
	require.NoError(t, err)
	var generation string
	for _, worktree := range worktrees {
		if utils.CanonicalPath(worktree.Path) == utils.CanonicalPath(worktreePath) {
			generation = worktree.Generation
			break
		}
	}
	require.NotEmpty(t, generation)

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	wrapper := `#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "remove" ]; then
	"$REAL_GIT" "$@" || exit $?
	mkdir -p "$3"
	printf 'replacement created during removal\n' > "$3/replacement"
	printf "error: failed to delete '%s': Directory not empty\n" "$3" >&2
	exit 1
fi
exec "$REAL_GIT" "$@"
`
	require.NoError(t, os.WriteFile(wrapperPath, []byte(wrapper), 0755))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = g.RemoveWorktree(worktreePath, false, generation)

	require.Error(t, err)
	assert.True(t, WorktreeWasRemoved(err))
	assert.ErrorContains(
		t,
		err,
		"worktree removed, but files remain at "+worktreePath,
	)
	assert.ErrorContains(
		t,
		err,
		"inspect the path and remove it only if it contains leftovers from the removed worktree",
	)
	assert.FileExists(t, filepath.Join(worktreePath, "replacement"))
}

func TestPruneWorktrees(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Create a worktree
	repo.CreateBranch(t, "to-prune")
	worktreePath := filepath.Join(t.TempDir(), "prune-wt")
	repo.CreateWorktree(t, worktreePath, "to-prune")

	// Manually remove the worktree directory
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("Failed to remove worktree directory: %v", err)
	}

	// Prune worktrees
	err := g.PruneWorktrees()
	if err != nil {
		t.Fatalf("PruneWorktrees() error = %v", err)
	}

	// Verify worktree is pruned
	worktrees, _ := g.ListWorktrees()
	for _, wt := range worktrees {
		if wt.Path == worktreePath {
			t.Error("Deleted worktree still exists after prune")
		}
	}
}

func TestPruneWorktreesRejectsActiveCreationReservation(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	reservation, err := g.reserveWorktreeCreation(
		filepath.Join(t.TempDir(), "creating-worktree"),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, reservation.release())
	})

	err = g.PruneWorktrees()

	require.ErrorContains(t, err, "worktree creation in progress")
}

func TestListBranches(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Create test branches
	branches := []string{"feature/test", "bugfix/issue-123", "release/v1.0"}
	for _, branch := range branches {
		repo.CreateBranch(t, branch)

		// Add a commit to each branch
		testFile := filepath.Join(repo.Path, fmt.Sprintf("%s.txt", strings.ReplaceAll(branch, "/", "-")))
		if err := os.WriteFile(testFile, []byte(branch), 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
		if err := repo.run("add", "."); err != nil {
			t.Fatalf("Failed to add files: %v", err)
		}
		if err := repo.run("commit", "-m", fmt.Sprintf("Commit for %s", branch)); err != nil {
			t.Fatalf("Failed to commit: %v", err)
		}
	}

	// Test without remote branches
	t.Run("LocalOnly", func(t *testing.T) {
		branchList, err := g.ListBranches(false)
		if err != nil {
			t.Fatalf("ListBranches(false) error = %v", err)
		}

		// Should have main + 3 created branches
		if len(branchList) < 4 {
			t.Errorf("ListBranches(false) returned %d branches, want at least 4", len(branchList))
		}

		// Verify branch properties
		foundCurrent := false
		for _, b := range branchList {
			if b.IsCurrent {
				foundCurrent = true
			}
			if b.IsRemote {
				t.Error("Found remote branch when includeRemote=false")
			}

			// Verify commit info
			if b.LastCommit.Hash == "" {
				t.Errorf("Branch %s has empty commit hash", b.Name)
			}
			if b.LastCommit.Message == "" {
				t.Errorf("Branch %s has empty commit message", b.Name)
			}
			if b.LastCommit.Author == "" {
				t.Errorf("Branch %s has empty commit author", b.Name)
			}
			if b.LastCommit.Date.IsZero() {
				t.Errorf("Branch %s has zero commit date", b.Name)
			}
		}

		if !foundCurrent {
			t.Error("No current branch found")
		}
	})
}

func TestListAvailableBranchesNormalizesRemoteAndExcludesCheckedOut(t *testing.T) {
	repo := NewTestRepository(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	gitOutput(t, repo.Path, "remote", "add", "origin", remotePath)

	repo.CreateBranch(t, "local-ready")
	gitOutput(t, repo.Path, "checkout", "main")
	repo.CreateBranch(t, "checked-out")
	gitOutput(t, repo.Path, "checkout", "main")
	repo.CreateBranch(t, "remote-only")
	commitTestFile(t, repo.Path, "remote.txt", "remote\n", "Remote branch")
	gitOutput(t, repo.Path, "push", "origin", "main", "local-ready", "remote-only")
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", "remote-only")
	gitOutput(t, repo.Path, "remote", "set-head", "origin", "-a")

	worktreePath := filepath.Join(t.TempDir(), "checked-out")
	repo.CreateWorktree(t, worktreePath, "checked-out")

	branches, err := New(repo.Path).ListAvailableBranches()
	if err != nil {
		t.Fatalf("ListAvailableBranches() error = %v", err)
	}

	byName := make(map[string]models.Branch, len(branches))
	for _, branch := range branches {
		byName[branch.Name] = branch
	}
	if _, ok := byName["main"]; ok {
		t.Error("current branch was offered as available")
	}
	if _, ok := byName["checked-out"]; ok {
		t.Error("branch checked out in another worktree was offered as available")
	}
	if got := byName["local-ready"]; got.IsRemote || got.Source != "local-ready" {
		t.Errorf("local-ready = %+v, want available local branch", got)
	}
	if got := byName["remote-only"]; !got.IsRemote ||
		got.Source != "refs/remotes/origin/remote-only" {
		t.Errorf("remote-only = %+v, want normalized origin branch", got)
	}
	if _, ok := byName["HEAD"]; ok {
		t.Error("remote symbolic HEAD was offered as a branch")
	}
}

func TestListAvailableBranchesUsesCustomRemoteFetchRefspec(t *testing.T) {
	repo := NewTestRepository(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	gitOutput(t, repo.Path, "remote", "add", "origin", remotePath)
	gitOutput(t, repo.Path, "config", "--unset-all", "remote.origin.fetch")
	gitOutput(
		t,
		repo.Path,
		"config",
		"--add",
		"remote.origin.fetch",
		"+refs/heads/*:refs/remotes/pull/*",
	)

	repo.CreateBranch(t, "custom-fetch")
	gitOutput(t, repo.Path, "push", "origin", "custom-fetch")
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", "custom-fetch")
	gitOutput(t, repo.Path, "fetch", "origin")

	branches, err := New(repo.Path).ListAvailableBranches()
	require.NoError(t, err)

	for _, branch := range branches {
		if branch.Source != "refs/remotes/pull/custom-fetch" {
			continue
		}
		assert.Equal(t, "custom-fetch", branch.Name)
		assert.True(t, branch.IsRemote)
		return
	}
	t.Fatalf("custom-fetch branch not found: %+v", branches)
}

func TestListAvailableBranchesUsesFullRefsAcrossNamespaceCollisions(t *testing.T) {
	repo := NewTestRepository(t)
	remotePath := filepath.Join(t.TempDir(), "upstream.git")
	gitOutput(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	gitOutput(t, repo.Path, "remote", "add", "team/upstream", remotePath)

	repo.CreateBranch(t, "topic")
	commitTestFile(t, repo.Path, "topic.txt", "topic\n", "Topic branch")
	gitOutput(t, repo.Path, "push", "team/upstream", "topic")
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", "topic")
	gitOutput(t, repo.Path, "branch", "team/upstream/topic")
	gitOutput(t, repo.Path, "fetch", "team/upstream")

	branches, err := New(repo.Path).ListAvailableBranches()
	if err != nil {
		t.Fatalf("ListAvailableBranches() error = %v", err)
	}

	bySource := make(map[string]models.Branch, len(branches))
	for _, branch := range branches {
		bySource[branch.Source] = branch
	}
	if got := bySource["team/upstream/topic"]; got.Name != "team/upstream/topic" ||
		got.IsRemote {
		t.Errorf("local collision branch = %+v, want canonical local identity", got)
	}
	const remoteSource = "refs/remotes/team/upstream/topic"
	if got := bySource[remoteSource]; got.Name != "topic" || !got.IsRemote {
		t.Errorf("remote collision branch = %+v, want topic from %s", got, remoteSource)
	}
}

func TestListAvailableBranchesLabelsDuplicateRemoteNamesBySource(t *testing.T) {
	repo := NewTestRepository(t)
	for _, remote := range []string{"origin", "upstream"} {
		remotePath := filepath.Join(t.TempDir(), remote+".git")
		gitOutput(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
		gitOutput(t, repo.Path, "remote", "add", remote, remotePath)
	}
	repo.CreateBranch(t, "topic")
	commitTestFile(t, repo.Path, "topic.txt", "topic\n", "Topic")
	gitOutput(t, repo.Path, "push", "origin", "topic")
	gitOutput(t, repo.Path, "push", "upstream", "topic")
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", "topic")

	branches, err := New(repo.Path).ListAvailableBranches()
	if err != nil {
		t.Fatalf("ListAvailableBranches() error = %v", err)
	}

	labels := make(map[string]string)
	for _, branch := range branches {
		if branch.Name == "topic" {
			labels[branch.Source] = branch.Label
		}
	}
	if got := labels["refs/remotes/origin/topic"]; got != "topic (origin/topic)" {
		t.Errorf("origin label = %q, want source-qualified label", got)
	}
	if got := labels["refs/remotes/upstream/topic"]; got != "topic (upstream/topic)" {
		t.Errorf("upstream label = %q, want source-qualified label", got)
	}
}

func TestListAvailableBranchesPreservesDelimiterCharacters(t *testing.T) {
	repo := NewTestRepository(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	gitOutput(t, repo.Path, "remote", "add", "origin", remotePath)

	branchName := "topic|review"
	if runtime.GOOS == "windows" {
		// NTFS cannot materialize a loose ref containing "|". The commit
		// subject still exercises NUL-delimited parsing on Windows, while the
		// ref-name case runs on filesystems that support it.
		branchName = "topic-review"
	}
	repo.CreateBranch(t, branchName)
	commitTestFile(t, repo.Path, "topic.txt", "topic\n", "Subject | details")
	gitOutput(t, repo.Path, "push", "origin", branchName)
	gitOutput(t, repo.Path, "checkout", "main")
	gitOutput(t, repo.Path, "branch", "-D", branchName)

	branches, err := New(repo.Path).ListAvailableBranches()
	if err != nil {
		t.Fatalf("ListAvailableBranches() error = %v", err)
	}

	source := "refs/remotes/origin/" + branchName
	for _, branch := range branches {
		if branch.Source != source {
			continue
		}
		if branch.Name != branchName {
			t.Errorf("branch name = %q, want %s", branch.Name, branchName)
		}
		if branch.LastCommit.Message != "Subject | details" {
			t.Errorf(
				"commit subject = %q, want Subject | details",
				branch.LastCommit.Message,
			)
		}
		return
	}
	t.Fatalf("remote branch %s not found: %+v", source, branches)
}

func TestAddWorktreeTrackingRemoteBranch(t *testing.T) {
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
	err := New(repo.Path).AddWorktreeTracking(
		worktreePath,
		"remote-only",
		"origin/remote-only",
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN", "custom_fleet_token"},
	)
	t.Setenv("KWT_GITHUB_TOKEN", "")
	t.Setenv("KWT_FLEET_TOKEN", "")
	t.Setenv("Custom_Fleet_Token", "")
	if err != nil {
		t.Fatalf("AddWorktreeTracking() error = %v", err)
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

func TestAddWorktreeTrackingRollsBackBranchWhenWorktreeFails(t *testing.T) {
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

	err := New(repo.Path).AddWorktreeTracking(
		occupiedPath,
		"remote-only",
		"refs/remotes/origin/remote-only",
		nil,
	)

	if err == nil {
		t.Fatal("AddWorktreeTracking() expected an error")
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

func TestAddWorktreeTrackingRejectsOptionLikeBranchName(t *testing.T) {
	repo := NewTestRepository(t)
	gitOutput(
		t,
		repo.Path,
		"update-ref",
		"refs/remotes/origin/-M",
		"HEAD",
	)

	worktreePath := filepath.Join(t.TempDir(), "option-like")
	err := New(repo.Path).AddWorktreeTracking(
		worktreePath,
		"-M",
		"refs/remotes/origin/-M",
		nil,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid local branch name "-M"`)
	assert.Equal(t, "main", gitOutput(t, repo.Path, "branch", "--show-current"))
	assert.NoDirExists(t, worktreePath)
}

func TestAddWorktreeTrackingReusesMatchingOrphanBranch(t *testing.T) {
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

	err := New(repo.Path).AddWorktreeTracking(
		worktreePath,
		"orphaned",
		"refs/remotes/origin/orphaned",
		nil,
	)

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

func TestAddWorktreeTrackingRejectsDivergentOrphanBranch(t *testing.T) {
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

	err := New(repo.Path).AddWorktreeTracking(
		worktreePath,
		"diverged",
		"refs/remotes/origin/diverged",
		nil,
	)

	require.ErrorContains(t, err, "points to a different commit")
	assert.NoDirExists(t, worktreePath)
}

func TestAddWorktreeExistingRefusesGenerationlessRegisteredWorktree(
	t *testing.T,
) {
	repo := NewTestRepository(t)
	repo.CreateBranch(t, "legacy-existing")
	worktreePath := filepath.Join(t.TempDir(), "legacy-existing")
	repo.CreateWorktree(t, worktreePath, "legacy-existing")
	keepPath := filepath.Join(worktreePath, "keep")
	require.NoError(t, os.WriteFile(keepPath, []byte("preserve me"), 0o600))

	err := New(repo.Path).AddWorktreeExisting(
		worktreePath,
		"legacy-existing",
		nil,
	)

	require.ErrorContains(t, err, "already registered without a generation")
	data, readErr := os.ReadFile(keepPath)
	require.NoError(t, readErr)
	assert.Equal(t, "preserve me", string(data))
}

func TestAddWorktreeExistingRemovesWorktreeAfterCheckoutFailure(t *testing.T) {
	repo := NewTestRepository(t)
	createBranchWithMissingBlob(t, repo, "broken-local")
	worktreePath := filepath.Join(t.TempDir(), "broken-local")

	err := New(repo.Path).AddWorktreeExisting(
		worktreePath,
		"broken-local",
		nil,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to check out existing-branch worktree")
	assert.NoDirExists(t, worktreePath)
	worktrees, listErr := New(repo.Path).ListWorktrees()
	require.NoError(t, listErr)
	for _, worktree := range worktrees {
		assert.NotEqual(t, worktreePath, worktree.Path)
	}
}

func TestAddWorktreeExistingRejectsRemoteOnlyBranch(t *testing.T) {
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

	err := New(repo.Path).AddWorktreeExisting(
		worktreePath,
		"remote-only-local-import",
		nil,
	)

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

func TestAddWorktreeTrackingRemovesWorktreeAndBranchAfterCheckoutFailure(
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

	err := New(repo.Path).AddWorktreeTracking(
		worktreePath,
		"broken-remote",
		"refs/remotes/origin/broken-remote",
		nil,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to check out worktree tracking")
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
	worktrees, listErr := New(repo.Path).ListWorktrees()
	require.NoError(t, listErr)
	for _, worktree := range worktrees {
		assert.NotEqual(t, worktreePath, worktree.Path)
	}
}

func TestGetRepositoryName(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	name, err := g.GetRepositoryName()
	if err != nil {
		t.Fatalf("GetRepositoryName() error = %v", err)
	}

	// Repository name should be the base of the temp directory
	expectedName := filepath.Base(repo.Path)
	if name != expectedName {
		t.Errorf("GetRepositoryName() = %s, want %s", name, expectedName)
	}
}

func TestGetRecentCommits(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Create multiple commits
	expectedMessages := []string{
		"Third commit",
		"Second commit",
		"First additional commit",
	}

	for i := len(expectedMessages) - 1; i >= 0; i-- {
		testFile := filepath.Join(repo.Path, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(testFile, fmt.Appendf(nil, "Content %d", i), 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
		if err := repo.run("add", "."); err != nil {
			t.Fatalf("Failed to add files: %v", err)
		}
		if err := repo.run("commit", "-m", expectedMessages[i]); err != nil {
			t.Fatalf("Failed to commit: %v", err)
		}

		// Small delay to ensure different timestamps
		time.Sleep(10 * time.Millisecond)
	}

	// Get recent commits
	commits, err := g.GetRecentCommits(repo.Path, 3)
	if err != nil {
		t.Fatalf("GetRecentCommits() error = %v", err)
	}

	if len(commits) != 3 {
		t.Errorf("GetRecentCommits() returned %d commits, want 3", len(commits))
	}

	// Verify commit messages (should be in reverse chronological order)
	for i, commit := range commits {
		if commit.Message != expectedMessages[i] {
			t.Errorf("Commit[%d].Message = %s, want %s", i, commit.Message, expectedMessages[i])
		}
		if commit.Hash == "" {
			t.Errorf("Commit[%d] has empty hash", i)
		}
		if commit.Author != "Test User" {
			t.Errorf("Commit[%d].Author = %s, want Test User", i, commit.Author)
		}
		if commit.Date.IsZero() {
			t.Errorf("Commit[%d] has zero date", i)
		}
	}
}

func TestGetCurrentBranch(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Test on main branch
	branch := g.getCurrentBranch(repo.Path)
	if branch != "main" && branch != "master" {
		t.Errorf("getCurrentBranch() = %s, want main or master", branch)
	}

	// Create and checkout a new branch
	repo.CreateBranch(t, "test-branch")

	branch = g.getCurrentBranch(repo.Path)
	if branch != "test-branch" {
		t.Errorf("getCurrentBranch() after checkout = %s, want test-branch", branch)
	}
}

func TestGetRootDir(t *testing.T) {
	repo := NewTestRepository(t)

	// Create a subdirectory
	subDir := filepath.Join(repo.Path, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}

	// Test from subdirectory
	g := New(subDir)
	rootDir, err := g.getRootDir()
	if err != nil {
		t.Fatalf("getRootDir() error = %v", err)
	}

	// macOS may use /private/var symlinks, so resolve paths before comparing
	resolvedRootDir, _ := filepath.EvalSymlinks(rootDir)
	resolvedRepoPath, _ := filepath.EvalSymlinks(repo.Path)

	if resolvedRootDir != resolvedRepoPath {
		t.Errorf("getRootDir() = %s, want %s", resolvedRootDir, resolvedRepoPath)
	}
}

func TestRunCommand(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Test successful command
	output, err := g.run("status", "--short")
	if err != nil {
		t.Fatalf("run('status --short') error = %v", err)
	}

	// Output should be empty for clean repository
	if strings.TrimSpace(output) != "" {
		t.Errorf("run('status --short') output = %s, want empty", output)
	}

	// Test failed command
	_, err = g.run("invalid-command")
	if err == nil {
		t.Error("run('invalid-command') should return error")
	}
}

func TestGetMainRepositoryPath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, repo *TestRepository) string // returns workDir
	}{
		{
			name: "from main repo root",
			setup: func(t *testing.T, repo *TestRepository) string {
				t.Helper()
				return repo.Path
			},
		},
		{
			name: "from main repo subdirectory",
			setup: func(t *testing.T, repo *TestRepository) string {
				t.Helper()
				subDir := filepath.Join(repo.Path, "sub", "dir")
				if err := os.MkdirAll(subDir, 0755); err != nil {
					t.Fatalf("failed to create subdir: %v", err)
				}
				return subDir
			},
		},
		{
			name: "from worktree root",
			setup: func(t *testing.T, repo *TestRepository) string {
				t.Helper()
				repo.CreateBranch(t, "test-main-path")
				wtPath := filepath.Join(t.TempDir(), "wt")
				repo.CreateWorktree(t, wtPath, "test-main-path")
				return wtPath
			},
		},
		{
			name: "from worktree subdirectory",
			setup: func(t *testing.T, repo *TestRepository) string {
				t.Helper()
				repo.CreateBranch(t, "test-main-path-sub")
				wtPath := filepath.Join(t.TempDir(), "wt-sub")
				repo.CreateWorktree(t, wtPath, "test-main-path-sub")
				subDir := filepath.Join(wtPath, "nested")
				if err := os.MkdirAll(subDir, 0755); err != nil {
					t.Fatalf("failed to create subdir: %v", err)
				}
				return subDir
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := NewTestRepository(t)
			workDir := tt.setup(t, repo)
			g := New(workDir)

			got, err := g.GetMainRepositoryPath()
			if err != nil {
				t.Fatalf("GetMainRepositoryPath() error = %v", err)
			}

			resolvedGot, _ := filepath.EvalSymlinks(got)
			resolvedWant, _ := filepath.EvalSymlinks(repo.Path)
			if resolvedGot != resolvedWant {
				t.Errorf("GetMainRepositoryPath() = %s, want %s", resolvedGot, resolvedWant)
			}
		})
	}
}

func TestListWorktrees_IsMainFromWorktree(t *testing.T) {
	repo := NewTestRepository(t)
	repo.CreateBranch(t, "test-is-main")
	wtPath := filepath.Join(t.TempDir(), "wt-is-main")
	repo.CreateWorktree(t, wtPath, "test-is-main")

	// Create Git instance from worktree path
	g := New(wtPath)
	worktrees, err := g.ListWorktrees()
	if err != nil {
		t.Fatalf("ListWorktrees() error = %v", err)
	}

	var foundMain bool
	for _, wt := range worktrees {
		if wt.IsMain {
			foundMain = true
			resolvedWtPath, _ := filepath.EvalSymlinks(wt.Path)
			resolvedRepoPath, _ := filepath.EvalSymlinks(repo.Path)
			if resolvedWtPath != resolvedRepoPath {
				t.Errorf("Main worktree path = %s, want %s", resolvedWtPath, resolvedRepoPath)
			}
		}
	}
	if !foundMain {
		t.Error("expected IsMain=true worktree not found")
	}
}

// Helper function to compare worktrees with path resolution
func containsWorktreeWithPath(worktrees []models.Worktree, path string) bool {
	resolvedPath, _ := filepath.EvalSymlinks(path)
	for _, wt := range worktrees {
		resolvedWtPath, _ := filepath.EvalSymlinks(wt.Path)
		if resolvedWtPath == resolvedPath {
			return true
		}
	}
	return false
}
