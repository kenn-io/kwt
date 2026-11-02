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
	runTmux := func(ctx context.Context, args ...string) (string, error) {
		command := exec.CommandContext(ctx, "tmux", append([]string{"-L", socket}, args...)...)
		command.Env = filteredEnviron(os.Environ(), func(name string) bool {
			return strings.EqualFold(name, "TMUX_TMPDIR")
		})
		output, err := command.CombinedOutput()
		return string(output), err
	}
	t.Setenv("TMUX_TMPDIR", filepath.Join(t.TempDir(), "creator"))
	protected := NewTmuxCommandForSocketWithStripNames("", socket, nil)
	err := protected.RunCommandContext(
		t.Context(), "new-session", "-d", "-s", session, "sleep", "60",
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Test contexts are canceled before cleanup runs.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_, _ = runTmux(cleanupCtx, "kill-server")
	})

	t.Setenv("TMUX_TMPDIR", filepath.Join(t.TempDir(), "probe"))
	state, err := ProbeProtectedSession(t.Context(), socket, session, nil, "")
	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state, "detached session")

	attachCtx, cancelAttach := context.WithCancel(t.Context())
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
		output, listErr := runTmux(t.Context(), "list-sessions", "-F", "#{session_attached}")
		return listErr == nil && strings.TrimSpace(output) == "1"
	}, 2*time.Second, 10*time.Millisecond)
	state, err = ProbeProtectedSession(t.Context(), socket, session, nil, "")
	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state, "attached session")

	_, err = runTmux(t.Context(), "kill-server")
	require.NoError(t, err)
	cancelAttach()
	require.Eventually(t, func() bool {
		state, err = ProbeProtectedSession(t.Context(), socket, session, nil, "")
		return err == nil && state == ProtectedSessionAbsent
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, ProtectedSessionAbsent, state, "missing server and session")

	state, err = ProbeProtectedSession(
		t.Context(), strings.Repeat("x", 200), session, nil, "",
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
		t.Context(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() {
		_ = legacy.RunCommandContext(context.WithoutCancel(t.Context()), "kill-server")
	})

	resolved, state, err := ResolveProtectedSessionCommand(
		t.Context(), socket, session, nil, tempDir,
	)

	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state)
	assert.Equal(t, tempDir, resolved.socketTempDir)
}
