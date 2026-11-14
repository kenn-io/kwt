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

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// fakeServer speaks enough of Rex's control and data protocols to exercise
// the client: it answers requests from a table and replays one terminal
// stream to each data connection.
type fakeServer struct {
	t      *testing.T
	socket string

	mu      sync.Mutex
	calls   []fakeCall
	answers map[string]func(payload json.RawMessage) (any, *Error)
	// stream is what each data connection receives after WELCOME.
	stream [][]byte
	nonce  uint64
	// written receives each com.superlogical.terminal.write payload.
	written chan string
}

type fakeCall struct {
	Method  string
	Payload json.RawMessage
}

const fakeTicket = "ticket-bytes"

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes; t.TempDir can exceed it.
	dir, err := os.MkdirTemp("", "rex")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeServer{
		t:       t,
		socket:  filepath.Join(dir, "s.sock"),
		answers: map[string]func(json.RawMessage) (any, *Error){},
		nonce:   42,
		written: make(chan string, 64),
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

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", f.socket)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/control", f.serveControl)
	mux.HandleFunc("/api/data", f.serveData)
	server := &http.Server{Handler: mux} //nolint:gosec // Test server on a private Unix socket.
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
	welcome, _ := json.Marshal(map[string]any{"type": "welcome", "protocol": controlProtocol})
	if conn.Write(ctx, websocket.MessageText, welcome) != nil {
		return
	}
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
	welcome := make([]byte, 32)
	welcome[0] = dataWelcome
	binary.LittleEndian.PutUint64(welcome[24:], f.nonce)
	if conn.Write(ctx, websocket.MessageBinary, welcome) != nil {
		return
	}
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
