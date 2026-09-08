package tmux

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListSessionsTreatsOnlyAbsentServerAsEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is unavailable on Windows")
	}
	fixture := filepath.Join(t.TempDir(), "tmux-fixture")
	require.NoError(t, os.WriteFile(fixture, []byte(`#!/bin/sh
printf '%s\n' "$KWT_TEST_TMUX_STDERR" >&2
exit "$KWT_TEST_TMUX_EXIT"
`), 0o700))
	for _, test := range []struct {
		name      string
		stderr    string
		exit      string
		wantError bool
	}{
		{name: "no server", stderr: "no server running on /tmp/tmux/socket", exit: "1"},
		{name: "missing socket", stderr: "error connecting to /tmp/tmux/socket (No such file or directory)", exit: "1"},
		{name: "permission denied", stderr: "error connecting to /tmp/tmux/socket (Permission denied)", exit: "1", wantError: true},
		{name: "unexpected error", stderr: "server rejected request", exit: "1", wantError: true},
		{name: "unexpected exit", stderr: "no server running on /tmp/tmux/socket", exit: "2", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KWT_TEST_TMUX_STDERR", test.stderr)
			t.Setenv("KWT_TEST_TMUX_EXIT", test.exit)
			command := NewTmuxCommand(fixture)
			names, namesErr := command.ListSessions()
			details, detailsErr := command.ListSessionsDetailed()
			if test.wantError {
				require.ErrorContains(t, namesErr, test.stderr)
				require.ErrorContains(t, detailsErr, test.stderr)
			} else {
				require.NoError(t, namesErr)
				require.NoError(t, detailsErr)
				require.Empty(t, names)
				require.Empty(t, details)
			}
		})
	}
}
