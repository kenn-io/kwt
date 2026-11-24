package rex

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// fakeServer speaks enough of Rex's control and data protocols to exercise
// the client: it answers requests from a table and replays one terminal
// stream to each data connection.
type fakeServer struct {
	t *testing.T
	// endpoint is the server's address in REX_SERVER form.
	endpoint string

	mu      sync.Mutex
	calls   []fakeCall
	answers map[string]func(payload json.RawMessage) (any, *Error)
	// stream is what each data connection receives after WELCOME.
	stream [][]byte
	nonce  uint64
	// written receives each com.superlogical.terminal.write payload.
	written chan string
	// controls are the open control connections, which push sends events to.
	controls map[*websocket.Conn]struct{}
	seq      int
	// silentData makes each data connection read the client's HELLO and
	// never answer it.
	silentData bool
	// streamDelay holds the stream after WELCOME, as an idle terminal does.
	streamDelay time.Duration
	// welcomeDelay holds the control connection's welcome, as a slow
	// server does.
	welcomeDelay time.Duration
}

type fakeCall struct {
	Method  string
	Payload json.RawMessage
}

const fakeTicket = "ticket-bytes"

// newFakeServer listens on a Unix socket, as a local Rex server does.
func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes; t.TempDir can exceed it.
	dir, err := os.MkdirTemp("", "rex")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	return serveFake(t, listener, "unix://"+socket)
}

// newFakeNetworkServer listens on a loopback TCP port, as a Rex server on a
// tailnet address does.
func newFakeNetworkServer(t *testing.T) *fakeServer {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return serveFake(t, listener, "http://"+listener.Addr().String())
}

func serveFake(t *testing.T, listener net.Listener, endpoint string) *fakeServer {
	t.Helper()
	f := &fakeServer{
		t:        t,
		endpoint: endpoint,
		answers:  map[string]func(json.RawMessage) (any, *Error){},
		nonce:    42,
		written:  make(chan string, 64),
		controls: map[*websocket.Conn]struct{}{},
	}
	f.answer("data.connect", func(json.RawMessage) (any, *Error) {
		return map[string]any{"ticket": base64.RawURLEncoding.EncodeToString([]byte(fakeTicket))}, nil
	})
	f.answer("com.superlogical.terminal.write", func(p json.RawMessage) (any, *Error) {
		var req struct {
			Args struct {
				Data string `json:"data"`
			} `json:"args"`
		}
		_ = json.Unmarshal(p, &req)
		f.written <- req.Args.Data
		return map[string]any{}, nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/control", f.serveControl)
	mux.HandleFunc("/api/data", f.serveData)
	server := &http.Server{Handler: mux} //nolint:gosec // Test server on a private socket or loopback port.
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return f
}

func (f *fakeServer) answer(method string, fn func(json.RawMessage) (any, *Error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = fn
}

func (f *fakeServer) answerValue(method string, value any) {
	f.answer(method, func(json.RawMessage) (any, *Error) { return value, nil })
}

// push sends a session's own event, such as session_view_changed, to every
// open control connection, framed the way Rex 0.1.0-preview.14 frames it.
func (f *fakeServer) push(sessionID string, event map[string]any) {
	f.t.Helper()
	f.pushServerEvent("session_event", map[string]any{"session_id": sessionID, "event": event})
}

// pushServerEvent sends a server event, such as session_destroyed, to every
// open control connection.
func (f *fakeServer) pushServerEvent(kind string, payload map[string]any) {
	f.t.Helper()
	f.mu.Lock()
	f.seq++
	frame, err := json.Marshal(map[string]any{
		"type":    "event",
		"seq":     f.seq,
		"payload": map[string]any{"type": kind, "payload": payload},
	})
	conns := make([]*websocket.Conn, 0, len(f.controls))
	for conn := range f.controls {
		conns = append(conns, conn)
	}
	f.mu.Unlock()
	require.NoError(f.t, err)
	for _, conn := range conns {
		_ = conn.Write(context.WithoutCancel(f.t.Context()), websocket.MessageText, frame)
	}
}

// dropControls closes every open control connection.
func (f *fakeServer) dropControls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for conn := range f.controls {
		_ = conn.CloseNow()
	}
}

// controlCount reports how many control connections are open.
func (f *fakeServer) controlCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.controls)
}

func (f *fakeServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeServer) callsTo(method string) []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []json.RawMessage
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c.Payload)
		}
	}
	return out
}

func (f *fakeServer) serveControl(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{controlProtocol}})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := context.WithoutCancel(r.Context())
	var hello struct {
		Type string `json:"type"`
	}
	if _, data, err := conn.Read(ctx); err != nil || json.Unmarshal(data, &hello) != nil || hello.Type != "hello" {
		return
	}
	f.mu.Lock()
	delay := f.welcomeDelay
	f.mu.Unlock()
	time.Sleep(delay)
	welcome, _ := json.Marshal(map[string]any{"type": "welcome", "protocol": controlProtocol})
	if conn.Write(ctx, websocket.MessageText, welcome) != nil {
		return
	}
	f.mu.Lock()
	f.controls[conn] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.controls, conn)
		f.mu.Unlock()
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var req struct {
			ID      string          `json:"id"`
			Method  string          `json:"method"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &req) != nil {
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{Method: req.Method, Payload: req.Payload})
		fn := f.answers[req.Method]
		f.mu.Unlock()
		response := map[string]any{"type": "response", "id": req.ID}
		if fn == nil {
			response["payload"] = map[string]any{}
		} else if value, rexErr := fn(req.Payload); rexErr != nil {
			response["error"] = map[string]any{"code": rexErr.Code, "message": rexErr.Message}
		} else {
			response["payload"] = value
		}
		out, _ := json.Marshal(response)
		if conn.Write(ctx, websocket.MessageText, out) != nil {
			return
		}
	}
}

func (f *fakeServer) serveData(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{dataProtocol}})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := context.WithoutCancel(r.Context())
	_, hello, err := conn.Read(ctx)
	if err != nil {
		return
	}
	if len(hello) < 1 || hello[0] != dataHello || string(hello[1:]) != fakeTicket {
		_ = conn.Write(ctx, websocket.MessageBinary, []byte{dataReject, 0x01})
		return
	}
	f.mu.Lock()
	silent, delay := f.silentData, f.streamDelay
	f.mu.Unlock()
	if silent {
		_, _, _ = conn.Read(ctx)
		return
	}
	welcome := make([]byte, 32)
	welcome[0] = dataWelcome
	binary.LittleEndian.PutUint64(welcome[24:], f.nonce)
	if conn.Write(ctx, websocket.MessageBinary, welcome) != nil {
		return
	}
	time.Sleep(delay)
	f.mu.Lock()
	stream := f.stream
	f.mu.Unlock()
	for _, chunk := range stream {
		if conn.Write(ctx, websocket.MessageBinary, chunk) != nil {
			return
		}
	}
	// Hold the stream open until the client goes away.
	_, _, _ = conn.Read(ctx)
}
