package cmd

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
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
