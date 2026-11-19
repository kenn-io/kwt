package tmuxrestore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	kwttmux "go.kenn.io/kwt/internal/tmux"
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

// outcomeWorktree marks a worktree entry Restore opens through kwt after it
// releases the socket's lock. No report leaves Restore with it.
const outcomeWorktree = "worktree"

const claimPoll = 50 * time.Millisecond

// ErrWorktreeChanged reports that a saved worktree session no longer matches
// its worktree: the path holds a different checkout, kwt now names its
// session differently, or kwt would open it on another tmux server.
var ErrWorktreeChanged = errors.New("worktree changed")

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
	// Workspace opens a saved worktree session through kwt's worktree open
	// path, offering restore to it in place of the layout preset. It returns
	// ErrWorktreeChanged when the worktree no longer matches the entry. When
	// nil, Restore skips worktree sessions.
	Workspace func(ctx context.Context, entry Entry, restore kwttmux.SkeletonRestorer) error
}

func (d RestoreDeps) withDefaults() RestoreDeps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	return d
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

func failed(report EntryReport, err error) EntryReport {
	report.Outcome, report.Reason = OutcomeFailed, err.Error()
	return report
}

// Restore restores the entries with ids, or every entry when all is set.
// Each socket's entries are restored under its lock, so a concurrent restore
// cannot create the same session. Worktree sessions are opened through kwt
// after the lock is released, because kwt's open path takes it again.
// Programs start when the panes claim their launches; Restore waits up to
// ClaimWait for that.
func Restore(ctx context.Context, deps RestoreDeps, ids []string, all bool) ([]EntryReport, error) {
	deps = deps.withDefaults()
	sockets, err := deps.Store.Sockets()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	type worktree struct {
		report int
		sock   Socket
		entry  Entry
	}
	var reports []EntryReport
	var worktrees []worktree
	var errs []error
	for _, sock := range sockets {
		r := newRestorer(ctx, deps, sock)
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
				report := r.restore(ctx, id)
				if report.Outcome == outcomeWorktree {
					worktrees = append(worktrees, worktree{report: len(reports), sock: sock, entry: *f.Entry(id)})
				}
				reports = append(reports, report)
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", sock.Path, err))
		}
	}
	for _, w := range worktrees {
		reports[w.report] = openWorktree(ctx, deps, w.sock, w.entry)
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

// RestoreWorkspace restores the saved worktree session req describes when a
// pending entry matches it exactly: the same session name, worktree path, and
// generation, on the server kwt is creating it on. It reports false when none
// does, so kwt's layout preset builds the session. Programs start as the
// panes claim their launches; RestoreWorkspace does not wait for them.
func RestoreWorkspace(ctx context.Context, deps RestoreDeps, req kwttmux.SkeletonRequest) (EntryReport, bool, error) {
	sock, err := requestSocket(req)
	if err != nil {
		return EntryReport{}, false, err
	}
	saved, err := deps.Store.Sockets()
	if err != nil {
		return EntryReport{}, false, err
	}
	if !slices.ContainsFunc(saved, func(s Socket) bool { return s.Path == sock.Path }) {
		return EntryReport{}, false, nil
	}
	return restoreWorkspace(ctx, deps.withDefaults(), sock, req, "")
}

func requestSocket(req kwttmux.SkeletonRequest) (Socket, error) {
	return SocketIn(req.SocketTempDir, cmp.Or(req.SocketName, "default"))
}

// openWorktree restores a worktree entry through kwt's worktree open path,
// which offers the session to restore before building a preset.
func openWorktree(ctx context.Context, deps RestoreDeps, sock Socket, entry Entry) EntryReport {
	report := EntryReport{ID: entry.ID, Session: entry.Session.Name, Socket: sock}
	if deps.Workspace == nil {
		report.Outcome, report.Reason = OutcomeSkipped, "worktree session"
		return report
	}
	var restored *EntryReport
	err := deps.Workspace(ctx, entry, func(ctx context.Context, req kwttmux.SkeletonRequest) (bool, error) {
		target, err := requestSocket(req)
		if err != nil {
			return false, err
		}
		if target.Path != sock.Path {
			return false, ErrWorktreeChanged
		}
		got, ok, err := restoreWorkspace(ctx, deps, sock, req, entry.ID)
		if !ok {
			// Returning an error keeps kwt from building a preset session
			// that would take the saved session's name.
			return false, cmp.Or(err, ErrWorktreeChanged)
		}
		restored = &got
		return true, err
	})
	switch {
	case restored != nil:
		return *restored
	case errors.Is(err, ErrWorktreeChanged):
		report.Outcome, report.Reason = OutcomeSkipped, "worktree changed"
	case err != nil:
		return failed(report, err)
	default:
		// kwt found the session already running and only repaired it.
		report.Outcome, report.Reason = OutcomeSkipped, "name in use"
	}
	return report
}

// restoreWorkspace restores the pending entry matching req, limited to the
// entry with id when id is set, through req's create path.
func restoreWorkspace(ctx context.Context, deps RestoreDeps, sock Socket, req kwttmux.SkeletonRequest, id string) (EntryReport, bool, error) {
	r := newRestorer(ctx, deps, sock)
	identity := kwttmux.WorkspacePathIdentity(req.WorkspacePath)
	var report EntryReport
	found := false
	err := deps.Store.WithLock(ctx, sock, func(f *Files) error {
		r.files = f
		i := slices.IndexFunc(f.Restorable.Entries, func(e Entry) bool {
			return (id == "" || e.ID == id) && e.Session.Name == req.Session &&
				e.Session.WorkspaceIdentity == identity && e.Session.WorkspaceGeneration == req.Generation
		})
		if i < 0 {
			return nil
		}
		entry := &f.Restorable.Entries[i]
		report = EntryReport{ID: entry.ID, Session: entry.Session.Name, Socket: sock}
		if !r.prepare(ctx, entry, &report) {
			return nil
		}
		found = true
		report = r.create(ctx, entry, report, func(b Builder) (LiveEndpoint, []string, error) {
			var live LiveEndpoint
			var notes []string
			err := req.Create(ctx, b.Token, func(ctx context.Context, firstPane string) error {
				created, err := b.Adopt(ctx, firstPane, r.firstWindow(entry))
				if err != nil {
					return err
				}
				live, notes, err = r.buildWindows(ctx, b, entry, created)
				return err
			})
			return live, notes, err
		})
		return nil
	})
	if err != nil {
		return report, found, err
	}
	if found && report.Outcome == OutcomeFailed {
		return report, true, fmt.Errorf("restore saved session %q: %s; `kwt session dismiss %s` opens it without the saved layout",
			report.Session, report.Reason, report.ID)
	}
	return report, found, nil
}

type restorer struct {
	deps      RestoreDeps
	tm        Tmux
	files     *Files
	protected protectedSet
}

func newRestorer(ctx context.Context, deps RestoreDeps, sock Socket) restorer {
	return restorer{
		deps:      deps,
		tm:        Tmux{Executable: deps.Executable, Socket: sock},
		protected: loadProtection(ctx, deps.Protection),
	}
}

func (r restorer) restore(ctx context.Context, id string) EntryReport {
	entry := r.files.Entry(id)
	report := EntryReport{ID: id, Session: entry.Session.Name, Socket: r.tm.Socket}
	if !r.prepare(ctx, entry, &report) {
		return report
	}
	taken, err := r.nameTaken(ctx, entry.Session.Name)
	if err != nil {
		return failed(report, err)
	}
	if taken {
		report.Outcome, report.Reason = OutcomeSkipped, "name in use"
		return report
	}
	if entry.Session.WorkspaceIdentity != "" {
		report.Outcome = outcomeWorktree
		return report
	}
	return r.create(ctx, entry, report, func(b Builder) (LiveEndpoint, []string, error) {
		created, err := b.CreateSession(ctx, entry.Session, r.firstWindow(entry))
		if err != nil {
			return LiveEndpoint{}, nil, err
		}
		return r.buildWindows(ctx, b, entry, created)
	})
}

// prepare settles what an earlier restore of entry left behind and checks
// that entry may be built now. It reports false, with report filled in, when
// entry must not be built.
func (r restorer) prepare(ctx context.Context, entry *Entry, report *EntryReport) bool {
	state, server, err := r.tm.Probe(ctx)
	if state == ServerUnknown {
		*report = failed(*report, err)
		return false
	}
	switch entry.State {
	case StateLaunching:
		if state == ServerRunning && server.PID == entry.LiveServerPID && r.exists(ctx, entry.LiveSessionID) {
			// Programs may already run here; starting them again would
			// open each conversation twice.
			fillLaunches(report, *entry, r.tm.Socket)
			report.Reason = "already restored"
			return false
		}
		resetEntry(entry)
	case StateCreating:
		// No program starts before launching, so anything still carrying
		// this restore's token is an unfinished skeleton.
		if _, err := (Builder{Tmux: r.tm, Token: entry.Token}).CleanupOwned(ctx); err != nil {
			*report = failed(*report, err)
			return false
		}
		resetEntry(entry)
	}
	switch {
	case len(entry.Session.Windows) == 0:
		*report = failed(*report, errors.New("saved session has no windows"))
		return false
	case r.protected.session(entry.Session):
		report.Outcome, report.Reason = OutcomeSkipped, "pull-request workspace"
		return false
	}
	return true
}

// create marks entry as creating under a new token, builds it, and hands
// off its launches. A failed build removes whatever carries the token and
// returns entry to pending.
func (r restorer) create(ctx context.Context, entry *Entry, report EntryReport, build func(Builder) (LiveEndpoint, []string, error)) EntryReport {
	entry.State, entry.Token = StateCreating, newEntryID()
	if err := r.files.Commit(); err != nil {
		resetEntry(entry)
		return failed(report, err)
	}
	b := Builder{Tmux: r.tm, Token: entry.Token, Home: r.deps.Home}
	live, notes, err := build(b)
	if err != nil {
		if _, cleanupErr := b.CleanupOwned(ctx); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		resetEntry(entry)
		if commitErr := r.files.Commit(); commitErr != nil {
			err = errors.Join(err, commitErr)
		}
		return failed(report, err)
	}
	report.Notes = notes
	entry.State = StateLaunching
	entry.LiveSessionID, entry.LiveServerPID, entry.LiveCreated = live.SessionID, live.ServerPID, live.Created
	entry.LaunchedAt = r.deps.Now().UnixMilli()
	if err := r.files.Commit(); err != nil {
		return failed(report, err)
	}
	r.typeLaunches(ctx, entry)
	fillLaunches(&report, *entry, r.tm.Socket)
	if !hasWaiting(report) {
		r.remove(entry.ID)
	}
	return report
}

func (r restorer) firstWindow(entry *Entry) Window {
	return r.files.Restorable.Windows[entry.Session.Windows[0].Key].Window
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

// buildWindows gives a new session its saved windows and panes and plans
// each pane's launch. A window another restore already brought back is
// linked, not rebuilt, and its programs are left to the restore that created
// it.
func (r restorer) buildWindows(ctx context.Context, b Builder, entry *Entry, created CreatedSession) (LiveEndpoint, []string, error) {
	out, err := r.tm.Run(ctx, "display-message", "-p", "-t", created.ID, "#{pid}\t#{session_created}")
	if err != nil {
		return LiveEndpoint{}, nil, fmt.Errorf("read restored session: %w", err)
	}
	pid, createdAt, _ := strings.Cut(strings.TrimSpace(out), "\t")
	live := LiveEndpoint{
		Socket: r.tm.Socket, Session: entry.Session.Name, SessionID: created.ID,
		ServerPID: pid, Created: createdAt,
	}

	windows := r.files.Restorable.Windows
	var notes []string
	for i, ref := range entry.Session.Windows {
		shared := windows[ref.Key]
		if shared.LiveServerPID == pid && r.windowExists(ctx, shared.LiveWindowID) {
			// The session's first window only holds the session open until
			// a real window takes its place.
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
