package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

func changeDir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func resetWorkspaceCommandDeps(t *testing.T) {
	t.Helper()
	origRegister := registerWorkspace
	origUnregister := unregisterWorkspace
	origLoad := loadWorkspaceConfig
	origSessions := listWorkspaceSessions
	t.Cleanup(func() {
		registerWorkspace = origRegister
		unregisterWorkspace = origUnregister
		loadWorkspaceConfig = origLoad
		listWorkspaceSessions = origSessions
	})
}

func TestWorkspaceAddRegistersCwdByDefault(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	dir := t.TempDir()
	changeDir(t, dir)
	var got models.Workspace
	registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		got = workspace
		workspace.Name = "resolved"
		return workspace, nil
	}

	cmd, stdout, _ := fleetTestCommand()
	err := runWorkspaceAdd(cmd, nil)

	require.NoError(t, err)
	// Normalize paths to handle macOS symlink differences (/var vs /private/var)
	expectedPath, _ := filepath.EvalSymlinks(dir)
	gotPath, _ := filepath.EvalSymlinks(got.Path)
	assert.Equal(t, expectedPath, gotPath)
	assert.Empty(t, got.Name)
	assert.Contains(t, stdout.String(), "resolved")
}

func TestWorkspaceAddUsesArgsAndNameFlag(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	var got models.Workspace
	registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		got = workspace
		return workspace, nil
	}

	cmd, _, _ := fleetTestCommand()
	workspaceAddName = "scratch"
	t.Cleanup(func() { workspaceAddName = "" })
	err := runWorkspaceAdd(cmd, []string{"/tmp/somewhere"})

	require.NoError(t, err)
	assert.Equal(t, "/tmp/somewhere", got.Path)
	assert.Equal(t, "scratch", got.Name)
}

func TestWorkspaceListShowsLiveState(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	loadWorkspaceConfig = func() (*models.Config, error) {
		return &models.Config{Workspaces: []models.Workspace{
			{Name: "notes", Path: "/Users/me/notes"},
			{Name: "scratch", Path: "/Users/me/scratch"},
		}}, nil
	}
	listWorkspaceSessions = func() ([]string, error) {
		return []string{tmuxDirSessionNameForTest("notes", "/Users/me/notes")}, nil
	}

	cmd, stdout, _ := fleetTestCommand()
	err := runWorkspaceList(cmd, nil)

	require.NoError(t, err)
	out := stdout.String()
	assert.Contains(t, out, "notes")
	assert.Contains(t, out, "live")
	assert.Contains(t, out, "scratch")
	assert.Contains(t, out, "stopped")
}

func TestWorkspaceListJSONReportsCanonicalAndEffectiveSessions(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	workspaceJSON = true
	t.Cleanup(func() { workspaceJSON = false })
	workspaces := []models.Workspace{
		{Name: "renamed", Path: "/Users/me/notes"},
		{Name: "scratch", Path: "/Users/me/scratch"},
	}
	loadWorkspaceConfig = func() (*models.Config, error) {
		return &models.Config{Workspaces: workspaces}, nil
	}
	oldLiveName := tmux.DirWorkspaceSessionName("old-name", workspaces[0].Path)
	listWorkspaceSessions = func() ([]string, error) {
		return []string{oldLiveName}, nil
	}

	cmd, stdout, _ := fleetTestCommand()
	require.NoError(t, runWorkspaceList(cmd, nil))

	var got []directoryWorkspaceRecord
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, directoryWorkspaceRecord{
		Name:        "renamed",
		Path:        "/Users/me/notes",
		SessionName: oldLiveName,
		SessionLive: true,
	}, got[0])
	assert.Equal(t, directoryWorkspaceRecord{
		Name: "scratch",
		Path: "/Users/me/scratch",
		SessionName: tmux.DirWorkspaceSessionName(
			"scratch",
			"/Users/me/scratch",
		),
		SessionLive: false,
	}, got[1])
}

func TestWorkspaceListJSONEmptyRegistryEmitsArray(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	workspaceJSON = true
	t.Cleanup(func() { workspaceJSON = false })
	loadWorkspaceConfig = func() (*models.Config, error) {
		return &models.Config{}, nil
	}

	cmd, stdout, _ := fleetTestCommand()
	require.NoError(t, runWorkspaceList(cmd, nil))

	assert.Equal(t, "[]\n", stdout.String())
	assert.NotContains(t, stdout.String(), "no workspaces registered")
}

func TestFindRegisteredDirectoryWorkspaceUsesCanonicalPath(t *testing.T) {
	workspaces := []models.Workspace{
		{Name: "notes", Path: "/Users/me/workspaces/notes"},
	}

	got, ok := findRegisteredDirectoryWorkspace(
		workspaces,
		"/Users/me/workspaces/./notes/",
	)
	require.True(t, ok)
	assert.Equal(t, workspaces[0], got)

	_, ok = findRegisteredDirectoryWorkspace(
		workspaces,
		"/Users/me/workspaces/scratch",
	)
	assert.False(t, ok)
}

func TestWorkspaceRemoveReportsLiveSession(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	loadWorkspaceConfig = func() (*models.Config, error) {
		return &models.Config{Workspaces: []models.Workspace{{Name: "notes", Path: "/Users/me/notes"}}}, nil
	}
	listWorkspaceSessions = func() ([]string, error) {
		return []string{tmuxDirSessionNameForTest("notes", "/Users/me/notes")}, nil
	}
	unregisterWorkspace = func(name string) error {
		assert.Equal(t, "notes", name)
		return nil
	}

	cmd, stdout, _ := fleetTestCommand()
	err := runWorkspaceRemove(cmd, []string{"notes"})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "still running")
}

func TestWorkspaceRemovePropagatesUnknownNameError(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	loadWorkspaceConfig = func() (*models.Config, error) { return &models.Config{}, nil }
	listWorkspaceSessions = func() ([]string, error) { return nil, nil }
	unregisterWorkspace = func(name string) error {
		return errors.New(`no workspace named "nope"; no workspaces registered`)
	}

	cmd, _, _ := fleetTestCommand()
	err := runWorkspaceRemove(cmd, []string{"nope"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no workspace named")
}

func TestWorkspaceRemoveReportsLiveSessionCaseInsensitive(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	loadWorkspaceConfig = func() (*models.Config, error) {
		return &models.Config{Workspaces: []models.Workspace{{Name: "Notes", Path: "/Users/me/notes"}}}, nil
	}
	listWorkspaceSessions = func() ([]string, error) {
		return []string{tmuxDirSessionNameForTest("Notes", "/Users/me/notes")}, nil
	}
	unregisterWorkspace = func(name string) error {
		assert.Equal(t, "notes", name)
		return nil
	}

	cmd, stdout, _ := fleetTestCommand()
	err := runWorkspaceRemove(cmd, []string{"notes"})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "still running")
}

func tmuxDirSessionNameForTest(name, path string) string {
	return tmux.DirWorkspaceSessionName(name, path)
}

// TestWorkspaceCmdIsolatesFromCwdConfig guards the config-isolation invariant:
// workspaceCmd overrides the root PersistentPreRunE (which merges the
// caller's cwd .kwt.toml) with a no-op, since workspace commands manage
// machine-level global state. If the override is removed, workspace
// subcommands fall back to root's cwd merge -- this test fails because the
// field goes nil.
func TestWorkspaceCmdIsolatesFromCwdConfig(t *testing.T) {
	require.NotNil(t, workspaceCmd.PersistentPreRunE,
		"workspace must define its own PersistentPreRunE to bypass root's cwd merge")
	require.NoError(t, workspaceCmd.PersistentPreRunE(workspaceCmd, nil),
		"workspace's PersistentPreRunE must be a no-op that never errors")
}

func TestWorkspaceListPropagatesSessionError(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	loadWorkspaceConfig = func() (*models.Config, error) {
		return &models.Config{Workspaces: []models.Workspace{{Name: "notes", Path: "/Users/me/notes"}}}, nil
	}
	listWorkspaceSessions = func() ([]string, error) {
		return nil, errors.New("tmux: no such file or directory")
	}

	cmd, _, _ := fleetTestCommand()
	err := runWorkspaceList(cmd, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to list tmux sessions")
}

func TestWorkspaceRemoveWarnsOnSessionCheckError(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	loadWorkspaceConfig = func() (*models.Config, error) {
		return &models.Config{Workspaces: []models.Workspace{{Name: "notes", Path: "/Users/me/notes"}}}, nil
	}
	listWorkspaceSessions = func() ([]string, error) {
		return nil, errors.New("tmux: no such file or directory")
	}
	unregisterWorkspace = func(name string) error {
		assert.Equal(t, "notes", name)
		return nil
	}

	cmd, stdout, stderr := fleetTestCommand()
	err := runWorkspaceRemove(cmd, []string{"notes"})

	require.NoError(t, err, "the session check is advisory; it must not fail the remove")
	assert.Contains(t, stdout.String(), "unregistered workspace notes")
	assert.Contains(t, stderr.String(), "warning: could not check for a live session")
}
