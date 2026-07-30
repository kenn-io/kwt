package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/registry"
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
	removeIfGeneration = tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	runTUITestGit(t, repoPath, "worktree", "remove", worktreePath)
	runTUITestGit(t, repoPath, "branch", "-D", "task7/remove-replacement")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"task7/remove-replacement",
		worktreePath,
	)

	cmd, _, _ := fleetTestCommand()
	err := runRemove(cmd, []string{worktreePath})

	require.ErrorContains(t, err, "generation changed")
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
	removeIfGeneration = tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:   worktreePath,
		Branch: "task7/remove-local-branch-delete-fails",
	}))

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run when the worktree was removed")
	}

	cmd, _, _ := fleetTestCommand()
	err = runRemove(cmd, []string{"task7/remove-local-branch-delete-fails"})

	require.ErrorContains(t, err, "worktree removed but failed to delete branch")
	assert.Equal(t, 1, calls)
	assert.NoDirExists(t, worktreePath)
	refreshedRegistry, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, registered := refreshedRegistry.Get(worktreePath)
	assert.False(t, registered)
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
	removeIfGeneration = tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:   worktreePath,
		Branch: "task7/remove-global-branch-delete-fails",
	}))

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run when the global worktree was removed")
	}

	cmd, _, _ := fleetTestCommand()
	err = runRemove(cmd, []string{"task7/remove-global-branch-delete-fails"})

	require.ErrorContains(t, err, "worktree removed but failed to delete branch")
	assert.Equal(t, 1, calls)
	assert.NoDirExists(t, worktreePath)
	refreshedRegistry, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, registered := refreshedRegistry.Get(worktreePath)
	assert.False(t, registered)
}

func resetRemoveCommandFlags(t *testing.T) {
	t.Helper()

	oldRemoveForce := removeForce
	oldRemoveDryRun := removeDryRun
	oldRemoveGlobal := removeGlobal
	oldRemoveIfGeneration := removeIfGeneration
	oldDeleteBranch := deleteBranch
	oldForceDeleteBranch := forceDeleteBranch

	t.Cleanup(func() {
		removeForce = oldRemoveForce
		removeDryRun = oldRemoveDryRun
		removeGlobal = oldRemoveGlobal
		removeIfGeneration = oldRemoveIfGeneration
		deleteBranch = oldDeleteBranch
		forceDeleteBranch = oldForceDeleteBranch
	})

	removeForce = false
	removeDryRun = false
	removeGlobal = false
	removeIfGeneration = ""
	deleteBranch = false
	forceDeleteBranch = false
}
