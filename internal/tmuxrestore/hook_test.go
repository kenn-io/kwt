package tmuxrestore

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hookFixture(t *testing.T) (HookTracker, string) {
	t.Helper()
	tm := privateSocket(t)
	startServer(t, tm, "-s", "work")
	pane, err := tm.Run(t.Context(), "display-message", "-p", "-t", "work", "#{pane_id}")
	require.NoError(t, err)
	pane = strings.TrimSpace(pane)
	return HookTracker{
		Tmux:       tm,
		Pane:       pane,
		Store:      OpenStore(t.TempDir()),
		Processes:  fakeProcesses{},
		Protection: noProtection,
		Now:        func() time.Time { return recordClock },
	}, pane
}

func paneRecord(t *testing.T, h HookTracker) *Record {
	t.Helper()
	value, err := h.Tmux.Run(t.Context(), "show-options", "-p", "-q", "-v", "-t", h.Pane, ResumeOption)
	require.NoError(t, err)
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	record, err := DecodeRecord(value)
	require.NoError(t, err)
	return record
}

func TestHookTrackerRecordsAndSaves(t *testing.T) {
	h, _ := hookFixture(t)

	h.Track(t.Context(), claudeEvent("c-1"))
	record := paneRecord(t, h)
	require.NotNil(t, record)
	assert.Equal(t, "c-1", record.ConversationID)

	var files Files
	require.NoError(t, h.Store.WithLock(t.Context(), h.Tmux.Socket, func(f *Files) error {
		files = *f
		return nil
	}))
	require.NotNil(t, files.Live)
	pane := files.Live.Windows[files.Live.Sessions[0].Windows[0].Key].Panes[0]
	require.NotNil(t, pane.Resume)
	assert.Equal(t, "c-1", pane.Resume.ConversationID)

	h.Track(t.Context(), claudeEvent("c-2"))
	assert.Equal(t, "c-2", paneRecord(t, h).ConversationID)

	end := claudeEvent("c-2")
	end.IsEnd, end.EndReason = true, "prompt_input_exit"
	h.Track(t.Context(), end)
	assert.Nil(t, paneRecord(t, h))
}

func TestHookTrackerSavesAgainWhenTheRecordChangesOrTheSnapshotAges(t *testing.T) {
	// An agent fires a hook around every tool call, so a hook that learned
	// nothing new leaves a snapshot saved moments ago alone.
	h, _ := hookFixture(t)
	now := recordClock
	h.Now = func() time.Time { return now }
	saved := func() string {
		var name string
		require.NoError(t, h.Store.WithLock(t.Context(), h.Tmux.Socket, func(f *Files) error {
			name = f.Live.Sessions[0].Name
			return nil
		}))
		return name
	}
	track := func(conversation string) {
		ev := claudeEvent(conversation)
		ev.Now = now
		h.Track(t.Context(), ev)
	}
	rename := func(from, to string) { mustRun(t, h.Tmux, "rename-session", "-t", from, to) }

	track("c-1")
	require.Equal(t, "work", saved())

	rename("work", "second")
	now = now.Add(time.Second)
	track("c-1")
	assert.Equal(t, "work", saved(), "the same conversation a second later")

	now = now.Add(hookSaveInterval)
	track("c-1")
	assert.Equal(t, "second", saved(), "the snapshot has aged")

	rename("second", "third")
	now = now.Add(time.Second)
	track("c-2")
	assert.Equal(t, "third", saved(), "a new conversation is saved at once")
}

func TestHookTrackerSkipsTheSaveWhenTheLockIsHeld(t *testing.T) {
	h, _ := hookFixture(t)
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.Store.WithLock(t.Context(), h.Tmux.Socket, func(*Files) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() { close(release); <-done }()

	h.Deadline = time.Now().Add(300 * time.Millisecond)
	started := time.Now()
	h.Track(t.Context(), claudeEvent("c-1"))
	assert.Less(t, time.Since(started), time.Second)
	require.NotNil(t, paneRecord(t, h), "the pane record is written before the save")
}

func TestHookTrackerIgnoresPanesOutsideTmux(t *testing.T) {
	// An empty target is tmux's current pane, so an agent outside tmux
	// would otherwise record its conversation on whatever pane that is.
	h, pane := hookFixture(t)
	h.Pane = ""

	h.Track(t.Context(), claudeEvent("c-1"))

	h.Pane = pane
	assert.Nil(t, paneRecord(t, h))
	require.NoError(t, h.Store.WithLock(t.Context(), h.Tmux.Socket, func(f *Files) error {
		assert.Nil(t, f.Live, "nothing is saved")
		return nil
	}))
}
