package tmuxrestore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var recordClock = time.Unix(1_800_000_000, 0)

func claudeEvent(conversation string) Event {
	return Event{
		Agent:          "claude",
		ConversationID: conversation,
		TranscriptPath: "/home/u/.claude/projects/p/" + conversation + ".jsonl",
		PermissionMode: "default",
		Now:            recordClock,
	}
}

type identifyCounter struct {
	calls int
	ref   *ProcessRef
	argv  []string
}

func (c *identifyCounter) identify() (*ProcessRef, []string) {
	c.calls++
	return c.ref, c.argv
}

func TestApplyStartsRecordFromFirstEvent(t *testing.T) {
	identify := &identifyCounter{
		ref:  &ProcessRef{PID: 42, StartMillis: 7},
		argv: []string{"claude", "--model", "opus"},
	}

	next, clear := Apply(nil, claudeEvent("c-1"), identify.identify)

	require.NotNil(t, next)
	assert.False(t, clear)
	assert.Equal(t, 1, identify.calls)
	assert.Equal(t, Record{
		Agent:          "claude",
		ConversationID: "c-1",
		Process:        &ProcessRef{PID: 42, StartMillis: 7},
		Argv:           []string{"claude", "--model", "opus"},
		TranscriptPath: "/home/u/.claude/projects/p/c-1.jsonl",
		PermissionMode: "default",
		RecordedAt:     recordClock.Unix(),
	}, *next)
}

func TestApplyNewConversationKeepsNothingFromThePrevious(t *testing.T) {
	prev := &Record{
		Agent:          "claude",
		ConversationID: "c-1",
		Process:        &ProcessRef{PID: 42, StartMillis: 7},
		Argv:           []string{"claude", "--model", "opus"},
		TranscriptPath: "/old.jsonl",
		PermissionMode: "plan",
		RecordedAt:     1,
	}
	identify := &identifyCounter{}
	ev := Event{Agent: "claude", ConversationID: "c-2", Now: recordClock}

	next, clear := Apply(prev, ev, identify.identify)

	require.NotNil(t, next)
	assert.False(t, clear)
	assert.Equal(t, 1, identify.calls)
	assert.Equal(t, Record{
		Agent:          "claude",
		ConversationID: "c-2",
		RecordedAt:     recordClock.Unix(),
	}, *next)
}

func TestApplySameConversationUpdatesHintsOnly(t *testing.T) {
	prev := &Record{
		Agent:          "claude",
		ConversationID: "c-1",
		Process:        &ProcessRef{PID: 42, StartMillis: 7},
		Argv:           []string{"claude"},
		TranscriptPath: "/old.jsonl",
		PermissionMode: "plan",
		RecordedAt:     1,
	}
	identify := &identifyCounter{}

	withMode := Event{
		Agent: "claude", ConversationID: "c-1",
		PermissionMode: "acceptEdits", Now: recordClock,
	}
	next, _ := Apply(prev, withMode, identify.identify)
	require.NotNil(t, next)
	assert.Equal(t, "acceptEdits", next.PermissionMode)
	assert.Equal(t, "/old.jsonl", next.TranscriptPath)
	assert.Equal(t, []string{"claude"}, next.Argv)
	assert.Equal(t, &ProcessRef{PID: 42, StartMillis: 7}, next.Process)
	assert.Equal(t, recordClock.Unix(), next.RecordedAt)

	withoutMode := Event{
		Agent: "claude", ConversationID: "c-1",
		TranscriptPath: "/new.jsonl", Now: recordClock,
	}
	next, _ = Apply(next, withoutMode, identify.identify)
	require.NotNil(t, next)
	assert.Equal(t, "acceptEdits", next.PermissionMode)
	assert.Equal(t, "/new.jsonl", next.TranscriptPath)
	assert.Equal(t, []string{"claude"}, next.Argv, "an unidentified process keeps the recorded one")
	assert.Equal(t, &ProcessRef{PID: 42, StartMillis: 7}, next.Process)
}

func TestApplySameConversationInANewProcessRecordsThatProcess(t *testing.T) {
	// Codex reports no deliberate exit, so its record outlives the process;
	// "codex resume" in the same pane then continues that conversation.
	prev := &Record{
		Agent:          "codex",
		ConversationID: "x-1",
		Process:        &ProcessRef{PID: 42, StartMillis: 7},
		Argv:           []string{"codex", "--sandbox", "read-only"},
		TranscriptPath: "/rollout.jsonl",
		RecordedAt:     1,
	}
	identify := &identifyCounter{
		ref:  &ProcessRef{PID: 99, StartMillis: 20},
		argv: []string{"codex", "resume", "x-1", "--sandbox", "workspace-write"},
	}

	next, clear := Apply(prev, Event{Agent: "codex", ConversationID: "x-1", Now: recordClock}, identify.identify)

	require.NotNil(t, next)
	assert.False(t, clear)
	assert.Equal(t, Record{
		Agent:          "codex",
		ConversationID: "x-1",
		Process:        &ProcessRef{PID: 99, StartMillis: 20},
		Argv:           []string{"codex", "resume", "x-1", "--sandbox", "workspace-write"},
		TranscriptPath: "/rollout.jsonl",
		RecordedAt:     recordClock.Unix(),
	}, *next)
}

func TestApplyIgnoresCodexPermissionMode(t *testing.T) {
	ev := Event{
		Agent: "codex", ConversationID: "x-1",
		PermissionMode: "default", Now: recordClock,
	}
	next, _ := Apply(nil, ev, (&identifyCounter{}).identify)
	require.NotNil(t, next)
	assert.Empty(t, next.PermissionMode)
}

func TestApplyEndingRules(t *testing.T) {
	prevFor := func(agent string) *Record {
		return &Record{Agent: agent, ConversationID: "c-1", RecordedAt: 1}
	}
	cases := []struct {
		agent  string
		reason string
		clear  bool
	}{
		{"claude", "prompt_input_exit", true},
		{"claude", "logout", true},
		{"claude", "clear", true},
		{"claude", "other", false},
		{"claude", "", false},
		{"codex", "other", false},
		{"codex", "prompt_input_exit", false},
	}
	for _, tc := range cases {
		t.Run(tc.agent+"/"+tc.reason, func(t *testing.T) {
			ev := Event{
				Agent: tc.agent, ConversationID: "c-1",
				IsEnd: true, EndReason: tc.reason, Now: recordClock,
			}
			next, clear := Apply(prevFor(tc.agent), ev, (&identifyCounter{}).identify)
			assert.Equal(t, tc.clear, clear)
			if tc.clear {
				assert.Nil(t, next)
			} else {
				require.NotNil(t, next)
				assert.Equal(t, "c-1", next.ConversationID)
			}
		})
	}
}

func TestApplyWithoutConversationIDChangesNothing(t *testing.T) {
	prev := &Record{Agent: "claude", ConversationID: "c-1", RecordedAt: 1}
	next, clear := Apply(prev, Event{Agent: "claude", Now: recordClock}, nil)
	assert.False(t, clear)
	assert.Same(t, prev, next)
}

func TestRecordEncodingRoundTrips(t *testing.T) {
	record := Record{
		Agent:          "codex",
		ConversationID: "x-1",
		Process:        &ProcessRef{PID: 9, StartMillis: 10},
		Argv:           []string{"codex", "-c", "a=b c"},
		RecordedAt:     11,
	}
	encoded, err := record.Encode()
	require.NoError(t, err)
	decoded, err := DecodeRecord(encoded)
	require.NoError(t, err)
	assert.Equal(t, record, *decoded)

	_, err = DecodeRecord("not json")
	assert.Error(t, err)
}
