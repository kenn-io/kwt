package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionManagerMasksConfiguredCredentialRetainedByExistingServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	const tokenName = "KWT_RETAINED_TOKEN_TEST"
	const tokenValue = "must-not-reach-session"
	tempDir, err := os.MkdirTemp("/tmp", "kwt-manager-")
	require.NoError(t, err)
	server := NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux", KWTServerSocketName, tempDir, nil,
	)
	t.Cleanup(func() {
		_ = server.RunCommandContext(context.Background(), "kill-server")
		require.NoError(t, os.RemoveAll(tempDir))
	})

	require.NoError(t, server.NewSessionWithCommandContext(
		context.Background(), "keeper", tempDir, "sleep 60",
	))
	require.NoError(t, server.RunCommandContext(
		context.Background(),
		"set-environment", "-g", tokenName, tokenValue,
	))

	manager := NewSessionManagerWithTempDir(nil, tempDir, tokenName)
	probeCommand := fmt.Sprintf(
		"if env | grep -q '^%s='; then printf 'credential-leaked\\n'; "+
			"else printf 'credential-clean\\n'; fi; sleep 60",
		tokenName,
	)
	session, err := manager.CreateSession(context.Background(), SessionOptions{
		Context:    "run",
		Identifier: "retained-token",
		WorkingDir: tempDir,
		Command:    probeCommand,
	})
	require.NoError(t, err)
	marker, err := server.RunCommandOutputContext(
		context.Background(),
		"show-environment", "-t", session.SessionName, tokenName,
	)
	require.NoError(t, err)
	assert.Equal(t, "-"+tokenName+"\n", marker)

	var paneEnvironment string
	require.Eventually(t, func() bool {
		paneEnvironment, err = server.RunCommandOutputContext(
			context.Background(),
			"capture-pane", "-p", "-S", "-", "-t", session.SessionName,
		)
		return err == nil && (strings.Contains(paneEnvironment, "credential-clean") ||
			strings.Contains(paneEnvironment, "credential-leaked"))
	}, 3*time.Second, 20*time.Millisecond)
	assert.False(
		t,
		strings.Contains(paneEnvironment, "credential-leaked"),
		"session pane inherited configured credential from server-global environment",
	)
}
