package tmuxrestore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func turnContext(model, effort string) string {
	return `{"timestamp":"2026-11-24T15:00:00Z","type":"turn_context","payload":{"cwd":"/src","model":"` +
		model + `","effort":"` + effort + `"}}` + "\n"
}

func writeRollout(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-2026-11-24T09-00-00-x-1.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600))
	return path
}

func outputLine(size int) string {
	return `{"type":"response_item","payload":{"output":"` + strings.Repeat("x", size) + `"}}` + "\n"
}

func TestCodexTurnReadsTheLatestTurn(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		{"short rollout", []string{turnContext("gpt-a", "xhigh"), outputLine(10), turnContext("gpt-b", "low"), outputLine(10)}},
		{
			// The turn context straddles a chunk boundary and the turn's
			// output, which mentions turn contexts, spans several chunks.
			name: "long turn after the change",
			lines: []string{
				turnContext("gpt-a", "xhigh"), outputLine(rolloutChunk - 40), turnContext("gpt-b", "low"),
				outputLine(3 * turnLineLimit), `{"type":"event_msg","payload":{"message":"\"type\":\"turn_context\""}}` + "\n",
			},
		},
		{"no trailing newline", []string{turnContext("gpt-a", "xhigh"), strings.TrimSuffix(turnContext("gpt-b", "low"), "\n")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := CodexTurn(writeRollout(t, tc.lines...))
			assert.Equal(t, "gpt-b", turn.Model)
			assert.Equal(t, "low", turn.Effort)
		})
	}
}

func event(kind string) string {
	return `{"timestamp":"2026-11-24T15:00:00Z","type":"event_msg","payload":{"type":"` + kind + `"}}` + "\n"
}

func TestCodexTurnTellsWhetherTheTurnFinished(t *testing.T) {
	cases := []struct {
		name       string
		lines      []string
		unfinished bool
	}{
		{"completed", []string{event("task_started"), turnContext("gpt-b", "low"), outputLine(10), event("task_complete")}, false},
		{"aborted by the person", []string{event("task_started"), turnContext("gpt-b", "low"), event("turn_aborted")}, false},
		{"cut short", []string{
			event("task_started"), turnContext("gpt-a", "high"), event("task_complete"),
			event("task_started"), turnContext("gpt-b", "low"), outputLine(10),
		}, true},
		{"cut short before its turn context", []string{
			event("task_started"), turnContext("gpt-b", "low"), event("task_complete"), event("task_started"),
		}, true},
		{
			// Output that quotes a completion is not one.
			name: "output quoting a completion",
			lines: []string{
				event("task_started"), turnContext("gpt-b", "low"),
				`{"type":"response_item","payload":{"output":"{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\"}}"}}` + "\n",
			},
			unfinished: true,
		},
		{"long output after a completed turn", []string{
			event("task_started"), turnContext("gpt-b", "low"), event("task_complete"), outputLine(3 * turnLineLimit),
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := CodexTurn(writeRollout(t, tc.lines...))
			assert.Equal(t, Turn{Model: "gpt-b", Effort: "low", Unfinished: tc.unfinished}, turn)
		})
	}
}

func TestCodexTurnUnknown(t *testing.T) {
	assert.Equal(t, Turn{}, CodexTurn(writeRollout(t, outputLine(10))), "no turn yet")
	assert.Equal(t, Turn{}, CodexTurn(filepath.Join(t.TempDir(), "gone.jsonl")))

	compressed := filepath.Join(t.TempDir(), "rollout-x-1.jsonl.zst")
	require.NoError(t, os.WriteFile(compressed, []byte(turnContext("gpt-b", "low")), 0o600))
	assert.Equal(t, Turn{}, CodexTurn(compressed), "a compressed rollout is not read")
}
