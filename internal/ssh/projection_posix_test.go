//go:build !windows

package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
)

func TestProjectionPreservesCompression(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH is unavailable")
	}
	for _, compression := range []string{"yes", "no"} {
		t.Run(compression, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(configPath, []byte("Host compression.example.test\n  Compression "+compression+"\n"), 0o600))
			output, err := exec.CommandContext(t.Context(), ssh, "-G", "-F", configPath, "compression.example.test").Output()
			require.NoError(t, err)
			projected, err := projectConfig(openssh.ParseConfig(output), []string{})
			require.NoError(t, err)
			args := append(projected.Arguments, "-G", "compression.example.test")
			output, err = exec.CommandContext(t.Context(), ssh, args...).Output()
			require.NoError(t, err)
			var replayedCompression string
			for _, option := range openssh.ParseConfig(output).Options {
				if option.Name == "compression" {
					replayedCompression = option.Value
				}
			}
			require.Equal(t, compression, replayedCompression)
		})
	}
}
