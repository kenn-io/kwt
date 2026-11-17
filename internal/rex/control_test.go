package rex

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// controlRun runs Control against server with pipes standing in for stdin
// and stdout, and collects each output line.
type controlRun struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan map[string]any
	done  chan error
}

func startControl(t *testing.T, server *fakeServer, windowID string) *controlRun {
	t.Helper()
	client, err := Dial(t.Context(), server.socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	run := &controlRun{t: t, in: inWriter, lines: make(chan map[string]any, 64), done: make(chan error, 1)}
	go func() {
		err := Control(t.Context(), client, windowID, inReader, outWriter)
		_ = outWriter.Close()
		run.done <- err
	}()
	go func() {
		scanner := bufio.NewScanner(outReader)
		scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for scanner.Scan() {
			var line map[string]any
			if json.Unmarshal(scanner.Bytes(), &line) != nil {
				line = map[string]any{"type": "unparsable", "text": scanner.Text()}
			}
			run.lines <- line
		}
		close(run.lines)
	}()
	t.Cleanup(func() { _ = inWriter.Close() })
	return run
}

func (r *controlRun) next() map[string]any {
	r.t.Helper()
	select {
	case line, ok := <-r.lines:
		require.True(r.t, ok, "control output ended")
		return line
	case <-time.After(5 * time.Second):
		require.FailNow(r.t, "no control output")
		return nil
	}
}

func (r *controlRun) send(line string) {
	r.t.Helper()
	_, err := io.WriteString(r.in, line+"\n")
	require.NoError(r.t, err)
}

func (r *controlRun) wait() error {
	r.t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		require.FailNow(r.t, "control kept running")
		return nil
	}
}

// layoutServer answers a session holding window:1 with two terminals, and
// a second session without it.
func layoutServer(t *testing.T) *fakeServer {
	t.Helper()
	server := newFakeServer(t)
	server.answerValue("session.list", map[string]any{"sessions": []any{
		map[string]any{"session_id": "session:other", "label": "other"},
		map[string]any{"session_id": "session:a", "label": "work"},
	}})
	windowView := func(sessionID string) map[string]any {
		if sessionID == "session:other" {
			return map[string]any{"session_id": sessionID, "label": "other", "revision": 1, "windows": []any{
				window("window:9", "elsewhere", "block:9", block("block:9", terminalCreator, "shell")),
			}}
		}
		return map[string]any{"session_id": sessionID, "label": "work", "revision": 4, "windows": []any{
			window("window:1", "editor", "block:2",
				block("block:1", terminalCreator, "shell"),
				block("block:2", terminalCreator, "logs"),
			),
		}}
	}
	server.answer("session.attach", func(p json.RawMessage) (any, *Error) {
		var req struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(p, &req)
		return map[string]any{"view": windowView(req.SessionID)}, nil
	})
	server.answer("session.view", func(json.RawMessage) (any, *Error) {
		return windowView("session:a"), nil
	})
	server.answer("com.superlogical.terminal.title", func(p json.RawMessage) (any, *Error) {
		if strings.Contains(string(p), "block:1") {
			return map[string]string{"title": "vim main.go"}, nil
		}
		return map[string]string{"title": ""}, nil
	})
	server.answer("com.superlogical.terminal.process", func(json.RawMessage) (any, *Error) {
		return map[string]any{
			"home":       "/home/user-a",
			"foreground": map[string]any{"name": "zsh", "cwd": "/home/user-a/src/app"},
		}, nil
	})
	return server
}

func TestControlWritesTheWindowsSessionViewFirst(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")

	line := run.next()
	assert.Equal(t, "view", line["type"])
	view := line["view"].(map[string]any)
	assert.Equal(t, "session:a", view["session_id"])
	terminals := line["terminals"].(map[string]any)
	assert.Equal(t, "vim main.go", terminals["block:1"].(map[string]any)["name"])
	assert.Equal(t, "zsh ~/src/app", terminals["block:2"].(map[string]any)["name"])
	assert.Len(t, server.callsTo("session.detach"), 1, "control leaves the session without the window")
}

func TestControlRereadsTheViewAfterChanges(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")
	run.next()

	server.push("session:a", map[string]any{"type": "session_view_changed", "payload": map[string]any{"revision": 5}})
	assert.Equal(t, "view", run.next()["type"])

	server.push("session:a", map[string]any{"type": "block_event", "payload": map[string]any{
		"block_id": "block:1", "creator_name": terminalCreator, "name": "title_changed",
		"payload": map[string]any{"title": "htop"},
	}})
	assert.Equal(t, "view", run.next()["type"])
}

func TestControlCollapsesChangesDuringARead(t *testing.T) {
	server := layoutServer(t)
	release := make(chan struct{})
	var once sync.Once
	server.answer("session.view", func(json.RawMessage) (any, *Error) {
		once.Do(func() { <-release })
		return map[string]any{"session_id": "session:a", "label": "work", "revision": 9, "windows": []any{}}, nil
	})
	run := startControl(t, server, "window:1")
	run.next()

	for i := range 5 {
		server.push("session:a", map[string]any{"type": "session_view_changed", "payload": map[string]any{"revision": 5 + i}})
	}
	require.Eventually(t, func() bool { return len(server.callsTo("session.view")) == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // let the remaining events arrive during the read
	close(release)

	assert.Equal(t, "view", run.next()["type"])
	assert.Equal(t, "view", run.next()["type"])
	select {
	case line := <-run.lines:
		assert.Failf(t, "changes during one read cost more than one more read", "extra line: %v", line)
	case <-time.After(300 * time.Millisecond):
	}
	assert.Len(t, server.callsTo("session.view"), 2)
}

func TestControlForwardsLayoutRequestsForItsSession(t *testing.T) {
	server := layoutServer(t)
	server.answerValue("session.new_split", map[string]any{"block_ids": []string{"block:3"}, "revision": 6})
	run := startControl(t, server, "window:1")
	run.next()

	run.send(`{"id":7,"method":"session.new_split","params":{"anchor_id":"block:2","direction":"horizontal","side":"after","focus":true}}`)

	reply := run.next()
	assert.Equal(t, "reply", reply["type"])
	assert.EqualValues(t, 7, reply["id"])
	assert.Equal(t, map[string]any{"block_ids": []any{"block:3"}, "revision": float64(6)}, reply["result"])
	calls := server.callsTo("session.new_split")
	require.Len(t, calls, 1)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(calls[0], &sent))
	assert.Equal(t, "session:a", sent["session_id"])
	assert.Equal(t, "block:2", sent["anchor_id"])
}

func TestControlReturnsRexErrorsToTheRequest(t *testing.T) {
	server := layoutServer(t)
	server.answer("session.focus_block", func(json.RawMessage) (any, *Error) {
		return nil, &Error{Code: "block_not_found", Message: "no such block"}
	})
	run := startControl(t, server, "window:1")
	run.next()

	run.send(`{"id":"a","method":"session.focus_block","params":{"block_id":"block:x"}}`)

	reply := run.next()
	assert.Equal(t, "a", reply["id"])
	assert.Equal(t, map[string]any{"code": "block_not_found", "message": "no such block"}, reply["error"])
}

func TestControlRefusesOtherMethods(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")
	run.next()

	run.send(`{"id":1,"method":"session.destroy","params":{}}`)

	reply := run.next()
	assert.Equal(t, "method_not_allowed", reply["error"].(map[string]any)["code"])
	assert.Empty(t, server.callsTo("session.destroy"))
}

func TestControlIgnoresAnotherSessionEnding(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")
	run.next()

	server.pushServerEvent("session_destroyed", map[string]any{"session_id": "session:other"})
	server.push("session:a", map[string]any{"type": "session_view_changed", "payload": map[string]any{"revision": 5}})

	assert.Equal(t, "view", run.next()["type"])
}

func TestControlEndsWhenTheSessionEnds(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")
	run.next()

	server.pushServerEvent("session_destroyed", map[string]any{"session_id": "session:a"})

	assert.Equal(t, map[string]any{"type": "ended"}, run.next())
	require.NoError(t, run.wait())
}

func TestControlEndsWhenInputCloses(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")
	run.next()

	require.NoError(t, run.in.Close())

	require.NoError(t, run.wait())
}

func TestControlReportsALostServer(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")
	run.next()

	server.dropControls()

	require.ErrorIs(t, run.wait(), ErrDisconnected)
}

func TestControlReportsAMissingWindow(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:404")

	require.ErrorIs(t, run.wait(), ErrWindowNotFound)
	assert.Len(t, server.callsTo("session.detach"), 2)
}

func TestControlAnswersAMalformedRequest(t *testing.T) {
	server := layoutServer(t)
	run := startControl(t, server, "window:1")
	run.next()

	run.send(`not json`)

	reply := run.next()
	assert.Nil(t, reply["id"])
	assert.Equal(t, "malformed_request", reply["error"].(map[string]any)["code"])
}
