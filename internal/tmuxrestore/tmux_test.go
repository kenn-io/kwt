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

// killServerAndWait models a completed server loss. kill-server only requests
// shutdown; another client can still reach the dying server until it exits.
func killServerAndWait(t *testing.T, tm Tmux) {
	t.Helper()
	_, err := tm.Run(t.Context(), "kill-server")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		state, _, err := tm.Probe(t.Context())
		return err == nil && state == ServerAbsent
	}, 5*time.Second, 10*time.Millisecond, "private tmux server did not stop")
}

func TestSocketFromTMUX(t *testing.T) {
	socket, ok := SocketFromTMUX("/private/tmp/tmux-501/default,123,0")
	require.True(t, ok)
	assert.Equal(t, "/private/tmp/tmux-501/default", socket.Path)
	assert.Equal(t, "123", ServerPIDFromTMUX("/private/tmp/tmux-501/default,123,0"))

	// A socket path can hold commas; the PID and session follow the last two.
	socket, ok = SocketFromTMUX("/tmp/a,b/tmux-501/default,123,0")
	require.True(t, ok)
	assert.Equal(t, "/tmp/a,b/tmux-501/default", socket.Path)
	assert.Equal(t, "123", ServerPIDFromTMUX("/tmp/a,b/tmux-501/default,123,0"))

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

	socket, err := NamedSocket("default")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(resolved, "tmux-"+uidString(), "default"), socket.Path)
	assert.Equal(t, "default", socket.Name)
}

func TestKWTSocketIgnoresTmuxTmpdirLikeKwtsServer(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	tmp, err := filepath.EvalSymlinks("/tmp")
	require.NoError(t, err)
	want := filepath.Join(tmp, "tmux-"+uidString(), "kwt")

	socket, err := SocketIn("", "kwt")
	require.NoError(t, err)
	assert.Equal(t, Socket{Path: want, Name: "kwt"}, socket)

	fromPane, ok := SocketFromTMUX(want + ",123,0")
	require.True(t, ok)
	assert.Equal(t, "kwt", fromPane.Name, "a pane on kwt's server names its socket")

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	socket, err = SocketIn(dir, "kwt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(resolved, "tmux-"+uidString(), "kwt"), socket.Path)
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

	killServerAndWait(t, tm)
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

func TestTmuxErrorsHideSavedConversationRecords(t *testing.T) {
	tm := privateSocket(t)
	startServer(t, tm)

	_, err := tm.Run(t.Context(), "set-option", "-p", "-t", "%999", ResumeOption, `{"args":["--secret-flag=hunter2"]}`)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
	assert.Contains(t, err.Error(), ResumeOption)
	assert.Contains(t, err.Error(), "%999", "the failing pane stays named")
}
