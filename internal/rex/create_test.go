package rex

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sentPayload(t *testing.T, server *fakeServer, method string) map[string]any {
	t.Helper()
	calls := server.callsTo(method)
	require.Len(t, calls, 1)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(calls[0], &sent))
	return sent
}

func TestNewWindowCreatesASessionWithAShell(t *testing.T) {
	server := newFakeServer(t)
	server.answerValue("session.create", map[string]any{
		"session_id": "session:new",
		"initial_windows": []map[string]any{
			{"window_id": "window:new", "block_ids": []string{"block:1"}, "revision": 1},
		},
	})
	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	defer client.Close()

	created, err := NewWindow(t.Context(), client, NewWindowOptions{Label: "work", Cwd: "/home/user-a/src"})

	require.NoError(t, err)
	assert.Equal(t, Created{SessionID: "session:new", WindowID: "window:new"}, created)
	assert.Equal(t, map[string]any{
		"label": "work",
		"initial_windows": []any{map[string]any{"layout": map[string]any{"block": map[string]any{
			"flavor":  "com.superlogical.terminal.shell",
			"options": map[string]any{"cwd": "/home/user-a/src"},
		}}}},
	}, sentPayload(t, server, "session.create"))
}

func TestNewWindowLetsRexNameTheSessionAndChooseTheDirectory(t *testing.T) {
	server := newFakeServer(t)
	server.answerValue("session.create", map[string]any{
		"session_id":      "session:new",
		"initial_windows": []map[string]any{{"window_id": "window:new", "revision": 1}},
	})
	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	defer client.Close()

	_, err = NewWindow(t.Context(), client, NewWindowOptions{})

	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"initial_windows": []any{map[string]any{"layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
		}}}},
	}, sentPayload(t, server, "session.create"))
}

func TestNewWindowAddsAWindowToASession(t *testing.T) {
	server := newFakeServer(t)
	// Rex adds a window only for a connection attached to its session.
	attached := false
	server.answer("session.attach", func(p json.RawMessage) (any, *Error) {
		attached = string(p) == `{"session_id":"session:a"}`
		return map[string]any{}, nil
	})
	server.answer("session.new_window", func(json.RawMessage) (any, *Error) {
		if !attached {
			return nil, &Error{Code: "conflict", Message: "stream is not attached to session"}
		}
		return map[string]any{"window_id": "window:2", "block_ids": []string{"block:9"}, "revision": 4}, nil
	})
	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	defer client.Close()

	created, err := NewWindow(t.Context(), client, NewWindowOptions{
		SessionID: "session:a", Label: "logs", Cwd: "/srv",
	})

	require.NoError(t, err)
	assert.Equal(t, Created{SessionID: "session:a", WindowID: "window:2"}, created)
	assert.Equal(t, map[string]any{
		"session_id":   "session:a",
		"window_label": "logs",
		"focus":        true,
		"layout": map[string]any{"block": map[string]any{
			"flavor":  "com.superlogical.terminal.shell",
			"options": map[string]any{"cwd": "/srv"},
		}},
	}, sentPayload(t, server, "session.new_window"))
}

func TestNewWindowReportsRexRefusing(t *testing.T) {
	server := newFakeServer(t)
	server.answer("session.attach", func(json.RawMessage) (any, *Error) {
		return nil, &Error{Code: "not_found", Message: "no such session"}
	})
	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	defer client.Close()

	_, err = NewWindow(t.Context(), client, NewWindowOptions{SessionID: "session:gone"})

	require.ErrorContains(t, err, "no such session")
}
