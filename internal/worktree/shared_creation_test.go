package worktree

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/pkg/models"
)

func TestManagerCreationReturnsSharedAcquisition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setup diagnostic fixture uses POSIX shell commands")
	}
	t.Setenv("KWT_HOME", t.TempDir())
	root := t.TempDir()
	runner := gitcmd.New().WithConfig("user.name", "Example").WithConfig("user.email", "example@example.com")
	for _, args := range [][]string{{"init", "-b", "main"}, {"commit", "--allow-empty", "-m", "initial"}} {
		_, err := runner.Output(t.Context(), root, args...)
		require.NoError(t, err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-")
	require.NoError(t, err)
	original := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = original; require.NoError(t, stderr.Close()) })
	manager := New(git.New(root), &models.Config{Worktree: models.WorktreeConfig{BaseDir: t.TempDir(), AutoMkdir: true}, RepositorySettings: []models.RepositorySetting{{Repository: root, SetupCommands: []string{"echo setup-warning >&2; exit 1"}}}})
	result, err := manager.Create(t.Context(), CreateOptions{Branch: "topic", NewBranch: true, RequireGeneration: true})
	require.NoError(t, err)
	require.DirExists(t, result.Path)
	require.NotEmpty(t, result.IdentityValue)
	require.Equal(t, "topic", result.OwnedBranch)
	observed, err := openSharedWorktrees(t, git.New(root)).ReadIdentity(t.Context(), result.Path, "kwt-generation")
	require.NoError(t, err)
	require.Equal(t, result.IdentityValue, observed)
	diagnostics, err := os.ReadFile(stderr.Name())
	require.NoError(t, err)
	require.Contains(t, string(diagnostics), "setup-warning")
	_, err = result.Rollback(t.Context(), managed.RollbackFreshOwned)
	require.NoError(t, err)
	require.NoDirExists(t, result.Path)
	_, err = runner.Output(t.Context(), root, "show-ref", "--verify", "refs/heads/topic")
	require.Error(t, err)
}
