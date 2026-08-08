package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotatingLogIsPrivateAndRetainsThreeBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	log, err := openRotatingLog(path, 16, 3)
	require.NoError(t, err)
	for value := range 6 {
		_, err = fmt.Fprintf(log, "entry-%02d\n", value)
		require.NoError(t, err)
	}
	require.NoError(t, log.Close())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.FileExists(t, path+".1")
	assert.FileExists(t, path+".2")
	assert.FileExists(t, path+".3")
	assert.NoFileExists(t, path+".4")
}

func TestRotatingLogRotatesAnOversizedExistingFileOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("already-too-large"), 0o600))
	log, err := openRotatingLog(path, 4, 3)
	require.NoError(t, err)
	require.NoError(t, log.Close())
	assert.FileExists(t, path+".1")
}

func TestRotatingLogRejectsSymlinkedActivePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.log")
	require.NoError(t, os.WriteFile(target, []byte("preserve"), 0o600))
	path := filepath.Join(dir, "daemon.log")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := openRotatingLog(path, 16, 3)
	require.Error(t, err)
	body, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "preserve", string(body))
}
