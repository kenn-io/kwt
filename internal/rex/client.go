// Package rex reads and attaches to terminals on a Rex server.
//
// Rex has no client that runs inside another terminal. Its CLI manages
// sessions, and its apps draw terminals from WebSocket data connections.
// This package speaks the server's control protocol (rex.control.v1) and
// terminal data protocol (rex.data.terminal.v1) over the server's Unix
// socket, so a terminal program such as Ghosthub can show one Rex terminal.
package rex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

const (
	controlProtocol = "rex.control.v1"
	dataProtocol    = "rex.data.terminal.v1"
	// apiRevision is the control API revision this client was written
	// against (Rex 0.1.0-preview.14).
	apiRevision = 12
	// Rex limits control messages to 1 MiB and data messages to 64 KiB.
	readLimit = 16 << 20

	terminalCreator = "com.superlogical.terminal"
)

// Error is a request that Rex answered with an error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// ErrDisconnected reports a control connection that the server closed.
var ErrDisconnected = errors.New("lost the connection to the Rex server")

// Client is one control connection to a Rex server.
type Client struct {
	conn   *websocket.Conn
	next   atomic.Uint64
	mu     sync.Mutex
	wait   map[string]chan response
	closed chan struct{}
}

type response struct {
	payload json.RawMessage
	err     error
}

func dial(ctx context.Context, socket, path, protocol string) (*websocket.Conn, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	conn, resp, err := websocket.Dial(ctx, "ws://rex"+path, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: transport},
		Subprotocols: []string{protocol},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Rex server at %s: %w", socket, err)
	}
	conn.SetReadLimit(readLimit)
	return conn, nil
}

// Dial opens a control connection to the server listening on socket.
func Dial(ctx context.Context, socket string) (*Client, error) {
	conn, err := dial(ctx, socket, "/api/control", controlProtocol)
	if err != nil {
		return nil, err
	}
	hello, err := json.Marshal(map[string]any{
		"type":         "hello",
		"api_revision": apiRevision,
		"client":       map[string]string{"kind": "cli", "name": "kwt"},
	})
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("rex hello: %w", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("rex hello: %w", err)
	}
	var welcome struct {
		Type     string `json:"type"`
		Protocol string `json:"protocol"`
		Error    *Error `json:"error"`
	}
	if err := json.Unmarshal(data, &welcome); err != nil || welcome.Type != "welcome" {
		_ = conn.CloseNow()
		if welcome.Error != nil {
			return nil, welcome.Error
		}
		return nil, fmt.Errorf("unexpected first message from Rex: %.200s", data)
	}
	c := &Client{conn: conn, wait: map[string]chan response{}, closed: make(chan struct{})}
	// The read loop outlives Dial's context; Close ends it.
	go c.read(context.WithoutCancel(ctx))
	return c, nil
}

func (c *Client) read(ctx context.Context) {
	defer func() {
		c.mu.Lock()
		for id, ch := range c.wait {
			ch <- response{err: ErrDisconnected}
			delete(c.wait, id)
		}
		c.mu.Unlock()
		close(c.closed)
	}()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
			Error   *Error          `json:"error"`
		}
		// Events are not used; only responses complete a call.
		if json.Unmarshal(data, &msg) != nil || msg.Type != "response" {
			continue
		}
		c.mu.Lock()
		ch := c.wait[msg.ID]
		delete(c.wait, msg.ID)
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		if msg.Error != nil {
			ch <- response{err: msg.Error}
		} else {
			ch <- response{payload: msg.Payload}
		}
	}
}

// Call sends one request and decodes its payload into result, if result is
// not nil.
func (c *Client) Call(ctx context.Context, method string, payload, result any) error {
	id := strconv.FormatUint(c.next.Add(1), 10)
	request, err := json.Marshal(map[string]any{"type": "request", "id": id, "method": method, "payload": payload})
	if err != nil {
		return err
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	c.wait[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
	}
	select {
	case <-c.closed:
		forget()
		return fmt.Errorf("%s: %w", method, ErrDisconnected)
	default:
	}
	if err := c.conn.Write(ctx, websocket.MessageText, request); err != nil {
		forget()
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w", method, r.err)
		}
		if result == nil || len(r.payload) == 0 {
			return nil
		}
		return json.Unmarshal(r.payload, result)
	case <-ctx.Done():
		forget()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// BlockCall calls a method that a terminal block provides, such as write.
func (c *Client) BlockCall(ctx context.Context, sessionID, blockID, method string, args, result any) error {
	payload := map[string]any{"session_id": sessionID, "block_id": blockID}
	if args != nil {
		payload["args"] = args
	}
	return c.Call(ctx, terminalCreator+"."+method, payload, result)
}

// Close ends the control connection. Rex keeps every session running.
func (c *Client) Close() error {
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	<-c.closed
	return err
}

// IsCode reports whether err is a Rex error with code.
func IsCode(err error, code string) bool {
	var rexErr *Error
	return errors.As(err, &rexErr) && rexErr.Code == code
}
