package tmuxrestore

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func claimFixture(t *testing.T) (Store, string) {
	t.Helper()
	store := OpenStore(t.TempDir())
	var id string
	update(t, store, func(f *Files) error {
		entry := launchingEntry(f, "30", 100, nil)
		entry.Launches = []PaneLaunch{
			{PaneID: "%4", Status: LaunchWaiting, Argv: []string{"claude", "--resume", "c-1"}, Dir: "/src"},
			{PaneID: "%5", Status: LaunchNone},
		}
		id = entry.ID
		return nil
	})
	return store, id
}

func TestClaimHandsTheLaunchToItsPaneOnce(t *testing.T) {
	store, id := claimFixture(t)

	target, err := Claim(t.Context(), store, storeSocket, id, 0, "%4", "30")
	require.NoError(t, err)
	assert.Equal(t, ExecTarget{Argv: []string{"claude", "--resume", "c-1"}, Dir: "/src"}, target)
	assert.Equal(t, LaunchStarted, read(t, store).Restorable.Entries[0].Launches[0].Status)

	_, err = Claim(t.Context(), store, storeSocket, id, 0, "%4", "30")
	assert.ErrorContains(t, err, "already started")
}

func TestLaunchRecordsAProgramThatCannotStart(t *testing.T) {
	store := OpenStore(t.TempDir())
	var id string
	update(t, store, func(f *Files) error {
		entry := launchingEntry(f, "30", 100, nil)
		entry.Launches = []PaneLaunch{{PaneID: "%4", Status: LaunchWaiting, Argv: []string{filepath.Join(t.TempDir(), "gone")}}}
		id = entry.ID
		return nil
	})

	err := Launch(t.Context(), store, storeSocket, id, 0, "%4", "30")

	require.Error(t, err)
	l := read(t, store).Restorable.Entries[0].Launches[0]
	assert.Equal(t, LaunchFailed, l.Status, "the restore report must not say it started")
	assert.Contains(t, l.Note, "did not start")
}

func TestClaimRejectsTheWrongPane(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		launch int
		pane   string
		server string
		want   string
	}{
		{"another pane", "", 0, "%9", "30", "not the restored pane"},
		{"another server", "", 0, "%4", "31", "not the restored pane"},
		{"a pane with nothing to start", "", 1, "%5", "30", "nothing to start"},
		{"an unknown launch", "", 7, "%4", "30", "no such launch"},
		{"an unknown entry", "nope", 0, "%4", "30", "no restore"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, id := claimFixture(t)
			if tc.id != "" {
				id = tc.id
			}
			_, err := Claim(t.Context(), store, storeSocket, id, tc.launch, tc.pane, tc.server)
			assert.ErrorContains(t, err, tc.want)
			if tc.id == "" {
				assert.Equal(t, LaunchWaiting, read(t, store).Restorable.Entries[0].Launches[0].Status)
			}
		})
	}
}

func TestSafeKwtPath(t *testing.T) {
	for path, safe := range map[string]bool{
		"/usr/local/bin/kwt":                        true,
		"/Applications/Ghosthub.app/Contents/kwt":   true,
		"/Users/me/Library/Application Support/kwt": true,
		"/home/me/.local/bin/kwt_1.2+x":             true,
		"/Users/José/bin/kwt":                       true,
		"/home/用户/bin/kwt":                          true,
		"/opt/a\u00a0b/kwt":                         false,
		"/opt/a\u202eb/kwt":                         false,
		"kwt":                                       false,
		"/opt/it's/kwt":                             false,
		"/opt/a\\b/kwt":                             false,
		"/opt/$(x)/kwt":                             false,
		"/opt/new\nline":                            false,
	} {
		assert.Equal(t, safe, safeKwtPath(path), path)
	}
}
