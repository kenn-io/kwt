package tmuxrestore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireTmux(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tmux is POSIX-only")
	}
	path, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found in PATH")
	}
	return path
}

// privateSocket returns a socket in a fresh directory, with its server killed
// at the end of the test. The directory name is short because Unix socket
// paths are limited to about 100 bytes.
func privateSocket(t *testing.T) Tmux {
	t.Helper()
	executable := requireTmux(t)
	dir, err := os.MkdirTemp("/tmp", "kwr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	tm := Tmux{Executable: executable, Socket: Socket{Path: filepath.Join(dir, "s")}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_, _ = tm.Run(ctx, "kill-server")
	})
	return tm
}

func startServer(t *testing.T, tm Tmux, args ...string) {
	t.Helper()
	full := append([]string{"-f", os.DevNull, "new-session", "-d"}, args...)
	_, err := tm.Run(t.Context(), full...)
	require.NoError(t, err)
}

func TestSocketFromTMUX(t *testing.T) {
	socket, ok := SocketFromTMUX("/private/tmp/tmux-501/default,123,0")
	require.True(t, ok)
	assert.Equal(t, "/private/tmp/tmux-501/default", socket.Path)

	_, ok = SocketFromTMUX("")
	assert.False(t, ok)
}

func TestSocketKeyIsStableAndDistinct(t *testing.T) {
	a := Socket{Path: "/tmp/tmux-501/default"}
	b := Socket{Path: "/tmp/tmux-501/kwt"}
	assert.Equal(t, a.Key(), Socket{Path: a.Path}.Key())
	assert.NotEqual(t, a.Key(), b.Key())
	assert.Len(t, a.Key(), 32)
}

func TestNamedSocketUsesTmuxTmpdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", dir)
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	socket, err := NamedSocket("kwt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(resolved, "tmux-"+uidString(), "kwt"), socket.Path)
	assert.Equal(t, "kwt", socket.Name)
}

func TestProbe(t *testing.T) {
	tm := privateSocket(t)

	state, _, err := tm.Probe(t.Context())
	require.NoError(t, err)
	assert.Equal(t, ServerAbsent, state, "a socket that was never created")

	startServer(t, tm)
	state, id, err := tm.Probe(t.Context())
	require.NoError(t, err)
	assert.Equal(t, ServerRunning, state)
	assert.NotEmpty(t, id.PID)
	assert.NotEmpty(t, id.StartTime)

	_, err = tm.Run(t.Context(), "kill-server")
	require.NoError(t, err)
	state, _, err = tm.Probe(t.Context())
	require.NoError(t, err)
	assert.Equal(t, ServerAbsent, state, "a stale socket file after kill-server")
}

func TestProbeLeavesUnreadableSocketsUnknown(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tm := privateSocket(t)
	startServer(t, tm)
	dir := filepath.Dir(tm.Socket.Path)
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	state, _, err := tm.Probe(t.Context())
	assert.Equal(t, ServerUnknown, state)
	assert.Error(t, err)
}
