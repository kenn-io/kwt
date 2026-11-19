package tmuxrestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// EntryState is where a restorable entry is in its restore.
type EntryState string

const (
	StatePending   EntryState = "pending"
	StateCreating  EntryState = "creating"
	StateLaunching EntryState = "launching"
)

// Launch status values for one pane.
const (
	// LaunchWaiting: the handoff was typed and the pane has not claimed it.
	LaunchWaiting = "waiting"
	LaunchStarted = "started"
	LaunchFailed  = "failed"
	// LaunchNone: nothing was started in the pane; Note says why.
	LaunchNone = "none"
)

// PaneLaunch is what a restore did in one pane of a window it created.
type PaneLaunch struct {
	WindowKey   string `json:"window_key"`
	WindowIndex int    `json:"window_index"`
	Index       int    `json:"index"`
	PaneID      string `json:"pane_id"`
	Agent       string `json:"agent,omitempty"`
	Status      string `json:"status"`
	Note        string `json:"note,omitempty"`
	// Dropped names launch flags that were not replayed.
	Dropped []string `json:"dropped,omitempty"`
	// Argv is a resumed conversation's command, handed over by Claim.
	Argv []string `json:"argv,omitempty"`
	// Command is a preset agent's configured command, typed as presets
	// type it.
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

// Entry is a session from a server that is gone.
type Entry struct {
	ID            string     `json:"id"`
	Server        ServerID   `json:"server"`
	Session       Session    `json:"session"`
	SavedAt       int64      `json:"saved_at"`
	State         EntryState `json:"state"`
	Token         string     `json:"token,omitempty"`
	LiveSessionID string     `json:"live_session_id,omitempty"`
	LiveServerPID string     `json:"live_server_pid,omitempty"`
	LiveCreated   string     `json:"live_created,omitempty"`
	// LaunchedAt is when the entry entered launching, in Unix milliseconds.
	LaunchedAt int64        `json:"launched_at,omitempty"`
	Launches   []PaneLaunch `json:"launches,omitempty"`
}

// SharedWindow is a saved window and, once a restore has created it, where
// it lives now so other entries can link it instead of building a copy.
type SharedWindow struct {
	Window        Window `json:"window"`
	LiveWindowID  string `json:"live_window_id,omitempty"`
	LiveServerPID string `json:"live_server_pid,omitempty"`
	CreatedBy     string `json:"created_by,omitempty"`
}

// Restorable holds every entry saved from one socket's lost servers.
type Restorable struct {
	Socket  Socket                  `json:"socket"`
	Entries []Entry                 `json:"entries"`
	Windows map[string]SharedWindow `json:"windows"`
}

// Store keeps snapshot files in one directory of kwt home.
type Store struct {
	Dir string
}

// OpenStore returns the store under kwtHome.
func OpenStore(kwtHome string) Store {
	return Store{Dir: filepath.Join(kwtHome, "tmux")}
}

// Files is one socket's live snapshot and restorable entries, loaded under
// the socket's lock.
type Files struct {
	Live       *Snapshot
	Restorable Restorable
	commit     func() error
}

// Commit writes the files now, before the update ends and while the lock is
// still held. Restore uses it to record each state before the step it
// describes.
func (f *Files) Commit() error {
	if f.commit == nil {
		return errors.New("commit outside a locked update")
	}
	return f.commit()
}

const (
	liveSuffix       = ".json"
	restorableSuffix = ".restorable.json"
	lockSuffix       = ".lock"
	lockRetry        = 10 * time.Millisecond
)

// WithLock loads sock's files, runs fn, and writes back what fn changed. The
// lock is held throughout; ctx bounds the wait for it.
func (s Store) WithLock(ctx context.Context, sock Socket, fn func(*Files) error) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return fmt.Errorf("secure snapshot directory: %w", err)
	}
	base := filepath.Join(s.Dir, sock.Key())
	lock := flock.New(base + lockSuffix)
	locked, err := lock.TryLockContext(ctx, lockRetry)
	if err != nil {
		return fmt.Errorf("lock snapshot files: %w", err)
	}
	if !locked {
		return errors.New("lock snapshot files: not acquired")
	}
	defer func() { _ = lock.Unlock() }()

	files := Files{Restorable: Restorable{Socket: sock, Windows: map[string]SharedWindow{}}}
	liveBefore, err := loadJSON(base+liveSuffix, &files.Live)
	if err != nil {
		return err
	}
	restorableBefore, err := loadJSON(base+restorableSuffix, &files.Restorable)
	if err != nil {
		return err
	}
	if files.Restorable.Windows == nil {
		files.Restorable.Windows = map[string]SharedWindow{}
	}
	if files.Restorable.Socket.Path == "" {
		files.Restorable.Socket = sock
	}
	// The entries are written first: a promotion that fails part way then
	// leaves the lost server's snapshot for the next save to promote again,
	// which replaces pending entries by name rather than adding to them.
	files.commit = func() error {
		var err error
		if restorableBefore, err = writeOrRemove(base+restorableSuffix, files.Restorable,
			len(files.Restorable.Entries) == 0, restorableBefore); err != nil {
			return err
		}
		liveBefore, err = writeOrRemove(base+liveSuffix, files.Live, files.Live == nil, liveBefore)
		return err
	}
	if err := fn(&files); err != nil {
		return err
	}
	return files.commit()
}

// writeOrRemove writes value to path, or removes path when empty, and
// returns the bytes now on disk.
func writeOrRemove(path string, value any, empty bool, before []byte) ([]byte, error) {
	if empty {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return before, fmt.Errorf("remove %s: %w", filepath.Base(path), err)
		}
		return nil, nil
	}
	return writeJSON(path, value, before)
}

// loadJSON decodes path into target and returns the bytes read. A file that
// does not decode is moved aside so one bad write cannot block every save.
func loadJSON(path string, target any) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		if renameErr := os.Rename(path, path+".corrupt"); renameErr != nil {
			return nil, fmt.Errorf("move aside corrupt %s: %w", filepath.Base(path), renameErr)
		}
		return nil, nil
	}
	return data, nil
}

func writeJSON(path string, value any, before []byte) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return before, fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	if bytes.Equal(data, before) {
		return before, nil
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return before, fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return before, fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := temp.Close(); err != nil {
		return before, fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return before, fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return data, nil
}

func newEntryID() string { return strings.ToLower(rand.Text()) }

// Promote turns the live snapshot's sessions into pending entries and
// clears the live snapshot. A pending entry with the same session name is
// replaced: the newer entry is the state the user had most recently. Entries
// being restored are kept, unless their restore was onto the server now
// lost: that server's own snapshot supersedes them.
func (f *Files) Promote() {
	if f.Live == nil {
		return
	}
	live := f.Live
	f.Live = nil
	f.Restorable.Entries = slices.DeleteFunc(f.Restorable.Entries, func(e Entry) bool {
		return e.State != StatePending && e.LiveServerPID == live.Server.PID
	})
	for _, session := range live.Sessions {
		f.Restorable.Entries = slices.DeleteFunc(f.Restorable.Entries, func(e Entry) bool {
			return e.State == StatePending && e.Session.Name == session.Name
		})
		f.Restorable.Entries = append(f.Restorable.Entries, Entry{
			ID:      newEntryID(),
			Server:  live.Server,
			Session: session,
			SavedAt: live.SavedAt,
			State:   StatePending,
		})
		for _, ref := range session.Windows {
			if _, ok := f.Restorable.Windows[ref.Key]; !ok {
				f.Restorable.Windows[ref.Key] = SharedWindow{Window: live.Windows[ref.Key]}
			}
		}
	}
	f.GarbageCollectWindows()
}

// Settle finishes restores on snap's server. A waiting pane that no longer
// exists has closed; an entry with no waiting pane is done and removed.
// readAt is when the snapshot read began, in Unix milliseconds: a read that
// began before a restore entered launching cannot show its panes.
func (f *Files) Settle(snap *Snapshot, readAt int64) {
	panes := map[string]bool{}
	for _, window := range snap.Windows {
		for _, pane := range window.Panes {
			panes[pane.ID] = true
		}
	}
	f.Restorable.Entries = slices.DeleteFunc(f.Restorable.Entries, func(e Entry) bool {
		if e.State != StateLaunching || e.LiveServerPID != snap.Server.PID || readAt <= e.LaunchedAt {
			return false
		}
		return !slices.ContainsFunc(e.Launches, func(l PaneLaunch) bool {
			return l.Status == LaunchWaiting && panes[l.PaneID]
		})
	})
	f.GarbageCollectWindows()
}

// Forget removes one exact session from the live snapshot and from the
// restorable entries.
func (f *Files) Forget(server ServerID, sessionID, created string) bool {
	matches := func(s Session) bool { return s.ID == sessionID && s.Created == created }
	removed := false
	if f.Live != nil && f.Live.Server.PID == server.PID {
		before := len(f.Live.Sessions)
		f.Live.Sessions = slices.DeleteFunc(f.Live.Sessions, matches)
		removed = len(f.Live.Sessions) != before
	}
	before := len(f.Restorable.Entries)
	f.Restorable.Entries = slices.DeleteFunc(f.Restorable.Entries, func(e Entry) bool {
		restored := e.LiveServerPID == server.PID && e.LiveSessionID == sessionID && e.LiveCreated == created
		return restored || e.Server.PID == server.PID && matches(e.Session)
	})
	removed = removed || len(f.Restorable.Entries) != before
	f.GarbageCollectWindows()
	return removed
}

// Dismiss deletes entries and returns the IDs it did not find.
func (f *Files) Dismiss(ids []string) []string {
	var unknown []string
	for _, id := range ids {
		before := len(f.Restorable.Entries)
		f.Restorable.Entries = slices.DeleteFunc(f.Restorable.Entries, func(e Entry) bool { return e.ID == id })
		if len(f.Restorable.Entries) == before {
			unknown = append(unknown, id)
		}
	}
	f.GarbageCollectWindows()
	return unknown
}

// GarbageCollectWindows drops shared windows no entry references.
func (f *Files) GarbageCollectWindows() {
	used := map[string]bool{}
	for _, entry := range f.Restorable.Entries {
		for _, ref := range entry.Session.Windows {
			used[ref.Key] = true
		}
	}
	for key := range f.Restorable.Windows {
		if !used[key] {
			delete(f.Restorable.Windows, key)
		}
	}
}

// Entry returns a pointer to the entry with id.
func (f *Files) Entry(id string) *Entry {
	for i := range f.Restorable.Entries {
		if f.Restorable.Entries[i].ID == id {
			return &f.Restorable.Entries[i]
		}
	}
	return nil
}

// Sockets lists every socket that has a snapshot file.
func (s Store) Sockets() ([]Socket, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("list snapshot files: %w", err)
	}
	seen := map[string]bool{}
	var sockets []Socket
	for _, path := range paths {
		var header struct {
			Socket Socket `json:"socket"`
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &header) != nil || header.Socket.Path == "" {
			continue
		}
		if !seen[header.Socket.Path] {
			seen[header.Socket.Path] = true
			sockets = append(sockets, header.Socket)
		}
	}
	return sockets, nil
}
