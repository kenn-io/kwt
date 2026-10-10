package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
	shared "go.kenn.io/kwt/worktree"
)

func TestRunBytesWithEnvironmentPreservesEmbeddedNUL(t *testing.T) {
	g := NewForInventory(context.Background(), t.TempDir(), nil)
	g.executable = os.Args[0]

	got, err := g.RunBytesWithEnvironment(
		map[string]string{"KWT_TEST_GIT_BYTE_HELPER": "1"},
		"-test.run=^TestGitByteRunnerHelperProcess$",
		"--",
		"nul",
	)

	require.NoError(t, err)
	assert.True(t, bytes.Equal([]byte{'a', 0, 'b'}, got), "output = %v", got)
}

func TestRunBytesWithEnvironmentBoundsStdoutAndStderrIndependently(t *testing.T) {
	tests := []struct {
		name     string
		scenario string
		wantErr  error
	}{
		{name: "stdout", scenario: "stdout-limit", wantErr: ErrStdoutLimitExceeded},
		{name: "stderr", scenario: "stderr-limit", wantErr: ErrStderrLimitExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewForInventory(context.Background(), t.TempDir(), nil)
			g.executable = os.Args[0]

			_, err := g.RunBytesWithEnvironment(
				map[string]string{"KWT_TEST_GIT_BYTE_HELPER": "1"},
				"-test.run=^TestGitByteRunnerHelperProcess$",
				"--",
				tt.scenario,
			)

			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestRunBytesWithEnvironmentPreservesRunErrorWhenOutputLimitExceeded(t *testing.T) {
	g := NewForInventory(context.Background(), t.TempDir(), nil)
	g.executable = os.Args[0]

	_, err := g.RunBytesWithEnvironment(
		map[string]string{"KWT_TEST_GIT_BYTE_HELPER": "1"},
		"-test.run=^TestGitByteRunnerHelperProcess$",
		"--",
		"stderr-limit-exit",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStderrLimitExceeded)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
}

func TestRunBytesWithEnvironmentPreservesRunErrorOnNonZeroExit(t *testing.T) {
	g := NewForInventory(context.Background(), t.TempDir(), nil)
	g.executable = os.Args[0]

	_, err := g.RunBytesWithEnvironment(
		map[string]string{"KWT_TEST_GIT_BYTE_HELPER": "1"},
		"-test.run=^TestGitByteRunnerHelperProcess$",
		"--",
		"stderr-exit",
	)

	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
	assert.ErrorContains(t, err, "bounded diagnostic")
}

func TestRunBytesWithEnvironmentPreservesExecutableLookupError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	g := NewForInventory(context.Background(), t.TempDir(), nil)

	_, err := g.RunBytesWithEnvironment(nil, "status")

	require.Error(t, err)
	var executableErr *exec.Error
	require.ErrorAs(t, err, &executableErr)
	assert.NotEmpty(t, executableErr.Err)
}

func TestRunBytesWithEnvironmentHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	g := NewForInventory(ctx, t.TempDir(), nil)
	g.executable = os.Args[0]

	_, err := g.RunBytesWithEnvironment(
		map[string]string{"KWT_TEST_GIT_BYTE_HELPER": "1"},
		"-test.run=^TestGitByteRunnerHelperProcess$",
		"--",
		"sleep",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunBytesWithEnvironmentBoundsRetainedOutputPipes(t *testing.T) {
	for _, descriptor := range []string{"stdout", "stderr"} {
		t.Run(descriptor, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "descendant.pid")
			t.Cleanup(func() {
				data, err := os.ReadFile(pidPath)
				if err != nil {
					return
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					return
				}
				process, err := os.FindProcess(pid)
				if err == nil {
					_ = process.Kill()
				}
			})
			g := NewForInventory(context.Background(), t.TempDir(), nil)
			g.executable = os.Args[0]

			started := time.Now()
			_, err := g.RunBytesWithEnvironment(
				map[string]string{
					// The fixture runs this race-instrumented test binary, unlike
					// Git. Exclude the race runtime's one-second exit sleep
					// from the command's retained-pipe timing.
					"GORACE":                      "atexit_sleep_ms=0",
					"KWT_TEST_GIT_BYTE_HELPER":    "1",
					"KWT_TEST_GIT_DESCENDANT_PID": pidPath,
				},
				"-test.run=^TestGitByteRunnerHelperProcess$",
				"--",
				"retain-"+descriptor,
			)
			elapsed := time.Since(started)

			require.Error(t, err)
			assert.ErrorIs(t, err, exec.ErrWaitDelay)
			assert.Less(t, elapsed, time.Second)
		})
	}
}

func TestRunCommandAllowsSuccessfulCommandsToFinishRetainedOutputPipes(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		process, err := os.FindProcess(pid)
		if err == nil {
			_ = process.Kill()
		}
	})
	donePath := filepath.Join(t.TempDir(), "descendant.done")
	t.Setenv("KWT_TEST_GIT_BYTE_HELPER", "1")
	t.Setenv("KWT_TEST_GIT_DESCENDANT_PID", pidPath)
	t.Setenv("KWT_TEST_GIT_PIPE_HOLDER_DONE", donePath)
	t.Setenv("KWT_TEST_GIT_PIPE_HOLDER_DELAY", "2s")
	g := New(t.TempDir())
	g.executable = os.Args[0]

	_, err := g.RunCommand(
		"-test.run=^TestGitByteRunnerHelperProcess$",
		"--",
		"retain-stdout",
	)

	require.NoError(t, err)
	assert.NoFileExists(t, donePath)
}

func TestInventoryGitAllowsBranchDeletionToFinishRetainedHookPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX Git hook")
	}

	repo := NewTestRepository(t)
	repo.CreateBranch(t, "delete-with-retained-hook-pipe")
	require.NoError(t, repo.run("checkout", "main"))
	hooksDir := t.TempDir()
	donePath := filepath.Join(t.TempDir(), "hook-descendant.done")
	t.Setenv("KWT_TEST_GIT_HOOK_DONE", donePath)
	require.NoError(t, os.WriteFile(
		filepath.Join(hooksDir, "reference-transaction"),
		[]byte("#!/bin/sh\nif [ \"$1\" = committed ]; then\n  (sleep 2; : > \"$KWT_TEST_GIT_HOOK_DONE\") &\nfi\n"),
		0o755,
	))
	require.NoError(t, repo.run("config", "core.hooksPath", hooksDir))
	g := NewForInventory(context.Background(), repo.Path, nil)

	_, err := g.RunCommand("branch", "-D", "delete-with-retained-hook-pipe")

	require.NoError(t, err)
	assert.NoFileExists(t, donePath)
	_, err = g.RunCommand(
		"show-ref",
		"--verify",
		"refs/heads/delete-with-retained-hook-pipe",
	)
	require.Error(t, err)
}

func TestRunBytesWithEnvironmentUsesInventoryEnvironmentAndOverrides(t *testing.T) {
	t.Setenv("GIT_DIR", "/tmp/redirected.git")
	t.Setenv("KWT_TEST_SECRET", "secret")
	g := NewForInventory(
		context.Background(),
		t.TempDir(),
		[]string{"KWT_TEST_SECRET"},
	)
	g.executable = os.Args[0]

	got, err := g.RunBytesWithEnvironment(
		map[string]string{
			"GIT_OPTIONAL_LOCKS":       "0",
			"KWT_TEST_GIT_BYTE_HELPER": "1",
			"LC_ALL":                   "C",
		},
		"-test.run=^TestGitByteRunnerHelperProcess$",
		"--",
		"environment",
	)

	require.NoError(t, err)
	assert.Equal(
		t,
		"git_dir=|secret=|locks=0|locale=C",
		string(got),
	)
}

func TestGitByteRunnerHelperProcess(t *testing.T) {
	if os.Getenv("KWT_TEST_GIT_BYTE_HELPER") != "1" {
		return
	}
	if os.Getenv("KWT_TEST_GIT_PIPE_HOLDER") == "1" {
		delay := 2 * time.Second
		if configured := os.Getenv("KWT_TEST_GIT_PIPE_HOLDER_DELAY"); configured != "" {
			parsed, err := time.ParseDuration(configured)
			if err != nil {
				os.Exit(5)
			}
			delay = parsed
		}
		time.Sleep(delay)
		if donePath := os.Getenv("KWT_TEST_GIT_PIPE_HOLDER_DONE"); donePath != "" {
			if err := os.WriteFile(donePath, []byte("done\n"), 0o600); err != nil {
				os.Exit(6)
			}
		}
		os.Exit(0)
	}

	switch os.Args[len(os.Args)-1] {
	case "nul":
		_, _ = os.Stdout.Write([]byte{'a', 0, 'b'})
	case "stdout-limit":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte{'o'}, commandOutputLimit+1))
		_, _ = os.Stderr.Write([]byte("bounded diagnostic"))
	case "stderr-limit":
		_, _ = os.Stdout.Write([]byte("bounded output"))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{'e'}, commandOutputLimit+1))
	case "stderr-limit-exit":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{'e'}, commandOutputLimit+1))
		os.Exit(7)
	case "stderr-exit":
		_, _ = os.Stderr.Write([]byte("bounded diagnostic"))
		os.Exit(7)
	case "sleep":
		time.Sleep(time.Minute)
	case "retain-stdout", "retain-stderr":
		descriptor := strings.TrimPrefix(os.Args[len(os.Args)-1], "retain-")
		descendant := exec.Command(
			os.Args[0],
			"-test.run=^TestGitByteRunnerHelperProcess$",
		)
		descendant.Env = append(os.Environ(), "KWT_TEST_GIT_PIPE_HOLDER=1")
		if descriptor == "stdout" {
			descendant.Stdout = os.Stdout
		} else {
			descendant.Stderr = os.Stderr
		}
		if err := descendant.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(
			os.Getenv("KWT_TEST_GIT_DESCENDANT_PID"),
			[]byte(strconv.Itoa(descendant.Process.Pid)),
			0o600,
		); err != nil {
			os.Exit(4)
		}
	case "environment":
		_, _ = fmt.Fprintf(
			os.Stdout,
			"git_dir=%s|secret=%s|locks=%s|locale=%s",
			os.Getenv("GIT_DIR"),
			os.Getenv("KWT_TEST_SECRET"),
			os.Getenv("GIT_OPTIONAL_LOCKS"),
			os.Getenv("LC_ALL"),
		)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestGitOperationsUseInstanceContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewWithContext(ctx, t.TempDir()).RunCommand("version")

	assert.ErrorIs(t, err, context.Canceled)
}

func TestInventoryEnvironmentRemovesGitRoutingAndCredentials(t *testing.T) {
	got := inventoryEnvironment([]string{
		"PATH=/usr/bin",
		"HOME=/home/test",
		"GIT_DIR=/tmp/redirected.git",
		"git_work_tree=/tmp/redirected-worktree",
		"GIT_COMMON_DIR=/tmp/redirected-common",
		"GIT_INDEX_FILE=/tmp/redirected-index",
		"GIT_NAMESPACE=redirected",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.filemode",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_GLOBAL=/tmp/global.gitconfig",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/objects",
		"KWT_GITHUB_TOKEN=builtin-secret",
		"CUSTOM_FLEET_TOKEN=custom-secret",
	}, []string{"CUSTOM_FLEET_TOKEN"})

	assert.ElementsMatch(t, []string{
		"PATH=/usr/bin",
		"HOME=/home/test",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.filemode",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_GLOBAL=/tmp/global.gitconfig",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/objects",
		"GIT_TERMINAL_PROMPT=0",
	}, got)
}

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

func newSeparateGitDirectoryRepository(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	mainPath := filepath.Join(base, "main-worktree")
	separateGitDir := filepath.Join(base, "repository.git")
	linkedPath := filepath.Join(base, "linked-worktree")
	gitOutput(
		t,
		base,
		"init",
		"-b",
		"main",
		"--separate-git-dir",
		separateGitDir,
		mainPath,
	)
	gitOutput(t, mainPath, "config", "user.name", "Test User")
	gitOutput(t, mainPath, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(
		filepath.Join(mainPath, "README.md"),
		[]byte("# Separate Git directory\n"),
		0o644,
	))
	gitOutput(t, mainPath, "add", "README.md")
	gitOutput(t, mainPath, "commit", "-m", "Initial commit")
	gitOutput(t, mainPath, "branch", "topic")
	gitOutput(t, mainPath, "worktree", "add", linkedPath, "topic")
	return mainPath, linkedPath
}

func newBareContainerRepository(t *testing.T) (string, string, string) {
	return newNamedBareContainerRepository(t, ".bare")
}

func newNamedBareContainerRepository(
	t *testing.T,
	controlDirectory string,
) (string, string, string) {
	t.Helper()
	container := filepath.Join(t.TempDir(), "widget")
	barePath := filepath.Join(container, controlDirectory)
	seedPath := filepath.Join(t.TempDir(), "seed")
	mainPath := filepath.Join(container, "main")
	linkedPath := filepath.Join(container, "feature-topic")
	require.NoError(t, os.MkdirAll(container, 0o755))
	gitOutput(t, filepath.Dir(seedPath), "init", "-b", "main", seedPath)
	gitOutput(t, seedPath, "config", "user.name", "Test User")
	gitOutput(t, seedPath, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(
		filepath.Join(seedPath, "README.md"),
		[]byte("# Bare container\n"),
		0o644,
	))
	gitOutput(t, seedPath, "add", "README.md")
	gitOutput(t, seedPath, "commit", "-m", "Initial commit")
	gitOutput(t, filepath.Dir(barePath), "clone", "--bare", seedPath, barePath)
	gitOutput(t, barePath, "worktree", "add", mainPath, "main")
	gitOutput(t, mainPath, "branch", "feature/topic")
	gitOutput(t, barePath, "worktree", "add", linkedPath, "feature/topic")
	return container, mainPath, linkedPath
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
	worktrees, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
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

func TestListWorktreesKeepsGenerationStableWhenDirectoryChanges(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "stable-generation")
	worktreePath := filepath.Join(t.TempDir(), "stable-generation")
	repo.CreateWorktree(t, worktreePath, "stable-generation")

	before, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
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

	after, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	for _, worktree := range after {
		if utils.CanonicalPath(worktree.Path) == utils.CanonicalPath(worktreePath) {
			assert.Equal(t, generation, worktree.Generation)
			return
		}
	}
	t.Fatal("worktree missing after ordinary directory change")
}

func TestInspectWorktreesReportsMissingDirectoryWithoutInitializing(t *testing.T) {
	repo := NewTestRepository(t)
	repo.CreateBranch(t, "missing-topic")
	worktreePath := filepath.Join(t.TempDir(), "missing-topic")
	repo.CreateWorktree(t, worktreePath, "missing-topic")
	g := New(repo.Path)
	adminDir, err := shared.ReadWorktreeBacklink(t.Context(), worktreePath)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(worktreePath))

	inspections, err := inspectSharedWorktrees(t, g)
	require.NoError(t, err)
	inspection := requireWorktreeInspection(t, inspections, worktreePath)
	assert.False(t, inspection.Exists)
	assert.True(t, inspection.Prunable)
	assert.Equal(t, shared.GenerationMissing, inspection.GenerationStatus)
	assert.NoFileExists(t, filepath.Join(adminDir, "kwt-generation"))
}

func TestWorktreeInspectionJSONUsesSnakeCaseFields(t *testing.T) {
	encoded, err := json.Marshal(shared.Entry{
		Path:             "/worktrees/topic",
		GenerationStatus: shared.GenerationValid,
		IsMain:           true,
		LockedReason:     "maintenance",
	})
	require.NoError(t, err)

	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	assert.Equal(t, "/worktrees/topic", fields["path"])
	assert.Equal(t, string(shared.GenerationValid), fields["generation_status"])
	assert.Equal(t, true, fields["is_main"])
	assert.Equal(t, "maintenance", fields["locked_reason"])
	assert.NotContains(t, fields, "Path")
}

func requireWorktreeInspection(
	t *testing.T,
	inspections []shared.Entry,
	path string,
) shared.Entry {
	t.Helper()
	for _, inspection := range inspections {
		if utils.PathKey(inspection.Path) == utils.PathKey(path) {
			return inspection
		}
	}
	t.Fatalf("worktree inspection missing for %s", path)
	return shared.Entry{}
}

func TestBranchUpstream(t *testing.T) {
	tests := []struct {
		name       string
		remote     string
		remoteURL  string
		mergeRef   string
		wantBranch string
	}{
		{
			name:       "same repository",
			remote:     "origin",
			remoteURL:  "https://github.com/acme/widget.git",
			mergeRef:   "refs/heads/topic",
			wantBranch: "topic",
		},
		{
			name:       "fork remote",
			remote:     "fork",
			remoteURL:  "git@github.com:alice/widget.git",
			mergeRef:   "refs/heads/topic",
			wantBranch: "topic",
		},
		{
			name:       "slash remote name",
			remote:     "fork/alice",
			remoteURL:  "ssh://git@github.com/alice/widget.git",
			mergeRef:   "refs/heads/team/topic",
			wantBranch: "team/topic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := NewTestRepository(t)
			require.NoError(t, repo.run("config", "branch.topic.remote", tt.remote))
			require.NoError(t, repo.run("config", "branch.topic.merge", tt.mergeRef))
			require.NoError(t, repo.run("config", "remote."+tt.remote+".url", tt.remoteURL))

			got, err := New(repo.Path).BranchUpstream("topic")
			require.NoError(t, err)
			assert.Equal(t, tt.remote, got.Remote)
			assert.Equal(t, tt.wantBranch, got.Branch)
			assert.Equal(t, tt.remoteURL, got.RepositoryURL)
		})
	}
}

func TestBranchUpstreamResolvesInsteadOfURL(t *testing.T) {
	repo := NewTestRepository(t)
	require.NoError(t, repo.run("config", "branch.topic.remote", "origin"))
	require.NoError(t, repo.run("config", "branch.topic.merge", "refs/heads/topic"))
	require.NoError(t, repo.run("config", "remote.origin.url", "gh:acme/widget.git"))
	require.NoError(t, repo.run("config", "url.https://github.com/.insteadOf", "gh:"))

	got, err := New(repo.Path).BranchUpstream("topic")

	require.NoError(t, err)
	assert.Equal(t, "https://github.com/acme/widget.git", got.RepositoryURL)
}

func TestBranchUpstreamRejectsMissingUpstream(t *testing.T) {
	repo := NewTestRepository(t)

	_, err := New(repo.Path).BranchUpstream("topic")

	require.ErrorContains(t, err, "upstream remote")
}

func TestListWorktreesDoesNotAdoptGenerationFromAnotherRepository(t *testing.T) {
	repoA := NewTestRepository(t)
	repoB := NewTestRepository(t)
	repoA.CreateBranch(t, "repo-a-worktree")
	repoB.CreateBranch(t, "repo-b-worktree")
	worktreePath := filepath.Join(t.TempDir(), "reused-worktree")
	repoA.CreateWorktree(t, worktreePath, "repo-a-worktree")

	before, err := openSharedWorktrees(t, New(repoA.Path)).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
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
	repoBGeneration, err := openSharedWorktrees(t, New(repoB.Path)).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	require.NotEqual(t, repoAGeneration, repoBGeneration)

	_, err = openSharedWorktrees(t, New(repoA.Path)).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})

	require.ErrorContains(t, err, "belongs to a different repository")
}

func TestReadWorktreeGenerationRejectsAnotherWorktreeAdministrativeDirectory(
	t *testing.T,
) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "first-worktree")
	repo.CreateBranch(t, "second-worktree")
	firstPath := filepath.Join(t.TempDir(), "first-worktree")
	secondPath := filepath.Join(t.TempDir(), "second-worktree")
	repo.CreateWorktree(t, firstPath, "first-worktree")
	repo.CreateWorktree(t, secondPath, "second-worktree")
	firstGeneration, err := openSharedWorktrees(t, g).EnsureIdentity(t.Context(), firstPath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	secondGeneration, err := openSharedWorktrees(t, g).EnsureIdentity(t.Context(), secondPath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	require.NotEqual(t, firstGeneration, secondGeneration)
	secondAdminDir, err := shared.ReadWorktreeBacklink(t.Context(), secondPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(firstPath, ".git"),
		[]byte("gitdir: "+secondAdminDir+"\n"),
		0o600,
	))

	generation, err := openSharedWorktrees(t, g).ReadIdentity(t.Context(), firstPath, "kwt-generation")

	require.NoError(t, err)
	assert.Equal(t, firstGeneration, generation)
}

func TestReadWorktreeGenerationClassifiesMissingWorktree(t *testing.T) {
	repo := NewTestRepository(t)

	_, err := openSharedWorktrees(t, New(repo.Path)).ReadIdentity(t.Context(), filepath.Join(t.TempDir(), "missing-worktree"), "kwt-generation")

	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrWorktreeNotFound)
}

func TestReadWorktreeGenerationClassifiesRemovedWorkingDirectory(t *testing.T) {
	repo := NewTestRepository(t)
	repo.CreateBranch(t, "removed-worktree")
	worktreePath := filepath.Join(t.TempDir(), "removed-worktree")
	repo.CreateWorktree(t, worktreePath, "removed-worktree")
	_, err := openSharedWorktrees(t, New(repo.Path)).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(worktreePath))

	_, err = NewForInventory(t.Context(), worktreePath, nil).WorktreeRepository(t.Context(), nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrWorktreeNotFound)
}

func TestReadWorktreeGenerationClassifiesAnotherRepositoryOwner(t *testing.T) {
	staleRepo := NewTestRepository(t)
	currentRepo := NewTestRepository(t)
	staleRepo.CreateBranch(t, "stale-owner")
	currentRepo.CreateBranch(t, "current-owner")
	worktreePath := filepath.Join(t.TempDir(), "reused-worktree")
	staleRepo.CreateWorktree(t, worktreePath, "stale-owner")
	_, err := openSharedWorktrees(t, New(staleRepo.Path)).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	require.NoError(t, staleRepo.run(
		"worktree", "remove", "--force", worktreePath,
	))
	currentRepo.CreateWorktree(t, worktreePath, "current-owner")
	_, err = openSharedWorktrees(t, New(currentRepo.Path)).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)

	_, err = openSharedWorktrees(t, New(staleRepo.Path)).ReadIdentity(t.Context(), worktreePath, "kwt-generation")

	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrWorktreeRepositoryMismatch)
}

func TestWorktreeGenerationSupportsSeparateGitDirectory(t *testing.T) {
	base := t.TempDir()
	worktreePath := filepath.Join(base, "worktree")
	separateGitDir := filepath.Join(base, "repository.git")
	gitOutput(
		t,
		base,
		"init",
		"--separate-git-dir",
		separateGitDir,
		worktreePath,
	)
	g := New(worktreePath)

	generation, err := openSharedWorktrees(t, g).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})

	require.NoError(t, err)
	require.NoError(t, shared.ValidateWorktreeGeneration(generation))
	repeated, err := openSharedWorktrees(t, g).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	assert.Equal(t, generation, repeated)
}

func TestWorktreeGenerationRecoversFromRelativeAdministrativeGitDir(
	t *testing.T,
) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "relative-admin-gitdir")
	worktreePath := filepath.Join(t.TempDir(), "relative-admin-gitdir")
	repo.CreateWorktree(t, worktreePath, "relative-admin-gitdir")
	generation, err := openSharedWorktrees(t, g).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	adminDir, err := shared.ReadWorktreeBacklink(t.Context(), worktreePath)
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

	recovered, err := openSharedWorktrees(t, g).EnsureIdentity(t.Context(), worktreePath, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})

	require.NoError(t, err)
	assert.Equal(t, generation, recovered)
}

func TestListWorktreesWaitsForConcurrentWorktreeReplacement(t *testing.T) {
	repo := NewTestRepository(t)
	g := New(repo.Path)
	repo.CreateBranch(t, "original")
	worktreePath := filepath.Join(t.TempDir(), "replacement")
	repo.CreateWorktree(t, worktreePath, "original")
	before, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
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

	result := make(chan []shared.Entry, 1)
	listErr := make(chan error, 1)
	go func() {
		worktrees, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
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

func TestGetMainRepositoryPathResolvesSeparateGitDirectoryMain(t *testing.T) {
	mainPath, _ := newSeparateGitDirectoryRepository(t)

	got, err := New(mainPath).GetMainRepositoryPath()

	require.NoError(t, err)
	assert.Equal(t, utils.PathKey(mainPath), utils.PathKey(got))
}

func TestGetMainRepositoryPathResolvesBareContainerAnchor(t *testing.T) {
	container, mainPath, linkedPath := newBareContainerRepository(t)

	for _, worktreePath := range []string{filepath.Join(container, ".bare"), mainPath, linkedPath} {
		got, err := New(worktreePath).GetMainRepositoryPath()

		require.NoError(t, err)
		assert.Equal(t, utils.PathKey(mainPath), utils.PathKey(got))
	}
}

func TestBareRepositoryInventory(t *testing.T) {
	for _, layout := range []string{"bare-clone", "bare-dotgit"} {
		t.Run(layout, func(t *testing.T) {
			repo := NewTestRepository(t)
			repositoryPath := repo.Path
			if layout == "bare-clone" {
				repositoryPath = filepath.Join(t.TempDir(), "repo.git")
				gitOutput(t, repo.Path, "clone", "--bare", repo.Path, repositoryPath)
			} else {
				gitOutput(t, repo.Path, "config", "core.bare", "true")
			}
			linkedPath := filepath.Join(t.TempDir(), "topic")
			gitOutput(t, repositoryPath, "worktree", "add", "-b", "topic", linkedPath)

			for _, directory := range []string{repositoryPath, linkedPath} {
				g := New(directory)
				root, err := g.GetMainRepositoryPath()
				require.NoError(t, err)
				assert.Equal(t, utils.PathKey(repositoryPath), utils.PathKey(root))

				worktrees, err := openSharedWorktrees(t, g).List(
					t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true},
				)
				require.NoError(t, err)
				require.Len(t, worktrees, 1)
				assert.Equal(t, utils.PathKey(linkedPath), utils.PathKey(worktrees[0].Path))
				assert.Equal(t, "topic", worktrees[0].Branch)
				assert.False(t, worktrees[0].IsMain)
			}
		})
	}
}

func TestGetBareContainerPathRecognizesContainerWorktrees(t *testing.T) {
	container, mainPath, linkedPath := newBareContainerRepository(t)

	for _, worktreePath := range []string{mainPath, linkedPath} {
		got, err := New(worktreePath).GetBareContainerPath()

		require.NoError(t, err)
		assert.Equal(t, utils.PathKey(container), utils.PathKey(got))
	}
}

func TestGetBareContainerPathIgnoresRegularRepository(t *testing.T) {
	repo := NewTestRepository(t)

	got, err := New(repo.Path).GetBareContainerPath()

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetBareContainerPathIgnoresOtherBareRepositoryNames(t *testing.T) {
	_, mainPath, _ := newNamedBareContainerRepository(t, "repo.git")

	got, err := New(mainPath).GetBareContainerPath()

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetMainRepositoryPathRejectsUnresolvedSeparateGitDirectoryLinkedWorktree(
	t *testing.T,
) {
	_, linkedPath := newSeparateGitDirectoryRepository(t)

	_, err := New(linkedPath).GetMainRepositoryPath()

	require.ErrorContains(t, err, "main worktree path")
}

func TestGetMainRepositoryPathUsesCoreWorktreeForSeparateGitDirectoryLinkedWorktree(
	t *testing.T,
) {
	mainPath, linkedPath := newSeparateGitDirectoryRepository(t)
	_, err := New(mainPath).RunCommand("config", "core.worktree", mainPath)
	require.NoError(t, err)

	got, err := New(linkedPath).GetMainRepositoryPath()

	require.NoError(t, err)
	assert.Equal(t, utils.PathKey(mainPath), utils.PathKey(got))
}

func TestGetMainRepositoryPathRejectsUnrelatedCoreWorktree(t *testing.T) {
	mainPath, linkedPath := newSeparateGitDirectoryRepository(t)
	unrelatedPath := t.TempDir()
	_, err := New(mainPath).RunCommand(
		"config",
		"core.worktree",
		unrelatedPath,
	)
	require.NoError(t, err)

	_, err = New(linkedPath).GetMainRepositoryPath()

	require.ErrorContains(t, err, "configured core.worktree")
}

func TestInspectWorktreesNormalizesSeparateGitDirectoryMain(t *testing.T) {
	mainPath, linkedPath := newSeparateGitDirectoryRepository(t)

	inspections, err := inspectSharedWorktrees(t, New(mainPath))

	require.NoError(t, err)
	require.Len(t, inspections, 2)
	mainInspection := requireWorktreeInspection(t, inspections, mainPath)
	linkedInspection := requireWorktreeInspection(t, inspections, linkedPath)
	assert.True(t, mainInspection.IsMain)
	assert.False(t, linkedInspection.IsMain)
}

func TestInspectWorktreesExcludesBareContainerControlDirectory(t *testing.T) {
	container, mainPath, linkedPath := newBareContainerRepository(t)

	inspections, err := inspectSharedWorktrees(t, New(linkedPath))

	require.NoError(t, err)
	require.Len(t, inspections, 2)
	mainInspection := requireWorktreeInspection(t, inspections, mainPath)
	linkedInspection := requireWorktreeInspection(t, inspections, linkedPath)
	assert.True(t, mainInspection.IsMain)
	assert.False(t, linkedInspection.IsMain)
	for _, inspection := range inspections {
		assert.NotEqual(t, utils.PathKey(filepath.Join(container, ".bare")),
			utils.PathKey(inspection.Path))
	}
}

func TestListWorktrees_IsMainFromWorktree(t *testing.T) {
	repo := NewTestRepository(t)
	repo.CreateBranch(t, "test-is-main")
	wtPath := filepath.Join(t.TempDir(), "wt-is-main")
	repo.CreateWorktree(t, wtPath, "test-is-main")

	// Create Git instance from worktree path
	g := New(wtPath)
	worktrees, err := openSharedWorktrees(t, g).List(t.Context(), shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
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
func containsWorktreeWithPath(worktrees []shared.Entry, path string) bool {
	resolvedPath, _ := filepath.EvalSymlinks(path)
	for _, wt := range worktrees {
		resolvedWtPath, _ := filepath.EvalSymlinks(wt.Path)
		if resolvedWtPath == resolvedPath {
			return true
		}
	}
	return false
}
