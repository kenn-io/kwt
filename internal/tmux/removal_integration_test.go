package tmux

import (
	"context"
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

func TestRemovalSessionGuardRejectsLiveSessionWithoutChangingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := shortRemovalTmuxTempDir(t)
	outputPath := filepath.Join(tempDir, "activity")
	scriptPath := filepath.Join(tempDir, "activity.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(
		"while :; do printf x >> \"$1\"; sleep 0.02; done\n",
	), 0o700))
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const session = "kwt-removal-quiesce-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session,
		"/bin/sh", scriptPath, outputPath,
	))
	t.Cleanup(func() { _ = command.RunCommandContext(context.Background(), "kill-server") })
	require.Eventually(t, func() bool {
		info, err := os.Stat(outputPath)
		return err == nil && info.Size() > 2
	}, time.Second, 10*time.Millisecond)
	condition := removalConditionForTest(t, command, session, tempDir)

	lease, err := NewRemovalSessionGuard("tmux").Quiesce(
		context.Background(), condition,
	)

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "stop the session")
	before, err := os.Stat(outputPath)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		info, statErr := os.Stat(outputPath)
		return statErr == nil && info.Size() > before.Size()
	}, time.Second, 10*time.Millisecond)
	assert.True(t, command.HasSession(session))
}

func TestRemovalSessionGuardAcceptsMissingNamedSocketWhenAbsenceWasConfirmed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	tempDir := shortRemovalTmuxTempDir(t)
	guard := isolatedRemovalSessionGuard(tempDir)
	err := quiesceAndTerminateForTest(guard, RemovalSessionCondition{
		SessionName:             "kwt-missing-protected-session",
		Absent:                  true,
		SocketName:              "kwt-missing-protected-socket",
		ProtectedSocketTopology: true,
	})

	require.NoError(t, err)
}

func TestRemovalSessionGuardRejectsDelimiterBearingWorkspaceMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	tempDir := shortRemovalTmuxTempDir(t)
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const session = "kwt-removal-marker-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "30",
	))
	t.Cleanup(func() { _ = command.RunCommandContext(context.Background(), "kill-server") })
	require.NoError(t, command.RunCommandContext(
		context.Background(),
		"set-option",
		"-t",
		session,
		workspaceIdentityOption,
		"attacker|"+strings.Repeat("a", 64),
	))

	lease, err := isolatedRemovalSessionGuard(tempDir).Quiesce(
		context.Background(),
		RemovalSessionCondition{
			SessionName:         "different-session",
			WorkspacePath:       "/worktrees/topic",
			WorkspaceGeneration: "0123456789abcdef0123456789abcdef",
			SocketDirectory:     tempDir,
			Absent:              true,
		},
	)

	assert.Nil(t, lease)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed session inventory")
}

func isolatedRemovalSessionGuard(tempDir string) RemovalSessionGuard {
	return &removalSessionGuard{
		command:          "tmux",
		inspect:          inspectRemovalSessions,
		inspectProtected: probeProtectedSessionCommand,
		serverCommands: func(condition RemovalSessionCondition) serverCommands {
			return newServerCommands(WorkspaceSessionsOptions{
				Command:              "tmux",
				KWTServerTempDir:     tempDir,
				DefaultServerTempDir: tempDir,
				StripNames:           removalStripNames(condition),
			})
		},
	}
}

func shortRemovalTmuxTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "kwt-rm-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	return directory
}

func quiesceAndTerminateForTest(
	guard RemovalSessionGuard,
	condition RemovalSessionCondition,
) error {
	lease, err := guard.Quiesce(context.Background(), condition)
	if err != nil {
		return err
	}
	return lease.Terminate(context.Background())
}

func removalConditionForTest(
	t *testing.T,
	command *TmuxCommand,
	session string,
	socketDirectory string,
) RemovalSessionCondition {
	t.Helper()
	output, err := command.RunCommandOutputContext(
		context.Background(), "list-sessions", "-F",
		"#{pid}|#{session_id}|#{session_created}|#{session_name}",
	)
	require.NoError(t, err)
	parts := strings.SplitN(strings.TrimSpace(output), "|", 4)
	require.Len(t, parts, 4)
	return RemovalSessionCondition{
		SessionName: session, ServerPID: parts[0], SessionID: parts[1],
		CreatedAt: parts[2], SocketDirectory: socketDirectory,
	}
}

func TestRemovalSessionConditionRejectsNoncanonicalIdentity(t *testing.T) {
	for _, condition := range []RemovalSessionCondition{
		{SessionName: "topic", ServerPID: "01", SessionID: "$2", CreatedAt: "3"},
		{SessionName: "topic", ServerPID: "1", SessionID: "$02", CreatedAt: "3"},
		{SessionName: "topic", ServerPID: "1", SessionID: "$2", CreatedAt: "+3"},
		{SessionName: "topic", Absent: true, SocketDirectory: "relative"},
		{SessionName: "topic", Absent: true, SocketName: "../other"},
		{SessionName: "topic", Absent: true, SocketName: "../other", SocketDirectory: "/tmp"},
	} {
		err := validateRemovalSessionCondition(condition)
		require.Error(t, err)
	}
}
