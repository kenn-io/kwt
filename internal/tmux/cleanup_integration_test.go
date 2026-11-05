package tmux

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTmuxCommandAtomicallyTerminatesMatchingWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	ctx := t.Context()
	tempDir, err := os.MkdirTemp("/tmp", "kwt-cleanup-")
	require.NoError(t, err)
	command := NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux", KWTServerSocketName, tempDir, nil,
	)
	t.Cleanup(func() {
		// Test contexts are canceled before cleanup runs.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = command.RunCommandContext(cleanupCtx, "kill-server")
		require.NoError(t, os.RemoveAll(tempDir))
	})

	session := `kwt-wt-topic;'"-01234567`
	require.NoError(t, command.NewSessionWithCommandContext(
		ctx, session, tempDir, "sleep 60",
	))
	require.NoError(t, command.SetOptionContext(
		ctx,
		session,
		workspaceCleanupOption(resolverTestGeneration),
		"1",
	))

	err = command.KillWorkspaceSessionIfMatchingContext(
		ctx,
		WorkspaceEndpointRequest{
			SessionName:         session,
			WorkspaceGeneration: resolverTestGeneration,
		},
	)

	require.NoError(t, err)
	assert.False(t, command.HasSession(session))
}

func TestTmuxCommandTerminatesVerifiedPreCleanupMarkerWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	ctx := t.Context()
	tempDir, err := os.MkdirTemp("/tmp", "kwt-cleanup-")
	require.NoError(t, err)
	command := NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux", KWTServerSocketName, tempDir, nil,
	)
	t.Cleanup(func() {
		// Test contexts are canceled before cleanup runs.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = command.RunCommandContext(cleanupCtx, "kill-server")
		require.NoError(t, os.RemoveAll(tempDir))
	})

	session := "kwt-wt-legacy-main-01234567"
	require.NoError(t, command.NewSessionWithCommandContext(
		ctx, session, tempDir, "sleep 60",
	))
	require.NoError(t, command.SetOptionContext(
		ctx,
		session,
		workspaceGenerationOption,
		resolverTestGeneration,
	))

	err = command.KillWorkspaceSessionIfMatchingContext(
		ctx,
		WorkspaceEndpointRequest{
			SessionName:         session,
			WorkspaceGeneration: resolverTestGeneration,
		},
	)

	require.NoError(t, err)
	assert.False(t, command.HasSession(session))
}
