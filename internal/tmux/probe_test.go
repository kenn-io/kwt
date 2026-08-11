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
		err     error
		want    ProtectedSessionState
		wantErr bool
	}{
		{name: "attached", output: "expected\t1\n", want: ProtectedSessionLive},
		{name: "detached", output: "expected\t0\n", want: ProtectedSessionLive},
		{name: "no server", err: fakeProbeExitError(1), want: ProtectedSessionAbsent},
		{name: "unexpected session", output: "other\t0\n", want: ProtectedSessionIndeterminate, wantErr: true},
		{name: "multiple sessions", output: "expected\t0\nother\t0\n", want: ProtectedSessionIndeterminate, wantErr: true},
		{name: "malformed", output: "expected\n", want: ProtectedSessionIndeterminate, wantErr: true},
		{name: "command failure", err: fakeProbeExitError(2), want: ProtectedSessionIndeterminate, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := classifyProtectedSessionProbe("expected", test.output, test.err)
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
		errors.Join(context.Canceled, fakeProbeExitError(1)),
	)

	assert.Equal(t, ProtectedSessionIndeterminate, state)
	assert.ErrorIs(t, err, context.Canceled)
}
