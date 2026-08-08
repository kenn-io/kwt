package worktree

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileCacheRoundTripsSnapshot(t *testing.T) {
	home := t.TempDir()
	cache, diagnostic, err := NewFileCache(home)
	require.NoError(t, err)
	assert.Nil(t, diagnostic)
	want := Result{ObservedAt: time.Unix(7, 0).UTC(), Snapshot: Snapshot{
		Entries:       []Entry{{Path: "/repo", Branch: "main"}},
		LaunchEntries: []Entry{{Path: "/repo", Branch: "main"}},
	}}
	require.NoError(t, cache.Store(want))
	want.Snapshot.Entries[0].Branch = "updated"
	require.NoError(t, cache.Store(want))

	reloaded, diagnostic, err := NewFileCache(home)
	require.NoError(t, err)
	assert.Nil(t, diagnostic)
	got, ok := reloaded.Current()
	require.True(t, ok)
	assert.Equal(t, want.ObservedAt, got.ObservedAt)
	assert.Equal(t, want.Snapshot, got.Snapshot)
}

func TestFileCacheWrongVersionIsDisposable(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "cache")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "inventory-v1.json"), []byte(`{"version":99}`), 0o600))

	cache, diagnostic, err := NewFileCache(home)
	require.NoError(t, err)
	assert.NotNil(t, diagnostic)
	_, ok := cache.Current()
	assert.False(t, ok)
}
