package tmuxrestore

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kwt/internal/agentstate"
)

// protectionBudget bounds the pull-request store read during a hook, so a
// long import holding that store's lock cannot use up the hook's time.
const protectionBudget = 300 * time.Millisecond

// hookSaveInterval is how old the snapshot must be before a hook that
// changed neither its pane's record nor the pane's saved activity saves
// again. Agents fire a hook around every tool call, and each save reads the
// whole server under the socket's lock.
const hookSaveInterval = 5 * time.Second

// HookTracker keeps a pane's resume record current from agent hook events
// and saves the socket's snapshot afterwards.
type HookTracker struct {
	Tmux       Tmux
	Pane       string
	Store      Store
	Processes  ProcessTable
	Protection Protection
	ClaudeDir  string
	Now        func() time.Time
	// Deadline is when the hook must be done. Zero means no limit.
	Deadline time.Time
	// StartPID is where the search for the agent process begins; zero means
	// this process's parent.
	StartPID int32
}

// Track applies ev to the pane's record, then saves. It never fails: a hook
// must not interrupt the agent, and a missed save only leaves an older
// snapshot in place.
func (h HookTracker) Track(ctx context.Context, ev Event) {
	if !strings.HasPrefix(h.Pane, "%") || ev.ConversationID == "" {
		return
	}
	if !h.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, h.Deadline)
		defer cancel()
	}
	changed, activity, err := h.updateRecord(ctx, ev)
	if err != nil {
		return
	}
	var interval time.Duration
	if !changed {
		interval = hookSaveInterval
	}
	protection := h.Protection
	if protection != nil {
		protection = func(ctx context.Context) ([]ProtectedWorkspace, error) {
			ctx, cancel := context.WithTimeout(ctx, protectionBudget)
			defer cancel()
			return h.Protection(ctx)
		}
	}
	_, _ = Save(ctx, SaveDeps{
		Tmux: h.Tmux, Store: h.Store, Processes: h.Processes,
		Protection: protection, ClaudeDir: h.ClaudeDir, Now: h.Now, MinInterval: interval,
		Pane: h.Pane, Activity: activity,
	})
}

// updateRecord applies ev to the pane's record and reports whether anything
// but the record's time changed, and the activity the pane's hook recorded.
func (h HookTracker) updateRecord(ctx context.Context, ev Event) (bool, *Activity, error) {
	output, err := h.Tmux.Run(ctx, "display-message", "-p", "-t", h.Pane,
		"#{pane_pid}\t#{"+agentstate.Option+"}\t#{"+ResumeOption+"}")
	if err != nil {
		return false, nil, err
	}
	fields := strings.SplitN(strings.TrimSuffix(output, "\n"), "\t", 3)
	for len(fields) < 3 {
		fields = append(fields, "")
	}
	panePIDText, activity, value := fields[0], parseActivity(fields[1]), fields[2]
	var prev *Record
	if value != "" {
		prev, _ = DecodeRecord(value)
	}
	panePID, _ := strconv.ParseInt(panePIDText, 10, 32)
	start := h.StartPID
	if start == 0 {
		start = int32(os.Getppid())
	}
	identify := func() (*ProcessRef, []string) {
		if h.Processes == nil {
			return nil, nil
		}
		return IdentifyAgent(ctx, h.Processes, start, int32(panePID), ev.Agent)
	}
	if ev.Now.IsZero() {
		ev.Now = time.Now()
		if h.Now != nil {
			ev.Now = h.Now()
		}
	}
	next, clear := Apply(prev, ev, identify)
	if clear {
		ref, _ := identify()
		if _, err := h.Tmux.Run(ctx, "set-option", "-p", "-t", h.Pane, ClosedOption, closedMarker(ev.ConversationID, ref)); err != nil {
			return false, activity, err
		}
		_, err = h.Tmux.Run(ctx, "set-option", "-p", "-u", "-t", h.Pane, ResumeOption)
		return true, activity, err
	}
	if next == nil {
		return false, activity, nil
	}
	encoded, err := next.Encode()
	if err != nil {
		return false, activity, err
	}
	if encoded == value {
		return false, activity, nil
	}
	changed := true
	if prev != nil {
		was := *prev
		was.RecordedAt = next.RecordedAt
		changed = !reflect.DeepEqual(was, *next)
	}
	_, err = h.Tmux.Run(ctx, "set-option", "-p", "-t", h.Pane, ResumeOption, encoded)
	return changed, activity, err
}
