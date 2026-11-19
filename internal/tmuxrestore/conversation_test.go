package tmuxrestore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func touch(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
}

func noEnv(string) string { return "" }

func TestFindClaudeConversation(t *testing.T) {
	home := t.TempDir()
	rec := Record{Agent: "claude", ConversationID: "c-1", TranscriptPath: filepath.Join(home, "gone.jsonl")}

	assert.Equal(t, PresenceUnknown, FindConversation(rec, home, noEnv), "no projects directory to search")

	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "projects", "p"), 0o700))
	assert.Equal(t, PresenceAbsent, FindConversation(rec, home, noEnv))

	touch(t, filepath.Join(home, ".claude", "projects", "other", "c-1.jsonl"))
	assert.Equal(t, PresenceFound, FindConversation(rec, home, noEnv), "found by ID after the hinted path moved")

	configDir := t.TempDir()
	touch(t, filepath.Join(configDir, "projects", "p", "c-2.jsonl"))
	env := func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return configDir
		}
		return ""
	}
	assert.Equal(t, PresenceFound, FindConversation(Record{Agent: "claude", ConversationID: "c-2"}, home, env))

	hinted := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	touch(t, hinted)
	assert.Equal(t, PresenceFound, FindConversation(Record{Agent: "claude", ConversationID: "c-9", TranscriptPath: hinted}, home, noEnv))
}

func TestFindCodexConversation(t *testing.T) {
	home := t.TempDir()
	rec := Record{Agent: "codex", ConversationID: "0199-abc", TranscriptPath: filepath.Join(home, "rollout.jsonl")}

	assert.Equal(t, PresenceUnknown, FindConversation(rec, home, noEnv))

	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "archived_sessions"), 0o700))
	assert.Equal(t, PresenceAbsent, FindConversation(rec, home, noEnv))

	touch(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "18", "rollout-2026-11-18T01-02-03-0199-abc.jsonl.zst"))
	assert.Equal(t, PresenceFound, FindConversation(rec, home, noEnv), "a compressed rollout still resumes")
}
