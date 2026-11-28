package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/pkg/models"
)

func TestConfigSetRoundTripsDaemonDuration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	oldLocal := configSetLocal
	configSetLocal = false
	t.Cleanup(func() { configSetLocal = oldLocal })

	require.NoError(t, runConfigSet(
		&cobra.Command{},
		[]string{"daemon.idle_timeout", "2h"},
	))
	snapshot, err := config.LoadGlobalSnapshot()
	require.NoError(t, err)
	assert.Equal(t, 2*time.Hour, snapshot.Config.Daemon.IdleTimeout)
}

func TestConfigSetRejectsDirectProjectRegistryWrites(t *testing.T) {
	for _, key := range []string{"projects", "Projects", "projects.path"} {
		t.Run(key, func(t *testing.T) {
			configHome := t.TempDir()
			t.Setenv("KWT_HOME", configHome)
			viper.Reset()
			t.Cleanup(viper.Reset)
			oldLocal := configSetLocal
			configSetLocal = false
			t.Cleanup(func() { configSetLocal = oldLocal })
			projectPath := filepath.Join(t.TempDir(), "widget")
			contents := "[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = " +
				strconv.Quote(projectPath) + "\nlast_touched = 'before'\n"
			require.NoError(t, os.WriteFile(
				filepath.Join(configHome, "config.toml"),
				[]byte(contents),
				0o600,
			))

			err := runConfigSet(
				&cobra.Command{},
				[]string{key, "[]"},
			)

			require.ErrorContains(t, err, "kwt projects")
			snapshot, err := config.LoadGlobalSnapshotAt(configHome)
			require.NoError(t, err)
			assert.Equal(t, []models.Project{{
				Repository:  "github.com/acme/widget",
				Name:        "widget",
				Path:        projectPath,
				LastTouched: "before",
			}}, snapshot.Config.Projects)
		})
	}
}

func TestConfigSetRejectsPullRequestPolicy(t *testing.T) {
	for _, tc := range []struct {
		key   string
		local bool
	}{
		{"pull_request.trusted_merge_drivers", false},
		{"pull_request.trusted_merge_drivers", true},
		{"Pull_Request", false},
	} {
		t.Run(tc.key+strconv.FormatBool(tc.local), func(t *testing.T) {
			home := t.TempDir()
			t.Chdir(t.TempDir())
			t.Setenv("KWT_HOME", home)
			viper.Reset()
			t.Cleanup(viper.Reset)
			old := configSetLocal
			configSetLocal = tc.local
			t.Cleanup(func() { configSetLocal = old })
			path := filepath.Join(home, "config.toml")
			before := []byte("[pull_request]\ntrusted_merge_drivers = ['safe']\n")
			require.NoError(t, os.WriteFile(path, before, 0o600))
			err := runConfigSet(&cobra.Command{}, []string{tc.key, "true"})
			require.ErrorContains(t, err, tc.key)
			require.ErrorContains(t, err, "edit [pull_request] in the global config.toml")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.False(t, viper.IsSet(tc.key))
			_, err = os.Stat(".kwt.toml")
			assert.True(t, os.IsNotExist(err))
		})
	}
}
