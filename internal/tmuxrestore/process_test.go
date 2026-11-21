package tmuxrestore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProcesses map[int32]ProcInfo

func (f fakeProcesses) Lookup(_ context.Context, pid int32) (ProcInfo, error) {
	info, ok := f[pid]
	if !ok {
		return ProcInfo{}, ErrProcessNotFound
	}
	return info, nil
}

func proc(pid, ppid int32, name string, argv ...string) ProcInfo {
	return ProcInfo{PID: pid, PPID: ppid, Name: name, Argv: argv, StartMillis: int64(pid) * 10}
}

func TestIdentifyAgent(t *testing.T) {
	cases := []struct {
		name    string
		table   fakeProcesses
		start   int32
		panePID int32
		agent   string
		want    *ProcessRef
		argv    []string
	}{
		{
			name: "agent is the pane process",
			table: fakeProcesses{
				10: proc(10, 1, "claude", "claude", "--model", "opus"),
				11: proc(11, 10, "sh", "sh", "-c", "kwt agent-hook run"),
			},
			start: 11, panePID: 10, agent: "claude",
			want: &ProcessRef{PID: 10, StartMillis: 100},
			argv: []string{"claude", "--model", "opus"},
		},
		{
			name: "agent is a child of the pane shell",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 10, "codex", "/opt/bin/codex", "--sandbox", "read-only"),
				21: proc(21, 20, "sh", "sh", "-c", "kwt"),
			},
			start: 21, panePID: 10, agent: "codex",
			want: &ProcessRef{PID: 20, StartMillis: 200},
			argv: []string{"/opt/bin/codex", "--sandbox", "read-only"},
		},
		{
			name: "node runs the claude package",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 10, "node", "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", "--model", "opus"),
			},
			start: 20, panePID: 10, agent: "claude",
			want: &ProcessRef{PID: 20, StartMillis: 200},
			argv: []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", "--model", "opus"},
		},
		{
			name: "native codex binary with a platform suffix",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 10, "node", "node", "/usr/lib/node_modules/@openai/codex/bin/codex.js"),
				30: proc(30, 20, "codex-aarch64-apple-darwin", "codex-aarch64-apple-darwin", "-s", "workspace-write"),
			},
			start: 30, panePID: 10, agent: "codex",
			want: &ProcessRef{PID: 30, StartMillis: 300},
			argv: []string{"codex-aarch64-apple-darwin", "-s", "workspace-write"},
		},
		{
			name: "npm-installed claude runs a native claude.exe",
			table: fakeProcesses{
				10: proc(10, 1, "bash", "-bash"),
				20: proc(20, 10, "claude.exe", "/m/node_modules/@anthropic-ai/claude-code/bin/claude.exe", "--model", "haiku"),
				21: proc(21, 20, "sh", "sh", "-c", "kwt"),
			},
			start: 21, panePID: 10, agent: "claude",
			want: &ProcessRef{PID: 20, StartMillis: 200},
			argv: []string{"/m/node_modules/@anthropic-ai/claude-code/bin/claude.exe", "--model", "haiku"},
		},
		{
			// agy is a link to the antigravity binary.
			name: "agy started through its link",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 10, "antigravity", "/m/antigravity-cli/1.5.14/agy", "--model", "gemini-pro"),
				21: proc(21, 20, "sh", "sh", "-c", "kwt"),
			},
			start: 21, panePID: 10, agent: "agy",
			want: &ProcessRef{PID: 20, StartMillis: 200},
			argv: []string{"/m/antigravity-cli/1.5.14/agy", "--model", "gemini-pro"},
		},
		{
			name: "no agent below the pane",
			table: fakeProcesses{
				5:  proc(5, 1, "claude", "claude"),
				10: proc(10, 5, "zsh", "-zsh"),
				20: proc(20, 10, "sh", "sh"),
			},
			start: 20, panePID: 10, agent: "claude",
		},
		{
			name: "agent outside the pane",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 1, "claude", "claude"),
				21: proc(21, 20, "sh", "sh"),
			},
			start: 21, panePID: 10, agent: "claude",
		},
		{
			name: "unreadable arguments keep the process",
			table: fakeProcesses{
				10: {PID: 10, PPID: 1, Name: "claude", StartMillis: 100},
			},
			start: 10, panePID: 10, agent: "claude",
			want: &ProcessRef{PID: 10, StartMillis: 100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, argv := IdentifyAgent(t.Context(), tc.table, tc.start, tc.panePID, tc.agent)
			assert.Equal(t, tc.want, ref)
			assert.Equal(t, tc.argv, argv)
		})
	}
}

func TestHookInPane(t *testing.T) {
	daemon := "/codex-home/packages/app-server-daemon/bin/codex"
	cases := []struct {
		name  string
		table ProcessTable
		start int32
		want  bool
	}{
		{
			name: "hook below the agent in the pane",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 10, "codex", "codex", "-m", "gpt-5"),
				30: proc(30, 20, "kwt", "kwt", "agent-hook", "run"),
			},
			start: 30, want: true,
		},
		{
			name: "hook from a server started outside the pane",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 1, "codex", daemon, "app-server", "--managed-daemon"),
				30: proc(30, 20, "kwt", "kwt", "agent-hook", "run"),
			},
			start: 30,
		},
		{
			// The server stays a child of the session that started it, so
			// another session's hook runs inside this pane's process tree.
			name: "hook from a server the pane's agent started",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 10, "codex", "codex"),
				25: proc(25, 20, "codex", daemon, "app-server", "--managed-daemon"),
				30: proc(30, 25, "kwt", "kwt", "agent-hook", "run"),
			},
			start: 30,
		},
		{
			name: "hook from a server started through npm's wrapper",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 1, "node", "node", "/lib/@openai/codex/bin/codex.js", "app-server"),
				30: proc(30, 20, "kwt", "kwt", "agent-hook", "run"),
			},
			start: 30,
		},
		{
			// A directory named app-server is an option value, not the
			// server subcommand.
			name: "hook below an agent working in an app-server directory",
			table: fakeProcesses{
				10: proc(10, 1, "zsh", "-zsh"),
				20: proc(20, 10, "codex", "codex", "--no-daemon", "-C", "app-server", "--add-dir", "app-server"),
				30: proc(30, 20, "kwt", "kwt", "agent-hook", "run"),
			},
			start: 30, want: true,
		},
		{
			name:  "unreadable process table",
			table: failingProcesses{},
			start: 30, want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HookInPane(t.Context(), tc.table, tc.start, 10))
		})
	}
}

type failingProcesses struct{}

func (failingProcesses) Lookup(context.Context, int32) (ProcInfo, error) {
	return ProcInfo{}, errors.New("permission denied")
}

func TestCheckLiveness(t *testing.T) {
	table := fakeProcesses{10: proc(10, 1, "claude", "claude")}

	assert.Equal(t, LivenessAlive, CheckLiveness(t.Context(), table, &ProcessRef{PID: 10, StartMillis: 100}))
	assert.Equal(t, LivenessGone, CheckLiveness(t.Context(), table, &ProcessRef{PID: 10, StartMillis: 99}),
		"a reused PID is a different process")
	assert.Equal(t, LivenessGone, CheckLiveness(t.Context(), table, &ProcessRef{PID: 11, StartMillis: 110}))
	assert.Equal(t, LivenessUnknown, CheckLiveness(t.Context(), table, nil))
	assert.Equal(t, LivenessUnknown, CheckLiveness(t.Context(), failingProcesses{}, &ProcessRef{PID: 10, StartMillis: 100}))
}

// TestProcessHelper is the process TestSystemProcessesTracksARealProcess
// tracks.
func TestProcessHelper(t *testing.T) {
	if os.Getenv("KWT_PROCESS_HELPER") == "" {
		t.Skip("helper process")
	}
	time.Sleep(30 * time.Second)
}

func TestSystemProcessesTracksARealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process tree")
	}
	// kwt's test harness points gopsutil at an empty process table on Linux;
	// this test needs the real one.
	if runtime.GOOS == "linux" {
		t.Setenv("HOST_PROC", "/proc")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessHelper$")
	cmd.Env = append(os.Environ(), "KWT_PROCESS_HELPER=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := int32(cmd.Process.Pid)
	agent := filepath.Base(os.Args[0])

	table := SystemProcesses()
	// Start can return before the process table shows the new program, so
	// wait for it, reporting what the table last showed.
	var ref *ProcessRef
	var argv []string
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		info, err := table.Lookup(t.Context(), pid)
		require.NoError(c, err)
		ref, argv = IdentifyAgent(t.Context(), table, pid, pid, agent)
		require.NotNil(c, ref, "process %+v is not identified as %s", info, agent)
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, pid, ref.PID)
	assert.Equal(t, cmd.Args, argv)
	assert.Equal(t, LivenessAlive, CheckLiveness(t.Context(), table, ref))

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	assert.Equal(t, LivenessGone, CheckLiveness(t.Context(), table, ref))
}
