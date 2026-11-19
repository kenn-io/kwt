package tmuxrestore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	kwttmux "go.kenn.io/kwt/internal/tmux"
)

// Pane is one saved pane, keyed by its index in the window. tmux assigns
// layout cells by pane index, so the index is what ties a saved pane to its
// place in the layout.
type Pane struct {
	Index       int     `json:"index"`
	ID          string  `json:"id"`
	Left        int     `json:"left"`
	Top         int     `json:"top"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Active      bool    `json:"active,omitempty"`
	Dir         string  `json:"dir"`
	Resume      *Record `json:"resume,omitempty"`
	PresetAgent string  `json:"preset_agent,omitempty"`
}

// Window is one saved window. A window linked into several sessions is saved
// once and referenced from each.
type Window struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	AutoRename bool   `json:"auto_rename,omitempty"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Layout     string `json:"layout"`
	Zoomed     bool   `json:"zoomed,omitempty"`
	Panes      []Pane `json:"panes"`
}

// WindowRef places a window in a session.
type WindowRef struct {
	Key    string `json:"key"`
	Index  int    `json:"index"`
	Active bool   `json:"active,omitempty"`
}

// Session is one saved session.
type Session struct {
	ID                  string      `json:"id"`
	Name                string      `json:"name"`
	Created             string      `json:"created"`
	Path                string      `json:"path"`
	WorkspaceIdentity   string      `json:"workspace_identity,omitempty"`
	WorkspaceGeneration string      `json:"workspace_generation,omitempty"`
	Windows             []WindowRef `json:"windows"`
}

// Snapshot is the state of one tmux server.
type Snapshot struct {
	Socket   Socket            `json:"socket"`
	Server   ServerID          `json:"server"`
	SavedAt  int64             `json:"saved_at"`
	Sessions []Session         `json:"sessions"`
	Windows  map[string]Window `json:"windows"`
	// Gone maps a pane ID and conversation ID to when a save first found
	// that conversation's agent process gone.
	Gone map[string]int64 `json:"gone,omitempty"`
}

// paneFields lists the list-panes format fields in order. The resume record
// is last; JSON escapes tabs and newlines, so it cannot split a line.
var paneFields = []string{
	"#{session_id}", "#{session_name}", "#{session_created}", "#{session_path}",
	"#{@kwt-workspace-id}", "#{@kwt-workspace-generation}",
	"#{window_id}", "#{window_index}", "#{window_name}", "#{automatic-rename}",
	"#{window_width}", "#{window_height}", "#{window_layout}",
	"#{window_active}", "#{window_zoomed_flag}",
	"#{pane_index}", "#{pane_left}", "#{pane_top}", "#{pane_width}", "#{pane_height}",
	"#{pane_active}", "#{pane_current_path}", "#{pane_id}",
	"#{" + kwttmux.PaneAgentOption + "}", "#{" + ResumeOption + "}",
}

var paneFormat = strings.Join(paneFields, "\t")

// ReadSnapshot reads every session on the server. The server ID is read in
// the same call so the snapshot cannot mix two servers.
func ReadSnapshot(ctx context.Context, t Tmux) (Snapshot, error) {
	output, err := t.Run(ctx, "list-panes", "-a", "-F", "#{pid}\t#{start_time}\t"+paneFormat)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read tmux panes: %w", err)
	}
	var server ServerID
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	rest := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return Snapshot{}, fmt.Errorf("unexpected tmux pane line")
		}
		server = ServerID{PID: fields[0], StartTime: fields[1]}
		rest = append(rest, fields[2])
	}
	snap, err := parsePanes(server, strings.Join(rest, "\n"))
	if err != nil {
		return Snapshot{}, err
	}
	snap.Socket = t.Socket
	return snap, nil
}

func parsePanes(server ServerID, output string) (Snapshot, error) {
	snap := Snapshot{Server: server, Windows: map[string]Window{}}
	sessionIndex := map[string]int{}
	seenRef := map[string]bool{}
	seenPane := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", len(paneFields))
		if len(f) != len(paneFields) {
			return Snapshot{}, fmt.Errorf("unexpected tmux pane line with %d fields", len(f))
		}
		ints, err := atois(f[7], f[10], f[11], f[15], f[16], f[17], f[18], f[19])
		if err != nil {
			return Snapshot{}, err
		}
		windowIndex, width, height := ints[0], ints[1], ints[2]
		key := server.PID + ":" + f[6]

		i, ok := sessionIndex[f[0]]
		if !ok {
			i = len(snap.Sessions)
			sessionIndex[f[0]] = i
			snap.Sessions = append(snap.Sessions, Session{
				ID: f[0], Name: f[1], Created: f[2], Path: f[3],
				WorkspaceIdentity: f[4], WorkspaceGeneration: f[5],
			})
		}
		refKey := f[0] + "\x00" + key
		if !seenRef[refKey] {
			seenRef[refKey] = true
			snap.Sessions[i].Windows = append(snap.Sessions[i].Windows, WindowRef{
				Key: key, Index: windowIndex, Active: f[13] == "1",
			})
		}

		window, ok := snap.Windows[key]
		if !ok {
			window = Window{
				Key: key, Name: f[8], AutoRename: f[9] == "1",
				Width: width, Height: height, Layout: f[12], Zoomed: f[14] == "1",
			}
		}
		paneKey := key + "\x00" + f[15]
		if !seenPane[paneKey] {
			seenPane[paneKey] = true
			pane := Pane{
				Index: ints[3], Left: ints[4], Top: ints[5], Width: ints[6], Height: ints[7],
				Active: f[20] == "1", Dir: f[21], ID: f[22], PresetAgent: f[23],
			}
			if f[24] != "" {
				if record, err := DecodeRecord(f[24]); err == nil {
					pane.Resume = record
				}
			}
			window.Panes = append(window.Panes, pane)
		}
		snap.Windows[key] = window
	}
	for key, window := range snap.Windows {
		slices.SortFunc(window.Panes, func(a, b Pane) int { return cmp.Compare(a.Index, b.Index) })
		snap.Windows[key] = window
	}
	for i := range snap.Sessions {
		slices.SortFunc(snap.Sessions[i].Windows, func(a, b WindowRef) int { return cmp.Compare(a.Index, b.Index) })
	}
	return snap, nil
}

func atois(values ...string) ([]int, error) {
	out := make([]int, len(values))
	for i, value := range values {
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("unexpected tmux number %q: %w", value, err)
		}
		out[i] = n
	}
	return out, nil
}
