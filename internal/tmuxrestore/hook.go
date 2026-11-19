package tmuxrestore

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// protectionBudget bounds the pull-request store read during a hook, so a
// long import holding that store's lock cannot use up the hook's time.
const protectionBudget = 300 * time.Millisecond

// HookTracker keeps a pane's resume record current from agent hook events
// and saves the socket's snapshot afterwards.
type HookTracker struct {
	Tmux       Tmux
	Pane       string
	Store      Store
	Processes  ProcessTable
	Protection Protection
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
	if err := h.updateRecord(ctx, ev); err != nil {
		return
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
		Protection: protection, Now: h.Now,
	})
}

func (h HookTracker) updateRecord(ctx context.Context, ev Event) error {
	output, err := h.Tmux.Run(ctx, "display-message", "-p", "-t", h.Pane,
		"#{pane_pid}\t#{"+ResumeOption+"}")
	if err != nil {
		return err
	}
	panePIDText, value, _ := strings.Cut(strings.TrimSuffix(output, "\n"), "\t")
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
		_, err = h.Tmux.Run(ctx, "set-option", "-p", "-u", "-t", h.Pane, ResumeOption)
		return err
	}
	if next == nil {
		return nil
	}
	encoded, err := next.Encode()
	if err != nil {
		return err
	}
	if encoded == value {
		return nil
	}
	_, err = h.Tmux.Run(ctx, "set-option", "-p", "-t", h.Pane, ResumeOption, encoded)
	return err
}
