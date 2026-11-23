// Package tmuxrestore saves tmux sessions, with each pane's agent
// conversation, and rebuilds them after the tmux server is lost to a reboot
// or a kill-server.
package tmuxrestore

import (
	"encoding/json"
	"fmt"
	"time"
)

// ResumeOption is the tmux pane user option holding a pane's Record.
const ResumeOption = "@kwt_agent_resume"

// ClosedOption is the tmux pane user option naming the last conversation the
// person ended on purpose, and the process that ran it, so a save does not
// record it again from that process while it exits.
const ClosedOption = "@kwt_agent_closed"

// closedMarker is the ClosedOption value for a conversation ended in the
// process ref. A later process that resumes the conversation does not match
// it. Without a process, it names only the conversation.
func closedMarker(conversationID string, ref *ProcessRef) string {
	if ref == nil {
		return conversationID
	}
	return fmt.Sprintf("%s@%d.%d", conversationID, ref.PID, ref.StartMillis)
}

// ProcessRef identifies one process. The start time keeps a reused PID from
// being mistaken for the original process.
type ProcessRef struct {
	PID         int32 `json:"pid"`
	StartMillis int64 `json:"start_ms"`
}

// Record is everything restore needs to resume the agent conversation that
// ran in one pane. The fields describe a single conversation: a new
// conversation ID replaces the whole record.
type Record struct {
	Agent          string      `json:"agent"`
	ConversationID string      `json:"conversation_id"`
	Process        *ProcessRef `json:"process,omitempty"`
	Argv           []string    `json:"argv,omitempty"`
	// TranscriptPath is a hint for locating the conversation, not proof that
	// it exists or is gone.
	TranscriptPath string `json:"transcript_path,omitempty"`
	// PermissionMode is the last Claude permission mode a hook reported.
	PermissionMode string `json:"permission_mode,omitempty"`
	RecordedAt     int64  `json:"recorded_at"`
	// Discovered marks a record a save read from the running agent rather
	// than one a hook wrote. It lives only in snapshots.
	Discovered bool `json:"discovered,omitempty"`
}

// Event is one agent hook event reduced to the fields a Record uses.
type Event struct {
	Agent          string
	ConversationID string
	TranscriptPath string
	PermissionMode string
	IsEnd          bool
	EndReason      string
	Now            time.Time
}

// Apply returns the record a pane should hold after ev, and whether the pane
// option should be cleared instead. identify finds the agent process the
// event came from. A new conversation starts a new record; the same
// conversation in a new process, such as "codex resume" after an exit, keeps
// its hints and takes that process and its arguments.
func Apply(prev *Record, ev Event, identify func() (*ProcessRef, []string)) (*Record, bool) {
	if ev.ConversationID == "" {
		return prev, false
	}
	if ev.IsEnd && prev != nil && (prev.ConversationID != ev.ConversationID || prev.Agent != ev.Agent) {
		// The pane already holds another conversation, and a late end of an
		// earlier one says nothing about it.
		return prev, false
	}
	if ev.IsEnd && deliberateEnd(ev.Agent, ev.EndReason) {
		return nil, true
	}
	ref, argv := identify()
	var next Record
	if prev == nil || prev.ConversationID != ev.ConversationID || prev.Agent != ev.Agent {
		next = Record{Agent: ev.Agent, ConversationID: ev.ConversationID, Process: ref, Argv: argv}
	} else {
		next = *prev
		if ref != nil && (prev.Process == nil || *ref != *prev.Process) {
			next.Process, next.Argv = ref, argv
		}
	}
	if ev.TranscriptPath != "" {
		next.TranscriptPath = ev.TranscriptPath
	}
	if ev.PermissionMode != "" && ev.Agent == "claude" {
		next.PermissionMode = ev.PermissionMode
	}
	// A hook now speaks for this conversation, so later saves keep what it
	// reports instead of reading the agent again.
	next.Discovered = false
	next.RecordedAt = ev.Now.Unix()
	return &next, false
}

// deliberateEnd reports a SessionEnd that means the user closed the
// conversation. Codex reports "other" even for a normal close, so only
// Claude's explicit reasons qualify; anything else may be a shutdown signal.
func deliberateEnd(agent, reason string) bool {
	if agent != "claude" {
		return false
	}
	switch reason {
	case "prompt_input_exit", "logout", "clear":
		return true
	default:
		return false
	}
}

// Encode returns the pane option value for r.
func (r Record) Encode() (string, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode resume record: %w", err)
	}
	return string(data), nil
}

// DecodeRecord parses a pane option value.
func DecodeRecord(value string) (*Record, error) {
	var record Record
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return nil, fmt.Errorf("decode resume record: %w", err)
	}
	return &record, nil
}
