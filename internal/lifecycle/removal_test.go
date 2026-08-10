package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/service"
)

func TestRemovalServiceRemovesWorktreeAndRegistryRecord(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "remove-me")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	reg, err := registry.NewAt(home)
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "example/widget", Branch: "remove-me", Path: worktreePath,
		Generation: generation,
	}))

	result, err := NewRemovalService(RemovalServiceOptions{Home: home}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath: repositoryPath,
			Path:           worktreePath, ExpectedGeneration: generation,
			DeleteBranch: true,
		},
	)

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
	assert.True(t, result.BranchDeleted)
	assert.True(t, result.RegistryUnregistered)
	assert.Equal(t, "remove-me", result.Branch)
	assert.NoDirExists(t, worktreePath)
	reloaded, err := registry.NewAt(home)
	require.NoError(t, err)
	_, registered := reloaded.Get(worktreePath)
	assert.False(t, registered)
}

func TestRemovalServiceRejectsChangedGeneration(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "keep-me")

	result, err := NewRemovalService(RemovalServiceOptions{Home: t.TempDir()}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath:     repositoryPath,
			Path:               worktreePath,
			ExpectedGeneration: "0123456789abcdef0123456789abcdef",
		},
	)

	require.Error(t, err)
	assert.False(t, result.WorktreeRemoved)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceRejectsActiveCreation(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "creating")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	reg, err := registry.NewAt(home)
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "example/widget", Branch: "creating", Path: worktreePath,
		Generation: generation, CreationToken: "creating",
	}))
	release, acquired, err := reg.AcquireCreation(worktreePath)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() { require.NoError(t, release()) })

	result, err := NewRemovalService(RemovalServiceOptions{Home: home}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath: repositoryPath,
			Path:           worktreePath, ExpectedGeneration: generation,
		},
	)

	require.Error(t, err)
	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.Conflict, typed.Code)
	assert.True(t, typed.Retryable)
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
	reloaded, reloadErr := registry.NewAt(home)
	require.NoError(t, reloadErr)
	_, registered := reloaded.Get(worktreePath)
	assert.True(t, registered)
}

func TestRemovalServiceIgnoresDaemonRepositoryRoutingEnvironment(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "routed-remove")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	otherRepository, _ := removalRepository(t, "other-worktree")
	t.Setenv("GIT_DIR", filepath.Join(otherRepository, ".git"))
	t.Setenv("GIT_WORK_TREE", otherRepository)

	result, err := NewRemovalService(RemovalServiceOptions{Home: t.TempDir()}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath:     repositoryPath,
			Path:               worktreePath,
			ExpectedGeneration: generation,
		},
	)

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
	assert.NoDirExists(t, worktreePath)
}

func TestClassifyRemovalErrorHidesUnexpectedCredentialBearingCause(t *testing.T) {
	const secret = "removal-password"
	cause := errors.New("fetch ssh://user:" + secret + "@example.invalid/repository")

	err := classifyRemovalError(cause, RemovalResult{
		Path: "/worktrees/topic", WorktreeRemoved: true,
	})

	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.Internal, typed.Code)
	assert.Equal(t, "internal failure", typed.Message)
	assert.NotContains(t, typed.Message, secret)
	assert.Equal(t, "/worktrees/topic", typed.Details["path"])
	assert.Equal(t, true, typed.Details["worktree_removed"])
	assert.ErrorIs(t, err, cause)
}

func removalRepository(t *testing.T, branch string) (string, string) {
	t.Helper()
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	require.NoError(t, os.MkdirAll(repositoryPath, 0o755))
	runRemovalGit(t, repositoryPath, "init", "-b", "main")
	runRemovalGit(t, repositoryPath, "config", "user.email", "test@example.com")
	runRemovalGit(t, repositoryPath, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("test\n"), 0o644))
	runRemovalGit(t, repositoryPath, "add", "README.md")
	runRemovalGit(t, repositoryPath, "commit", "-m", "initial")
	runRemovalGit(t, repositoryPath, "branch", branch)
	worktreePath := filepath.Join(t.TempDir(), branch)
	runRemovalGit(t, repositoryPath, "worktree", "add", worktreePath, branch)
	return repositoryPath, worktreePath
}

func runRemovalGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
