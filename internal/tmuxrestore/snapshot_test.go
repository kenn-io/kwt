package tmuxrestore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwttmux "go.kenn.io/kwt/internal/tmux"
)

func paneLine(fields ...string) string { return strings.Join(fields, "\t") }

func TestParsePanesGroupsSessionsWindowsAndPanes(t *testing.T) {
	record := `{"agent":"claude","conversation_id":"c-1","recorded_at":5}`
	output := strings.Join([]string{
		// session, window @1 with two panes listed out of index order
		paneLine("$1", "work", "100", "/src/a", "", "", "@1", "0", "edit", "0", "120", "40", "abcd,120x40,0,0{60x40,0,0,1,59x40,61,0,2}", "1", "0", "1", "61", "0", "59", "40", "0", "/src/a/sub", "%2", "", ""),
		paneLine("$1", "work", "100", "/src/a", "", "", "@1", "0", "edit", "0", "120", "40", "abcd,120x40,0,0{60x40,0,0,1,59x40,61,0,2}", "1", "0", "0", "0", "0", "60", "40", "1", "/src/a", "%1", "roborev", record),
		// window @2 is linked into both sessions
		paneLine("$1", "work", "100", "/src/a", "", "", "@2", "1", "logs", "1", "120", "40", "ef01,120x40,0,0,3", "0", "1", "0", "0", "0", "120", "40", "1", "/var/log", "%3", "", ""),
		paneLine("$2", "ops", "200", "/src/b", "id-b", "7", "@2", "4", "logs", "1", "120", "40", "ef01,120x40,0,0,3", "1", "1", "0", "0", "0", "120", "40", "1", "/var/log", "%3", "", "not json"),
	}, "\n") + "\n"

	snap, err := parsePanes(ServerID{PID: "77", StartTime: "9"}, output)
	require.NoError(t, err)

	require.Len(t, snap.Sessions, 2)
	work := snap.Sessions[0]
	assert.Equal(t, Session{
		ID: "$1", Name: "work", Created: "100", Path: "/src/a",
		Windows: []WindowRef{
			{Key: "77:@1", Index: 0, Active: true},
			{Key: "77:@2", Index: 1, Active: false},
		},
	}, work)
	ops := snap.Sessions[1]
	assert.Equal(t, "id-b", ops.WorkspaceIdentity)
	assert.Equal(t, "7", ops.WorkspaceGeneration)
	assert.Equal(t, []WindowRef{{Key: "77:@2", Index: 4, Active: true}}, ops.Windows)

	require.Len(t, snap.Windows, 2, "a linked window is stored once")
	edit := snap.Windows["77:@1"]
	assert.Equal(t, "edit", edit.Name)
	assert.False(t, edit.AutoRename)
	assert.False(t, edit.Zoomed)
	require.Len(t, edit.Panes, 2)
	assert.Equal(t, 0, edit.Panes[0].Index, "panes are ordered by index")
	assert.Equal(t, "roborev", edit.Panes[0].PresetAgent)
	require.NotNil(t, edit.Panes[0].Resume)
	assert.Equal(t, "c-1", edit.Panes[0].Resume.ConversationID)
	assert.Equal(t, Pane{Index: 1, ID: "%2", Left: 61, Top: 0, Width: 59, Height: 40, Dir: "/src/a/sub"}, edit.Panes[1])

	logs := snap.Windows["77:@2"]
	assert.True(t, logs.AutoRename)
	assert.True(t, logs.Zoomed)
	require.Len(t, logs.Panes, 1, "the linked window's pane is not duplicated")
	assert.Nil(t, logs.Panes[0].Resume, "an unreadable record is dropped, the pane kept")
}

func TestParsePanesRejectsShortLines(t *testing.T) {
	_, err := parsePanes(ServerID{PID: "1"}, "$1\twork\n")
	assert.Error(t, err)
}

func TestReadSnapshotFromLiveServer(t *testing.T) {
	tm := privateSocket(t)
	dir := t.TempDir()
	startServer(t, tm, "-s", "work", "-n", "edit", "-c", dir, "-x", "100", "-y", "30")
	ctx := t.Context()
	_, err := tm.Run(ctx, "split-window", "-h", "-t", "work:edit", "-c", dir)
	require.NoError(t, err)
	_, err = tm.Run(ctx, "set-option", "-p", "-t", "work:edit.1", ResumeOption,
		`{"agent":"codex","conversation_id":"x-1","recorded_at":3}`)
	require.NoError(t, err)
	_, err = tm.Run(ctx, "set-option", "-p", "-t", "work:edit.0", kwttmux.PaneAgentOption, "roborev")
	require.NoError(t, err)
	_, err = tm.Run(ctx, "new-session", "-d", "-s", "ops", "-c", dir)
	require.NoError(t, err)
	_, err = tm.Run(ctx, "link-window", "-s", "work:edit", "-t", "ops:5")
	require.NoError(t, err)

	snap, err := ReadSnapshot(ctx, tm)
	require.NoError(t, err)

	assert.NotEmpty(t, snap.Server.PID)
	require.Len(t, snap.Sessions, 2)
	assert.Len(t, snap.Windows, 2, "work:edit is shared with ops")
	var edit Window
	for _, ref := range snap.Sessions[0].Windows {
		if snap.Windows[ref.Key].Name == "edit" {
			edit = snap.Windows[ref.Key]
		}
	}
	require.Len(t, edit.Panes, 2)
	assert.Equal(t, "roborev", edit.Panes[0].PresetAgent)
	require.NotNil(t, edit.Panes[1].Resume)
	assert.Equal(t, "x-1", edit.Panes[1].Resume.ConversationID)
	assert.Equal(t, 100, edit.Width)
	assert.NotEmpty(t, edit.Layout)
	assert.Equal(t, resolvedDir(dir), resolvedDir(edit.Panes[0].Dir))

	var opsRefs []WindowRef
	for _, s := range snap.Sessions {
		if s.Name == "ops" {
			opsRefs = s.Windows
		}
	}
	require.Len(t, opsRefs, 2)
}

func TestReadSnapshotWithoutAUTF8Locale(t *testing.T) {
	// A tmux client outside a UTF-8 locale prints each tab as "_", which
	// would merge every field of the saved line into one.
	t.Setenv("LANG", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LC_ALL", "C")
	tm := privateSocket(t)
	startServer(t, tm, "-s", "work", "-n", "edit")

	snap, err := ReadSnapshot(t.Context(), tm)
	require.NoError(t, err)
	require.Len(t, snap.Sessions, 1)
	assert.Equal(t, "work", snap.Sessions[0].Name)
}

// resolvedDir drops macOS's /private prefix, which tmux reports for temp
// directories under /var, so paths can be compared.
func resolvedDir(dir string) string {
	return strings.TrimPrefix(dir, "/private")
}
