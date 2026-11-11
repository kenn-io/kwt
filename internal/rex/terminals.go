package rex

import (
	"context"
	"errors"
	"fmt"
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
				err := c.BlockCall(ctx, view.SessionID, b.BlockID, "title", map[string]any{}, &title)
				// A block that closed since the view keeps its row until
				// the next listing; it has no title to read.
				if err != nil && !IsCode(err, "not_found") && !IsCode(err, "block_not_found") {
					return nil, fmt.Errorf("title of %s: %w", b.BlockID, err)
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
					Focused:      b.BlockID == w.FocusedBlockID,
				})
			}
		}
	}
	return found, nil
}
