package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func TestRemoveLocalPublishesOnceAfterSuccessfulRemoval(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	worktreePath := filepath.Join(t.TempDir(), "remove-local")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-local", worktreePath)

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run after removal")
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-local"})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestRemoveLocalRejectsRecreatedWorktree(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	worktreePath := filepath.Join(t.TempDir(), "remove-replacement")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"task7/remove-replacement",
		worktreePath,
	)
	info, err := os.Stat(worktreePath)
	require.NoError(t, err)
	removeIfCreatedAt = info.ModTime().Format(time.RFC3339Nano)
	replacementTime := info.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(
		worktreePath,
		replacementTime,
		replacementTime,
	))

	cmd, _, _ := fleetTestCommand()
	err = runRemove(cmd, []string{worktreePath})

	require.ErrorContains(t, err, "creation identity changed")
	assert.DirExists(t, worktreePath)
}

func TestRemoveLocalDoesNotPublishOnDryRun(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	removeDryRun = true
	worktreePath := filepath.Join(t.TempDir(), "remove-local-dry-run")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-local-dry-run", worktreePath)

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-local-dry-run"})

	require.NoError(t, err)
	assert.Zero(t, calls)
	assert.DirExists(t, worktreePath)
}

func TestRemoveLocalDoesNotPublishWhenEveryRemovalFails(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	worktreePath := filepath.Join(t.TempDir(), "remove-local-dirty")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-local-dirty", worktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty"), 0644))

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-local-dirty"})

	require.NoError(t, err)
	assert.Zero(t, calls)
	assert.DirExists(t, worktreePath)
}

func TestRemoveLocalPublishesWhenWorktreeRemovedButBranchDeleteFails(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	deleteBranch = true
	worktreePath := filepath.Join(t.TempDir(), "remove-local-branch-delete-fails")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-local-branch-delete-fails", worktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "change.txt"), []byte("change"), 0644))
	runTUITestGit(t, worktreePath, "add", ".")
	runTUITestGit(t, worktreePath, "commit", "-m", "unmerged worktree commit")

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run when the worktree was removed")
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-local-branch-delete-fails"})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.NoDirExists(t, worktreePath)
}

func TestRemoveGlobalPublishesOnceAfterSuccessfulRemoval(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	baseDir := t.TempDir()
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, baseDir)
	t.Chdir(t.TempDir())

	removeGlobal = true
	worktreePath := filepath.Join(baseDir, "remove-global")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-global", worktreePath)

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run after global removal")
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-global"})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestRemoveGlobalDoesNotPublishOnDryRun(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	baseDir := t.TempDir()
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, baseDir)
	t.Chdir(t.TempDir())

	removeGlobal = true
	removeDryRun = true
	worktreePath := filepath.Join(baseDir, "remove-global-dry-run")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-global-dry-run", worktreePath)

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-global-dry-run"})

	require.NoError(t, err)
	assert.Zero(t, calls)
	assert.DirExists(t, worktreePath)
}

func TestRemoveGlobalDoesNotPublishWhenEveryRemovalFails(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	baseDir := t.TempDir()
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, baseDir)
	t.Chdir(t.TempDir())

	removeGlobal = true
	worktreePath := filepath.Join(baseDir, "remove-global-dirty")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-global-dirty", worktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty"), 0644))

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-global-dirty"})

	require.NoError(t, err)
	assert.Zero(t, calls)
	assert.DirExists(t, worktreePath)
}

func TestRemoveGlobalPublishesWhenWorktreeRemovedButBranchDeleteFails(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	baseDir := t.TempDir()
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, baseDir)
	t.Chdir(t.TempDir())

	removeGlobal = true
	deleteBranch = true
	worktreePath := filepath.Join(baseDir, "remove-global-branch-delete-fails")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "task7/remove-global-branch-delete-fails", worktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "change.txt"), []byte("change"), 0644))
	runTUITestGit(t, worktreePath, "add", ".")
	runTUITestGit(t, worktreePath, "commit", "-m", "unmerged worktree commit")

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run when the global worktree was removed")
	}

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{"task7/remove-global-branch-delete-fails"})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.NoDirExists(t, worktreePath)
}

func resetRemoveCommandFlags(t *testing.T) {
	t.Helper()

	oldRemoveForce := removeForce
	oldRemoveDryRun := removeDryRun
	oldRemoveGlobal := removeGlobal
	oldRemoveIfCreatedAt := removeIfCreatedAt
	oldDeleteBranch := deleteBranch
	oldForceDeleteBranch := forceDeleteBranch

	t.Cleanup(func() {
		removeForce = oldRemoveForce
		removeDryRun = oldRemoveDryRun
		removeGlobal = oldRemoveGlobal
		removeIfCreatedAt = oldRemoveIfCreatedAt
		deleteBranch = oldDeleteBranch
		forceDeleteBranch = oldForceDeleteBranch
	})

	removeForce = false
	removeDryRun = false
	removeGlobal = false
	removeIfCreatedAt = ""
	deleteBranch = false
	forceDeleteBranch = false
}
