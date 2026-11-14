package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProbeExitError int

func (e fakeProbeExitError) Error() string { return "tmux exited" }
func (e fakeProbeExitError) ExitCode() int { return int(e) }

func TestProbeProtectedSessionWithoutTmux(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing"))
	socket := fmt.Sprintf("kwt-probe-%d", time.Now().UnixNano())
	for _, legacy := range []string{"", t.TempDir()} {
		state, err := ProbeProtectedSession(t.Context(), socket, "session", nil, legacy)
		require.NoError(t, err)
		assert.Equal(t, ProtectedSessionAbsent, state)
	}

	for _, endpoint := range []string{"canonical", "legacy"} {
		t.Run(endpoint+" endpoint", func(t *testing.T) {
			legacy := t.TempDir()
			root := legacy
			if endpoint == "canonical" {
				if runtime.GOOS == "windows" {
					t.Skip("tmux's canonical Unix socket directory is unavailable on Windows")
				}
				root = "/tmp"
			}
			dir := filepath.Join(root, fmt.Sprintf("tmux-%d", os.Getuid()))
			if err := os.Mkdir(dir, 0700); err == nil {
				t.Cleanup(func() { _ = os.Remove(dir) })
			} else {
				require.ErrorIs(t, err, os.ErrExist)
			}
			// Any existing entry is uncertain without tmux, even a stale file.
			file, err := os.CreateTemp(dir, "kwt-probe-")
			require.NoError(t, err)
			require.NoError(t, file.Close())
			t.Cleanup(func() { require.NoError(t, os.Remove(file.Name())) })
			state, err := ProbeProtectedSession(t.Context(), filepath.Base(file.Name()), "session", nil, legacy)
			assert.ErrorIs(t, err, exec.ErrNotFound)
			assert.Equal(t, ProtectedSessionIndeterminate, state)
		})
	}

	t.Run("inspection failure", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows reports a non-directory ancestor as an absent path")
		}
		legacy := filepath.Join(t.TempDir(), "not-a-directory")
		require.NoError(t, os.WriteFile(legacy, nil, 0600))
		state, err := ProbeProtectedSession(t.Context(), socket, "session", nil, legacy)
		assert.ErrorIs(t, err, exec.ErrNotFound)
		assert.Equal(t, ProtectedSessionIndeterminate, state)
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		state, err := ProbeProtectedSession(ctx, socket, "session", nil, "")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, ProtectedSessionIndeterminate, state)
	})
}

func TestClassifyProtectedSessionProbe(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		stderr  string
		err     error
		want    ProtectedSessionState
		wantErr bool
	}{
		{name: "matching session", output: "expected\n", want: ProtectedSessionLive},
		{name: "no server", stderr: "no server running on /tmp/tmux/socket\n", err: fakeProbeExitError(1), want: ProtectedSessionAbsent},
		{name: "missing socket", stderr: "error connecting to /tmp/tmux/socket (No such file or directory)\n", err: fakeProbeExitError(1), want: ProtectedSessionAbsent},
		{name: "missing session", stderr: "can't find session: expected\n", err: fakeProbeExitError(1), want: ProtectedSessionAbsent},
		{name: "tmux 2.1 missing session", stderr: "can't find session expected\n", err: fakeProbeExitError(1), want: ProtectedSessionAbsent},
		{name: "permission failure", stderr: "error connecting to /tmp/tmux/socket (Permission denied)\n", err: fakeProbeExitError(1), want: ProtectedSessionIndeterminate, wantErr: true},
		{name: "unexpected session", output: "other\n", want: ProtectedSessionIndeterminate, wantErr: true},
		{name: "multiple sessions", output: "expected\nother\n", want: ProtectedSessionIndeterminate, wantErr: true},
		{name: "empty output", want: ProtectedSessionIndeterminate, wantErr: true},
		{name: "command failure", err: fakeProbeExitError(2), want: ProtectedSessionIndeterminate, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := classifyProtectedSessionProbe("expected", test.output, test.stderr, test.err)
			assert.Equal(t, test.want, state)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestClassifyProtectedSessionProbePreservesCancellation(t *testing.T) {
	state, err := classifyProtectedSessionProbe(
		"expected",
		"",
		"no server running on /tmp/tmux/socket\n",
		errors.Join(context.Canceled, exec.ErrNotFound),
	)

	assert.Equal(t, ProtectedSessionIndeterminate, state)
	assert.ErrorIs(t, err, context.Canceled)
}
