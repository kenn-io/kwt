//go:build !windows

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBrowseCLIRejectsNamedPipe(t *testing.T) {
	pipe := filepath.Join(t.TempDir(), "pipe")
	require.NoError(t, syscall.Mkfifo(pipe, 0600))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrowseCLIHelper$")
	cmd.Env = append(os.Environ(), "KWT_TEST_BROWSE_PATH="+pipe, "KWT_HOME="+t.TempDir())
	_, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "a non-directory must fail without waiting for a writer")
}
