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
