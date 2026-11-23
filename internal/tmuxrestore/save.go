package tmuxrestore

import (
	"context"
	"fmt"
	"time"
)

// goneGrace is how long a save waits, after first finding an agent process
// gone, before it drops the conversation. A shutdown kills the tmux server
// within this window, so a conversation interrupted by a reboot survives.
const goneGrace = 10 * time.Second

// SaveDeps are the inputs to Save.
type SaveDeps struct {
	Tmux       Tmux
	Store      Store
	Processes  ProcessTable
	Protection Protection
	// ClaudeDir is Claude Code's directory, where a save reads which
	// conversation a Claude process without a record runs. Empty skips that.
	ClaudeDir string
	Now       func() time.Time
	// MinInterval, when set, makes a save leave alone a snapshot of the
	// running server that is younger than this. A lost or replaced server is
	// still noticed.
	MinInterval time.Duration
}

// SaveResult reports what a save found and did.
type SaveResult struct {
	Server   ServerState
	Promoted bool
}

// Save records the socket's current server. When the server that produced
// the last snapshot is gone, its sessions become restorable. A probe that
// fails leaves every file as it was. The server is read under the socket's
// lock, so saves that overlap publish in the order they read and none can
// replace a newer view with an older one.
func Save(ctx context.Context, deps SaveDeps) (SaveResult, error) {
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	protected := loadProtection(ctx, deps.Protection)
	var result SaveResult
	err := deps.Store.WithLock(ctx, deps.Tmux.Socket, func(f *Files) error {
		state, server, err := deps.Tmux.Probe(ctx)
		result.Server = state
		if err != nil {
			return err
		}
		if state == ServerAbsent {
			if f.Live != nil {
				f.Promote()
				result.Promoted = true
			}
			f.Release(ServerID{})
			return nil
		}
		if protected.failed {
			// A snapshot taken now would have to leave out every workspace
			// session and conversation; the last good one is worth more.
			return fmt.Errorf("snapshot left as it was: %w", protected.err)
		}
		if f.Live != nil && f.Live.Server == server {
			if age := now().UnixMilli() - f.Live.SavedAt; age >= 0 && age < deps.MinInterval.Milliseconds() {
				return nil
			}
		}

		readAt := now().UnixMilli()
		snap, err := ReadSnapshot(ctx, deps.Tmux)
		if err != nil {
			result.Server = ServerUnknown
			return err
		}
		protected.apply(ctx, &snap)
		if f.Live != nil && f.Live.Server != snap.Server {
			f.Promote()
			result.Promoted = true
		}
		f.Release(snap.Server)
		f.Settle(&snap, readAt)
		var previous map[string]int64
		if f.Live != nil {
			previous = f.Live.Gone
			if f.Live.Server == snap.Server {
				carryDiscovered(&snap, f.Live)
			}
		}
		discover(ctx, &snap, deps.Processes, deps.ClaudeDir, now())
		reconcile(ctx, &snap, previous, deps.Processes, now())
		snap.SavedAt = now().UnixMilli()
		f.Live = &snap
		return nil
	})
	return result, err
}

// carryDiscovered keeps the conversations earlier saves read from running
// agents. No pane option holds them, so without this an agent that exits
// while tmux shuts down would lose its conversation at the next save;
// reconcile drops it only after the same grace a hook record gets.
func carryDiscovered(snap *Snapshot, earlier *Snapshot) {
	for key, window := range snap.Windows {
		old, ok := earlier.Windows[key]
		if !ok {
			continue
		}
		for i := range window.Panes {
			pane := &window.Panes[i]
			if pane.Resume != nil {
				continue
			}
			for _, was := range old.Panes {
				if was.ID == pane.ID && was.Resume != nil && was.Resume.Discovered &&
					was.Resume.ConversationID != pane.Closed {
					pane.Resume = was.Resume
				}
			}
		}
	}
}

// reconcile drops conversations whose agent exited while tmux kept running.
// The first save that finds the process gone only notes the time; a later
// save at least goneGrace after that drops the conversation.
func reconcile(ctx context.Context, snap *Snapshot, previous map[string]int64, table ProcessTable, now time.Time) {
	snap.Gone = map[string]int64{}
	for key, window := range snap.Windows {
		for i := range window.Panes {
			pane := &window.Panes[i]
			if pane.Resume == nil || table == nil {
				continue
			}
			goneKey := pane.ID + "\x00" + pane.Resume.ConversationID
			if CheckLiveness(ctx, table, pane.Resume.Process) != LivenessGone {
				continue
			}
			since, seen := previous[goneKey]
			if !seen {
				since = now.Unix()
			}
			snap.Gone[goneKey] = since
			if now.Unix()-since >= int64(goneGrace/time.Second) {
				pane.Resume = nil
			}
		}
		snap.Windows[key] = window
	}
	if len(snap.Gone) == 0 {
		snap.Gone = nil
	}
}
