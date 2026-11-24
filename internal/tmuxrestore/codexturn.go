package tmuxrestore

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Turn is the model and reasoning effort a conversation last used. Codex
// lets the person change both mid-conversation, so the launch arguments can
// be out of date.
type Turn struct {
	Model  string
	Effort string
}

const (
	// rolloutChunk is how much of a rollout CodexTurn reads at a time.
	rolloutChunk = 1 << 20
	// turnLineLimit bounds a line CodexTurn will parse. A turn context is a
	// few KB; longer lines hold output and are skipped unread.
	turnLineLimit = 1 << 20
)

var turnContextType = []byte(`"type":"turn_context"`)

// CodexTurn reads a Codex rollout from its end for the latest turn's model
// and effort. Codex writes a turn context at the start of every turn, so
// the last one holds the settings the person was using. Rollouts grow to
// gigabytes, so it reads backward and stops at the first match. A
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
			if !overlong {
				if turn, ok := parseTurnContext(data[nl+1 : end]); ok {
					return turn
				}
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
		if turn, ok := parseTurnContext(partial); ok {
			return turn
		}
	}
	return Turn{}
}

func parseTurnContext(line []byte) (Turn, bool) {
	if !bytes.Contains(line, turnContextType) {
		return Turn{}, false
	}
	var entry struct {
		Type    string `json:"type"`
		Payload struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "turn_context" || entry.Payload.Model == "" {
		return Turn{}, false
	}
	return Turn{Model: entry.Payload.Model, Effort: entry.Payload.Effort}, true
}
