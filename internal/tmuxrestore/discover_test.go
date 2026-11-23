package tmuxrestore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// processesWithFiles is a process table that can list open files, as on
// Linux.
type processesWithFiles struct {
	fakeProcesses
	files map[int32][]string
}

func (p processesWithFiles) OpenFiles(_ context.Context, pid int32) ([]string, error) {
	return p.files[pid], nil
}

// countingProcesses counts process-tree scans.
type countingProcesses struct {
	fakeProcesses
	scans *int
}

func (p countingProcesses) Parents(ctx context.Context) (map[int32]int32, error) {
	*p.scans++
	return p.fakeProcesses.Parents(ctx)
}

func writeClaudeSession(t *testing.T, dir string, pid int32, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sessions"), 0o700))
	path := filepath.Join(dir, "sessions", strconv.Itoa(int(pid))+".json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func paneSnapshot(pane Pane) *Snapshot {
	return &Snapshot{Windows: map[string]Window{"w": {Key: "w", Panes: []Pane{pane}}}}
}

const codexRollout = "/home/user/.codex/sessions/2026/11/20/rollout-2026-11-20T08-15-12-01a1f94f-b341-7d21-ba7d-6a7055f5baae.jsonl"

func TestDiscoverReadsConversationFromRunningAgent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	claudeDir := t.TempDir()
	writeClaudeSession(t, claudeDir, 30, `{"pid":30,"sessionId":"c-live","cwd":"/src"}`)
	writeClaudeSession(t, claudeDir, 31, `{"pid":99,"sessionId":"c-other"}`)

	cases := []struct {
		name  string
		table ProcessTable
		want  *Record
	}{
		{
			name: "Claude Code names its conversation in its session file",
			table: fakeProcesses{
				10: proc(10, 1, "zsh"),
				20: proc(20, 10, "mise", "mise", "exec", "--", "claude"),
				30: proc(30, 20, "claude", "claude", "--model", "opus"),
			},
			want: &Record{
				Agent: "claude", ConversationID: "c-live",
				Process: &ProcessRef{PID: 30, StartMillis: 300}, Argv: []string{"claude", "--model", "opus"},
				RecordedAt: now.Unix(),
			},
		},
		{
			name: "a session file left by another process names nothing",
			table: fakeProcesses{
				10: proc(10, 1, "zsh"),
				31: proc(31, 10, "claude", "claude"),
			},
		},
		{
			name: "Codex holds its rollout file open below its launcher",
			table: processesWithFiles{
				fakeProcesses: fakeProcesses{
					10: proc(10, 1, "bash"),
					20: proc(20, 10, "node", "node", "/usr/lib/node_modules/@openai/codex/bin/codex.js", "--yolo"),
					30: proc(30, 20, "codex-x86_64-unknown-linux-musl", "/usr/lib/codex-x86_64-unknown-linux-musl", "--yolo"),
				},
				files: map[int32][]string{30: {"/dev/null", codexRollout}},
			},
			want: &Record{
				Agent: "codex", ConversationID: "01a1f94f-b341-7d21-ba7d-6a7055f5baae",
				Process:        &ProcessRef{PID: 30, StartMillis: 300},
				Argv:           []string{"/usr/lib/codex-x86_64-unknown-linux-musl", "--yolo"},
				TranscriptPath: codexRollout, RecordedAt: now.Unix(),
			},
		},
		{
			name: "Codex writing two conversations names neither",
			table: processesWithFiles{
				fakeProcesses: fakeProcesses{
					10: proc(10, 1, "bash"),
					30: proc(30, 10, "codex", "codex"),
				},
				files: map[int32][]string{30: {
					codexRollout,
					strings.Replace(codexRollout, "6a7055f5baae", "6a7055f5baaf", 1),
				}},
			},
		},
		{
			name: "Codex's shared app server names nothing",
			table: processesWithFiles{
				fakeProcesses: fakeProcesses{
					10: proc(10, 1, "bash"),
					30: proc(30, 10, "codex", "codex", "app-server"),
				},
				files: map[int32][]string{30: {codexRollout}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := paneSnapshot(Pane{ID: "%1", PID: 10, Command: "node"})
			discover(t.Context(), snap, tc.table, claudeDir, now)
			assert.Equal(t, tc.want, snap.Windows["w"].Panes[0].Resume)
		})
	}
}

func TestDiscoverLeavesHookRecordsAndIdlePanesAlone(t *testing.T) {
	claudeDir := t.TempDir()
	writeClaudeSession(t, claudeDir, 30, `{"pid":30,"sessionId":"c-live"}`)
	scans := 0
	table := countingProcesses{fakeProcesses: fakeProcesses{
		10: proc(10, 1, "zsh"),
		30: proc(30, 10, "claude", "claude"),
	}, scans: &scans}
	hooked := &Record{Agent: "claude", ConversationID: "c-hook", RecordedAt: 1}

	atPrompt := paneSnapshot(Pane{ID: "%1", PID: 10, Command: "-zsh"})
	discover(t.Context(), atPrompt, table, claudeDir, time.Now())
	assert.Nil(t, atPrompt.Windows["w"].Panes[0].Resume)
	assert.Zero(t, scans, "a pane at its prompt costs no process scan")

	recorded := paneSnapshot(Pane{ID: "%1", PID: 10, Command: "claude", Resume: hooked})
	discover(t.Context(), recorded, table, claudeDir, time.Now())
	assert.Same(t, hooked, recorded.Windows["w"].Panes[0].Resume)
	assert.Zero(t, scans)
}

// A save of a live server records the conversation of an agent that has no
// hook record, so a restore after the server is lost resumes it.
func TestSaveRecordsConversationOfAgentWithoutHooks(t *testing.T) {
	f := newSaveFixture(t)
	startServer(t, f.tm, "-s", "work", "sleep 600")
	out, err := f.tm.Run(t.Context(), "display-message", "-p", "-t", "work", "#{pane_pid}")
	require.NoError(t, err)
	panePID64, err := strconv.ParseInt(strings.TrimSpace(out), 10, 32)
	require.NoError(t, err)
	panePID := int32(panePID64)
	claudeDir := t.TempDir()
	writeClaudeSession(t, claudeDir, 9001, `{"pid":9001,"sessionId":"c-unhooked"}`)
	procs := fakeProcesses{
		panePID: proc(panePID, 1, "sleep", "sleep", "600"),
		9001:    proc(9001, panePID, "claude", "claude"),
	}

	_, err = Save(t.Context(), SaveDeps{
		Tmux: f.tm, Store: f.store, Processes: procs, Protection: f.prs,
		ClaudeDir: claudeDir, Now: func() time.Time { return f.now },
	})
	require.NoError(t, err)
	killServerAndWait(t, f.tm)
	f.save(t)

	files := f.files(t)
	require.Len(t, files.Restorable.Entries, 1)
	entry := files.Restorable.Entries[0]
	resume := files.Restorable.Windows[entry.Session.Windows[0].Key].Window.Panes[0].Resume
	require.NotNil(t, resume)
	assert.Equal(t, "claude", resume.Agent)
	assert.Equal(t, "c-unhooked", resume.ConversationID)
}
