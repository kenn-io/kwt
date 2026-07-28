package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestRemoveWorktree(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)

	// Create a worktree to remove
	repo.CreateBranch(t, "to-remove")
	worktreePath := filepath.Join(t.TempDir(), "remove-wt")
	repo.CreateWorktree(t, worktreePath, "to-remove")

	// Remove the worktree
	err := g.RemoveWorktree(worktreePath, false)
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

	err = New(worktreePath).RemoveWorktree(worktreePath, false)

	if err != nil {
		t.Fatalf("RemoveWorktree() error = %v", err)
	}
	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree path still exists after removal: stat error = %v", statErr)
	}
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
