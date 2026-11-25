package tmuxrestore

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
)

// Turn is what a Codex rollout says about its latest turn: the model and
// reasoning effort it used, which the person can change mid-conversation,
// and whether it was still running when the rollout stops.
type Turn struct {
	Model  string
	Effort string
	// Unfinished means the turn started but never completed or was
	// aborted, as when the machine restarts while Codex works.
	Unfinished bool
}

const (
	// rolloutChunk is how much of a rollout CodexTurn reads at a time.
	rolloutChunk = 1 << 20
	// turnLineLimit bounds a line CodexTurn will parse. A turn context is a
	// few KB; longer lines hold output and are skipped unread.
	turnLineLimit = 1 << 20
)

// turnMarkers are the rollout entries CodexTurn reads. Codex writes
// task_started and a turn context when a turn begins, and task_complete or
// turn_aborted when it ends.
var turnMarkers = [][]byte{
	[]byte(`"turn_context"`), []byte(`"task_started"`), []byte(`"task_complete"`), []byte(`"turn_aborted"`),
}

// CodexTurn reads a Codex rollout from its end for the latest turn. The
// last turn context holds the settings the person was using, and the turn
// is unfinished when no completion follows its start. Rollouts grow to
// gigabytes, so it reads backward and stops once it knows both. A
// compressed or unreadable rollout yields an empty Turn.
func CodexTurn(path string) Turn {
	if !strings.HasSuffix(path, ".jsonl") {
		return Turn{}
	}
	f, err := os.Open(path)
	if err != nil {
		return Turn{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Turn{}
	}
	// partial holds the start of the line that ends where the bytes read so
	// far begin; its own start is still before pos.
	var partial []byte
	overlong := false
	var turn Turn
	decided := false
	// read applies one line and reports whether the turn is known.
	read := func(line []byte) bool {
		kind, model, effort := parseTurnLine(line)
		switch kind {
		case "turn_context":
			if !decided {
				turn.Unfinished, decided = true, true
			}
			turn.Model, turn.Effort = model, effort
		case "task_started":
			if !decided {
				turn.Unfinished, decided = true, true
			}
		case "task_complete", "turn_aborted":
			decided = true
		}
		return decided && turn.Model != ""
	}
	for pos := info.Size(); pos > 0; {
		n := min(int64(rolloutChunk), pos)
		pos -= n
		data := make([]byte, n, int(n)+len(partial))
		if _, err := f.ReadAt(data, pos); err != nil && err != io.EOF {
			return Turn{}
		}
		data = append(data, partial...)
		end := len(data)
		for {
			nl := bytes.LastIndexByte(data[:end], '\n')
			if nl < 0 {
				break
			}
			if !overlong && read(data[nl+1:end]) {
				return turn
			}
			overlong = false
			end = nl
		}
		partial = data[:end]
		if len(partial) > turnLineLimit {
			partial, overlong = nil, true
		}
	}
	if !overlong {
		read(partial)
	}
	return turn
}

// parseTurnLine returns which turn marker a rollout line is, if any, with a
// turn context's model and effort. Output can quote a marker, so only the
// entry's own type counts.
func parseTurnLine(line []byte) (kind, model, effort string) {
	if !slices.ContainsFunc(turnMarkers, func(m []byte) bool { return bytes.Contains(line, m) }) {
		return "", "", ""
	}
	var entry struct {
		Type    string `json:"type"`
		Payload struct {
			Type   string `json:"type"`
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return "", "", ""
	}
	switch {
	case entry.Type == "turn_context" && entry.Payload.Model != "":
		return "turn_context", entry.Payload.Model, entry.Payload.Effort
	case entry.Type == "event_msg":
		switch entry.Payload.Type {
		case "task_started", "task_complete", "turn_aborted":
			return entry.Payload.Type, "", ""
		}
	}
	return "", "", ""
}
