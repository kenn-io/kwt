package tmuxrestore

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Listed describes one restorable entry for front ends.
type Listed struct {
	ID      string   `json:"id"`
	Socket  Socket   `json:"socket"`
	Session string   `json:"session"`
	SavedAt int64    `json:"saved_at"`
	Windows int      `json:"windows"`
	Panes   int      `json:"panes"`
	Agents  []string `json:"agents"`
	// Interrupted names the agent of each pane whose turn the server's loss
	// cut short; "restore --continue" continues those.
	Interrupted []string   `json:"interrupted"`
	NameTaken   bool       `json:"name_taken"`
	Worktree    bool       `json:"worktree"`
	State       EntryState `json:"state"`
}

// ListDeps are the inputs to SaveAndList.
type ListDeps struct {
	Executable string
	Store      Store
	Processes  ProcessTable
	Protection Protection
	ClaudeDir  string
	// Home and Getenv locate conversations, to tell which were cut short.
	Home   string
	Getenv func(string) string
	Now    func() time.Time
}

// SaveAndList saves each socket, then lists its restorable entries. A socket
// that fails is reported and the rest still listed.
func SaveAndList(ctx context.Context, deps ListDeps, sockets []Socket) ([]Listed, []error) {
	listed := []Listed{}
	var errs []error
	getenv := deps.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	seen := map[string]bool{}
	for _, socket := range sockets {
		if seen[socket.Path] {
			continue
		}
		seen[socket.Path] = true
		tm := Tmux{Executable: deps.Executable, Socket: socket}
		result, err := Save(ctx, SaveDeps{
			Tmux: tm, Store: deps.Store, Processes: deps.Processes,
			Protection: deps.Protection, ClaudeDir: deps.ClaudeDir, Now: deps.Now,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("save %s: %w", socket.Path, err))
		}
		err = deps.Store.WithLock(ctx, socket, func(f *Files) error {
			live := map[string]bool{}
			if f.Live != nil && result.Server == ServerRunning {
				for _, session := range f.Live.Sessions {
					live[session.Name] = true
				}
			}
			for _, entry := range f.Restorable.Entries {
				listed = append(listed, describe(f.Restorable, entry, live[entry.Session.Name], deps.Home, getenv))
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s: %w", socket.Path, err))
		}
	}
	return listed, errs
}

func describe(r Restorable, entry Entry, nameTaken bool, home string, getenv func(string) string) Listed {
	item := Listed{
		ID: entry.ID, Socket: r.Socket, Session: entry.Session.Name,
		SavedAt: entry.SavedAt, Windows: len(entry.Session.Windows),
		Agents: []string{}, Interrupted: []string{}, NameTaken: nameTaken,
		Worktree: entry.Session.WorkspaceIdentity != "", State: entry.State,
	}
	for _, ref := range entry.Session.Windows {
		window := r.Windows[ref.Key].Window
		item.Panes += len(window.Panes)
		for _, pane := range window.Panes {
			if pane.Resume == nil {
				continue
			}
			item.Agents = append(item.Agents, pane.Resume.Agent)
			if _, turn := latestTurn(*pane.Resume, home, getenv); cutShort(pane, turn) {
				item.Interrupted = append(item.Interrupted, pane.Resume.Agent)
			}
		}
	}
	return item
}
