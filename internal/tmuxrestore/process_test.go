package tmuxrestore

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"

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

func TestSystemProcessesTracksARealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process tree")
	}
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	require.NoError(t, cmd.Start())
	pid := int32(cmd.Process.Pid)

	table := SystemProcesses()
	ref, argv := IdentifyAgent(t.Context(), table, pid, pid, "sleep")
	require.NotNil(t, ref)
	assert.Equal(t, pid, ref.PID)
	assert.Equal(t, []string{"sleep", "30"}, argv)
	assert.Equal(t, LivenessAlive, CheckLiveness(t.Context(), table, ref))

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	assert.Equal(t, LivenessGone, CheckLiveness(t.Context(), table, ref))
}
