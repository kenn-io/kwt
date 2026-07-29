package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func workspaceTestEnv(t *testing.T) string {
	t.Helper()
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })
	configHome := t.TempDir()
	t.Setenv("KWT_HOME", configHome)
	return configHome
}

func registeredWorkspaces(t *testing.T) []models.Workspace {
	t.Helper()
	globalViper := viper.New()
	globalViper.SetConfigFile(filepath.Join(getConfigDir(), configName+"."+configType))
	globalViper.SetConfigType(configType)
	require.NoError(t, globalViper.ReadInConfig())
	var workspaces []models.Workspace
	require.NoError(t, globalViper.UnmarshalKey("workspaces", &workspaces))
	return workspaces
}

func TestRegisterWorkspaceDefaultsNameAndPersists(t *testing.T) {
	workspaceTestEnv(t)
	dir := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	stored, err := RegisterWorkspace(models.Workspace{Path: dir})

	require.NoError(t, err)
	assert.Equal(t, "notes", stored.Name)
	workspaces := registeredWorkspaces(t)
	require.Len(t, workspaces, 1)
	assert.Equal(t, "notes", workspaces[0].Name)
}

func TestRegisterWorkspaceRejectsMissingPath(t *testing.T) {
	workspaceTestEnv(t)

	_, err := RegisterWorkspace(models.Workspace{Path: filepath.Join(t.TempDir(), "absent")})

	require.Error(t, err)
	assert.True(t, os.IsNotExist(errors.Unwrap(err)),
		"a missing path must wrap the underlying stat error, not report it as a non-directory")
}

func TestRegisterWorkspaceRejectsFile(t *testing.T) {
	workspaceTestEnv(t)
	file := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	_, err := RegisterWorkspace(models.Workspace{Path: file})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestRegisterWorkspaceRejectsTmuxFormatPath(t *testing.T) {
	workspaceTestEnv(t)
	dir := filepath.Join(t.TempDir(), "notes#archive")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	_, err := RegisterWorkspace(models.Workspace{Path: dir})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmux format syntax")
}

func TestRegisterWorkspaceWrapsStatErrorWhenParentIsFile(t *testing.T) {
	workspaceTestEnv(t)
	file := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	_, err := RegisterWorkspace(models.Workspace{Path: filepath.Join(file, "child")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace path")
	assert.NotContains(t, err.Error(), "is not a directory",
		"a stat failure must report the underlying error, not the not-a-directory message")
	assert.Error(t, errors.Unwrap(err), "the stat error must be wrapped for errors.Is/As")
}

func TestRegisterWorkspaceUpdatesNameForSamePath(t *testing.T) {
	workspaceTestEnv(t)
	dir := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := RegisterWorkspace(models.Workspace{Name: "old", Path: dir})
	require.NoError(t, err)

	_, err = RegisterWorkspace(models.Workspace{Name: "new", Path: dir})

	require.NoError(t, err)
	workspaces := registeredWorkspaces(t)
	require.Len(t, workspaces, 1)
	assert.Equal(t, "new", workspaces[0].Name)
}

func TestRegisterWorkspaceRejectsDuplicateNameForDifferentPath(t *testing.T) {
	workspaceTestEnv(t)
	base := t.TempDir()
	first := filepath.Join(base, "a")
	second := filepath.Join(base, "b")
	require.NoError(t, os.MkdirAll(first, 0o755))
	require.NoError(t, os.MkdirAll(second, 0o755))
	_, err := RegisterWorkspace(models.Workspace{Name: "notes", Path: first})
	require.NoError(t, err)

	_, err = RegisterWorkspace(models.Workspace{Name: "notes", Path: second})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--name")
}

// TestRegisterWorkspaceDedupesUnexpandedStoredPath guards against a stored
// workspace path like "~/notes" (unexpanded, e.g. hand-edited into the
// config, or written before path normalization existed) failing to dedupe
// against the same directory registered later via its resolved absolute
// path. Without normalizing the comparison, this would either duplicate the
// entry or reject the re-registration as a name collision.
func TestRegisterWorkspaceDedupesUnexpandedStoredPath(t *testing.T) {
	configHome := workspaceTestEnv(t)
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	notesDir := filepath.Join(tempHome, "notes")
	require.NoError(t, os.MkdirAll(notesDir, 0o755))

	configPath := filepath.Join(configHome, configName+"."+configType)
	unexpanded := "[[workspaces]]\nname = \"old\"\npath = \"~/notes\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(unexpanded), 0o644))

	stored, err := RegisterWorkspace(models.Workspace{Name: "new", Path: notesDir})

	require.NoError(t, err)
	assert.Equal(t, "new", stored.Name)
	workspaces := registeredWorkspaces(t)
	require.Len(t, workspaces, 1, "resolved path must dedupe against the unexpanded stored path")
	assert.Equal(t, "new", workspaces[0].Name)
}

func TestUnregisterWorkspace(t *testing.T) {
	workspaceTestEnv(t)
	dir := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := RegisterWorkspace(models.Workspace{Path: dir})
	require.NoError(t, err)

	require.NoError(t, UnregisterWorkspace("notes"))
	assert.Empty(t, registeredWorkspaces(t))

	err = UnregisterWorkspace("notes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no workspace")
}
