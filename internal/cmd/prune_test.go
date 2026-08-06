package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/pkg/models"
)

func TestPrunePublishesAfterSuccessfulNormalPrune(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestPruneDoesNotPublishWhenNormalPruneFails(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	initCommandTestConfig(t, t.TempDir())
	t.Chdir(t.TempDir())

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestPruneExpiredPublishesOnceAfterUnregisteringExpiredEntry(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	initCommandTestConfig(t, t.TempDir())
	pruneExpired = true
	expiredPath := filepath.Join(t.TempDir(), "missing-expired")
	registerExpiredWorktree(t, expiredPath)

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	reg, err := registry.New()
	require.NoError(t, err)
	_, exists := reg.Get(expiredPath)
	assert.False(t, exists)
}

func TestPruneExpiredDoesNotPublishOnNoop(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	initCommandTestConfig(t, t.TempDir())
	pruneExpired = true

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Zero(t, calls)
}

func TestPruneExpiredDoesNotPublishOnDryRun(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	initCommandTestConfig(t, t.TempDir())
	pruneExpired = true
	pruneDryRun = true
	expiredPath := filepath.Join(t.TempDir(), "missing-expired-dry-run")
	registerExpiredWorktree(t, expiredPath)

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Zero(t, calls)
	reg, err := registry.New()
	require.NoError(t, err)
	_, exists := reg.Get(expiredPath)
	assert.True(t, exists)
}

func TestPruneExpiredPreservesLiveWorktreeWithoutGeneration(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "legacy-expired")
	runTUITestGit(t, repoPath, "branch", "feature/legacy-expired")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		worktreePath,
		"feature/legacy-expired",
	)
	registerExpiredWorktree(t, worktreePath)
	pruneExpired = true
	pruneForce = true

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.DirExists(t, worktreePath)
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reg.Get(worktreePath)
	assert.True(t, exists)
}

func TestPruneExpiredPreservesReplacementGeneration(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expired-replacement")
	runTUITestGit(t, repoPath, "branch", "feature/expired-original")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		worktreePath,
		"feature/expired-original",
	)
	originalGeneration, generationErr := git.New(repoPath).WorktreeGeneration(
		worktreePath,
	)
	require.NoError(t, generationErr)
	expiredAt := time.Now().Add(-time.Hour)
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "repo",
		Branch:     "feature/expired-original",
		Path:       worktreePath,
		ExpiresAt:  &expiredAt,
		Generation: originalGeneration,
	}))

	runTUITestGit(t, repoPath, "worktree", "remove", "--force", worktreePath)
	runTUITestGit(t, repoPath, "branch", "feature/expired-replacement")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		worktreePath,
		"feature/expired-replacement",
	)
	pruneExpired = true
	pruneForce = true

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.DirExists(t, worktreePath)
	reloaded, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reloaded.Get(worktreePath)
	assert.True(t, exists)
}

func TestPruneExpiredRemovesMatchingGeneration(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expired-matching")
	runTUITestGit(t, repoPath, "branch", "feature/expired-matching")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		worktreePath,
		"feature/expired-matching",
	)
	generation, generationErr := git.New(repoPath).WorktreeGeneration(
		worktreePath,
	)
	require.NoError(t, generationErr)
	expiredAt := time.Now().Add(-time.Hour)
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "repo",
		Branch:     "feature/expired-matching",
		Path:       worktreePath,
		ExpiresAt:  &expiredAt,
		Generation: generation,
	}))
	pruneExpired = true
	pruneForce = true

	cmd, _, _ := fleetTestCommand()
	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.NoDirExists(t, worktreePath)
	reloaded, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reloaded.Get(worktreePath)
	assert.False(t, exists)
}

func TestPruneExpiredCompletesBookkeepingAfterGitDeregistersWithResidualFiles(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell wrapper")
	}
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expired-residual")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"task7/expired-residual",
		worktreePath,
	)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	expiredAt := time.Now().Add(-time.Hour)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "repo",
		Branch:     "task7/expired-residual",
		Path:       worktreePath,
		ExpiresAt:  &expiredAt,
		Generation: generation,
	}))
	pruneExpired = true

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
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
	require.NoError(t, os.WriteFile(wrapperPath, []byte(wrapper), 0755))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	publishCalls := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		publishCalls++
	}
	cmd, _, _ := fleetTestCommand()
	output := captureStdout(t, func() {
		err = runPrune(cmd, nil)
	})

	require.NoError(t, err)
	assert.Contains(t, output, "worktree removed, but files remain at ")
	assert.Contains(t, output, "Removed 1 expired worktree(s), skipped 0")
	assert.NotContains(t, output, "Failed to remove worktree")
	assert.Equal(t, 1, publishCalls)
	assert.FileExists(t, filepath.Join(worktreePath, "residual"))
	reloaded, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reloaded.Get(worktreePath)
	assert.False(t, exists)
}

func resetPruneCommandFlags(t *testing.T) {
	t.Helper()

	oldPruneExpired := pruneExpired
	oldPruneDryRun := pruneDryRun
	oldPruneForce := pruneForce

	t.Cleanup(func() {
		pruneExpired = oldPruneExpired
		pruneDryRun = oldPruneDryRun
		pruneForce = oldPruneForce
	})

	pruneExpired = false
	pruneDryRun = false
	pruneForce = false
}

func registerExpiredWorktree(t *testing.T, path string) {
	t.Helper()

	reg, err := registry.New()
	require.NoError(t, err)
	expiredAt := time.Now().Add(-time.Hour)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "repo",
		Branch:     "task7/expired",
		Path:       path,
		ExpiresAt:  &expiredAt,
	}))
}
