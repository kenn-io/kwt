package rex

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Terminal is one terminal block in a Rex session window.
type Terminal struct {
	SessionID    string `json:"session_id"`
	SessionLabel string `json:"session_label"`
	WindowID     string `json:"window_id"`
	WindowLabel  string `json:"window_label"`
	// WindowIndex is the window's 1-based position in its session.
	WindowIndex int    `json:"window_index"`
	BlockID     string `json:"block_id"`
	Label       string `json:"label"`
	// Title is the title the program running in the terminal last set.
	Title string `json:"title,omitempty"`
	// Program is the name of the terminal's foreground process.
	Program string `json:"program,omitempty"`
	// Cwd is the foreground process's working directory.
	Cwd string `json:"cwd,omitempty"`
	// Name is what Rex shows for the terminal: its title, or else its
	// program and directory, with the account's home shown as ~.
	Name string `json:"name"`
	// Focused marks the block that has focus in its window.
	Focused bool `json:"focused,omitempty"`
}

type sessionView struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label"`
	Windows   []struct {
		WindowID       string `json:"window_id"`
		Label          string `json:"label"`
		FocusedBlockID string `json:"focused_block_id"`
		Layers         []struct {
			Blocks []struct {
				BlockID     string `json:"block_id"`
				CreatorName string `json:"creator_name"`
				Label       string `json:"label"`
			} `json:"blocks"`
		} `json:"layers"`
	} `json:"windows"`
}

// Terminals lists the terminal blocks placed in every session's windows.
// A session that ends while it is being listed is left out.
func (c *Client) Terminals(ctx context.Context) ([]Terminal, error) {
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := c.Call(ctx, "session.list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	terminals := []Terminal{}
	for _, s := range list.Sessions {
		var attached struct {
			View sessionView `json:"view"`
		}
		err := c.Call(ctx, "session.attach", map[string]string{"session_id": s.SessionID}, &attached)
		if IsCode(err, "session_not_found") {
			continue
		}
		if err != nil {
			return nil, err
		}
		found, err := c.terminalsIn(ctx, attached.View)
		// Detach even after a failure so the server stops sending this
		// connection the session's events.
		detachErr := c.Call(ctx, "session.detach", map[string]string{"session_id": s.SessionID}, nil)
		if err != nil {
			return nil, err
		}
		if detachErr != nil && !IsCode(detachErr, "session_not_found") {
			return nil, detachErr
		}
		terminals = append(terminals, found...)
	}
	return terminals, nil
}

// ErrTerminalNotFound reports a block that no session's windows hold.
var ErrTerminalNotFound = errors.New("no Rex terminal has that block ID")

// Terminal finds the terminal block blockID. Block IDs are unique across
// a server's sessions.
func (c *Client) Terminal(ctx context.Context, blockID string) (Terminal, error) {
	terminals, err := c.Terminals(ctx)
	if err != nil {
		return Terminal{}, err
	}
	for _, t := range terminals {
		if t.BlockID == blockID {
			return t, nil
		}
	}
	return Terminal{}, fmt.Errorf("%w: %s", ErrTerminalNotFound, blockID)
}

func (c *Client) terminalsIn(ctx context.Context, view sessionView) ([]Terminal, error) {
	var found []Terminal
	for i, w := range view.Windows {
		for _, layer := range w.Layers {
			for _, b := range layer.Blocks {
				if b.CreatorName != terminalCreator {
					continue
				}
				var title struct {
					Title string `json:"title"`
				}
				if err := c.blockDetail(ctx, view.SessionID, b.BlockID, "title", &title); err != nil {
					return nil, err
				}
				var process struct {
					Foreground *struct {
						Name string `json:"name"`
						Cwd  string `json:"cwd"`
					} `json:"foreground"`
					Home string `json:"home"`
				}
				if err := c.blockDetail(ctx, view.SessionID, b.BlockID, "process", &process); err != nil {
					return nil, err
				}
				var program, cwd string
				if fg := process.Foreground; fg != nil {
					program, cwd = fg.Name, fg.Cwd
				}
				found = append(found, Terminal{
					SessionID:    view.SessionID,
					SessionLabel: view.Label,
					WindowID:     w.WindowID,
					WindowLabel:  w.Label,
					WindowIndex:  i + 1,
					BlockID:      b.BlockID,
					Label:        b.Label,
					Title:        title.Title,
					Program:      program,
					Cwd:          cwd,
					Name:         displayName(title.Title, program, cwd, process.Home, b.Label),
					Focused:      b.BlockID == w.FocusedBlockID,
				})
			}
		}
	}
	return found, nil
}

// blockDetail calls a terminal block's read-only method. A block that closed
// since the view keeps its row until the next listing, without details, as
// does a block on a server too old to offer the method.
func (c *Client) blockDetail(ctx context.Context, session, block, method string, out any) error {
	err := c.BlockCall(ctx, session, block, method, map[string]any{}, out)
	if err != nil && !IsCode(err, "not_found") && !IsCode(err, "block_not_found") &&
		!IsCode(err, "method_not_found") {
		return fmt.Errorf("%s of %s: %w", method, block, err)
	}
	return nil
}

// displayName names a terminal the way Rex does: by the title its program
// set, else by its foreground program and directory, else by its label.
func displayName(title, program, cwd, home, label string) string {
	if title != "" {
		return title
	}
	if program == "" {
		return label
	}
	if cwd == "" {
		return program
	}
	if home != "" {
		if cwd == home {
			cwd = "~"
		} else if rel, ok := strings.CutPrefix(cwd, strings.TrimSuffix(home, "/")+"/"); ok {
			cwd = "~/" + rel
		}
	}
	return program + " " + cwd
}
