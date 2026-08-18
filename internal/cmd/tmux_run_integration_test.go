package cmd

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/tmux"
)

func TestTmuxRunAutoCleanupUsesPaneServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir, err := os.MkdirTemp("/tmp", "kwt-run-")
	require.NoError(t, err)
	manager := tmux.NewSessionManagerWithTempDir(nil, tempDir)
	kwtServer := tmux.NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux", tmux.KWTServerSocketName, tempDir, nil,
	)
	defaultServer := tmux.NewTmuxCommandInTempDirWithStripNames("tmux", tempDir, nil)
	t.Cleanup(func() {
		_ = kwtServer.RunCommandContext(context.Background(), "kill-server")
		_ = defaultServer.RunCommandContext(context.Background(), "kill-server")
		require.NoError(t, os.RemoveAll(tempDir))
	})

	previousManager := newTmuxRunSessionManager
	previousDetach := tmuxRunDetach
	previousCleanup := tmuxRunAutoCleanup
	previousContext := tmuxRunContext
	previousIdentifier := tmuxRunIdentifier
	previousWorktree := tmuxRunWorktree
	t.Cleanup(func() {
		newTmuxRunSessionManager = previousManager
		tmuxRunDetach = previousDetach
		tmuxRunAutoCleanup = previousCleanup
		tmuxRunContext = previousContext
		tmuxRunIdentifier = previousIdentifier
		tmuxRunWorktree = previousWorktree
	})
	var protectedNames []string
	newTmuxRunSessionManager = func(
		_ *tmux.SessionConfig,
		names ...string,
	) *tmux.SessionManager {
		protectedNames = append([]string(nil), names...)
		return manager
	}
	tmuxRunDetach = true
	tmuxRunAutoCleanup = true
	tmuxRunContext = "integration"
	tmuxRunIdentifier = "cleanup"
	tmuxRunWorktree = ""
	initCommandTestConfig(t, t.TempDir())
	viper.Set("fleet.token_env", "CUSTOM_FLEET_TOKEN")
	t.Setenv("CUSTOM_FLEET_TOKEN", "secret")

	command := &cobra.Command{}
	command.SetContext(context.Background())
	require.NoError(t, runTmuxRun(command, []string{"sleep 0.5"}))
	assert.ElementsMatch(t, []string{
		"KWT_GITHUB_TOKEN",
		"KWT_FLEET_TOKEN",
		"CUSTOM_FLEET_TOKEN",
	}, protectedNames)
	sessions, err := manager.ListSessions()
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, tmux.KWTServerSocketName, sessions[0].SocketName)
	require.Eventually(t, func() bool {
		sessions, listErr := manager.ListSessions()
		return listErr == nil && len(sessions) == 0
	}, 3*time.Second, 20*time.Millisecond)
	defaultSessions, err := defaultServer.ListSessionsContext(context.Background())
	require.NoError(t, err)
	assert.Empty(t, defaultSessions)
}
