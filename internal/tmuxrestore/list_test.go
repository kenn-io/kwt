package tmuxrestore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveAndListReportsEntriesAcrossSockets(t *testing.T) {
	a := privateSocket(t)
	b := privateSocket(t)
	store := OpenStore(t.TempDir())
	now := time.Unix(1_800_000_000, 0)
	deps := ListDeps{
		Executable: a.Executable, Store: store, Processes: fakeProcesses{},
		Protection: noProtection, Now: func() time.Time { return now },
	}

	startServer(t, a, "-s", "work")
	_, err := a.Run(t.Context(), "split-window", "-t", "work")
	require.NoError(t, err)
	setResume(t, a, "work:0.1", Record{Agent: "codex", ConversationID: "x-1"})
	startServer(t, b, "-s", "work")
	_, errs := SaveAndList(t.Context(), deps, []Socket{a.Socket, b.Socket})
	require.Empty(t, errs)

	_, err = a.Run(t.Context(), "kill-server")
	require.NoError(t, err)
	_, err = b.Run(t.Context(), "kill-server")
	require.NoError(t, err)
	startServer(t, b, "-s", "work")

	listed, errs := SaveAndList(t.Context(), deps, []Socket{a.Socket, b.Socket, a.Socket})
	require.Empty(t, errs)
	require.Len(t, listed, 2)
	byPath := map[string]Listed{}
	for _, entry := range listed {
		byPath[entry.Socket.Path] = entry
	}
	fromA := byPath[a.Socket.Path]
	assert.Equal(t, "work", fromA.Session)
	assert.Equal(t, 1, fromA.Windows)
	assert.Equal(t, 2, fromA.Panes)
	assert.Equal(t, []string{"codex"}, fromA.Agents)
	assert.False(t, fromA.NameTaken)
	assert.Equal(t, StatePending, fromA.State)
	assert.True(t, byPath[b.Socket.Path].NameTaken, "b's new server already has a work session")
	assert.NotEqual(t, fromA.ID, byPath[b.Socket.Path].ID)
}
