package tmuxrestore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildLaunchClaude(t *testing.T) {
	cases := []struct {
		name     string
		argv     string
		mode     string
		want     string
		dropped  []string
		fallback bool
	}{
		{
			name: "settings kept in both value forms",
			argv: "claude --model opus --settings=/s.json --add-dir /a /b --verbose",
			want: "claude --model opus --settings=/s.json --add-dir /a /b --verbose --resume c-1",
		},
		{
			name: "tool lists kept",
			argv: "claude --allowedTools Bash Edit --disallowed-tools Write",
			want: "claude --allowedTools Bash Edit --disallowed-tools Write --resume c-1",
		},
		{
			name: "one-time actions and selectors dropped",
			argv: "claude --worktree feat --tmux -r old --session-id s1 --fork-session -c fix the bug",
			want: "claude --resume c-1",
		},
		{
			name:    "unknown flags reported by name only",
			argv:    "claude --frobnicate=secret --model opus -Zvalue",
			want:    "claude --model opus --resume c-1",
			dropped: []string{"--frobnicate", "-Z"},
		},
		{
			name: "recorded mode replaces skip-permissions and keeps bypass reachable",
			argv: "claude --dangerously-skip-permissions --model opus",
			mode: "plan",
			want: "claude --model opus --permission-mode plan --allow-dangerously-skip-permissions --resume c-1",
		},
		{
			name: "allow-only bypass is kept",
			argv: "claude --allow-dangerously-skip-permissions --permission-mode default",
			mode: "acceptEdits",
			want: "claude --allow-dangerously-skip-permissions --permission-mode acceptEdits --resume c-1",
		},
		{
			name: "no recorded mode keeps launch flags",
			argv: "claude --dangerously-skip-permissions",
			want: "claude --dangerously-skip-permissions --resume c-1",
		},
		{
			// The versioned path is gone once Claude updates; the pane's PATH
			// finds the current install.
			name: "an installed binary is started by name",
			argv: "/m/installs/2.1.329/node_modules/@anthropic-ai/claude-code/bin/claude.exe --model haiku",
			want: "claude --model haiku --resume c-1",
		},
		{
			name: "node-hosted claude keeps its script",
			argv: "node /lib/@anthropic-ai/claude-code/cli.js --model opus hello",
			want: "node /lib/@anthropic-ai/claude-code/cli.js --model opus --resume c-1",
		},
		{
			name: "node options before the script stay with node",
			argv: "node --max-old-space-size=8192 --no-warnings /lib/@anthropic-ai/claude-code/cli.js --model opus",
			want: "node --max-old-space-size=8192 --no-warnings /lib/@anthropic-ai/claude-code/cli.js --model opus --resume c-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := Record{Agent: "claude", ConversationID: "c-1", Argv: strings.Fields(tc.argv), PermissionMode: tc.mode}
			got := BuildLaunch(rec, "claude", Turn{})
			assert.Equal(t, tc.want, strings.Join(got.Argv, " "))
			assert.Equal(t, tc.dropped, got.Dropped)
			assert.False(t, got.Fallback)
		})
	}
}

func TestBuildLaunchStartsTheConfiguredAgentProgram(t *testing.T) {
	rec := Record{Agent: "claude", ConversationID: "c-1", Argv: []string{"/m/2.1.329/bin/claude.exe", "--model", "haiku"}}

	assert.Equal(t, []string{"/opt/bin/claude", "--model", "haiku", "--resume", "c-1"},
		BuildLaunch(rec, "/opt/bin/claude --verbose", Turn{}).Argv)
	assert.Equal(t, []string{"claude", "--model", "haiku", "--resume", "c-1"},
		BuildLaunch(rec, "env FOO=1 claude", Turn{}).Argv, "a wrapper command is not the agent's program")
	// Restore runs the program without a shell, so nothing would expand ~.
	assert.Equal(t, []string{"claude", "--model", "haiku", "--resume", "c-1"},
		BuildLaunch(rec, "~/bin/claude", Turn{}).Argv, "a home-relative program is found by name instead")
}

func TestBuildLaunchCodex(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		want    []string
		dropped []string
	}{
		{
			name: "settings and repeated overrides kept in order",
			argv: []string{"codex", "-m", "gpt-5", "--sandbox", "workspace-write", "-c", "a=b", "-c", "c=d", "--add-dir", "/x", "--search"},
			want: []string{"codex", "-m", "gpt-5", "--sandbox", "workspace-write", "-c", "a=b", "-c", "c=d", "--add-dir", "/x", "--search", "resume", "x-1"},
		},
		{
			name: "subcommands, images, and prompts dropped",
			argv: []string{"codex", "-a", "never", "-i", "shot.png", "resume", "--last", "explain this"},
			want: []string{"codex", "-a", "never", "resume", "x-1"},
		},
		{
			name: "bypass alias kept",
			argv: []string{"codex", "--yolo", "--profile", "work"},
			want: []string{"codex", "--yolo", "--profile", "work", "resume", "x-1"},
		},
		{
			name: "own-process hooks kept",
			argv: []string{"codex", "--no-daemon", "-m", "gpt-5"},
			want: []string{"codex", "--no-daemon", "-m", "gpt-5", "resume", "x-1"},
		},
		{
			// The platform binary behind npm's codex wrapper is not on PATH.
			name: "native platform binary started as codex",
			argv: []string{"/m/@openai/codex/vendor/codex-x86_64-unknown-linux-musl", "-s", "read-only"},
			want: []string{"codex", "-s", "read-only", "resume", "x-1"},
		},
		{
			name:    "unknown flag dropped",
			argv:    []string{"codex", "--telemetry", "-s", "read-only"},
			want:    []string{"codex", "-s", "read-only", "resume", "x-1"},
			dropped: []string{"--telemetry"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildLaunch(Record{Agent: "codex", ConversationID: "x-1", Argv: tc.argv}, "codex", Turn{})
			assert.Equal(t, tc.want, got.Argv)
			assert.Equal(t, tc.dropped, got.Dropped)
		})
	}
}

func TestBuildLaunchAgy(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		want    []string
		dropped []string
	}{
		{
			// agy's installer links agy to a versioned binary.
			name: "settings kept and the installed binary started by name",
			argv: []string{
				"/m/installs/antigravity-cli/1.5.14/agy", "--model", "gemini-pro-high", "--mode", "plan",
				"--add-dir", "/x", "--add-dir", "/y", "--sandbox", "--conversation", "old",
			},
			want: []string{
				"agy", "--model", "gemini-pro-high", "--mode", "plan",
				"--add-dir", "/x", "--add-dir", "/y", "--sandbox", "--conversation", "g-1",
			},
		},
		{
			name: "prompts and one-time actions dropped",
			argv: []string{"agy", "--effort", "high", "-i", "fix the bug", "--continue", "--new-project"},
			want: []string{"agy", "--effort", "high", "--conversation", "g-1"},
		},
		{
			name:    "unknown flag dropped",
			argv:    []string{"agy", "--telemetry", "--dangerously-skip-permissions"},
			want:    []string{"agy", "--dangerously-skip-permissions", "--conversation", "g-1"},
			dropped: []string{"--telemetry"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildLaunch(Record{Agent: "agy", ConversationID: "g-1", Argv: tc.argv}, "", Turn{})
			assert.Equal(t, tc.want, got.Argv)
			assert.Equal(t, tc.dropped, got.Dropped)
		})
	}
}

func TestBuildLaunchFallsBackToTheConfiguredCommand(t *testing.T) {
	got := BuildLaunch(Record{Agent: "codex", ConversationID: "x-1"}, "codex --yolo", Turn{})
	assert.Equal(t, []string{"codex", "--yolo", "resume", "x-1"}, got.Argv)
	assert.True(t, got.Fallback)

	got = BuildLaunch(Record{Agent: "claude", ConversationID: "c-1", PermissionMode: "plan"}, "", Turn{})
	assert.Equal(t, []string{"claude", "--permission-mode", "plan", "--resume", "c-1"}, got.Argv)
	assert.True(t, got.Fallback)
}

func TestBuildLaunchCodexUsesTheLatestTurn(t *testing.T) {
	turn := Turn{Model: "gpt-b", Effort: "low"}
	cases := []struct {
		name string
		argv []string
		turn Turn
		want []string
	}{
		{
			name: "launch model and effort replaced",
			argv: []string{"codex", "-m", "gpt-a", "-c", "model_reasoning_effort=xhigh", "-c", "a=b", "--yolo"},
			turn: turn,
			want: []string{"codex", "-c", "a=b", "--yolo", "-m", "gpt-b", "-c", "model_reasoning_effort=low", "resume", "x-1"},
		},
		{
			name: "long and inline spellings replaced",
			argv: []string{"codex", "--model=gpt-a", "--config", "model_reasoning_effort = high", "-c=model_reasoning_effort=high"},
			turn: turn,
			want: []string{"codex", "-m", "gpt-b", "-c", "model_reasoning_effort=low", "resume", "x-1"},
		},
		{
			name: "settings added to a launch without them",
			argv: []string{"codex", "--yolo"},
			turn: turn,
			want: []string{"codex", "--yolo", "-m", "gpt-b", "-c", "model_reasoning_effort=low", "resume", "x-1"},
		},
		{
			name: "effort kept when the turn names none",
			argv: []string{"codex", "-m", "gpt-a", "-c", "model_reasoning_effort=xhigh"},
			turn: Turn{Model: "gpt-b"},
			want: []string{"codex", "-c", "model_reasoning_effort=xhigh", "-m", "gpt-b", "resume", "x-1"},
		},
		{
			name: "launch kept when the turn is unknown",
			argv: []string{"codex", "-m", "gpt-a"},
			want: []string{"codex", "-m", "gpt-a", "resume", "x-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildLaunch(Record{Agent: "codex", ConversationID: "x-1", Argv: tc.argv}, "codex", tc.turn)
			assert.Equal(t, tc.want, got.Argv)
		})
	}

	claude := BuildLaunch(Record{Agent: "claude", ConversationID: "c-1", Argv: []string{"claude", "--model", "opus"}}, "claude", turn)
	assert.Equal(t, []string{"claude", "--model", "opus", "--resume", "c-1"}, claude.Argv, "only Codex reports its turns")
}
