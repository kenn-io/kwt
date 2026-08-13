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

func TestRemovalSessionGuardQuiescesAndResumesCapturedSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
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
	require.NoError(t, err)
	quiesced, err := os.Stat(outputPath)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	stillQuiesced, err := os.Stat(outputPath)
	require.NoError(t, err)
	assert.Equal(t, quiesced.Size(), stillQuiesced.Size())

	require.NoError(t, lease.Resume())
	require.Eventually(t, func() bool {
		info, statErr := os.Stat(outputPath)
		return statErr == nil && info.Size() > stillQuiesced.Size()
	}, time.Second, 10*time.Millisecond)
}

func TestRemovalSessionGuardRejectsWindowSharedWithAnotherSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const target = "kwt-removal-shared-target"
	const peer = "kwt-removal-shared-peer"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", target, "sleep", "60",
	))
	condition := removalConditionForTest(t, command, target, tempDir)
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", peer, "sleep", "60",
	))
	require.NoError(t, command.RunCommandContext(
		context.Background(), "link-window", "-s", target+":0", "-t", peer+":1",
	))
	t.Cleanup(func() { _ = command.RunCommandContext(context.Background(), "kill-server") })

	lease, err := NewRemovalSessionGuard("tmux").Quiesce(
		context.Background(), condition,
	)
	if lease != nil {
		require.NoError(t, lease.Resume())
	}

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "shared")
	assert.True(t, command.HasSession(target))
	assert.True(t, command.HasSession(peer))
}

func TestRemovalSessionGuardRejectsPaneChangeBeforeAtomicFreeze(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const session = "kwt-removal-pane-race-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() { _ = command.RunCommandContext(context.Background(), "kill-server") })
	condition := removalConditionForTest(t, command, session, tempDir)
	wrapper := filepath.Join(t.TempDir(), "tmux-wrapper")
	require.NoError(t, os.WriteFile(wrapper, []byte(
		"#!/bin/sh\n/usr/bin/tmux \"$@\"\nstatus=$?\n"+
			"case \" $* \" in *\" display-message -p -t \"*) "+
			"/usr/bin/tmux -f /dev/null split-window -d -t "+session+" sleep 60 ;; esac\n"+
			"exit $status\n",
	), 0o700))

	lease, err := NewRemovalSessionGuard(wrapper).Quiesce(
		context.Background(), condition,
	)

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "identity changed")
	output, listErr := command.RunCommandOutputContext(
		context.Background(), "list-panes", "-s", "-t", session, "-F", "#{pane_id}",
	)
	require.NoError(t, listErr)
	assert.Len(t, strings.Fields(output), 2)
}

func TestRemovalSessionGuardTerminatesHupIgnoringWriterWhileQuiesced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "activity")
	scriptPath := filepath.Join(tempDir, "ignore-hup.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(
		"trap '' HUP TERM\nwhile :; do printf x >> \"$1\"; sleep 0.02; done\n",
	), 0o700))
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const session = "kwt-removal-ignore-hup-test"
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
	require.NoError(t, err)

	require.NoError(t, lease.Terminate(context.Background()))
	terminated, err := os.Stat(outputPath)
	require.NoError(t, err)
	time.Sleep(150 * time.Millisecond)
	stillTerminated, err := os.Stat(outputPath)
	require.NoError(t, err)
	assert.Equal(t, terminated.Size(), stillTerminated.Size())
	assert.False(t, command.HasSession(session))
}

func TestRemovalSessionGuardDoesNotDowngradeIdentityMismatchAfterPanesExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir := t.TempDir()
	command := NewTmuxCommandInTempDir("tmux", tempDir)
	const session = "kwt-removal-stale-authority-test"
	require.NoError(t, command.RunCommandContext(
		context.Background(), "new-session", "-d", "-s", session, "sleep", "60",
	))
	t.Cleanup(func() { _ = command.RunCommandContext(context.Background(), "kill-server") })
	condition := removalConditionForTest(t, command, session, tempDir)
	lease, err := NewRemovalSessionGuard("tmux").Quiesce(
		context.Background(), condition,
	)
	require.NoError(t, err)
	live := lease.(*liveRemovalSessionLease)
	live.condition.SessionName = "stale-authority"

	err = live.Terminate(context.Background())

	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "identity changed")
}

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
	err = quiesceAndTerminateForTest(guard, RemovalSessionCondition{
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
	err = quiesceAndTerminateForTest(guard, RemovalSessionCondition{
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
	err = quiesceAndTerminateForTest(guard, RemovalSessionCondition{
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
	err = quiesceAndTerminateForTest(guard, RemovalSessionCondition{
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
	err = quiesceAndTerminateForTest(guard, RemovalSessionCondition{
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
	err := quiesceAndTerminateForTest(guard, RemovalSessionCondition{
		SessionName: "kwt-missing-protected-session",
		Absent:      true,
		SocketName:  "kwt-missing-protected-socket",
	})

	require.NoError(t, err)
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
