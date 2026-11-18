package rex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nextEvent(t *testing.T, client *Client) Event {
	t.Helper()
	select {
	case event, ok := <-client.Events():
		require.True(t, ok, "events closed")
		return event
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no event")
		return Event{}
	}
}

func TestClientDeliversSessionEvents(t *testing.T) {
	server := newFakeServer(t)
	client, err := Dial(t.Context(), server.endpoint)
	require.NoError(t, err)
	defer client.Close()
	require.Eventually(t, func() bool { return server.controlCount() == 1 }, 5*time.Second, 10*time.Millisecond)

	server.push("session:a", map[string]any{"type": "session_view_changed", "payload": map[string]any{"revision": 3}})

	event := nextEvent(t, client)
	assert.Equal(t, "session:a", event.SessionID)
	assert.Equal(t, "session_view_changed", event.Type)
	assert.JSONEq(t, `{"revision":3}`, string(event.Payload))
}

func TestClientDeliversServerEventsWithTheirSession(t *testing.T) {
	server := newFakeServer(t)
	client, err := Dial(t.Context(), server.endpoint)
	require.NoError(t, err)
	defer client.Close()
	require.Eventually(t, func() bool { return server.controlCount() == 1 }, 5*time.Second, 10*time.Millisecond)

	server.pushServerEvent("session_destroyed", map[string]any{"session_id": "session:a"})

	event := nextEvent(t, client)
	assert.Equal(t, "session:a", event.SessionID)
	assert.Equal(t, "session_destroyed", event.Type)
}

func TestClientEventsCloseWithTheConnection(t *testing.T) {
	server := newFakeServer(t)
	client, err := Dial(t.Context(), server.endpoint)
	require.NoError(t, err)
	defer client.Close()
	require.Eventually(t, func() bool { return server.controlCount() == 1 }, 5*time.Second, 10*time.Millisecond)

	server.dropControls()

	select {
	case _, ok := <-client.Events():
		assert.False(t, ok)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "events stayed open")
	}
}

func TestClientKeepsTheNewestEventWhenItFallsBehind(t *testing.T) {
	client := &Client{events: make(chan Event, eventBuffer)}
	frame := func(kind string, revision int) json.RawMessage {
		data, err := json.Marshal(map[string]any{"type": "session_event", "payload": map[string]any{
			"session_id": "session:a",
			"event":      map[string]any{"type": kind, "payload": map[string]any{"revision": revision}},
		}})
		require.NoError(t, err)
		return data
	}
	for i := range eventBuffer {
		client.deliver(frame("session_view_changed", i))
	}
	client.deliver(frame("session_destroyed", eventBuffer))
	close(client.events)

	var types []string
	for event := range client.events {
		types = append(types, event.Type)
	}
	require.NotEmpty(t, types)
	assert.Contains(t, types, EventOverflow, "a reader must learn that events were dropped")
	assert.Equal(t, "session_destroyed", types[len(types)-1])
	assert.Len(t, types, eventBuffer)
}
