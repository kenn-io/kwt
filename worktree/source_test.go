package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceProjectsFiltersInaccessibleRegistrations(t *testing.T) {
	home := t.TempDir()
	repository := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Mkdir(repository, 0o755))
	command := exec.Command("git", "init", repository)
	require.NoError(t, command.Run())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = 'github.com/acme/repo'\nname = 'repo'\npath = '"+repository+"'\n\n"+
			"[[projects]]\nrepository = 'github.com/acme/missing'\nname = 'missing'\npath = '"+filepath.Join(t.TempDir(), "missing")+"'\n",
	), 0o600))

	result, err := NewSource(SourceOptions{Home: home}).Load(context.Background(), Request{
		View: ViewProjects, UntrustedConfig: IgnoreUntrustedConfig,
	})

	require.NoError(t, err)
	require.Len(t, result.Snapshot.Projects, 1)
	assert.Equal(t, "github.com/acme/repo", result.Snapshot.Projects[0].Repository)
}

func TestSourceRejectsRelativeRepositoryDirectory(t *testing.T) {
	_, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewRepository, WorkingDirectory: "relative", UntrustedConfig: IgnoreUntrustedConfig,
	})
	assert.ErrorContains(t, err, "must be absolute")
}
