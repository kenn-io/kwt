package tmux

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeProtectedSessionAgainstRealTmux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("kwt-probe-%d", time.Now().UnixNano())
	session := "expected"
	runTmux := func(args ...string) (string, error) {
		command := exec.Command("tmux", append([]string{"-L", socket}, args...)...)
		command.Env = filteredEnviron(os.Environ(), func(name string) bool {
			return strings.EqualFold(name, "TMUX_TMPDIR")
		})
		output, err := command.CombinedOutput()
		return string(output), err
	}
	t.Setenv("TMUX_TMPDIR", filepath.Join(t.TempDir(), "creator"))
	protected := NewTmuxCommandForSocketWithStripNames("", socket, nil)
	err := protected.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	t.Setenv("TMUX_TMPDIR", filepath.Join(t.TempDir(), "probe"))
	state, err := ProbeProtectedSession(context.Background(), socket, session, nil, "")
	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state, "detached session")

	attachCtx, cancelAttach := context.WithCancel(context.Background())
	attach := exec.CommandContext(
		attachCtx, "tmux", "-L", socket, "-C", "attach-session", "-t", session,
	)
	attach.Env = filteredEnviron(os.Environ(), func(name string) bool {
		return strings.EqualFold(name, "TMUX_TMPDIR")
	})
	attach.Stdout = io.Discard
	attach.Stderr = io.Discard
	attachInput, err := attach.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, attach.Start())
	t.Cleanup(func() {
		_ = attachInput.Close()
		cancelAttach()
		_ = attach.Wait()
	})
	require.Eventually(t, func() bool {
		output, listErr := runTmux("list-sessions", "-F", "#{session_attached}")
		return listErr == nil && strings.TrimSpace(output) == "1"
	}, 2*time.Second, 10*time.Millisecond)
	state, err = ProbeProtectedSession(context.Background(), socket, session, nil, "")
	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state, "attached session")

	_, err = runTmux("kill-server")
	require.NoError(t, err)
	cancelAttach()
	require.Eventually(t, func() bool {
		state, err = ProbeProtectedSession(context.Background(), socket, session, nil, "")
		return err == nil && state == ProtectedSessionAbsent
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, ProtectedSessionAbsent, state, "missing server and session")

	state, err = ProbeProtectedSession(
		context.Background(), strings.Repeat("x", 200), session, nil, "",
	)
	assert.Equal(t, ProtectedSessionIndeterminate, state)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "protected endpoint is absent")
}

func TestResolveProtectedSessionFindsLegacyTmuxTempDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("kwt-legacy-probe-%d", time.Now().UnixNano())
	session := "legacy"
	tempDir, err := os.MkdirTemp("/tmp", "kwt-legacy-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(tempDir)) })
	legacy := NewTmuxCommandForSocketInTempDirWithStripNames(
		"", socket, tempDir, nil,
	)
	require.NoError(t, legacy.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() {
		_ = legacy.RunCommandContext(context.Background(), "kill-server")
	})

	resolved, state, err := ResolveProtectedSessionCommand(
		context.Background(), socket, session, nil, tempDir,
	)

	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state)
	assert.Equal(t, tempDir, resolved.socketTempDir)
}
