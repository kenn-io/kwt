//go:build !windows

package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunOutputCancellationTerminatesDescendantProcessGroup(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	script := fmt.Sprintf(
		`(exec >/dev/null 2>&1; sleep 30) & child=$!; printf '%%s' "$child" > %s; wait`,
		shellQuote(pidPath),
	)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, _, err := runOutput(ctx, []string{"/bin/sh", "-c", script}, os.Environ(), nil)
		result <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		encoded, err := os.ReadFile(pidPath)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(encoded)))
			require.NoError(t, err)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NotZero(t, pid, "descendant did not start")
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	cancel()
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("resolver process did not stop after cancellation")
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("resolver descendant survived cancellation")
}

func TestAccountLoginShellHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := accountLoginShell(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
