package tmux

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKillSessionIfPresentTreatsOnlyExplicitAbsenceAsSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is unavailable on Windows")
	}
	fixture := filepath.Join(t.TempDir(), "tmux-fixture")
	require.NoError(t, os.WriteFile(fixture, []byte(`#!/bin/sh
printf '%s\n' "$KWT_TEST_TMUX_STDERR" >&2
exit "$KWT_TEST_TMUX_EXIT"
`), 0o700))

	tests := []struct {
		name      string
		stderr    string
		exit      string
		wantError bool
	}{
		{name: "no server", stderr: "no server running on test socket", exit: "1"},
		{name: "missing socket", stderr: "error connecting to /tmp/tmux/socket (No such file or directory)", exit: "1"},
		{name: "missing session", stderr: "can't find session: workspace", exit: "1"},
		{name: "permission failure", stderr: "error connecting to /tmp/tmux/socket (Permission denied)", exit: "1", wantError: true},
		{name: "unexpected failure", stderr: "server rejected request", exit: "2", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KWT_TEST_TMUX_STDERR", test.stderr)
			t.Setenv("KWT_TEST_TMUX_EXIT", test.exit)
			command := NewTmuxCommand(fixture)

			err := command.KillSessionIfPresentContext(
				context.Background(),
				"workspace",
			)

			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestKillSessionIfPresentPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewTmuxCommand("tmux").KillSessionIfPresentContext(ctx, "workspace")

	require.ErrorIs(t, err, context.Canceled)
}
