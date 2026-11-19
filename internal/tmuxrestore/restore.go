package tmuxrestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// Restore outcomes.
const (
	OutcomeRestored = "restored"
	// OutcomePartial: the session is back, but a pane is still waiting to
	// claim its launch or its launch failed.
	OutcomePartial  = "partially restored"
	OutcomeSkipped  = "skipped"
	OutcomeFailed   = "failed"
	OutcomeNotFound = "not found"
)

const claimPoll = 50 * time.Millisecond

// RestoreDeps are the inputs to Restore.
type RestoreDeps struct {
	Store Store
	// Executable is tmux.
	Executable string
	// Kwt is the absolute kwt path typed into panes for the launch handoff.
	Kwt string
	// Agents maps agent names to their configured commands.
	Agents     map[string]string
	Home       string
	Getenv     func(string) string
	Protection Protection
	// ClaimWait is how long Restore waits for panes to claim launches
	// before it reports them as still waiting.
	ClaimWait time.Duration
	Now       func() time.Time
}

// PaneReport is what a restore did in one pane.
type PaneReport struct {
	Window  int      `json:"window"`
	Pane    int      `json:"pane"`
	Agent   string   `json:"agent,omitempty"`
	Status  string   `json:"status"`
	Note    string   `json:"note,omitempty"`
	Dropped []string `json:"dropped,omitempty"`
}

// LiveEndpoint identifies the session a restore created.
type LiveEndpoint struct {
	Socket    Socket `json:"socket"`
	Session   string `json:"session"`
	SessionID string `json:"session_id"`
	ServerPID string `json:"server_pid"`
	Created   string `json:"created"`
}

// EntryReport is what a restore did with one entry.
type EntryReport struct {
	ID      string        `json:"id"`
	Session string        `json:"session,omitempty"`
	Socket  Socket        `json:"socket,omitzero"`
	Outcome string        `json:"outcome"`
	Reason  string        `json:"reason,omitempty"`
	Live    *LiveEndpoint `json:"live,omitempty"`
	Notes   []string      `json:"notes,omitempty"`
	Panes   []PaneReport  `json:"panes,omitempty"`
}

// Restore restores the entries with ids, or every entry when all is set.
// Each socket's entries are restored under its lock, so a concurrent restore
// cannot create the same session. Programs start when the panes claim their
// launches; Restore waits up to ClaimWait for that.
func Restore(ctx context.Context, deps RestoreDeps, ids []string, all bool) ([]EntryReport, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Getenv == nil {
		deps.Getenv = os.Getenv
	}
	sockets, err := deps.Store.Sockets()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var reports []EntryReport
	var errs []error
	for _, sock := range sockets {
		r := restorer{deps: deps, tm: Tmux{Executable: deps.Executable, Socket: sock}, protected: loadProtection(ctx, deps.Protection)}
		err := deps.Store.WithLock(ctx, sock, func(f *Files) error {
			r.files = f
			var selected []string
			for _, entry := range f.Restorable.Entries {
				if all || wanted[entry.ID] {
					selected = append(selected, entry.ID)
					delete(wanted, entry.ID)
				}
			}
			for _, id := range selected {
				reports = append(reports, r.restore(ctx, id))
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", sock.Path, err))
		}
	}
	for _, id := range ids {
		if wanted[id] {
			reports = append(reports, EntryReport{ID: id, Outcome: OutcomeNotFound})
		}
	}
	if err := awaitClaims(ctx, deps, reports); err != nil {
		errs = append(errs, err)
	}
	return reports, errors.Join(errs...)
}

type restorer struct {
	deps      RestoreDeps
	tm        Tmux
	files     *Files
	protected protectedSet
}

func (r restorer) restore(ctx context.Context, id string) EntryReport {
	entry := r.files.Entry(id)
	report := EntryReport{ID: id, Session: entry.Session.Name, Socket: r.tm.Socket}
	fail := func(err error) EntryReport {
		report.Outcome, report.Reason = OutcomeFailed, err.Error()
		return report
	}
	state, server, err := r.tm.Probe(ctx)
	if state == ServerUnknown {
		return fail(err)
	}

	switch entry.State {
	case StateLaunching:
		if state == ServerRunning && server.PID == entry.LiveServerPID && r.exists(ctx, entry.LiveSessionID) {
			// Programs may already run here; starting them again would
			// open each conversation twice.
			fillLaunches(&report, *entry, r.tm.Socket)
			report.Reason = "already restored"
			return report
		}
		resetEntry(entry)
	case StateCreating:
		// No program starts before launching, so anything still carrying
		// this restore's token is an unfinished skeleton.
		if _, err := (Builder{Tmux: r.tm, Token: entry.Token}).CleanupOwned(ctx); err != nil {
			return fail(err)
		}
		resetEntry(entry)
	}

	switch {
	case entry.Session.WorkspaceIdentity != "":
		report.Outcome, report.Reason = OutcomeSkipped, "worktree session"
		return report
	case r.protected.session(entry.Session):
		report.Outcome, report.Reason = OutcomeSkipped, "pull-request workspace"
		return report
	}
	taken, err := r.nameTaken(ctx, entry.Session.Name)
	if err != nil {
		return fail(err)
	}
	if taken {
		report.Outcome, report.Reason = OutcomeSkipped, "name in use"
		return report
	}

	entry.State, entry.Token = StateCreating, newEntryID()
	if err := r.files.Commit(); err != nil {
		resetEntry(entry)
		return fail(err)
	}
	b := Builder{Tmux: r.tm, Token: entry.Token, Home: r.deps.Home}
	live, notes, err := r.build(ctx, b, entry)
	if err != nil {
		if _, cleanupErr := b.CleanupOwned(ctx); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		resetEntry(entry)
		if commitErr := r.files.Commit(); commitErr != nil {
			err = errors.Join(err, commitErr)
		}
		return fail(err)
	}
	report.Notes = notes
	entry.State = StateLaunching
	entry.LiveSessionID, entry.LiveServerPID, entry.LiveCreated = live.SessionID, live.ServerPID, live.Created
	entry.LaunchedAt = r.deps.Now().UnixMilli()
	if err := r.files.Commit(); err != nil {
		return fail(err)
	}
	r.typeLaunches(ctx, entry)
	fillLaunches(&report, *entry, r.tm.Socket)
	if !hasWaiting(report) {
		r.remove(id)
	}
	return report
}

func (r restorer) remove(id string) {
	r.files.Restorable.Entries = slices.DeleteFunc(r.files.Restorable.Entries, func(e Entry) bool { return e.ID == id })
	r.files.GarbageCollectWindows()
}

func resetEntry(e *Entry) {
	e.State, e.Token = StatePending, ""
	e.LiveSessionID, e.LiveServerPID, e.LiveCreated = "", "", ""
	e.LaunchedAt, e.Launches = 0, nil
}

func (r restorer) exists(ctx context.Context, target string) bool {
	if target == "" {
		return false
	}
	out, err := r.tm.Run(ctx, "display-message", "-p", "-t", target, "#{session_id}")
	return err == nil && strings.TrimSpace(out) != ""
}

func (r restorer) windowExists(ctx context.Context, windowID string) bool {
	if windowID == "" {
		return false
	}
	out, err := r.tm.Run(ctx, "display-message", "-p", "-t", windowID, "#{window_id}")
	return err == nil && strings.TrimSpace(out) == windowID
}

func (r restorer) nameTaken(ctx context.Context, name string) (bool, error) {
	state, _, err := r.tm.Probe(ctx)
	switch state {
	case ServerAbsent:
		return false, nil
	case ServerUnknown:
		return false, err
	}
	out, err := r.tm.Run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return false, fmt.Errorf("list sessions: %w", err)
	}
	return slices.Contains(strings.Split(strings.TrimSpace(out), "\n"), name), nil
}

// build creates the session's windows and panes and plans each pane's
// launch. A window another restore already brought back is linked, not
// rebuilt, and its programs are left to the restore that created it.
func (r restorer) build(ctx context.Context, b Builder, entry *Entry) (LiveEndpoint, []string, error) {
	refs := entry.Session.Windows
	if len(refs) == 0 {
		return LiveEndpoint{}, nil, errors.New("saved session has no windows")
	}
	windows := r.files.Restorable.Windows
	created, err := b.CreateSession(ctx, entry.Session, windows[refs[0].Key].Window)
	if err != nil {
		return LiveEndpoint{}, nil, err
	}
	out, err := r.tm.Run(ctx, "display-message", "-p", "-t", created.ID, "#{pid}\t#{session_created}")
	if err != nil {
		return LiveEndpoint{}, nil, fmt.Errorf("read restored session: %w", err)
	}
	pid, createdAt, _ := strings.Cut(strings.TrimSpace(out), "\t")
	live := LiveEndpoint{
		Socket: r.tm.Socket, Session: entry.Session.Name, SessionID: created.ID,
		ServerPID: pid, Created: createdAt,
	}

	var notes []string
	for i, ref := range refs {
		shared := windows[ref.Key]
		if shared.LiveServerPID == pid && r.windowExists(ctx, shared.LiveWindowID) {
			// The first window from CreateSession only holds the session
			// open until a real window takes its place.
			if err := b.LinkWindow(ctx, created.ID, shared.LiveWindowID, ref.Index, i == 0 && created.WindowIndex == ref.Index); err != nil {
				return live, notes, err
			}
			if i == 0 && created.WindowIndex != ref.Index {
				if _, err := r.tm.Run(ctx, "kill-window", "-t", created.WindowID); err != nil {
					return live, notes, fmt.Errorf("remove placeholder window: %w", err)
				}
			}
			continue
		}
		var built BuiltWindow
		if i == 0 {
			built, err = b.BuildFirstWindow(ctx, created, ref.Index, shared.Window)
		} else {
			built, err = b.BuildWindow(ctx, created.ID, ref.Index, shared.Window)
		}
		if err != nil {
			return live, notes, err
		}
		shared.LiveWindowID, shared.LiveServerPID, shared.CreatedBy = built.ID, pid, entry.ID
		windows[ref.Key] = shared
		if built.LayoutDiffers {
			notes = append(notes, fmt.Sprintf("window %d: layout differs", ref.Index))
		}
		entry.Launches = append(entry.Launches, r.plan(ref, shared.Window, built)...)
	}
	if err := b.FinishSession(ctx, created.ID, entry.Session); err != nil {
		return live, notes, err
	}
	return live, notes, nil
}

// plan decides what starts in each pane of a window this restore created.
func (r restorer) plan(ref WindowRef, w Window, built BuiltWindow) []PaneLaunch {
	launches := make([]PaneLaunch, 0, len(built.Panes))
	for i, bp := range built.Panes {
		pane := w.Panes[i]
		l := PaneLaunch{
			WindowKey: ref.Key, WindowIndex: ref.Index, Index: pane.Index,
			PaneID: bp.ID, Status: LaunchNone, Dir: pane.Dir,
		}
		hasProgram := pane.Resume != nil || pane.PresetAgent != ""
		switch {
		case !hasProgram:
		case bp.DirMissing:
			l.Note = "directory missing"
		case r.protected.dir(pane.Dir):
			l.Note = "pull-request directory"
		case pane.Resume != nil:
			l.Agent = pane.Resume.Agent
			if FindConversation(*pane.Resume, r.deps.Home, r.deps.Getenv) == PresenceAbsent {
				l.Note = "conversation not found"
				break
			}
			args := BuildLaunch(*pane.Resume, r.deps.Agents[pane.Resume.Agent])
			l.Status, l.Argv, l.Dropped = LaunchWaiting, args.Argv, args.Dropped
			if args.Fallback {
				l.Note = "launch settings not recorded; configured command used"
			}
		default:
			l.Agent = pane.PresetAgent
			command := strings.TrimSpace(r.deps.Agents[pane.PresetAgent])
			if command == "" {
				l.Note = "agent not configured"
				break
			}
			l.Status, l.Command = LaunchWaiting, command
		}
		launches = append(launches, l)
	}
	return launches
}

// typeLaunches types each waiting pane's line. A resumed conversation gets
// the handoff, which claims its arguments from the restorable file; a preset
// agent gets its configured command, as a preset would.
func (r restorer) typeLaunches(ctx context.Context, entry *Entry) {
	for i := range entry.Launches {
		l := &entry.Launches[i]
		if l.Status != LaunchWaiting {
			continue
		}
		line := l.Command
		if line == "" {
			if !safeKwtPath(r.deps.Kwt) {
				l.Status, l.Note = LaunchFailed, "unsafe kwt path"
				continue
			}
			line = fmt.Sprintf("'%s' session exec %s %d", r.deps.Kwt, entry.ID, i)
		}
		if _, err := r.tm.Run(ctx, "send-keys", "-t", l.PaneID, "-l", line,
			";", "send-keys", "-t", l.PaneID, "Enter"); err != nil {
			l.Status, l.Note = LaunchFailed, "could not type the launch"
			continue
		}
		if l.Command != "" {
			l.Status = LaunchStarted
		}
	}
	// A failed write leaves the panes waiting in the file; a later save
	// settles them once they close.
	_ = r.files.Commit()
}

// fillLaunches reports entry's live session and pane launches.
func fillLaunches(report *EntryReport, entry Entry, sock Socket) {
	report.Panes = nil
	for _, l := range entry.Launches {
		report.Panes = append(report.Panes, PaneReport{
			Window: l.WindowIndex, Pane: l.Index, Agent: l.Agent,
			Status: l.Status, Note: l.Note, Dropped: l.Dropped,
		})
	}
	report.Live = &LiveEndpoint{
		Socket: sock, Session: entry.Session.Name, SessionID: entry.LiveSessionID,
		ServerPID: entry.LiveServerPID, Created: entry.LiveCreated,
	}
	setOutcome(report)
}

func setOutcome(report *EntryReport) {
	report.Outcome = OutcomeRestored
	if slices.ContainsFunc(report.Panes, func(p PaneReport) bool {
		return p.Status == LaunchWaiting || p.Status == LaunchFailed
	}) {
		report.Outcome = OutcomePartial
	}
}

// awaitClaims polls until no reported pane is waiting or ClaimWait passes,
// then removes entries whose panes have all claimed their launches.
func awaitClaims(ctx context.Context, deps RestoreDeps, reports []EntryReport) error {
	deadline := time.Now().Add(deps.ClaimWait)
	for {
		waiting := false
		var errs []error
		for i := range reports {
			if !hasWaiting(reports[i]) {
				continue
			}
			if err := refresh(ctx, deps.Store, &reports[i]); err != nil {
				errs = append(errs, err)
				continue
			}
			waiting = waiting || hasWaiting(reports[i])
		}
		if !waiting || time.Now().After(deadline) {
			return errors.Join(errs...)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(claimPoll):
		}
	}
}

func hasWaiting(report EntryReport) bool {
	return slices.ContainsFunc(report.Panes, func(p PaneReport) bool { return p.Status == LaunchWaiting })
}

// refresh rereads an entry's launches and removes the entry once none is
// waiting.
func refresh(ctx context.Context, store Store, report *EntryReport) error {
	return store.WithLock(ctx, report.Socket, func(f *Files) error {
		entry := f.Entry(report.ID)
		if entry == nil {
			// A save removes an entry only when no restored pane is
			// waiting, which in the seconds after a restore means the
			// panes claimed their launches.
			for i := range report.Panes {
				if report.Panes[i].Status == LaunchWaiting {
					report.Panes[i].Status = LaunchStarted
				}
			}
			setOutcome(report)
			return nil
		}
		fillLaunches(report, *entry, report.Socket)
		if !hasWaiting(*report) {
			restorer{files: f}.remove(report.ID)
		}
		return nil
	})
}
