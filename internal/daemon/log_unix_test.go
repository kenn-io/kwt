//go:build unix

package daemon

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRotatingLogRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	result := make(chan error, 1)
	go func() {
		_, err := openRotatingLog(path, 16, 3)
		result <- err
	}()
	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("opening a FIFO as a daemon log blocked")
	}
}
