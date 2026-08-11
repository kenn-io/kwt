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
		output, err := command.CombinedOutput()
		return string(output), err
	}
	_, err := runTmux("new-session", "-d", "-s", session, "sleep", "60")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	state, err := ProbeProtectedSession(context.Background(), socket, session)
	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state, "detached session")

	attachCtx, cancelAttach := context.WithCancel(context.Background())
	attach := exec.CommandContext(
		attachCtx, "tmux", "-L", socket, "-C", "attach-session", "-t", session,
	)
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
	state, err = ProbeProtectedSession(context.Background(), socket, session)
	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionLive, state, "attached session")

	_, err = runTmux("kill-server")
	require.NoError(t, err)
	cancelAttach()
	state, err = ProbeProtectedSession(context.Background(), socket, session)
	require.NoError(t, err)
	assert.Equal(t, ProtectedSessionAbsent, state, "missing server and session")

	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	t.Setenv("TMUX_TMPDIR", notDirectory)
	state, err = ProbeProtectedSession(context.Background(), "operational-failure", session)
	assert.Equal(t, ProtectedSessionIndeterminate, state)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "protected endpoint is absent")
}
