package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/discovery"
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

func TestRemoveLocalUnregistersLegacyEntry(t *testing.T) {
	resetFleetCommandDeps(t)
	resetRemoveCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)
	worktreePath := filepath.Join(t.TempDir(), "remove-local-legacy")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"task7/remove-local-legacy",
		worktreePath,
	)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:                   worktreePath,
		Branch:                 "task7/remove-local-legacy",
		UnreviewedRemoteSource: true,
	}))

	cmd, _, _ := fleetTestCommand()
	err = runRemove(cmd, []string{"task7/remove-local-legacy"})

	require.NoError(t, err)
	refreshedRegistry, err := registry.New()
	require.NoError(t, err)
	_, registered := refreshedRegistry.Get(worktreePath)
	assert.False(t, registered)
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
	setRemoveGenerationFlag(t, cmd, removeIfGeneration)
	err := runRemove(cmd, []string{worktreePath})

	require.ErrorContains(t, err, "generation changed")
	assert.DirExists(t, worktreePath)
}

func TestRemoveRejectsExplicitInvalidGeneration(t *testing.T) {
	tests := []struct {
		name       string
		generation string
	}{
		{name: "empty", generation: ""},
		{name: "malformed", generation: "not-a-generation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetFleetCommandDeps(t)
			resetRemoveCommandFlags(t)

			repoPath := newTUITestRepo(t)
			initCommandTestConfig(t, t.TempDir())
			t.Chdir(repoPath)
			worktreePath := filepath.Join(t.TempDir(), "remove-invalid")
			runTUITestGit(
				t,
				repoPath,
				"worktree",
				"add",
				"-b",
				"task7/remove-invalid",
				worktreePath,
			)

			cmd, _, _ := fleetTestCommand()
			setRemoveGenerationFlag(t, cmd, tt.generation)

			err := runRemove(cmd, []string{worktreePath})

			require.ErrorContains(
				t,
				err,
				"--if-generation must be a 32-character hexadecimal value",
			)
			assert.DirExists(t, worktreePath)
		})
	}
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
		Path:       worktreePath,
		Branch:     "task7/remove-local-branch-delete-fails",
		Generation: removeIfGeneration,
	}))

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run when the worktree was removed")
	}

	cmd, _, _ := fleetTestCommand()
	setRemoveGenerationFlag(t, cmd, removeIfGeneration)
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

func TestMatchGlobalRemovalEntriesPrefersExactPath(t *testing.T) {
	exactPath := filepath.Join(t.TempDir(), "foo")
	exact := &discovery.GlobalWorktreeEntry{
		Path:   exactPath,
		Branch: "task/foo",
	}
	prefix := &discovery.GlobalWorktreeEntry{
		Path:   exactPath + "-old",
		Branch: "task/foo-old",
	}

	matches := matchGlobalRemovalEntries(
		[]*discovery.GlobalWorktreeEntry{exact, prefix},
		exactPath,
	)

	require.Len(t, matches, 1)
	assert.Same(t, exact, matches[0])
}

func TestMatchGlobalRemovalEntriesNormalizesWindowsSeparators(t *testing.T) {
	exact := &discovery.GlobalWorktreeEntry{
		Path:   `C:/work/foo`,
		Branch: "task/foo",
	}
	prefix := &discovery.GlobalWorktreeEntry{
		Path:   `C:/work/foo-old`,
		Branch: "task/foo-old",
	}

	matches := matchGlobalRemovalEntries(
		[]*discovery.GlobalWorktreeEntry{exact, prefix},
		`C:\work\foo`,
	)

	require.Len(t, matches, 1)
	assert.Same(t, exact, matches[0])
}

func TestMatchGlobalRemovalEntriesPreservesUnixLiteralBackslash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a path separator on Windows")
	}
	baseDir := t.TempDir()
	literal := &discovery.GlobalWorktreeEntry{
		Path:   filepath.Join(baseDir, `foo\bar`),
		Branch: "literal-backslash",
	}
	slashed := &discovery.GlobalWorktreeEntry{
		Path:   filepath.Join(baseDir, "foo", "bar"),
		Branch: "slash-separated",
	}

	matches := matchGlobalRemovalEntries(
		[]*discovery.GlobalWorktreeEntry{literal, slashed},
		literal.Path,
	)

	require.Len(t, matches, 1)
	assert.Same(t, literal, matches[0])
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
		Path:       worktreePath,
		Branch:     "task7/remove-global-branch-delete-fails",
		Generation: removeIfGeneration,
	}))

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
		assert.NoDirExists(t, worktreePath, "publish should run when the global worktree was removed")
	}

	cmd, _, _ := fleetTestCommand()
	setRemoveGenerationFlag(t, cmd, removeIfGeneration)
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

func setRemoveGenerationFlag(
	t *testing.T,
	cmd *cobra.Command,
	generation string,
) {
	t.Helper()
	cmd.Flags().StringVar(&removeIfGeneration, "if-generation", "", "")
	require.NoError(t, cmd.Flags().Set("if-generation", generation))
}
