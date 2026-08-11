package tmux

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProbeExitError int

func (e fakeProbeExitError) Error() string { return "tmux exited" }
func (e fakeProbeExitError) ExitCode() int { return int(e) }

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
		errors.Join(context.Canceled, fakeProbeExitError(1)),
	)

	assert.Equal(t, ProtectedSessionIndeterminate, state)
	assert.ErrorIs(t, err, context.Canceled)
}
