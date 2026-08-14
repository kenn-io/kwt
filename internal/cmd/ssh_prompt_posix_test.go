//go:build darwin || linux

package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTerminalPasswordCancellationRestoresEcho(t *testing.T) {
	master, terminal, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = master.Close()
		_ = terminal.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, readErr := readTerminalPassword(ctx, terminal)
		result <- readErr
	}()
	require.Eventually(t, func() bool {
		enabled, stateErr := terminalEchoEnabled(terminal)
		return stateErr == nil && !enabled
	}, time.Second, time.Millisecond)
	cancel()

	select {
	case readErr := <-result:
		assert.ErrorIs(t, readErr, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("terminal password read did not stop after cancellation")
	}
	enabled, err := terminalEchoEnabled(terminal)
	require.NoError(t, err)
	assert.True(t, enabled)
}
