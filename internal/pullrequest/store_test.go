package pullrequest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileStorePersistsProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-requests.json")
	store := NewFileStore(path)
	record := Provenance{
		PullRequestID: "github:github.com/acme/widget#1",
		Repository:    "github.com/acme/widget",
		RepositoryAliases: []string{
			"github.com/legacy/widget",
			"github.com/acme/widget",
		},
		Number: 1,
	}

	require.NoError(t, store.Update(context.Background(), func(records map[string]Provenance) error {
		records[record.PullRequestID] = record
		return nil
	}))

	reopened := NewFileStore(path)
	require.NoError(t, reopened.View(context.Background(), func(records map[string]Provenance) error {
		assert.Equal(t, record, records[record.PullRequestID])
		return nil
	}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestFileStoreSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-requests.json")
	stores := []*FileStore{NewFileStore(path), NewFileStore(path)}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := stores[i%len(stores)]
			require.NoError(t, store.Update(context.Background(), func(records map[string]Provenance) error {
				key := "pr-" + strconv.Itoa(i)
				records[key] = Provenance{PullRequestID: key, Number: i}
				return nil
			}))
		}(i)
	}
	wg.Wait()

	require.NoError(t, NewFileStore(path).View(context.Background(), func(records map[string]Provenance) error {
		assert.Len(t, records, 20)
		return nil
	}))
}

func TestFileStoreRejectsMalformedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-requests.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0600))

	err := NewFileStore(path).View(context.Background(), func(map[string]Provenance) error { return nil })

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

func TestFileStoreDoesNotRewriteUnchangedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-requests.json")
	store := NewFileStore(path)
	require.NoError(t, store.Update(context.Background(), func(records map[string]Provenance) error {
		records["pr-1"] = Provenance{PullRequestID: "pr-1", Number: 1}
		return nil
	}))
	before, err := os.Stat(path)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	require.NoError(t, store.Update(context.Background(), func(map[string]Provenance) error { return nil }))

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime())
}

func TestFileStoreRemoveIfMatchDeletesObservedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-requests.json")
	store := NewFileStore(path)
	record := Provenance{PullRequestID: "pr-17", Number: 17, HeadSHA: "head-17"}
	require.NoError(t, store.Update(context.Background(), func(records map[string]Provenance) error {
		records[record.PullRequestID] = record
		return nil
	}))

	removed, err := store.RemoveIfMatch(context.Background(), record.PullRequestID, record)

	require.NoError(t, err)
	assert.True(t, removed)
	require.NoError(t, store.View(context.Background(), func(records map[string]Provenance) error {
		_, exists := records[record.PullRequestID]
		assert.False(t, exists)
		return nil
	}))
}

func TestFileStoreRemoveIfMatchPreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-requests.json")
	store := NewFileStore(path)
	observed := Provenance{PullRequestID: "pr-17", Number: 17, HeadSHA: "old-head"}
	replacement := observed
	replacement.HeadSHA = "replacement-head"
	require.NoError(t, store.Update(context.Background(), func(records map[string]Provenance) error {
		records[observed.PullRequestID] = replacement
		return nil
	}))

	removed, err := store.RemoveIfMatch(context.Background(), observed.PullRequestID, observed)

	require.NoError(t, err)
	assert.False(t, removed)
	require.NoError(t, store.View(context.Background(), func(records map[string]Provenance) error {
		assert.Equal(t, replacement, records[observed.PullRequestID])
		return nil
	}))
}

func TestFileStoreRemoveIfMatchPreservesReimportedWorkspaceGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-requests.json")
	store := NewFileStore(path)
	observed := Provenance{
		PullRequestID: "pr-17", Number: 17, HeadSHA: "same-head",
		Workspace: Workspace{
			Path: "/worktrees/pr-17", Branch: "pr-17",
			Generation: "0123456789abcdef0123456789abcdef",
		},
	}
	replacement := observed
	replacement.Workspace.Generation = "fedcba9876543210fedcba9876543210"
	require.NoError(t, store.Update(context.Background(), func(records map[string]Provenance) error {
		records[observed.PullRequestID] = replacement
		return nil
	}))

	removed, err := store.RemoveIfMatch(
		context.Background(), observed.PullRequestID, observed,
	)

	require.NoError(t, err)
	assert.False(t, removed)
	require.NoError(t, store.View(context.Background(), func(records map[string]Provenance) error {
		assert.Equal(t, replacement, records[observed.PullRequestID])
		return nil
	}))
}
