package tmux

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemovalSessionGuardTerminatesOnlyCapturedIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const session = "kwt-removal-,}-identity-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() {
		_ = command.RunCommandContext(context.Background(), "kill-server")
	})
	output, err := command.RunCommandOutputContext(
		context.Background(), "list-sessions", "-F",
		"#{pid}|#{session_id}|#{session_created}|#{session_name}",
	)
	require.NoError(t, err)
	parts := strings.SplitN(strings.TrimSpace(output), "|", 4)
	require.Len(t, parts, 4)

	guard := NewRemovalSessionGuard("tmux")
	err = guard.ValidateAndTerminate(context.Background(), RemovalSessionCondition{
		SessionName: session, ServerPID: parts[0], SessionID: parts[1],
		CreatedAt: parts[2], SocketDirectory: tempDir,
	})

	require.NoError(t, err)
	assert.False(t, command.HasSession(session))
}

func TestRemovalSessionGuardTerminatesCapturedIdentityOnNamedSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	const socket = "kwt-pr-removal-test"
	command := NewTmuxCommandForSocketWithStripNames("tmux", socket, nil)
	const session = "kwt-protected-removal-identity-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() {
		_ = command.RunCommandContext(context.Background(), "kill-server")
	})
	output, err := command.RunCommandOutputContext(
		context.Background(), "list-sessions", "-F",
		"#{pid}|#{session_id}|#{session_created}|#{session_name}",
	)
	require.NoError(t, err)
	parts := strings.SplitN(strings.TrimSpace(output), "|", 4)
	require.Len(t, parts, 4)

	guard := NewRemovalSessionGuard("tmux")
	err = guard.ValidateAndTerminate(context.Background(), RemovalSessionCondition{
		SessionName: session, ServerPID: parts[0], SessionID: parts[1],
		CreatedAt: parts[2], SocketName: socket,
	})

	require.NoError(t, err)
	assert.False(t, command.HasSession(session))
}

func TestRemovalSessionGuardTerminatesCapturedIdentityOnNamedSocketInTempDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir, err := os.MkdirTemp("/tmp", "kwt-rm-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	const socket = "legacy"
	command := NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux", socket, tempDir, nil,
	)
	const session = "kwt-legacy-protected-removal-identity-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() {
		_ = command.RunCommandContext(context.Background(), "kill-server")
	})
	output, err := command.RunCommandOutputContext(
		context.Background(), "list-sessions", "-F",
		"#{pid}|#{session_id}|#{session_created}|#{session_name}",
	)
	require.NoError(t, err)
	parts := strings.SplitN(strings.TrimSpace(output), "|", 4)
	require.Len(t, parts, 4)

	guard := NewRemovalSessionGuard("tmux")
	err = guard.ValidateAndTerminate(context.Background(), RemovalSessionCondition{
		SessionName: session, ServerPID: parts[0], SessionID: parts[1],
		CreatedAt: parts[2], SocketName: socket, SocketDirectory: tempDir,
	})

	require.NoError(t, err)
	assert.False(t, command.HasSession(session))
}

func TestRemovalSessionGuardRejectsReplacementIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const session = "kwt-removal-replacement-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() {
		_ = command.RunCommandContext(context.Background(), "kill-server")
	})
	output, err := command.RunCommandOutputContext(
		context.Background(), "list-sessions", "-F",
		"#{pid}|#{session_id}|#{session_created}|#{session_name}",
	)
	require.NoError(t, err)
	parts := strings.SplitN(strings.TrimSpace(output), "|", 4)
	require.Len(t, parts, 4)

	guard := NewRemovalSessionGuard("tmux")
	err = guard.ValidateAndTerminate(context.Background(), RemovalSessionCondition{
		SessionName: session, ServerPID: differentCanonicalPID(parts[0]), SessionID: parts[1],
		CreatedAt: parts[2], SocketDirectory: tempDir,
	})

	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.True(t, command.HasSession(session))
}

func TestRemovalSessionGuardRejectsRenamedSessionAndSameNameReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const original = "kwt-removal-original"
	const renamed = "kwt-removal-renamed"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", original, "sleep", "60",
	))
	t.Cleanup(func() {
		_ = command.RunCommandContext(context.Background(), "kill-server")
	})
	output, err := command.RunCommandOutputContext(
		context.Background(), "list-sessions", "-F",
		"#{pid}|#{session_id}|#{session_created}|#{session_name}",
	)
	require.NoError(t, err)
	parts := strings.SplitN(strings.TrimSpace(output), "|", 4)
	require.Len(t, parts, 4)

	require.NoError(t, command.RunCommandContext(
		context.Background(), "rename-session", "-t", parts[1]+":", renamed,
	))
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", original, "sleep", "60",
	))

	guard := NewRemovalSessionGuard("tmux")
	err = guard.ValidateAndTerminate(context.Background(), RemovalSessionCondition{
		SessionName: original, ServerPID: parts[0], SessionID: parts[1],
		CreatedAt: parts[2], SocketDirectory: tempDir,
	})

	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Equal(t, "tmux session identity changed after confirmation", conditionErr.Reason)
	assert.True(t, command.HasSession(original), "replacement session must remain running")
	assert.True(t, command.HasSession(renamed), "captured renamed session must remain running")
}

func differentCanonicalPID(value string) string {
	if value == "1" {
		return "2"
	}
	return "1"
}

func TestRemovalSessionGuardAcceptsMissingNamedSocketWhenAbsenceWasConfirmed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	guard := NewRemovalSessionGuard("tmux")
	err := guard.ValidateAndTerminate(context.Background(), RemovalSessionCondition{
		SessionName: "kwt-missing-protected-session",
		Absent:      true,
		SocketName:  "kwt-missing-protected-socket",
	})

	require.NoError(t, err)
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
