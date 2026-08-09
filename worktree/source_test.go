package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func testExpansion(t *testing.T) ExpansionContext {
	t.Helper()
	expansion, err := CaptureExpansionContext()
	require.NoError(t, err)
	return expansion
}

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
		View: ViewProjects, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
	})

	require.NoError(t, err)
	require.Len(t, result.Snapshot.Projects, 1)
	assert.Equal(t, "github.com/acme/repo", result.Snapshot.Projects[0].Repository)
}

func TestSourceRejectsRelativeRepositoryDirectory(t *testing.T) {
	_, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewRepository, WorkingDirectory: "relative", Expansion: testExpansion(t),
		UntrustedConfig: IgnoreUntrustedConfig,
	})
	assert.ErrorContains(t, err, "must be absolute")
}

func TestSourceRejectsMissingPathExpansionContext(t *testing.T) {
	_, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewProjects, UntrustedConfig: IgnoreUntrustedConfig,
	})

	assert.ErrorContains(t, err, "path-expansion working directory must be absolute")
}

func TestSourcePropagatesRepositoryInventoryErrors(t *testing.T) {
	workingDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workingDirectory, ".git"),
		[]byte("gitdir: /missing/repository\n"),
		0o600,
	))

	_, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewRepository, WorkingDirectory: workingDirectory, UntrustedConfig: IgnoreUntrustedConfig,
		Expansion: testExpansion(t),
	})

	require.Error(t, err)
}

func TestSourceReadsProvenanceOnlyWhenRequested(t *testing.T) {
	home := t.TempDir()
	baseDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[worktree]\nbasedir = '"+baseDirectory+"'\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "pull-requests.json"), []byte("{"), 0o600))
	source := NewSource(SourceOptions{Home: home})

	_, err := source.Load(context.Background(), Request{
		View: ViewDashboard, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
	})
	require.NoError(t, err)

	_, err = source.Load(context.Background(), Request{
		View: ViewDashboard, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
		IncludeProtectedSockets: true,
	})
	require.ErrorContains(t, err, "failed to read pull-request provenance")
}

func TestSourceSeparatesLaunchInventoryFromDashboardEntries(t *testing.T) {
	launchDirectory := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main", launchDirectory},
		{"-C", launchDirectory, "remote", "add", "origin", "https://github.com/acme/launch.git"},
		{"-C", launchDirectory, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"},
	} {
		command := exec.Command("git", args...)
		require.NoError(t, command.Run())
	}
	source := &currentSource{}

	entries, launchEntries, err := source.loadDashboard(context.Background(), Request{
		LaunchDirectory: launchDirectory,
	}, &models.Config{Worktree: models.WorktreeConfig{BaseDir: t.TempDir()}})

	require.NoError(t, err)
	require.NotEmpty(t, entries)
	require.NotEmpty(t, launchEntries)
	canonicalLaunchDirectory, err := filepath.EvalSymlinks(launchDirectory)
	require.NoError(t, err)
	for _, entry := range launchEntries {
		assert.Equal(t, canonicalLaunchDirectory, entry.Path)
		assert.Equal(t, "github.com/acme/launch", entry.Repository.FullPath)
	}
}
