package tmuxrestore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwttmux "go.kenn.io/kwt/internal/tmux"
)

// restoreFixture is one private tmux socket with an isolated home. Panes run
// /bin/sh, and a fake kwt records each handoff line a pane runs.
type restoreFixture struct {
	tm     Tmux
	store  Store
	home   string
	dir    string
	kwt    string
	logDir string
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := &restoreFixture{tm: privateSocket(t), store: OpenStore(t.TempDir()), home: t.TempDir(), dir: t.TempDir()}
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.home, ".config"))
	t.Setenv("SHELL", "/bin/sh")
	f.logDir = t.TempDir()
	t.Setenv("FAKE_KWT_LOG", f.logDir)
	f.kwt = filepath.Join(t.TempDir(), "kwt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_KWT_LOG/$(printf %s \"$TMUX_PANE\" | tr -d %)\"\n"
	require.NoError(t, os.WriteFile(f.kwt, []byte(script), 0o700))
	return f
}

func (f *restoreFixture) save(t *testing.T) SaveResult {
	t.Helper()
	result, err := Save(t.Context(), SaveDeps{Tmux: f.tm, Store: f.store, Protection: noProtection})
	require.NoError(t, err)
	return result
}

// lose saves the server's sessions, then kills the server so they become
// restorable.
func (f *restoreFixture) lose(t *testing.T) {
	t.Helper()
	f.save(t)
	mustRun(t, f.tm, "kill-server")
	require.True(t, f.save(t).Promoted)
}

func (f *restoreFixture) deps() RestoreDeps {
	return RestoreDeps{
		Store:      f.store,
		Executable: f.tm.Executable,
		Kwt:        f.kwt,
		Agents:     map[string]string{"claude": "claude", "codex": "codex", "roborev": "touch roborev-started"},
		Home:       f.home,
		Getenv:     func(string) string { return "" },
		Protection: noProtection,
		ClaimWait:  100 * time.Millisecond,
	}
}

func (f *restoreFixture) entries(t *testing.T) []Entry {
	t.Helper()
	var entries []Entry
	require.NoError(t, f.store.WithLock(t.Context(), f.tm.Socket, func(files *Files) error {
		entries = files.Restorable.Entries
		return nil
	}))
	return entries
}

func (f *restoreFixture) entryNamed(t *testing.T, name string) Entry {
	t.Helper()
	for _, entry := range f.entries(t) {
		if entry.Session.Name == name {
			return entry
		}
	}
	require.FailNow(t, "no entry", name)
	return Entry{}
}

func (f *restoreFixture) restore(t *testing.T, deps RestoreDeps, ids ...string) []EntryReport {
	t.Helper()
	reports, err := Restore(t.Context(), deps, ids, len(ids) == 0)
	require.NoError(t, err)
	return reports
}

// handoffs returns the lines the fake kwt recorded in a pane.
func (f *restoreFixture) handoffs(t *testing.T, paneID string) []string {
	t.Helper()
	path := filepath.Join(f.logDir, strings.TrimPrefix(paneID, "%"))
	var data []byte
	require.Eventually(t, func() bool {
		var err error
		data, err = os.ReadFile(path)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "pane %s never ran the handoff", paneID)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func claudeRecord(conversation string, argv ...string) Record {
	return Record{Agent: "claude", ConversationID: conversation, Argv: argv, RecordedAt: 1}
}

func TestRestoreRebuildsTheSessionAndHandsOffLaunches(t *testing.T) {
	f := newRestoreFixture(t)
	startServer(t, f.tm, "-s", "work", "-c", f.dir, "-x", "120", "-y", "30")
	mustRun(t, f.tm, "split-window", "-h", "-t", "work", "-c", f.dir)
	setResume(t, f.tm, "work.0", claudeRecord("c-1", "claude", "--model", "opus", "--frobnicate=secret", "hi"))
	mustRun(t, f.tm, "set-option", "-p", "-t", "work.1", kwttmux.PaneAgentOption, "roborev")
	f.lose(t)
	id := f.entryNamed(t, "work").ID

	reports := f.restore(t, f.deps(), id)

	require.Len(t, reports, 1)
	report := reports[0]
	assert.Equal(t, OutcomePartial, report.Outcome, "the fake kwt never claims its launch")
	require.NotNil(t, report.Live)
	assert.Equal(t, "work", report.Live.Session)
	assert.NotEmpty(t, report.Live.SessionID)
	assert.NotEmpty(t, report.Live.Created)
	require.Len(t, report.Panes, 2)
	assert.Equal(t, PaneReport{Window: 0, Pane: 0, Agent: "claude", Status: LaunchWaiting, Dropped: []string{"--frobnicate"}}, report.Panes[0])
	assert.Equal(t, PaneReport{Window: 0, Pane: 1, Agent: "roborev", Status: LaunchStarted}, report.Panes[1])

	panes := strings.Fields(mustRun(t, f.tm, "list-panes", "-t", "work", "-F", "#{pane_id}"))
	require.Len(t, panes, 2)
	assert.Equal(t, []string{"session exec " + id + " 0"}, f.handoffs(t, panes[0]))
	assert.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(f.dir, "roborev-started"))
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the preset pane runs its configured command")

	entry := f.entryNamed(t, "work")
	assert.Equal(t, StateLaunching, entry.State, "a pane is still waiting to claim its launch")
	assert.Equal(t, report.Live.SessionID, entry.LiveSessionID)
}

func TestRestoreHandsOffWithItsOwnKwtHome(t *testing.T) {
	// The server's panes need not have the KWT_HOME restore runs with, and
	// the handoff must read the restorable file restore wrote.
	f := newRestoreFixture(t)
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$KWT_HOME\" \"$*\" >> \"$FAKE_KWT_LOG/$(printf %s \"$TMUX_PANE\" | tr -d %)\"\n"
	require.NoError(t, os.WriteFile(f.kwt, []byte(script), 0o700))
	startServer(t, f.tm, "-s", "work", "-c", f.dir)
	setResume(t, f.tm, "work.0", claudeRecord("c-1", "claude"))
	f.lose(t)
	id := f.entryNamed(t, "work").ID
	deps := f.deps()
	deps.Getenv = func(name string) string {
		if name == "KWT_HOME" {
			return "custom"
		}
		return ""
	}

	f.restore(t, deps, id)

	pane := strings.TrimSpace(mustRun(t, f.tm, "list-panes", "-t", "work", "-F", "#{pane_id}"))
	assert.Equal(t, []string{filepath.Dir(f.store.Dir) + " session exec " + id + " 0"}, f.handoffs(t, pane))
}

func TestRestoreSkipsANameInUse(t *testing.T) {
	f := newRestoreFixture(t)
	startServer(t, f.tm, "-s", "work", "-c", f.dir)
	f.lose(t)
	startServer(t, f.tm, "-s", "work")

	reports := f.restore(t, f.deps())

	require.Len(t, reports, 1)
	assert.Equal(t, OutcomeSkipped, reports[0].Outcome)
	assert.Equal(t, "name in use", reports[0].Reason)
	assert.Equal(t, StatePending, f.entryNamed(t, "work").State)
}

func TestRestoreRetriesAnInterruptedCreate(t *testing.T) {
	for _, leftover := range []bool{true, false} {
		name := "nothing left over"
		if leftover {
			name = "a partly built session left over"
		}
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t)
			startServer(t, f.tm, "-s", "work", "-c", f.dir)
			f.lose(t)
			entry := f.entryNamed(t, "work")
			require.NoError(t, f.store.WithLock(t.Context(), f.tm.Socket, func(files *Files) error {
				files.Entry(entry.ID).State = StateCreating
				files.Entry(entry.ID).Token = "interrupted"
				return nil
			}))
			stale := ""
			if leftover {
				startServer(t, f.tm, "-s", "keep")
				created, err := Builder{Tmux: f.tm, Token: "interrupted", Home: f.home}.CreateSession(
					t.Context(), entry.Session, Window{Panes: []Pane{{Dir: f.dir}}})
				require.NoError(t, err)
				stale = created.ID
			}

			reports := f.restore(t, f.deps(), entry.ID)

			require.Len(t, reports, 1)
			assert.Equal(t, OutcomeRestored, reports[0].Outcome)
			require.NotNil(t, reports[0].Live)
			ids := strings.Fields(mustRun(t, f.tm, "list-sessions", "-F", "#{session_id}"))
			assert.Contains(t, ids, reports[0].Live.SessionID)
			if leftover {
				assert.NotContains(t, ids, stale, "the leftover is removed")
				assert.Len(t, ids, 2)
			}
			assert.Empty(t, f.entries(t), "nothing was left to launch")
		})
	}
}

func TestRestoreRetriesALaunch(t *testing.T) {
	f := newRestoreFixture(t)
	startServer(t, f.tm, "-s", "work", "-c", f.dir)
	setResume(t, f.tm, "work.0", claudeRecord("c-1", "claude"))
	f.lose(t)
	id := f.entryNamed(t, "work").ID
	first := f.restore(t, f.deps(), id)[0]
	require.NotNil(t, first.Live)
	pane := strings.TrimSpace(mustRun(t, f.tm, "list-panes", "-t", "work", "-F", "#{pane_id}"))
	f.handoffs(t, pane)

	again := f.restore(t, f.deps(), id)[0]
	assert.Equal(t, OutcomePartial, again.Outcome)
	assert.Equal(t, "already restored", again.Reason)
	assert.Equal(t, first.Live, again.Live)
	assert.Len(t, f.handoffs(t, pane), 1, "nothing is typed twice")

	mustRun(t, f.tm, "kill-session", "-t", first.Live.SessionID)
	third := f.restore(t, f.deps(), id)[0]
	require.NotNil(t, third.Live)
	assert.NotEqual(t, *first.Live, *third.Live, "a lost restore starts over")
}

func TestRestoreBuildsASharedWindowOnceAndStartsItsProgramsOnce(t *testing.T) {
	cases := []struct {
		name    string
		restore func(t *testing.T, f *restoreFixture, a, b string) []EntryReport
		// linked reports whether a's window list includes the shared window.
		linked bool
	}{
		{"b first, then a", func(t *testing.T, f *restoreFixture, a, b string) []EntryReport {
			return append(f.restore(t, f.deps(), b), f.restore(t, f.deps(), a)...)
		}, true},
		{"a dismissed", func(t *testing.T, f *restoreFixture, a, b string) []EntryReport {
			require.NoError(t, f.store.WithLock(t.Context(), f.tm.Socket, func(files *Files) error {
				assert.Empty(t, files.Dismiss([]string{a}))
				return nil
			}))
			return f.restore(t, f.deps(), b)
		}, false},
		{"a name in use", func(t *testing.T, f *restoreFixture, a, b string) []EntryReport {
			startServer(t, f.tm, "-s", "a")
			return f.restore(t, f.deps(), a, b)
		}, false},
		{"restore all", func(t *testing.T, f *restoreFixture, a, b string) []EntryReport {
			return f.restore(t, f.deps())
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			startServer(t, f.tm, "-s", "a", "-n", "own-a", "-c", f.dir)
			mustRun(t, f.tm, "new-window", "-t", "a:1", "-n", "shared", "-c", f.dir)
			setResume(t, f.tm, "a:shared.0", claudeRecord("c-shared", "claude"))
			mustRun(t, f.tm, "new-session", "-d", "-s", "b", "-n", "own-b", "-c", f.dir)
			mustRun(t, f.tm, "link-window", "-s", "a:shared", "-t", "b:4")
			f.lose(t)
			a, b := f.entryNamed(t, "a").ID, f.entryNamed(t, "b").ID

			reports := tc.restore(t, f, a, b)

			starts := 0
			for _, report := range reports {
				for _, pane := range report.Panes {
					if pane.Agent == "claude" {
						starts++
					}
				}
			}
			assert.Equal(t, 1, starts, "the shared window's conversation resumes once")
			bWindows := mustRun(t, f.tm, "list-windows", "-t", "b", "-F", "#{window_index} #{window_name} #{window_id}")
			assert.Contains(t, bWindows, "4 shared")
			if tc.linked {
				aWindows := mustRun(t, f.tm, "list-windows", "-t", "a", "-F", "#{window_index} #{window_name} #{window_id}")
				shared := func(list string) string {
					for line := range strings.SplitSeq(strings.TrimSpace(list), "\n") {
						if fields := strings.Fields(line); fields[1] == "shared" {
							return fields[2]
						}
					}
					return ""
				}
				assert.Equal(t, shared(bWindows), shared(aWindows), "both sessions hold the same live window")
				assert.Contains(t, aWindows, "0 own-a")
			}
		})
	}
}

func TestRestoreReportsAMissingConversationAndUnsafeKwtPath(t *testing.T) {
	f := newRestoreFixture(t)
	startServer(t, f.tm, "-s", "work", "-c", f.dir)
	mustRun(t, f.tm, "split-window", "-t", "work", "-c", f.dir)
	setResume(t, f.tm, "work.0", claudeRecord("gone", "claude"))
	setResume(t, f.tm, "work.1", claudeRecord("here", "claude"))
	f.lose(t)
	require.NoError(t, os.MkdirAll(filepath.Join(f.home, ".claude", "projects", "p"), 0o700))
	touch(t, filepath.Join(f.home, ".claude", "projects", "p", "here.jsonl"))
	deps := f.deps()
	deps.Kwt = "/opt/it's/kwt"

	report := f.restore(t, deps)[0]

	require.Len(t, report.Panes, 2)
	assert.Equal(t, LaunchNone, report.Panes[0].Status)
	assert.Equal(t, "conversation not found", report.Panes[0].Note)
	assert.Equal(t, LaunchFailed, report.Panes[1].Status)
	assert.Equal(t, "unsafe kwt path", report.Panes[1].Note)
	assert.Equal(t, OutcomePartial, report.Outcome)
	assert.Empty(t, f.entries(t), "no pane is left waiting")
}

func TestRestoreReportsUnknownIDs(t *testing.T) {
	f := newRestoreFixture(t)
	reports := f.restore(t, f.deps(), "nope")
	require.Len(t, reports, 1)
	assert.Equal(t, EntryReport{ID: "nope", Outcome: OutcomeNotFound}, reports[0])
}
