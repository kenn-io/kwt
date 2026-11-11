package rex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallReturnsRexErrors(t *testing.T) {
	server := newFakeServer(t)
	server.answer("session.attach", func(json.RawMessage) (any, *Error) {
		return nil, &Error{Code: "session_not_found", Message: "no such session"}
	})
	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	defer client.Close()

	err = client.Call(t.Context(), "session.attach", map[string]string{"session_id": "s"}, nil)
	var rexErr *Error
	require.ErrorAs(t, err, &rexErr)
	assert.Equal(t, "session_not_found", rexErr.Code)
}

func view(sessionID, label string, windows ...map[string]any) map[string]any {
	return map[string]any{"view": map[string]any{
		"session_id": sessionID,
		"label":      label,
		"windows":    windows,
	}}
}

func window(id, label, focused string, blocks ...map[string]any) map[string]any {
	return map[string]any{
		"window_id":        id,
		"label":            label,
		"focused_block_id": focused,
		"layers":           []any{map[string]any{"kind": "tiled", "blocks": blocks}},
	}
}

func block(id, creator, label string) map[string]any {
	return map[string]any{"block_id": id, "creator_name": creator, "label": label}
}

func TestTerminalsListsTerminalBlocksInEverySession(t *testing.T) {
	server := newFakeServer(t)
	server.answerValue("session.list", map[string]any{"sessions": []any{
		map[string]any{"session_id": "session:a", "label": "work"},
		map[string]any{"session_id": "session:b", "label": "gone"},
	}})
	server.answer("session.attach", func(p json.RawMessage) (any, *Error) {
		var req struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(p, &req)
		if req.SessionID == "session:b" {
			// The session ended between the list and the attach.
			return nil, &Error{Code: "session_not_found", Message: "gone"}
		}
		return view("session:a", "work",
			window("window:1", "editor", "block:2",
				block("block:1", terminalCreator, "shell"),
				block("block:2", terminalCreator, "logs"),
				block("block:3", "com.superlogical.onboarding", "welcome"),
			),
		), nil
	})
	server.answer("com.superlogical.terminal.title", func(p json.RawMessage) (any, *Error) {
		if strings.Contains(string(p), "block:1") {
			return map[string]string{"title": "vim main.go"}, nil
		}
		return map[string]string{"title": ""}, nil
	})

	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	defer client.Close()
	terminals, err := client.Terminals(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []Terminal{
		{
			SessionID: "session:a", SessionLabel: "work",
			WindowID: "window:1", WindowLabel: "editor", WindowIndex: 1,
			BlockID: "block:1", Label: "shell", Title: "vim main.go",
		},
		{
			SessionID: "session:a", SessionLabel: "work",
			WindowID: "window:1", WindowLabel: "editor", WindowIndex: 1,
			BlockID: "block:2", Label: "logs", Focused: true,
		},
	}, terminals)
	assert.Len(t, server.callsTo("session.detach"), 1, "listing leaves the session it attached to")
}

func TestTerminalFindsTheSessionThatHoldsABlock(t *testing.T) {
	server := newFakeServer(t)
	server.answerValue("session.list", map[string]any{"sessions": []any{
		map[string]any{"session_id": "session:a", "label": "work"},
	}})
	server.answerValue("session.attach", view("session:a", "work",
		window("window:1", "editor", "", block("block:1", terminalCreator, "shell"))))
	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	defer client.Close()

	found, err := client.Terminal(t.Context(), "block:1")
	require.NoError(t, err)
	assert.Equal(t, "session:a", found.SessionID)

	_, err = client.Terminal(t.Context(), "block:missing")
	require.ErrorIs(t, err, ErrTerminalNotFound)
}

// syncBuffer collects the attach client's output for a test to inspect.
type syncBuffer struct {
	mu   sync.Mutex
	data strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func rexMessage(nonce uint64, body string) []byte {
	return fmt.Appendf(nil, "\x1b_rex1;%d;%s\x1b\\", nonce, body)
}

func TestAttachPaintsTheScreenThenStreamsOutputUntilTheProgramExits(t *testing.T) {
	server := newFakeServer(t)
	server.answerValue("com.superlogical.terminal.format", map[string]string{"content": "PAINTED"})
	server.stream = [][]byte{
		[]byte("GHOSTSNP-binary"),
		rexMessage(server.nonce, `{"type":"resize","cols":100,"rows":30}`),
		[]byte("live output"),
		rexMessage(server.nonce, `{"type":"child_exited","exit_code":7}`),
	}

	var out syncBuffer
	// Input that stays open, as a terminal's does.
	in, inWriter := io.Pipe()
	t.Cleanup(func() { _ = inWriter.Close() })
	exit, err := Attach(t.Context(), server.socket, AttachOptions{
		SessionID: "session:a",
		BlockID:   "block:1",
		In:        in,
		Out:       &out,
		Size:      Size{Columns: 100, Rows: 30},
	})
	require.NoError(t, err)
	require.NotNil(t, exit)
	assert.Equal(t, 7, *exit)

	got := out.String()
	assert.NotContains(t, got, "GHOSTSNP", "the binary snapshot never reaches the terminal")
	assert.NotContains(t, got, "rex1;", "Rex's own messages never reach the terminal")
	paint := strings.Index(got, "PAINTED")
	live := strings.Index(got, "live output")
	require.GreaterOrEqual(t, paint, 0)
	assert.Greater(t, live, paint, "output follows the first paint")

	methods := server.methods()
	assert.Less(t, indexOf(methods, "session.own_block"), indexOf(methods, "data.connect"),
		"the attach takes the block and sets its size before the snapshot is taken")
	resize := server.callsTo("com.superlogical.terminal.resize")
	require.NotEmpty(t, resize)
	assert.JSONEq(t, `{"session_id":"session:a","block_id":"block:1","args":{"columns":100,"rows":30}}`, string(resize[0]))
}

func TestAttachTypesInputAndFollowsResizes(t *testing.T) {
	server := newFakeServer(t)
	server.answerValue("com.superlogical.terminal.format", map[string]string{"content": ""})
	server.stream = [][]byte{rexMessage(server.nonce, `{"type":"resize"}`)}

	inReader, inWriter := io.Pipe()
	sizes := make(chan Size, 1)
	done := make(chan error, 1)
	go func() {
		_, err := Attach(t.Context(), server.socket, AttachOptions{
			SessionID: "session:a",
			BlockID:   "block:1",
			In:        inReader,
			Out:       io.Discard,
			Size:      Size{Columns: 80, Rows: 24},
			Sizes:     sizes,
		})
		done <- err
	}()

	_, err := inWriter.Write([]byte("ls\r"))
	require.NoError(t, err)
	select {
	case typed := <-server.written:
		assert.Equal(t, "ls\r", typed)
	case <-time.After(5 * time.Second):
		t.Fatal("typed input never reached Rex")
	}

	sizes <- Size{Columns: 120, Rows: 40}
	require.Eventually(t, func() bool {
		for _, p := range server.callsTo("com.superlogical.terminal.resize") {
			if strings.Contains(string(p), `"columns":120`) && strings.Contains(string(p), `"rows":40`) {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	// Closing input detaches; the Rex terminal keeps running.
	require.NoError(t, inWriter.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("attach did not end when its input closed")
	}
	assert.Empty(t, server.callsTo("block.close"))
}

func TestAttachReportsARejectedStream(t *testing.T) {
	server := newFakeServer(t)
	server.answerValue("data.connect", map[string]string{"ticket": "d3Jvbmc"})
	_, err := Attach(t.Context(), server.socket, AttachOptions{
		SessionID: "session:a", BlockID: "block:1",
		In: strings.NewReader(""), Out: io.Discard, Size: Size{Columns: 80, Rows: 24},
	})
	require.Error(t, err)
	assert.False(t, errors.Is(err, io.EOF))
	assert.Contains(t, err.Error(), "refused")
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}
