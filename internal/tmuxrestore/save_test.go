package tmuxrestore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kwttmux "go.kenn.io/kwt/internal/tmux"
)

func noProtection(context.Context) ([]ProtectedWorkspace, error) { return nil, nil }

type saveFixture struct {
	tm    Tmux
	store Store
	procs fakeProcesses
	now   time.Time
	prs   Protection
}

func newSaveFixture(t *testing.T) *saveFixture {
	return &saveFixture{
		tm:    privateSocket(t),
		store: OpenStore(t.TempDir()),
		procs: fakeProcesses{},
		now:   time.Unix(1_800_000_000, 0),
		prs:   noProtection,
	}
}

func (f *saveFixture) save(t *testing.T) SaveResult {
	t.Helper()
	result, err := Save(t.Context(), SaveDeps{
		Tmux: f.tm, Store: f.store, Processes: f.procs,
		Protection: f.prs, Now: func() time.Time { return f.now },
	})
	require.NoError(t, err)
	return result
}

func (f *saveFixture) files(t *testing.T) Files {
	t.Helper()
	var files Files
	require.NoError(t, f.store.WithLock(t.Context(), f.tm.Socket, func(fs *Files) error {
		files = *fs
		return nil
	}))
	return files
}

func setResume(t *testing.T, tm Tmux, target string, record Record) {
	t.Helper()
	value, err := record.Encode()
	require.NoError(t, err)
	_, err = tm.Run(t.Context(), "set-option", "-p", "-t", target, ResumeOption, value)
	require.NoError(t, err)
}

func TestSaveFollowsTheServerLifecycle(t *testing.T) {
	f := newSaveFixture(t)

	assert.Equal(t, SaveResult{Server: ServerAbsent}, f.save(t), "nothing to save yet")

	startServer(t, f.tm, "-s", "work")
	result := f.save(t)
	assert.Equal(t, ServerRunning, result.Server)
	files := f.files(t)
	require.NotNil(t, files.Live)
	assert.Equal(t, "work", files.Live.Sessions[0].Name)
	assert.Equal(t, f.now.UnixMilli(), files.Live.SavedAt, "saved_at is in Unix milliseconds")
	assert.Empty(t, files.Restorable.Entries)

	_, err := f.tm.Run(t.Context(), "kill-server")
	require.NoError(t, err)
	result = f.save(t)
	assert.Equal(t, SaveResult{Server: ServerAbsent, Promoted: true}, result)
	files = f.files(t)
	assert.Nil(t, files.Live)
	require.Len(t, files.Restorable.Entries, 1)
	assert.Equal(t, "work", files.Restorable.Entries[0].Session.Name)

	startServer(t, f.tm, "-s", "fresh")
	f.save(t)
	files = f.files(t)
	require.NotNil(t, files.Live)
	assert.Equal(t, "fresh", files.Live.Sessions[0].Name)
	assert.Len(t, files.Restorable.Entries, 1, "the lost session stays restorable")
}

func TestSavePromotesWhenTheServerIsReplaced(t *testing.T) {
	f := newSaveFixture(t)
	startServer(t, f.tm, "-s", "work")
	f.save(t)
	_, err := f.tm.Run(t.Context(), "kill-server")
	require.NoError(t, err)
	startServer(t, f.tm, "-s", "other")

	result := f.save(t)
	assert.True(t, result.Promoted)
	files := f.files(t)
	require.Len(t, files.Restorable.Entries, 1)
	assert.Equal(t, "work", files.Restorable.Entries[0].Session.Name)
	assert.Equal(t, "other", files.Live.Sessions[0].Name)
}

func TestSaveForgetsSessionsKilledOnAServerThatStaysUp(t *testing.T) {
	f := newSaveFixture(t)
	startServer(t, f.tm, "-s", "work", ";", "set-option", "-g", "exit-empty", "off")
	f.save(t)
	_, err := f.tm.Run(t.Context(), "kill-session", "-t", "work")
	require.NoError(t, err)

	result := f.save(t)

	assert.Equal(t, ServerRunning, result.Server, "the server has no sessions but is still up")
	assert.False(t, result.Promoted)
	assert.Empty(t, f.files(t).Restorable.Entries, "a session the user killed is not offered back")
}

func TestSaveNeverPublishesAnOlderViewThanAnotherSave(t *testing.T) {
	f := newSaveFixture(t)
	startServer(t, f.tm, "-s", "work")
	// The first save's tmux pauses once it has printed the panes, until the
	// test lets it go.
	gate := t.TempDir()
	paused := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\n'" + f.tm.Executable + "' \"$@\"; status=$?\n" +
		"case \" $* \" in *\" list-panes \"*)\n" +
		"  : > '" + gate + "/read'\n" +
		"  while [ ! -e '" + gate + "/go' ]; do sleep 0.02; done;;\nesac\nexit $status\n"
	require.NoError(t, os.WriteFile(paused, []byte(script), 0o700))
	save := func(tm Tmux, done chan<- error) {
		_, err := Save(t.Context(), SaveDeps{Tmux: tm, Store: f.store, Processes: f.procs,
			Protection: noProtection, Now: func() time.Time { return f.now }})
		done <- err
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go save(Tmux{Executable: paused, Socket: f.tm.Socket}, first)
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(gate, "read"))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	// With the first save's view read, the session is renamed and a second
	// save starts. If it could publish now, the first would then overwrite
	// the rename with its older view.
	mustRun(t, f.tm, "rename-session", "-t", "work", "renamed")
	go save(f.tm, second)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(gate, "go"), nil, 0o600))
	require.NoError(t, <-first)
	require.NoError(t, <-second)

	files := f.files(t)
	require.NotNil(t, files.Live)
	assert.Equal(t, "renamed", files.Live.Sessions[0].Name)
}

func TestSaveLeavesFilesAloneWhenTheProbeFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newSaveFixture(t)
	startServer(t, f.tm, "-s", "work")
	f.save(t)
	dir := filepath.Dir(f.tm.Socket.Path)
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := Save(t.Context(), SaveDeps{
		Tmux: f.tm, Store: f.store, Processes: f.procs,
		Protection: f.prs, Now: func() time.Time { return f.now },
	})
	assert.Error(t, err)
	require.NoError(t, os.Chmod(dir, 0o700))
	files := f.files(t)
	require.NotNil(t, files.Live, "the last good snapshot stays")
	assert.Empty(t, files.Restorable.Entries)
}

func TestSaveDropsAnExitedAgentOnlyAfterTwoSaves(t *testing.T) {
	f := newSaveFixture(t)
	startServer(t, f.tm, "-s", "work")
	setResume(t, f.tm, "work", Record{
		Agent: "codex", ConversationID: "x-1",
		Process: &ProcessRef{PID: 4242, StartMillis: 1},
	})
	resumeOf := func() *Record {
		files := f.files(t)
		require.NotNil(t, files.Live)
		return files.Live.Windows[files.Live.Sessions[0].Windows[0].Key].Panes[0].Resume
	}

	f.procs[4242] = ProcInfo{PID: 4242, PPID: 1, Name: "codex", StartMillis: 1}
	f.save(t)
	require.NotNil(t, resumeOf(), "alive")

	delete(f.procs, 4242)
	f.save(t)
	assert.NotNil(t, resumeOf(), "first save after exit keeps the record")

	f.now = f.now.Add(5 * time.Second)
	f.save(t)
	assert.NotNil(t, resumeOf(), "too soon to drop")

	f.now = f.now.Add(5 * time.Second)
	f.save(t)
	assert.Nil(t, resumeOf(), "dropped ten seconds after it was first found gone")

	f.now = f.now.Add(time.Minute)
	f.save(t)
	assert.Nil(t, resumeOf(), "stays dropped while the pane still holds the record")
}

func TestSaveKeepsRecordsWithUnknownProcesses(t *testing.T) {
	f := newSaveFixture(t)
	startServer(t, f.tm, "-s", "work")
	setResume(t, f.tm, "work", Record{Agent: "codex", ConversationID: "x-1"})
	f.save(t)
	f.now = f.now.Add(time.Hour)
	f.save(t)
	files := f.files(t)
	assert.NotNil(t, files.Live.Windows[files.Live.Sessions[0].Windows[0].Key].Panes[0].Resume)
}

func TestSaveExcludesPullRequestWorkspaces(t *testing.T) {
	f := newSaveFixture(t)
	prDir := t.TempDir()
	plainDir := t.TempDir()
	f.prs = func(context.Context) ([]ProtectedWorkspace, error) { return []ProtectedWorkspace{{Path: prDir}}, nil }

	startServer(t, f.tm, "-s", "pr", "-c", prDir)
	_, err := f.tm.Run(t.Context(), "set-option", "-t", "pr", "@kwt-workspace-id", kwttmux.WorkspacePathIdentity(prDir))
	require.NoError(t, err)
	_, err = f.tm.Run(t.Context(), "new-session", "-d", "-s", "plain", "-c", plainDir)
	require.NoError(t, err)
	_, err = f.tm.Run(t.Context(), "split-window", "-t", "plain", "-c", prDir)
	require.NoError(t, err)
	setResume(t, f.tm, "plain:0.0", Record{Agent: "claude", ConversationID: "c-0"})
	setResume(t, f.tm, "plain:0.1", Record{Agent: "claude", ConversationID: "c-1"})
	_, err = f.tm.Run(t.Context(), "set-option", "-p", "-t", "plain:0.1", kwttmux.PaneAgentOption, "roborev")
	require.NoError(t, err)

	f.save(t)
	files := f.files(t)
	require.Len(t, files.Live.Sessions, 1)
	plain := files.Live.Sessions[0]
	assert.Equal(t, "plain", plain.Name)
	panes := files.Live.Windows[plain.Windows[0].Key].Panes
	require.Len(t, panes, 2)
	assert.NotNil(t, panes[0].Resume)
	assert.Nil(t, panes[1].Resume, "no program restarts inside a pull-request checkout")
	assert.Empty(t, panes[1].PresetAgent)
	assert.Len(t, files.Live.Windows, 1, "the excluded session's window is not kept")
}

func TestSaveProtectsAPullRequestCheckoutThatMoved(t *testing.T) {
	// Provenance records the path at import. A moved checkout keeps its
	// generation, which kwt's open guard also matches.
	f := newSaveFixture(t)
	gitRun := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	repo := t.TempDir()
	gitRun("-C", repo, "init", "-q")
	gitRun("-C", repo, "commit", "-q", "--allow-empty", "-m", "init")
	recorded := filepath.Join(t.TempDir(), "pr")
	gitRun("-C", repo, "worktree", "add", "-q", "-b", "pr", recorded)
	gitDir := gitRun("-C", recorded, "rev-parse", "--absolute-git-dir")
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "kwt-generation"), []byte(worktreeGeneration+"\n"), 0o600))
	moved := filepath.Join(t.TempDir(), "moved")
	gitRun("-C", repo, "worktree", "move", recorded, moved)
	f.prs = func(context.Context) ([]ProtectedWorkspace, error) {
		return []ProtectedWorkspace{{Path: recorded, Generation: worktreeGeneration}}, nil
	}

	startServer(t, f.tm, "-s", "pr", "-c", moved)
	mustRun(t, f.tm, "set-option", "-t", "pr", "@kwt-workspace-id", kwttmux.WorkspacePathIdentity(moved))
	mustRun(t, f.tm, "set-option", "-t", "pr", "@kwt-workspace-generation", worktreeGeneration)
	mustRun(t, f.tm, "new-session", "-d", "-s", "plain", "-c", t.TempDir())
	mustRun(t, f.tm, "split-window", "-t", "plain", "-c", moved)
	setResume(t, f.tm, "plain:0.0", Record{Agent: "claude", ConversationID: "c-0"})
	setResume(t, f.tm, "plain:0.1", Record{Agent: "claude", ConversationID: "c-1"})

	f.save(t)

	files := f.files(t)
	require.Len(t, files.Live.Sessions, 1, "the moved pull-request session is not saved")
	panes := files.Live.Windows[files.Live.Sessions[0].Windows[0].Key].Panes
	require.Len(t, panes, 2)
	assert.NotNil(t, panes[0].Resume)
	assert.Nil(t, panes[1].Resume, "no program restarts inside the moved checkout")
}

func TestSaveFailsClosedWhenProtectionIsUnreadable(t *testing.T) {
	f := newSaveFixture(t)
	f.prs = func(context.Context) ([]ProtectedWorkspace, error) { return nil, errors.New("locked") }
	startServer(t, f.tm, "-s", "marked")
	_, err := f.tm.Run(t.Context(), "set-option", "-t", "marked", "@kwt-workspace-id", "abc")
	require.NoError(t, err)
	_, err = f.tm.Run(t.Context(), "new-session", "-d", "-s", "plain")
	require.NoError(t, err)
	setResume(t, f.tm, "plain", Record{Agent: "claude", ConversationID: "c-1"})

	f.save(t)
	files := f.files(t)
	require.Len(t, files.Live.Sessions, 1)
	assert.Equal(t, "plain", files.Live.Sessions[0].Name)
	pane := files.Live.Windows[files.Live.Sessions[0].Windows[0].Key].Panes[0]
	assert.Nil(t, pane.Resume)
}
