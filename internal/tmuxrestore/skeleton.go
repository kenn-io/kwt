package tmuxrestore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	kwttmux "go.kenn.io/kwt/internal/tmux"
)

const maxWindowSize = 10000

// Builder rebuilds saved sessions on one tmux server. It starts no programs.
type Builder struct {
	Tmux  Tmux
	Token string
	// Home replaces a saved directory that no longer exists.
	Home string
}

// CreatedSession is a new session with its first window and pane.
type CreatedSession struct {
	ID          string
	WindowID    string
	WindowIndex int
	PaneID      string
	// PathMissing and PaneDirMissing report that the session's directory or
	// its first pane's directory was gone, so Home was used.
	PathMissing    bool
	PaneDirMissing bool
}

// BuiltPane is one rebuilt pane, in saved index order.
type BuiltPane struct {
	Index      int
	ID         string
	DirMissing bool
}

// BuiltWindow is one rebuilt window.
type BuiltWindow struct {
	ID    string
	Panes []BuiltPane
	// LayoutDiffers reports that the saved layout could not be reproduced,
	// so the panes were tiled instead.
	LayoutDiffers bool
}

// literalDir escapes dir for tmux, which expands formats such as #(command)
// in start directories.
func literalDir(dir string) string { return strings.ReplaceAll(dir, "#", "##") }

// resolveDir returns dir, or home with false when dir no longer exists.
func resolveDir(dir, home string) (string, bool) {
	if dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, true
		}
	}
	return home, false
}

// CreateSession creates s with first as its first window and marks it with
// the builder's token in the same tmux command, so no moment exists where the
// session is live but unmarked. The token is set by exact name, because an
// untargeted command run from inside tmux applies to the caller's session.
func (b Builder) CreateSession(ctx context.Context, s Session, first Window) (CreatedSession, error) {
	path, pathOK := resolveDir(s.Path, b.Home)
	paneDir, paneOK := path, pathOK
	if len(first.Panes) > 0 {
		paneDir, paneOK = resolveDir(first.Panes[0].Dir, b.Home)
	}
	args := []string{"new-session", "-d", "-P", "-F",
		"#{session_id}\t#{window_id}\t#{window_index}\t#{pane_id}", "-s", s.Name, "-c", literalDir(path)}
	if !first.AutoRename && first.Name != "" {
		args = append(args, "-n", first.Name)
	}
	// "=name:" is an exact session match; tmux does not resolve a bare
	// "=name" for set-option.
	args = append(args, ";", "set-option", "-t", "="+s.Name+":", kwttmux.CreationTokenOption, b.Token)
	// The session stays unattached while it is built, so destroy-unattached
	// waits for its first attach, as it does for kwt's workspace sessions.
	executable, err := exec.LookPath(cmp.Or(b.Tmux.Executable, "tmux"))
	if err == nil {
		executable, err = filepath.Abs(executable)
	}
	if err != nil {
		return CreatedSession{}, fmt.Errorf("find tmux: %w", err)
	}
	args = append(args, kwttmux.HoldSessionLifetime(s.Name, executable)...)
	// A reboot empties /tmp, and tmux given a socket path does not create
	// its directory the way it does for a socket name.
	if err := os.MkdirAll(filepath.Dir(b.Tmux.Socket.Path), 0o700); err != nil {
		return CreatedSession{}, fmt.Errorf("create tmux socket directory: %w", err)
	}
	out, err := b.Tmux.Run(ctx, args...)
	if err != nil {
		return CreatedSession{}, fmt.Errorf("create session %q: %w", s.Name, err)
	}
	f := strings.Split(strings.TrimSpace(out), "\t")
	if len(f) != 4 {
		return CreatedSession{}, fmt.Errorf("create session %q: unexpected tmux output %q", s.Name, out)
	}
	index, err := atois(f[2])
	if err != nil {
		return CreatedSession{}, err
	}
	created := CreatedSession{
		ID: f[0], WindowID: f[1], WindowIndex: index[0], PaneID: f[3],
		PathMissing: !pathOK, PaneDirMissing: !paneOK,
	}
	if paneDir != path {
		// The session directory is where later windows open; the first
		// pane gets its own saved directory before anything runs in it.
		if _, err := b.Tmux.Run(ctx, "respawn-pane", "-k", "-c", literalDir(paneDir), "-t", created.PaneID); err != nil {
			return created, fmt.Errorf("move first pane of %q: %w", s.Name, err)
		}
	}
	return created, nil
}

// Adopt takes over a session kwt's worktree create path made, from its first
// pane, as CreateSession would have made it. The pane moves to its saved
// directory when that still exists; otherwise it stays where kwt started it.
func (b Builder) Adopt(ctx context.Context, paneID string, first Window) (CreatedSession, error) {
	out, err := b.Tmux.Run(ctx, "display-message", "-p", "-t", paneID,
		"#{session_id}\t#{window_id}\t#{window_index}\t#{pane_current_path}")
	if err != nil {
		return CreatedSession{}, fmt.Errorf("read new session: %w", err)
	}
	f := strings.Split(strings.TrimSpace(out), "\t")
	if len(f) != 4 {
		return CreatedSession{}, fmt.Errorf("read new session: unexpected tmux output %q", out)
	}
	index, err := atois(f[2])
	if err != nil {
		return CreatedSession{}, err
	}
	created := CreatedSession{ID: f[0], WindowID: f[1], WindowIndex: index[0], PaneID: paneID}
	if !first.AutoRename && first.Name != "" {
		if _, err := b.Tmux.Run(ctx, "rename-window", "-t", created.WindowID, first.Name); err != nil {
			return created, fmt.Errorf("name window: %w", err)
		}
	}
	if len(first.Panes) == 0 {
		return created, nil
	}
	dir, ok := resolveDir(first.Panes[0].Dir, b.Home)
	created.PaneDirMissing = !ok
	if ok && dir != f[3] {
		// Respawning without a command reruns kwt's login shell in the
		// saved directory.
		if _, err := b.Tmux.Run(ctx, "respawn-pane", "-k", "-c", literalDir(dir), "-t", paneID); err != nil {
			return created, fmt.Errorf("move first pane: %w", err)
		}
	}
	return created, nil
}

// BuildFirstWindow moves the session's first window to its saved index and
// gives it the saved panes and layout.
func (b Builder) BuildFirstWindow(ctx context.Context, created CreatedSession, index int, w Window) (BuiltWindow, error) {
	if created.WindowIndex != index {
		target := fmt.Sprintf("%s:%d", created.ID, index)
		if _, err := b.Tmux.Run(ctx, "move-window", "-s", created.WindowID, "-t", target); err != nil {
			return BuiltWindow{}, fmt.Errorf("move window to %d: %w", index, err)
		}
	}
	return b.fill(ctx, created.WindowID, BuiltPane{ID: created.PaneID, DirMissing: created.PaneDirMissing}, w)
}

// BuildWindow creates a window at its saved index with the saved panes and
// layout.
func (b Builder) BuildWindow(ctx context.Context, sessionID string, index int, w Window) (BuiltWindow, error) {
	dir, ok := b.Home, false
	if len(w.Panes) > 0 {
		dir, ok = resolveDir(w.Panes[0].Dir, b.Home)
	}
	args := []string{"new-window", "-d", "-P", "-F", "#{window_id}\t#{pane_id}",
		"-t", fmt.Sprintf("%s:%d", sessionID, index), "-c", literalDir(dir)}
	if !w.AutoRename && w.Name != "" {
		args = append(args, "-n", w.Name)
	}
	out, err := b.Tmux.Run(ctx, args...)
	if err != nil {
		return BuiltWindow{}, fmt.Errorf("create window %d: %w", index, err)
	}
	windowID, paneID, found := strings.Cut(strings.TrimSpace(out), "\t")
	if !found {
		return BuiltWindow{}, fmt.Errorf("create window %d: unexpected tmux output %q", index, out)
	}
	return b.fill(ctx, windowID, BuiltPane{ID: paneID, DirMissing: !ok}, w)
}

// fill splits the window into the saved panes and applies the saved layout.
// tmux places layout cells by pane index, so panes are created in index
// order. The splits happen at a temporary size large enough for any number
// of panes; a dense layout that fits its real size can still fail to split
// down to it one pane at a time.
func (b Builder) fill(ctx context.Context, windowID string, first BuiltPane, w Window) (BuiltWindow, error) {
	built := BuiltWindow{ID: windowID}
	if len(w.Panes) == 0 {
		return built, nil
	}
	first.Index = w.Panes[0].Index
	built.Panes = append(built.Panes, first)

	room := min(max(w.Width, w.Height, 4*len(w.Panes)+10), maxWindowSize)
	if _, err := b.Tmux.Run(ctx, "resize-window", "-t", windowID,
		"-x", fmt.Sprint(max(w.Width, room)), "-y", fmt.Sprint(max(w.Height, room))); err != nil {
		return built, fmt.Errorf("size window: %w", err)
	}
	newest := first.ID
	for _, pane := range w.Panes[1:] {
		dir, ok := resolveDir(pane.Dir, b.Home)
		out, err := b.Tmux.Run(ctx, "split-window", "-d", "-P", "-F", "#{pane_id}",
			"-c", literalDir(dir), "-t", newest, ";", "select-layout", "-t", windowID, "even-vertical")
		if err != nil {
			return built, fmt.Errorf("split pane %d: %w", pane.Index, err)
		}
		newest = strings.TrimSpace(out)
		built.Panes = append(built.Panes, BuiltPane{Index: pane.Index, ID: newest, DirMissing: !ok})
	}
	// The mark tells later saves which agent a preset started, so the pane
	// stays restorable after the next server loss too.
	for i, pane := range w.Panes {
		if pane.PresetAgent == "" {
			continue
		}
		if _, err := b.Tmux.Run(ctx, "set-option", "-p", "-t", built.Panes[i].ID,
			kwttmux.PaneAgentOption, pane.PresetAgent); err != nil {
			return built, fmt.Errorf("mark pane %d agent: %w", pane.Index, err)
		}
	}

	if _, err := b.Tmux.Run(ctx, "resize-window", "-t", windowID,
		"-x", fmt.Sprint(w.Width), "-y", fmt.Sprint(w.Height)); err != nil {
		return built, fmt.Errorf("size window: %w", err)
	}
	if _, err := b.Tmux.Run(ctx, "select-layout", "-t", windowID, w.Layout); err != nil {
		built.LayoutDiffers = true
	} else {
		same, err := b.placePanes(ctx, windowID, built.Panes, w)
		if err != nil {
			return built, err
		}
		built.LayoutDiffers = !same
	}
	if built.LayoutDiffers {
		if _, err := b.Tmux.Run(ctx, "select-layout", "-t", windowID, "tiled"); err != nil {
			return built, fmt.Errorf("tile window: %w", err)
		}
	}
	// resize-window pinned the size; let the window follow clients again.
	if _, err := b.Tmux.Run(ctx, "set-option", "-w", "-u", "-t", windowID, "window-size"); err != nil {
		return built, fmt.Errorf("release window size: %w", err)
	}

	if i := slices.IndexFunc(w.Panes, func(p Pane) bool { return p.Active }); i >= 0 {
		if _, err := b.Tmux.Run(ctx, "select-pane", "-t", built.Panes[i].ID); err != nil {
			return built, fmt.Errorf("select pane: %w", err)
		}
		if w.Zoomed && len(w.Panes) > 1 {
			if _, err := b.Tmux.Run(ctx, "resize-pane", "-Z", "-t", built.Panes[i].ID); err != nil {
				return built, fmt.Errorf("zoom pane: %w", err)
			}
		}
	}
	return built, nil
}

// layoutCell matches one pane's cell in a classic tmux layout,
// WIDTHxHEIGHT,LEFT,TOP, then the pane's number. A split cell has a bracket
// after TOP instead.
var layoutCell = regexp.MustCompile(`(\d+)x(\d+),(\d+),(\d+),(\d+)`)

// layoutNode is one cell of a layout in the JSON form newer tmux reports.
type layoutNode struct {
	W  int    `json:"w"`
	H  int    `json:"h"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
	ID string `json:"I"`
	// Index is listed so its "i" key, matched without case, does not
	// land in ID.
	Index int          `json:"i"`
	C     []layoutNode `json:"c"`
}

// layoutPlaces maps each pane ID in layout to its place, as "left top width
// height". tmux reports a zoomed window's layout unzoomed, so this is where
// each pane sits once the window is unzoomed.
func layoutPlaces(layout string) map[string]string {
	places := map[string]string{}
	if strings.HasPrefix(layout, "{") {
		var doc struct {
			L layoutNode `json:"L"`
		}
		if json.Unmarshal([]byte(layout), &doc) != nil {
			return places
		}
		var walk func(layoutNode)
		walk = func(n layoutNode) {
			if n.ID != "" {
				places[n.ID] = fmt.Sprintf("%d %d %d %d", n.X, n.Y, n.W, n.H)
			}
			for _, c := range n.C {
				walk(c)
			}
		}
		walk(doc.L)
		return places
	}
	for _, m := range layoutCell.FindAllStringSubmatch(layout, -1) {
		places["%"+m[5]] = m[3] + " " + m[4] + " " + m[1] + " " + m[2]
	}
	return places
}

// placePanes moves each built pane to its saved pane's place and reports
// whether the window now matches the saved layout. select-layout fills the
// layout's places in tmux's pane order, and tmux 3.6 cannot keep a saved
// order that differs from the places, such as a mirrored main pane.
// Swapping keeps each pane with the directory it was started in.
func (b Builder) placePanes(ctx context.Context, windowID string, panes []BuiltPane, w Window) (bool, error) {
	out, err := b.Tmux.Run(ctx, "list-panes", "-t", windowID, "-F",
		"#{pane_id} #{pane_left} #{pane_top} #{pane_width} #{pane_height}")
	if err != nil {
		return false, fmt.Errorf("read panes: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(w.Panes) {
		return false, nil
	}
	occupant := map[string]string{} // place -> pane ID
	place := map[string]string{}    // pane ID -> place
	for _, line := range lines {
		id, rect, _ := strings.Cut(line, " ")
		occupant[rect], place[id] = id, rect
	}
	saved := layoutPlaces(w.Layout)
	for i, p := range w.Panes {
		want, ok := saved[p.ID]
		if !ok {
			want = fmt.Sprintf("%d %d %d %d", p.Left, p.Top, p.Width, p.Height)
		}
		other, ok := occupant[want]
		if !ok {
			return false, nil
		}
		mine := panes[i].ID
		if other == mine {
			continue
		}
		if _, err := b.Tmux.Run(ctx, "swap-pane", "-d", "-s", mine, "-t", other); err != nil {
			return false, fmt.Errorf("place pane %d: %w", p.Index, err)
		}
		was := place[mine]
		occupant[want], occupant[was] = mine, other
		place[mine], place[other] = want, was
	}
	return true, nil
}

// LinkWindow links an existing window into a session at index. replace
// unlinks the window already at index.
func (b Builder) LinkWindow(ctx context.Context, sessionID, windowID string, index int, replace bool) error {
	args := []string{"link-window", "-d", "-s", windowID, "-t", fmt.Sprintf("%s:%d", sessionID, index)}
	if replace {
		args = append(args, "-k")
	}
	if _, err := b.Tmux.Run(ctx, args...); err != nil {
		return fmt.Errorf("link window to %d: %w", index, err)
	}
	return nil
}

// FinishSession selects the saved active window.
func (b Builder) FinishSession(ctx context.Context, sessionID string, s Session) error {
	i := slices.IndexFunc(s.Windows, func(ref WindowRef) bool { return ref.Active })
	if i < 0 {
		return nil
	}
	if _, err := b.Tmux.Run(ctx, "select-window", "-t", fmt.Sprintf("%s:%d", sessionID, s.Windows[i].Index)); err != nil {
		return fmt.Errorf("select window: %w", err)
	}
	return nil
}

// CleanupOwned kills every session carrying the builder's token and reports
// whether there was one. Matching by token rather than by ID covers a
// restore interrupted before it recorded the session ID.
func (b Builder) CleanupOwned(ctx context.Context) (bool, error) {
	out, err := b.Tmux.Run(ctx, "list-sessions", "-F", "#{session_id}\t#{"+kwttmux.CreationTokenOption+"}")
	if err != nil {
		if state, _, _ := b.Tmux.Probe(ctx); state == ServerAbsent {
			return false, nil
		}
		return false, fmt.Errorf("list sessions: %w", err)
	}
	killed := false
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		id, token, _ := strings.Cut(line, "\t")
		if b.Token == "" || token != b.Token {
			continue
		}
		if _, err := b.Tmux.Run(ctx, "kill-session", "-t", id); err != nil {
			return killed, fmt.Errorf("remove restored session: %w", err)
		}
		killed = true
	}
	return killed, nil
}
