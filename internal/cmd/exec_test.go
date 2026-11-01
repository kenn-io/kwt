package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExecuteInWorktreeStopsWhenCommandContextIsCanceled(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	err := executeInWorktree(canceled, t.TempDir(), []string{"git", "--version"}, false)

	require.ErrorIs(t, err, context.Canceled)
}
