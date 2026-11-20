package tmuxrestore

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var storeSocket = Socket{Path: "/tmp/tmux-501/default", Name: "default"}

func liveSnapshot(server string, sessions ...string) *Snapshot {
	snap := &Snapshot{
		Socket:  storeSocket,
		Server:  ServerID{PID: server, StartTime: "1"},
		SavedAt: 50,
		Windows: map[string]Window{},
	}
	for i, name := range sessions {
		key := server + ":@" + name
		snap.Windows[key] = Window{Key: key, Name: name, Panes: []Pane{{Index: 0, Dir: "/"}}}
		snap.Sessions = append(snap.Sessions, Session{
			ID: "$" + name, Name: name, Created: "10" + string(rune('0'+i)),
			Windows: []WindowRef{{Key: key}},
		})
	}
	return snap
}

func update(t *testing.T, store Store, fn func(*Files) error) {
	t.Helper()
	require.NoError(t, store.WithLock(t.Context(), storeSocket, fn))
}

func read(t *testing.T, store Store) Files {
	t.Helper()
	var files Files
	update(t, store, func(f *Files) error { files = *f; return nil })
	return files
}

func TestPromoteMakesPendingEntries(t *testing.T) {
	store := OpenStore(t.TempDir())
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work", "ops")
		return nil
	})
	update(t, store, func(f *Files) error { f.Promote(); return nil })

	files := read(t, store)
	assert.Nil(t, files.Live)
	require.Len(t, files.Restorable.Entries, 2)
	work := files.Restorable.Entries[0]
	assert.Equal(t, "work", work.Session.Name)
	assert.Equal(t, StatePending, work.State)
	assert.Equal(t, ServerID{PID: "10", StartTime: "1"}, work.Server)
	assert.Equal(t, int64(50), work.SavedAt)
	assert.Len(t, work.ID, 26)
	assert.NotEqual(t, work.ID, files.Restorable.Entries[1].ID)
	assert.Contains(t, files.Restorable.Windows, "10:@work")
	assert.Equal(t, storeSocket, files.Restorable.Socket)
}

func TestPromoteThatCannotSaveEntriesKeepsTheSnapshot(t *testing.T) {
	store := OpenStore(t.TempDir())
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work")
		return nil
	})
	restorable := filepath.Join(store.Dir, storeSocket.Key()+restorableSuffix)

	err := store.WithLock(t.Context(), storeSocket, func(f *Files) error {
		f.Promote()
		// A non-empty directory where the entries go makes their write fail,
		// as a full disk would.
		return os.MkdirAll(filepath.Join(restorable, "blocked"), 0o700)
	})
	require.Error(t, err)
	require.NoError(t, os.RemoveAll(restorable))

	files := read(t, store)
	require.NotNil(t, files.Live, "the next save can still promote the lost server's sessions")
	assert.Equal(t, "work", files.Live.Sessions[0].Name)
}

func TestPromoteKeepsEntriesFromEarlierServersWithTheSameName(t *testing.T) {
	store := OpenStore(t.TempDir())
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work", "ops")
		f.Promote()
		f.Restorable.Entries[1].State = StateLaunching
		f.Live = liveSnapshot("20", "work", "ops")
		f.Promote()
		return nil
	})

	files := read(t, store)
	var names []string
	var servers []string
	for _, entry := range files.Restorable.Entries {
		names = append(names, entry.Session.Name)
		servers = append(servers, entry.Server.PID)
	}
	assert.Equal(t, []string{"work", "ops", "work", "ops"}, names)
	assert.Equal(t, []string{"10", "10", "20", "20"}, servers)
	assert.Contains(t, files.Restorable.Windows, "10:@work")
	assert.Contains(t, files.Restorable.Windows, "20:@work")
}

func TestPromotingTheSameSnapshotAgainAddsNothing(t *testing.T) {
	// A promotion whose second write failed leaves the snapshot in place,
	// and the next save promotes it again.
	store := OpenStore(t.TempDir())
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work", "ops")
		f.Promote()
		return nil
	})
	before := read(t, store).Restorable.Entries
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work", "ops")
		f.Promote()
		return nil
	})

	assert.Equal(t, before, read(t, store).Restorable.Entries)
}

func TestForgetRemovesAnExactSessionWherePresent(t *testing.T) {
	store := OpenStore(t.TempDir())
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work", "ops")
		f.Promote()
		f.Live = liveSnapshot("20", "work")
		return nil
	})

	update(t, store, func(f *Files) error {
		assert.False(t, f.Forget(ServerID{PID: "10"}, "$work", "999"), "created time must match")
		assert.True(t, f.Forget(ServerID{PID: "10"}, "$work", "100"))
		assert.True(t, f.Forget(ServerID{PID: "20"}, "$work", "100"))
		return nil
	})

	files := read(t, store)
	require.NotNil(t, files.Live)
	assert.Empty(t, files.Live.Sessions)
	require.Len(t, files.Restorable.Entries, 1)
	assert.Equal(t, "ops", files.Restorable.Entries[0].Session.Name)
}

func TestDismissReportsUnknownIDs(t *testing.T) {
	store := OpenStore(t.TempDir())
	var id string
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work")
		f.Promote()
		id = f.Restorable.Entries[0].ID
		return nil
	})
	update(t, store, func(f *Files) error {
		assert.Equal(t, []string{"nope"}, f.Dismiss([]string{id, "nope"}))
		return nil
	})
	files := read(t, store)
	assert.Empty(t, files.Restorable.Entries)
	assert.Empty(t, files.Restorable.Windows)
}

func TestSharedWindowStaysWhileReferenced(t *testing.T) {
	store := OpenStore(t.TempDir())
	var ids []string
	update(t, store, func(f *Files) error {
		live := liveSnapshot("10", "a", "b")
		live.Sessions[1].Windows = append(live.Sessions[1].Windows, WindowRef{Key: "10:@a", Index: 1})
		f.Live = live
		f.Promote()
		for _, entry := range f.Restorable.Entries {
			ids = append(ids, entry.ID)
		}
		return nil
	})
	update(t, store, func(f *Files) error { f.Dismiss(ids[:1]); return nil })
	assert.Contains(t, read(t, store).Restorable.Windows, "10:@a", "b still links a's window")
}

func TestStoreFilesArePrivateAndSurviveCorruption(t *testing.T) {
	home := t.TempDir()
	store := OpenStore(home)
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work")
		f.Promote()
		f.Live = liveSnapshot("11", "next")
		return nil
	})
	live := filepath.Join(home, "tmux", storeSocket.Key()+".json")
	restorable := filepath.Join(home, "tmux", storeSocket.Key()+".restorable.json")
	for _, path := range []string{live, restorable} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Join(home, "tmux"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	require.NoError(t, os.WriteFile(restorable, []byte("{broken"), 0o600))
	files := read(t, store)
	assert.Empty(t, files.Restorable.Entries)
	require.NotNil(t, files.Live)
	_, err = os.Stat(restorable + ".corrupt")
	assert.NoError(t, err)
}

func TestStoreSerializesUpdates(t *testing.T) {
	store := OpenStore(t.TempDir())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			assert.NoError(t, store.WithLock(t.Context(), storeSocket, func(f *Files) error {
				if f.Live == nil {
					f.Live = liveSnapshot("10")
				}
				f.Live.SavedAt++
				time.Sleep(time.Millisecond)
				return nil
			}))
		})
	}
	wg.Wait()
	assert.Equal(t, int64(58), read(t, store).Live.SavedAt)
}

func TestStoreListsSockets(t *testing.T) {
	// A kwt home can have characters that mean something in a file pattern.
	store := OpenStore(filepath.Join(t.TempDir(), "kwt[dev]"))
	sockets, err := store.Sockets()
	require.NoError(t, err, "nothing is saved before the first save")
	assert.Empty(t, sockets)
	other := Socket{Path: "/tmp/tmux-501/kwt", Name: "kwt"}
	update(t, store, func(f *Files) error { f.Live = liveSnapshot("10", "w"); return nil })
	require.NoError(t, store.WithLock(t.Context(), other, func(f *Files) error {
		f.Live = liveSnapshot("11", "x")
		f.Live.Socket = other
		f.Promote()
		return nil
	}))
	sockets, err = store.Sockets()
	require.NoError(t, err)
	assert.ElementsMatch(t, []Socket{storeSocket, other}, sockets)
}

func TestForgetRemovesARestoredSession(t *testing.T) {
	var files Files
	files.Restorable.Windows = map[string]SharedWindow{}
	entry := launchingEntry(&files, "30", 100, map[string]string{"%1": LaunchWaiting})
	entry.LiveSessionID, entry.LiveCreated = "$4", "500"

	assert.False(t, files.Forget(ServerID{PID: "30"}, "$4", "499"), "another session that reused the ID")
	assert.True(t, files.Forget(ServerID{PID: "30"}, "$4", "500"))
	assert.Empty(t, files.Restorable.Entries)
}

func TestUpdatesByPathKeepTheSocketName(t *testing.T) {
	store := OpenStore(t.TempDir())
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work")
		f.Promote()
		return nil
	})
	require.NoError(t, store.WithLock(t.Context(), Socket{Path: storeSocket.Path}, func(*Files) error { return nil }))
	sockets, err := store.Sockets()
	require.NoError(t, err)
	assert.Equal(t, []Socket{storeSocket}, sockets)
}

func TestCommitWritesBeforeTheUpdateEnds(t *testing.T) {
	store := OpenStore(t.TempDir())
	update(t, store, func(f *Files) error {
		f.Live = liveSnapshot("10", "work")
		f.Promote()
		return nil
	})
	failed := store.WithLock(t.Context(), storeSocket, func(f *Files) error {
		f.Restorable.Entries[0].State = StateCreating
		f.Restorable.Entries[0].Token = "tok"
		if err := f.Commit(); err != nil {
			return err
		}
		return assert.AnError
	})
	require.ErrorIs(t, failed, assert.AnError)
	entry := read(t, store).Restorable.Entries[0]
	assert.Equal(t, StateCreating, entry.State, "a committed step survives a later failure")
	assert.Equal(t, "tok", entry.Token)
}

// launchingEntry adds an entry for "work", saved from a lost server of its
// own, that a restore onto server is starting programs for.
func launchingEntry(f *Files, server string, launchedAt int64, statuses map[string]string) *Entry {
	f.Live = liveSnapshot(strconv.Itoa(10+len(f.Restorable.Entries)), "work")
	f.Promote()
	entry := &f.Restorable.Entries[len(f.Restorable.Entries)-1]
	entry.State = StateLaunching
	entry.LiveServer = ServerID{PID: server, StartTime: "1"}
	entry.LaunchedAt = launchedAt
	for pane, status := range statuses {
		entry.Launches = append(entry.Launches, PaneLaunch{PaneID: pane, Status: status})
	}
	return entry
}

func TestSettleRemovesEntriesWhoseLaunchesFinished(t *testing.T) {
	running := func(panes ...string) *Snapshot {
		snap := liveSnapshot("30", "work")
		window := snap.Windows["30:@work"]
		window.Panes = nil
		for _, id := range panes {
			window.Panes = append(window.Panes, Pane{ID: id})
		}
		snap.Windows["30:@work"] = window
		return snap
	}
	cases := []struct {
		name     string
		server   string
		statuses map[string]string
		readAt   int64
		snap     *Snapshot
		kept     bool
	}{
		{"every launch started", "30", map[string]string{"%1": LaunchStarted, "%2": LaunchFailed}, 200, running("%1", "%2"), false},
		{"a pane still waiting", "30", map[string]string{"%1": LaunchWaiting, "%2": LaunchStarted}, 200, running("%1", "%2"), true},
		{"the waiting pane closed", "30", map[string]string{"%1": LaunchWaiting, "%2": LaunchStarted}, 200, running("%2"), false},
		{"a read from before the launch proves nothing", "30", map[string]string{"%1": LaunchWaiting}, 50, running(), true},
		{"another server's launches are left alone", "31", map[string]string{"%1": LaunchWaiting}, 200, running(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var files Files
			files.Restorable.Windows = map[string]SharedWindow{}
			launchingEntry(&files, tc.server, 100, tc.statuses)
			files.Settle(tc.snap, tc.readAt)
			assert.Equal(t, tc.kept, len(files.Restorable.Entries) == 1)
		})
	}
}

func TestPromoteReplacesALaunchingEntryRestoredOntoTheLostServer(t *testing.T) {
	var files Files
	files.Restorable.Windows = map[string]SharedWindow{}
	launchingEntry(&files, "30", 100, map[string]string{"%1": LaunchWaiting})
	other := launchingEntry(&files, "99", 100, map[string]string{"%1": LaunchWaiting})
	other.Session.Name = "ops"

	files.Live = liveSnapshot("30", "work")
	restored := &files.Restorable.Entries[0]
	restored.LiveSessionID = files.Live.Sessions[0].ID
	restored.LiveCreated = files.Live.Sessions[0].Created
	files.Promote()

	var states []string
	for _, entry := range files.Restorable.Entries {
		states = append(states, entry.Session.Name+" "+string(entry.State))
	}
	assert.Equal(t, []string{"ops launching", "work pending"}, states,
		"the restore onto server 30 is superseded by what server 30 last had")
}

func TestPromoteKeepsARestoreTheLostServerNeverSaved(t *testing.T) {
	// The server went down before any save recorded the restored session,
	// so the entry is still its only copy.
	var files Files
	files.Restorable.Windows = map[string]SharedWindow{}
	restored := launchingEntry(&files, "30", 100, map[string]string{"%1": LaunchStarted})
	restored.LiveSessionID, restored.LiveCreated = "$9", "900"

	files.Live = liveSnapshot("30", "ops")
	files.Promote()

	var states []string
	for _, entry := range files.Restorable.Entries {
		states = append(states, entry.Session.Name+" "+string(entry.State))
	}
	assert.Equal(t, []string{"work pending", "ops pending"}, states)
	assert.Empty(t, files.Restorable.Entries[0].Launches)
}

func TestReleaseReturnsRestoresOnAGoneServerToPending(t *testing.T) {
	var files Files
	files.Restorable.Windows = map[string]SharedWindow{}
	launchingEntry(&files, "30", 100, map[string]string{"%1": LaunchStarted})
	launchingEntry(&files, "31", 100, map[string]string{"%1": LaunchStarted})

	files.Release(ServerID{PID: "31", StartTime: "1"})

	require.Len(t, files.Restorable.Entries, 2)
	assert.Equal(t, StatePending, files.Restorable.Entries[0].State, "server 30 is gone")
	assert.Equal(t, StateLaunching, files.Restorable.Entries[1].State, "server 31 still runs it")
}
