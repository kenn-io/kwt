package rex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrWindowNotFound reports a window that no session holds.
var ErrWindowNotFound = errors.New("no Rex session has that window ID")

// controlMethods are the layout requests Control forwards, for a window's
// tiled layout and its floating layers. Each acts on the controlled
// session; everything else is refused.
var controlMethods = map[string]bool{
	"session.new_split":           true,
	"session.resize_pane":         true,
	"session.focus_block":         true,
	"session.focus_direction":     true,
	"session.focus_next_pane":     true,
	"session.focus_previous_pane": true,
	"session.zoom_block":          true,
	"block.close":                 true,
	"session.swap_blocks":         true,
	"session.new_layer":           true,
	"session.float_block":         true,
	"session.move_block":          true,
	"session.resize_layer":        true,
	"session.raise_layer":         true,
	"session.lower_layer":         true,
	"session.set_layer_state":     true,
	"session.close_layer":         true,
}

// terminalChanges are the terminal block events that change a pane's name.
var terminalChanges = map[string]bool{
	"title_changed":   true,
	"pwd_changed":     true,
	"process_changed": true,
}

// maxControlLine bounds one request line. Layout requests are small.
const maxControlLine = 1 << 20

// Control follows the session that holds windowID for a client that draws
// the window itself, such as Ghosthub. It writes one JSON object per line:
//
//	{"type":"view","view":SESSION_VIEW,"terminals":{BLOCK_ID:{"name",...}}}
//	{"type":"reply","id":ID,"result":RESULT} or {"type":"reply","id":ID,"error":{"code","message"}}
//	{"type":"ended"}
//
// A view comes first and again after each change to the session's layout or
// to a pane's name; changes during one read cost one more read. It reads
// requests, one per line, as {"id":ID,"method":METHOD,"params":{...}} for
// the methods in controlMethods, and fills in the session. It returns nil
// when the session ends or in closes.
func Control(ctx context.Context, client *Client, windowID string, in io.Reader, out io.Writer) error {
	sessionID, view, err := client.sessionWithWindow(ctx, windowID)
	if err != nil {
		return err
	}
	c := &controller{client: client, sessionID: sessionID, windowID: windowID, out: out}
	// Requests still running when Control returns are canceled, then
	// waited for, so none writes after it returns.
	defer c.stopRequests()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := c.writeView(ctx, view); err != nil {
		return err
	}

	inputDone := make(chan error, 1)
	go func() { inputDone <- c.readRequests(ctx, in) }()

	// A read reports its result as a refresh so that waiting for one that
	// no longer matters is not an ignored error.
	refreshed := make(chan refresh, 1)
	reading, again := false, false
	startRead := func() {
		if reading {
			again = true
			return
		}
		reading = true
		go func() { refreshed <- refresh{err: c.refresh(ctx)} }()
	}
	defer func() {
		if reading {
			cancel()
			<-refreshed
		}
	}()
	// ended is the last line: requests and reads still running stop first,
	// so no reply or view follows it.
	end := func() error {
		cancel()
		if reading {
			<-refreshed
			reading = false
		}
		c.stopRequests()
		return c.write(map[string]any{"type": "ended"})
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-inputDone:
			return err
		case result := <-refreshed:
			reading = false
			err := result.err
			if IsCode(err, "session_not_found") {
				return end()
			}
			if err != nil {
				return err
			}
			if again {
				again = false
				startRead()
			}
		case event, ok := <-client.Events():
			if !ok {
				return ErrDisconnected
			}
			if event.Type == EventOverflow {
				startRead()
				continue
			}
			if event.SessionID != sessionID {
				continue
			}
			switch event.Type {
			case "session_destroyed":
				return end()
			case "session_view_changed":
				startRead()
			case "block_event":
				var block struct {
					Creator string `json:"creator_name"`
					Name    string `json:"name"`
				}
				if json.Unmarshal(event.Payload, &block) == nil &&
					block.Creator == terminalCreator && terminalChanges[block.Name] {
					startRead()
				}
			}
		}
	}
}

// sessionWithWindow attaches to the session that holds windowID and
// returns its view. It leaves every other session it looked in.
func (c *Client) sessionWithWindow(ctx context.Context, windowID string) (string, json.RawMessage, error) {
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := c.Call(ctx, "session.list", map[string]any{}, &list); err != nil {
		return "", nil, err
	}
	for _, s := range list.Sessions {
		var attached struct {
			View json.RawMessage `json:"view"`
		}
		err := c.Call(ctx, "session.attach", map[string]string{"session_id": s.SessionID}, &attached)
		if IsCode(err, "session_not_found") {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		var view sessionView
		if err := json.Unmarshal(attached.View, &view); err != nil {
			return "", nil, fmt.Errorf("read session %s: %w", s.SessionID, err)
		}
		for _, w := range view.Windows {
			if w.WindowID == windowID {
				return s.SessionID, attached.View, nil
			}
		}
		err = c.Call(ctx, "session.detach", map[string]string{"session_id": s.SessionID}, nil)
		if err != nil && !IsCode(err, "session_not_found") {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("%w: %s", ErrWindowNotFound, windowID)
}

type refresh struct{ err error }

type controller struct {
	client    *Client
	sessionID string
	windowID  string

	mu  sync.Mutex
	out io.Writer

	requestsMu sync.Mutex
	stopped    bool
	requests   sync.WaitGroup
}

// startRequest reserves a place for one request, unless Control is
// returning.
func (c *controller) startRequest() bool {
	c.requestsMu.Lock()
	defer c.requestsMu.Unlock()
	if c.stopped {
		return false
	}
	c.requests.Add(1)
	return true
}

func (c *controller) stopRequests() {
	c.requestsMu.Lock()
	c.stopped = true
	c.requestsMu.Unlock()
	c.requests.Wait()
}

func (c *controller) write(line any) error {
	data, err := json.Marshal(line)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.out.Write(append(data, '\n'))
	return err
}

func (c *controller) refresh(ctx context.Context) error {
	var view json.RawMessage
	if err := c.client.Call(ctx, "session.view", map[string]string{"session_id": c.sessionID}, &view); err != nil {
		return err
	}
	return c.writeView(ctx, view)
}

// writeView writes the session's view with the names of the controlled
// window's terminals.
func (c *controller) writeView(ctx context.Context, raw json.RawMessage) error {
	var view sessionView
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("read session %s: %w", c.sessionID, err)
	}
	terminals := map[string]terminalDetail{}
	for _, w := range view.Windows {
		if w.WindowID != c.windowID {
			continue
		}
		for _, layer := range w.Layers {
			for _, b := range layer.Blocks {
				if b.CreatorName != terminalCreator {
					continue
				}
				detail, err := c.client.describeTerminal(ctx, c.sessionID, b.BlockID, b.Label)
				if err != nil {
					return err
				}
				terminals[b.BlockID] = detail
			}
		}
	}
	return c.write(map[string]any{"type": "view", "view": raw, "terminals": terminals})
}

type controlRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
}

// readRequests runs each request line, in order, until in ends. A later
// request can depend on an earlier one, such as two focus moves.
func (c *controller) readRequests(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), maxControlLine)
	for scanner.Scan() {
		// Reserve before decoding, so a malformed line's reply cannot
		// follow ended either.
		if !c.startRequest() {
			return nil
		}
		err := c.handleLine(ctx, scanner.Bytes())
		c.requests.Done()
		if err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (c *controller) handleLine(ctx context.Context, line []byte) error {
	var req controlRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return c.reply(nil, nil, &Error{Code: "malformed_request", Message: err.Error()})
	}
	return c.handle(ctx, req)
}

func (c *controller) handle(ctx context.Context, req controlRequest) error {
	if !controlMethods[req.Method] {
		return c.reply(req.ID, nil, &Error{
			Code:    "method_not_allowed",
			Message: fmt.Sprintf("kwt rex control does not forward %s", req.Method),
		})
	}
	params := req.Params
	if params == nil {
		params = map[string]any{}
	}
	params["session_id"] = c.sessionID
	var result json.RawMessage
	err := c.client.Call(ctx, req.Method, params, &result)
	var rexErr *Error
	switch {
	case errors.As(err, &rexErr):
		return c.reply(req.ID, nil, rexErr)
	case err != nil:
		return c.reply(req.ID, nil, &Error{Code: "request_failed", Message: err.Error()})
	}
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	return c.reply(req.ID, result, nil)
}

func (c *controller) reply(id, result json.RawMessage, rexErr *Error) error {
	// A request too malformed to have an ID gets a null one.
	line := map[string]any{"type": "reply", "id": id}
	if rexErr != nil {
		line["error"] = map[string]string{"code": rexErr.Code, "message": rexErr.Message}
	} else {
		line["result"] = result
	}
	return c.write(line)
}
