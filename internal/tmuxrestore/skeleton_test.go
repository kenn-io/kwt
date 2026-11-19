package tmuxrestore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kwttmux "go.kenn.io/kwt/internal/tmux"
)

func mustRun(t *testing.T, tm Tmux, args ...string) string {
	t.Helper()
	out, err := tm.Run(t.Context(), args...)
	require.NoError(t, err, "tmux %v", args)
	return out
}

func sessionNamed(t *testing.T, snap Snapshot, name string) Session {
	t.Helper()
	for _, s := range snap.Sessions {
		if s.Name == name {
			return s
		}
	}
	require.FailNow(t, "session not in snapshot", name)
	return Session{}
}

// rebuild copies the saved session name as newName, window by window.
func rebuild(t *testing.T, b Builder, snap Snapshot, name, newName string) (CreatedSession, map[int]BuiltWindow) {
	t.Helper()
	ctx := t.Context()
	s := sessionNamed(t, snap, name)
	s.Name = newName
	created, err := b.CreateSession(ctx, s, snap.Windows[s.Windows[0].Key])
	require.NoError(t, err)
	built := map[int]BuiltWindow{}
	for i, ref := range s.Windows {
		var window BuiltWindow
		if i == 0 {
			window, err = b.BuildFirstWindow(ctx, created, ref.Index, snap.Windows[ref.Key])
		} else {
			window, err = b.BuildWindow(ctx, created.ID, ref.Index, snap.Windows[ref.Key])
		}
		require.NoError(t, err)
		built[ref.Index] = window
	}
	require.NoError(t, b.FinishSession(ctx, created.ID, s))
	return created, built
}

func TestBuildWindowRestoresLayoutsByPaneIndex(t *testing.T) {
	cases := []struct {
		name   string
		width  string
		height string
		splits int
		flag   string
		layout string
	}{
		{"five side by side", "120", "24", 4, "-h", "even-horizontal"},
		{"five columns two rows tall", "120", "2", 4, "-h", "even-horizontal"},
		{"main pane on the right", "120", "30", 3, "-v", "main-vertical-mirrored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := privateSocket(t)
			startServer(t, tm, "-s", "src", "-x", tc.width, "-y", tc.height)
			for range tc.splits {
				mustRun(t, tm, "split-window", tc.flag, "-t", "src")
				mustRun(t, tm, "select-layout", "-t", "src", tc.layout)
			}
			snap, err := ReadSnapshot(t.Context(), tm)
			require.NoError(t, err)
			saved := snap.Windows[sessionNamed(t, snap, "src").Windows[0].Key]
			require.Len(t, saved.Panes, tc.splits+1)

			b := Builder{Tmux: tm, Token: "tok", Home: t.TempDir()}
			_, built := rebuild(t, b, snap, "src", "copy")

			assert.False(t, built[0].LayoutDiffers)
			require.Len(t, built[0].Panes, tc.splits+1)
			// Each saved pane comes back at its saved place. tmux 3.6
			// numbers panes by place, so a mirrored layout's numbers can
			// differ; the pane built for saved pane i is what matters.
			for i, pane := range built[0].Panes {
				p := saved.Panes[i]
				assert.Equal(t, p.Index, pane.Index)
				assert.Equal(t, fmt.Sprintf("%d %d %d %d", p.Left, p.Top, p.Width, p.Height),
					strings.TrimSpace(mustRun(t, tm, "display", "-p", "-t", pane.ID,
						"#{pane_left} #{pane_top} #{pane_width} #{pane_height}")))
			}
			assert.Equal(t, tc.width+"x"+tc.height,
				strings.TrimSpace(mustRun(t, tm, "display", "-p", "-t", "copy", "#{window_width}x#{window_height}")))
			assert.Empty(t, strings.TrimSpace(mustRun(t, tm, "show-options", "-w", "-v", "-t", "copy", "window-size")),
				"the window follows attached clients again")
		})
	}
}

func TestBuildKeepsWindowIndexesAndNaming(t *testing.T) {
	tm := privateSocket(t)
	startServer(t, tm, "-s", "boot")
	mustRun(t, tm, "set-option", "-g", "base-index", "1")
	mustRun(t, tm, "new-session", "-d", "-s", "src", "-n", "edit")
	mustRun(t, tm, "move-window", "-s", "src:edit", "-t", "src:3")
	mustRun(t, tm, "new-window", "-d", "-t", "src:5")
	snap, err := ReadSnapshot(t.Context(), tm)
	require.NoError(t, err)

	rebuild(t, Builder{Tmux: tm, Token: "tok", Home: t.TempDir()}, snap, "src", "copy")

	out := mustRun(t, tm, "list-windows", "-t", "copy", "-F", "#{window_index} #{window_name} #{automatic-rename}")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "3 edit 0", lines[0], "a named window keeps its name")
	assert.True(t, strings.HasPrefix(lines[1], "5 "))
	assert.True(t, strings.HasSuffix(lines[1], " 1"), "an automatically named window stays automatic")
}

func TestBuildMarksPanesWhereAPresetStartedAnAgent(t *testing.T) {
	// Without the mark, the next save after a restore would no longer know
	// which agent the pane runs, and a second server loss would lose it.
	tm := privateSocket(t)
	startServer(t, tm, "-s", "src")
	mustRun(t, tm, "split-window", "-t", "src")
	mustRun(t, tm, "set-option", "-p", "-t", "src:.1", kwttmux.PaneAgentOption, "roborev")
	snap, err := ReadSnapshot(t.Context(), tm)
	require.NoError(t, err)

	_, built := rebuild(t, Builder{Tmux: tm, Token: "tok", Home: t.TempDir()}, snap, "src", "copy")

	require.Len(t, built[0].Panes, 2)
	marks := []string{}
	for _, pane := range built[0].Panes {
		marks = append(marks, strings.TrimSpace(mustRun(t, tm, "display", "-p", "-t", pane.ID,
			"#{"+kwttmux.PaneAgentOption+"}")))
	}
	assert.Equal(t, []string{"", "roborev"}, marks)
}

func TestFinishRestoresActiveWindowPaneAndZoom(t *testing.T) {
	tm := privateSocket(t)
	startServer(t, tm, "-s", "src", "-x", "100", "-y", "30")
	mustRun(t, tm, "split-window", "-h", "-t", "src:0")
	mustRun(t, tm, "select-pane", "-t", "src:0.0")
	mustRun(t, tm, "resize-pane", "-Z", "-t", "src:0.0")
	mustRun(t, tm, "new-window", "-t", "src:1")
	mustRun(t, tm, "select-window", "-t", "src:1")
	snap, err := ReadSnapshot(t.Context(), tm)
	require.NoError(t, err)

	rebuild(t, Builder{Tmux: tm, Token: "tok", Home: t.TempDir()}, snap, "src", "copy")

	assert.Equal(t, "1", strings.TrimSpace(mustRun(t, tm, "display", "-p", "-t", "copy", "#{window_index}")))
	assert.Equal(t, "0 1", strings.TrimSpace(mustRun(t, tm, "display", "-p", "-t", "copy:0", "#{pane_index} #{window_zoomed_flag}")))
}

func TestBuildFallsBackToHomeForMissingDirectories(t *testing.T) {
	tm := privateSocket(t)
	gone := filepath.Join(t.TempDir(), "gone")
	kept := t.TempDir()
	require.NoError(t, os.Mkdir(gone, 0o700))
	startServer(t, tm, "-s", "src", "-c", gone)
	mustRun(t, tm, "split-window", "-t", "src", "-c", kept)
	snap, err := ReadSnapshot(t.Context(), tm)
	require.NoError(t, err)
	require.NoError(t, os.Remove(gone))

	home := t.TempDir()
	created, built := rebuild(t, Builder{Tmux: tm, Token: "tok", Home: home}, snap, "src", "copy")

	assert.True(t, created.PathMissing)
	require.Len(t, built[0].Panes, 2)
	assert.True(t, built[0].Panes[0].DirMissing)
	assert.False(t, built[0].Panes[1].DirMissing)
	paths := strings.Split(strings.TrimSpace(mustRun(t, tm, "list-panes", "-t", "copy", "-F", "#{pane_current_path}")), "\n")
	assert.Equal(t, []string{resolvedDir(home), resolvedDir(kept)}, []string{resolvedDir(paths[0]), resolvedDir(paths[1])})
}

func TestCreateSessionStartsAServerWhoseSocketDirectoryIsGone(t *testing.T) {
	// A reboot empties /tmp, taking tmux's socket directory with it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	tm := privateSocket(t)
	tm.Socket.Path = filepath.Join(filepath.Dir(tm.Socket.Path), "tmux-1", "default")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_, _ = tm.Run(ctx, "kill-server")
	})

	saved := Session{Name: "work", Path: home}
	first := Window{Width: 80, Height: 24, Panes: []Pane{{Dir: home}}}
	_, err := Builder{Tmux: tm, Token: "tok", Home: home}.CreateSession(t.Context(), saved, first)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Dir(tm.Socket.Path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "tmux's socket directory is private")
	assert.Equal(t, "work", strings.TrimSpace(mustRun(t, tm, "list-sessions", "-F", "#{session_name}")))
}

func TestCleanupOwnedKillsOnlySessionsWithTheToken(t *testing.T) {
	tm := privateSocket(t)
	dir := t.TempDir()
	ctx := t.Context()

	absent := Builder{Tmux: tm, Token: "tok-a"}
	killed, err := absent.CleanupOwned(ctx)
	require.NoError(t, err, "no server is nothing to clean up")
	assert.False(t, killed)

	startServer(t, tm, "-s", "boot")
	// Run as if from a pane in boot, so an untargeted command would hit it.
	client := strings.TrimSpace(mustRun(t, tm, "display", "-p", "-t", "boot", "#{socket_path},#{pid},#{session_id}"))
	t.Setenv("TMUX", strings.Replace(client, "$", "", 1))
	saved := Session{Name: "work", Path: dir}
	first := Window{Width: 80, Height: 24, Panes: []Pane{{Dir: dir}}}
	_, err = Builder{Tmux: tm, Token: "tok-a", Home: dir}.CreateSession(ctx, saved, first)
	require.NoError(t, err)

	killed, err = Builder{Tmux: tm, Token: "tok-b"}.CleanupOwned(ctx)
	require.NoError(t, err)
	assert.False(t, killed)

	killed, err = Builder{Tmux: tm, Token: "tok-a"}.CleanupOwned(ctx)
	require.NoError(t, err)
	assert.True(t, killed)
	sessions := mustRun(t, tm, "list-sessions", "-F", "#{session_name}")
	assert.Equal(t, "boot", strings.TrimSpace(sessions))

	mustRun(t, tm, "new-session", "-d", "-s", "work")
	killed, err = Builder{Tmux: tm, Token: "tok-a"}.CleanupOwned(ctx)
	require.NoError(t, err)
	assert.False(t, killed, "a same-named session without the token is the user's")
	assert.Contains(t, mustRun(t, tm, "list-sessions", "-F", "#{session_name}"), "work")
}

func TestBuildTakesSavedDirectoriesLiterally(t *testing.T) {
	// tmux expands formats in start directories; a directory a checkout
	// creates must not run commands or change where panes open.
	tm := privateSocket(t)
	startServer(t, tm, "-s", "src")
	mustRun(t, tm, "split-window", "-t", "src")
	mustRun(t, tm, "new-window", "-t", "src")
	snap, err := ReadSnapshot(t.Context(), tm)
	require.NoError(t, err)
	odd := filepath.Join(resolvedDir(t.TempDir()), "a#{session_name}#(echo b)")
	require.NoError(t, os.Mkdir(odd, 0o700))
	for i := range snap.Sessions {
		snap.Sessions[i].Path = odd
	}
	for key, w := range snap.Windows {
		for i := range w.Panes {
			w.Panes[i].Dir = odd
		}
		snap.Windows[key] = w
	}

	created, built := rebuild(t, Builder{Tmux: tm, Token: "tok", Home: t.TempDir()}, snap, "src", "copy")

	assert.False(t, created.PathMissing)
	paths := strings.Split(strings.TrimSpace(mustRun(t, tm, "list-panes", "-s", "-t", "copy", "-F", "#{pane_current_path}")), "\n")
	require.Len(t, paths, 3)
	for _, path := range paths {
		assert.Equal(t, resolvedDir(odd), resolvedDir(path))
	}
	for _, w := range built {
		for _, pane := range w.Panes {
			assert.False(t, pane.DirMissing)
		}
	}
}

func TestCreatedSessionOutlivesDestroyUnattached(t *testing.T) {
	// A restore after a reboot starts the server with the user's
	// configuration, and its sessions stay unattached while they are built.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte("set -g destroy-unattached on\n"), 0o600))
	tm := privateSocket(t)

	saved := Session{Name: "work", Path: home}
	first := Window{Width: 80, Height: 24, Panes: []Pane{{Dir: home}, {Dir: home}}}
	b := Builder{Tmux: tm, Token: "tok", Home: home}
	created, err := b.CreateSession(t.Context(), saved, first)
	require.NoError(t, err)
	_, err = b.BuildFirstWindow(t.Context(), created, created.WindowIndex, first)
	require.NoError(t, err)

	assert.Equal(t, "work", strings.TrimSpace(mustRun(t, tm, "list-sessions", "-F", "#{session_name}")))
}

func TestCreateSessionKeepsProtectedCredentialsOutOfTheServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("CUSTOM_FLEET_TOKEN", "secret")
	tm := privateSocket(t)
	tm.StripNames = []string{"custom_fleet_token"}

	saved := Session{Name: "work", Path: home}
	first := Window{Width: 80, Height: 24, Panes: []Pane{{Dir: home}}}
	_, err := Builder{Tmux: tm, Token: "tok", Home: home}.CreateSession(t.Context(), saved, first)
	require.NoError(t, err)

	out, _ := tm.Run(t.Context(), "show-environment", "-g", "CUSTOM_FLEET_TOKEN")
	assert.NotContains(t, out, "secret")
}

func TestBuildPlacesThePanesOfAZoomedWindow(t *testing.T) {
	// A zoomed window saves zoomed pane sizes; its places come from the
	// saved layout, which tmux keeps unzoomed.
	tm := privateSocket(t)
	startServer(t, tm, "-s", "src", "-x", "120", "-y", "30")
	for range 3 {
		mustRun(t, tm, "split-window", "-v", "-t", "src")
		mustRun(t, tm, "select-layout", "-t", "src", "main-vertical-mirrored")
	}
	places := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(mustRun(t, tm, "list-panes", "-t", "src", "-F",
		"#{pane_id} #{pane_left} #{pane_top} #{pane_width} #{pane_height}")), "\n") {
		id, rect, _ := strings.Cut(line, " ")
		places[id] = rect
	}
	mustRun(t, tm, "resize-pane", "-Z", "-t", "src:0.0")
	snap, err := ReadSnapshot(t.Context(), tm)
	require.NoError(t, err)
	saved := snap.Windows[sessionNamed(t, snap, "src").Windows[0].Key]
	require.True(t, saved.Zoomed)

	_, built := rebuild(t, Builder{Tmux: tm, Token: "tok", Home: t.TempDir()}, snap, "src", "copy")

	assert.False(t, built[0].LayoutDiffers)
	mustRun(t, tm, "resize-pane", "-Z", "-t", built[0].Panes[0].ID)
	require.Len(t, built[0].Panes, 4)
	for i, pane := range built[0].Panes {
		assert.Equal(t, places[saved.Panes[i].ID],
			strings.TrimSpace(mustRun(t, tm, "display", "-p", "-t", pane.ID,
				"#{pane_left} #{pane_top} #{pane_width} #{pane_height}")))
	}
}
