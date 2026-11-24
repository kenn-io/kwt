package rex

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
)

// shellFlavor is the block flavor for the user's login shell.
const shellFlavor = "com.superlogical.terminal.shell"

// NewWindowOptions describes a new Rex window holding one shell.
type NewWindowOptions struct {
	// SessionID names the session to add the window to. Empty creates a
	// session for it.
	SessionID string
	// Label names the new session, or the new window when SessionID is
	// set. Empty lets Rex choose.
	Label string
	// Cwd is the shell's working directory, relative to this process's
	// directory unless absolute. A server reached at a network address
	// needs an absolute path on its own machine. Empty lets Rex choose.
	Cwd string
}

// Created identifies a new window and its session.
type Created struct {
	SessionID string `json:"session_id"`
	WindowID  string `json:"window_id"`
}

// NewWindow creates a window with one shell in it, in a new session unless
// options name one. A window added to a session becomes its active window.
func NewWindow(ctx context.Context, client *Client, options NewWindowOptions) (Created, error) {
	block := map[string]any{"flavor": shellFlavor}
	if cwd := options.Cwd; cwd != "" {
		switch {
		case client.network && !path.IsAbs(cwd):
			// Rex servers run on macOS and Linux, and this machine's
			// directory means nothing on another one.
			return Created{}, fmt.Errorf("the directory %q must be an absolute path on the Rex server's machine", cwd)
		case !client.network:
			// The server has its own working directory.
			abs, err := filepath.Abs(cwd)
			if err != nil {
				return Created{}, err
			}
			cwd = abs
		}
		block["options"] = map[string]any{"cwd": cwd}
	}
	layout := map[string]any{"block": block}

	if options.SessionID != "" {
		request := map[string]any{"session_id": options.SessionID, "layout": layout, "focus": true}
		if options.Label != "" {
			request["window_label"] = options.Label
		}
		// Rex changes a session only for a connection attached to it.
		attach := map[string]string{"session_id": options.SessionID}
		if err := client.Call(ctx, "session.attach", attach, nil); err != nil {
			return Created{}, err
		}
		var reply struct {
			WindowID string `json:"window_id"`
		}
		if err := client.Call(ctx, "session.new_window", request, &reply); err != nil {
			return Created{}, err
		}
		return Created{SessionID: options.SessionID, WindowID: reply.WindowID}, nil
	}

	request := map[string]any{"initial_windows": []any{map[string]any{"layout": layout}}}
	if options.Label != "" {
		request["label"] = options.Label
	}
	var reply struct {
		SessionID      string `json:"session_id"`
		InitialWindows []struct {
			WindowID string `json:"window_id"`
		} `json:"initial_windows"`
	}
	if err := client.Call(ctx, "session.create", request, &reply); err != nil {
		return Created{}, err
	}
	if len(reply.InitialWindows) != 1 {
		return Created{}, errors.New("session.create did not report the new window")
	}
	return Created{SessionID: reply.SessionID, WindowID: reply.InitialWindows[0].WindowID}, nil
}
