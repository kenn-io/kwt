package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func TestGlobalConfigStoreSerializesIndependentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[ui]\nicons = true\n"), 0o640))
	first := globalConfigStore{path: path}
	second := globalConfigStore{path: path}
	entered := make(chan string, 2)
	release := make(chan struct{})
	firstDone := make(chan error, 1)

	go func() {
		_, err := first.mutate(func(current *viper.Viper) (bool, error) {
			entered <- "first"
			<-release
			current.Set("ui.tilde_home", true)
			return true, nil
		})
		firstDone <- err
	}()
	require.Equal(t, "first", <-entered)

	secondDone := make(chan error, 1)
	go func() {
		_, err := second.mutate(func(current *viper.Viper) (bool, error) {
			entered <- "second"
			current.Set("finder.preview", false)
			return true, nil
		})
		secondDone <- err
	}()
	select {
	case got := <-entered:
		t.Fatalf("second mutation entered before unlock: %s", got)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.Equal(t, "second", <-entered)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)

	stored, err := readGlobalViper(path)
	require.NoError(t, err)
	assert.True(t, stored.GetBool("ui.icons"))
	assert.True(t, stored.GetBool("ui.tilde_home"))
	assert.False(t, stored.GetBool("finder.preview"))
}

func TestGlobalConfigStorePreservesModeAndUnrelatedSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(
		path,
		[]byte("[ui]\nicons = true\n\n[custom]\nvalue = \"kept\"\n"),
		0o640,
	))
	require.NoError(t, os.Chmod(path, 0o640))

	changed, err := (globalConfigStore{path: path}).mutate(
		func(current *viper.Viper) (bool, error) {
			current.Set("ui.tilde_home", true)
			return true, nil
		},
	)

	require.NoError(t, err)
	assert.True(t, changed)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	stored, err := readGlobalViper(path)
	require.NoError(t, err)
	assert.Equal(t, "kept", stored.GetString("custom.value"))
	temps, err := filepath.Glob(filepath.Join(dir, ".config-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, temps)
}

func TestGlobalConfigStoreDoesNotOverwriteMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[ui\nicons = true\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))

	called := false
	changed, err := (globalConfigStore{path: path}).mutate(
		func(*viper.Viper) (bool, error) {
			called = true
			return true, nil
		},
	)

	require.Error(t, err)
	assert.False(t, changed)
	assert.False(t, called)
	stored, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, original, stored)
}

func TestGlobalConfigStoreEnsurePublishesCompleteFileAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := "[ui]\nicons = true\n"
	oldHook := beforeGlobalConfigPublish
	t.Cleanup(func() { beforeGlobalConfigPublish = oldHook })
	var observed bool
	beforeGlobalConfigPublish = func(tempPath, targetPath string) {
		assert.Equal(t, path, targetPath)
		_, statErr := os.Stat(targetPath)
		assert.ErrorIs(t, statErr, os.ErrNotExist)
		data, readErr := os.ReadFile(tempPath)
		require.NoError(t, readErr)
		assert.Equal(t, contents, string(data))
		observed = true
	}

	created, err := (globalConfigStore{path: path}).ensure(contents)

	require.NoError(t, err)
	assert.True(t, created)
	assert.True(t, observed)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, contents, string(data))
}

func TestGlobalConfigStoreEnsurePreservesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	linkPath := filepath.Join(dir, "config.toml")
	targetPath := filepath.Join(dir, "managed", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("managed", "config.toml"), linkPath))
	contents := "[ui]\nicons = true\n"

	created, err := (globalConfigStore{path: linkPath}).ensure(contents)

	require.NoError(t, err)
	assert.True(t, created)
	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	stored, err := os.ReadFile(targetPath)
	require.NoError(t, err)
	assert.Equal(t, contents, string(stored))
}

func TestGlobalConfigStoreEnsurePreservesDanglingSymlinkChain(t *testing.T) {
	dir := t.TempDir()
	linkPath := filepath.Join(dir, "config.toml")
	intermediatePath := filepath.Join(dir, "current.toml")
	targetPath := filepath.Join(dir, "managed", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("managed", "config.toml"), intermediatePath))
	require.NoError(t, os.Symlink("current.toml", linkPath))
	contents := "[ui]\nicons = true\n"

	created, err := (globalConfigStore{path: linkPath}).ensure(contents)

	require.NoError(t, err)
	assert.True(t, created)
	for _, path := range []string{linkPath, intermediatePath} {
		info, statErr := os.Lstat(path)
		require.NoError(t, statErr)
		assert.NotZero(t, info.Mode()&os.ModeSymlink)
	}
	stored, err := os.ReadFile(targetPath)
	require.NoError(t, err)
	assert.Equal(t, contents, string(stored))
}

func TestGlobalConfigStorePreservesConfigSymlink(t *testing.T) {
	for _, tt := range []struct {
		name       string
		targetPath func(string) string
	}{
		{name: "relative", targetPath: func(string) string { return "managed.toml" }},
		{name: "absolute", targetPath: func(dir string) string { return filepath.Join(dir, "managed.toml") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			linkPath := filepath.Join(dir, "config.toml")
			targetValue := tt.targetPath(dir)
			targetPath := targetValue
			if !filepath.IsAbs(targetPath) {
				targetPath = filepath.Join(dir, targetPath)
			}
			require.NoError(t, os.WriteFile(targetPath, []byte("[ui]\nicons = true\n"), 0o640))
			require.NoError(t, os.Chmod(targetPath, 0o640))
			require.NoError(t, os.Symlink(targetValue, linkPath))

			changed, err := (globalConfigStore{path: linkPath}).mutate(
				func(current *viper.Viper) (bool, error) {
					current.Set("ui.tilde_home", true)
					return true, nil
				},
			)

			require.NoError(t, err)
			assert.True(t, changed)
			info, err := os.Lstat(linkPath)
			require.NoError(t, err)
			assert.NotZero(t, info.Mode()&os.ModeSymlink)
			stored, err := readGlobalViper(targetPath)
			require.NoError(t, err)
			assert.True(t, stored.GetBool("ui.tilde_home"))
			info, err = os.Stat(targetPath)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
		})
	}
}

func TestGlobalConfigWritersShareOneFileLock(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		write   func(string) error
	}{
		{
			name: "set global",
			write: func(string) error {
				return SetGlobal("ui.tilde_home", true)
			},
		},
		{
			name: "register project",
			write: func(path string) error {
				repository := filepath.Join(filepath.Dir(path), "repository")
				if err := os.Mkdir(repository, 0o755); err != nil {
					return err
				}
				return RegisterProject(models.Project{
					Repository: "github.com/acme/widget",
					Name:       "widget",
					Path:       repository,
				})
			},
		},
		{
			name: "register workspace",
			write: func(path string) error {
				workspace := filepath.Join(filepath.Dir(path), "workspace")
				if err := os.Mkdir(workspace, 0o755); err != nil {
					return err
				}
				_, err := RegisterWorkspace(models.Workspace{Name: "notes", Path: workspace})
				return err
			},
		},
		{
			name: "unregister workspace",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte(
					"[[workspaces]]\nname = \"notes\"\npath = \"/tmp/notes\"\n",
				), 0o600))
			},
			write: func(string) error { return UnregisterWorkspace("notes") },
		},
		{
			name: "startup migration",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte(
					"[layouts]\nauto_launch_on_add = false\n",
				), 0o600))
			},
			write: func(path string) error {
				_, err := backfillWorkspaceConfig(path)
				return err
			},
		},
		{
			name:  "default creation",
			write: func(string) error { return Init() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			configHome := t.TempDir()
			t.Setenv("KWT_HOME", configHome)
			path := filepath.Join(configHome, "config.toml")
			if tt.name != "default creation" {
				require.NoError(t, os.WriteFile(path, []byte("[ui]\nicons = true\n"), 0o600))
			}
			if tt.prepare != nil {
				tt.prepare(t, path)
			}

			lock := flock.New(path+".lock", flock.SetPermissions(0o600))
			require.NoError(t, lock.Lock())
			done := make(chan error, 1)
			go func() { done <- tt.write(path) }()
			select {
			case err := <-done:
				_ = lock.Unlock()
				t.Fatalf("writer completed before global config unlock: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			require.NoError(t, lock.Unlock())
			require.NoError(t, <-done)
		})
	}
}
