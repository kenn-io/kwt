package cmd

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIErrorOutputKeepsDiagnosticsWithoutUsage(t *testing.T) {
	binary := buildDaemonTestBinary(t, daemonTestBuild{Name: "kwt"})
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		name   string
		config string
		args   []string
		want   string
	}{
		{
			name: "missing workspace directory",
			args: []string{"workspace", "add", filepath.Join(t.TempDir(), "missing")},
			want: "missing",
		},
		{
			name:   "invalid configuration",
			config: "[invalid",
			args:   []string{"workspace", "list"},
			want:   "config",
		},
		{
			name: "unknown flag",
			args: []string{"open", "--unknown-flag"},
			want: "unknown flag: --unknown-flag",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newDaemonTestHome(t, test.config)
			stdout, stderr, err := runDaemonCommand(t, binary, home, test.args...)
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.Empty(t, stdout)
			assert.Contains(t, string(stderr), "Error:")
			assert.Contains(t, string(stderr), test.want)
			assert.NotContains(t, string(stderr), "Usage:")
		})
	}

	stdout, stderr, err := runDaemonCommand(
		t, binary, newDaemonTestHome(t, ""), "open", "--help",
	)
	require.NoError(t, err, string(stderr))
	assert.Contains(t, string(stdout), "Usage:")
	assert.Contains(t, string(stdout), "--start-session")
}
