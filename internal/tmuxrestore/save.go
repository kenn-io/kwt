package tmuxrestore

import (
	"context"
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
	Now        func() time.Time
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
		state, _, err := deps.Tmux.Probe(ctx)
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
		}
		reconcile(ctx, &snap, previous, deps.Processes, now())
		snap.SavedAt = now().UnixMilli()
		f.Live = &snap
		return nil
	})
	return result, err
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
