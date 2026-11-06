package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentHookRunHelperProcess is the kwt process the tests below start. It
// goes through Execute with the real arguments, as an agent's hook would.
func TestAgentHookRunHelperProcess(t *testing.T) {
	if os.Getenv("KWT_TEST_AGENT_HOOK_RUN") != "1" {
		t.Skip("helper process")
	}
	os.Args = []string{
		os.Args[0], "agent-hook", "run", "--agent", "claude", "--source", "kwt-tmux-agent-state",
	}
	Execute()
}

func TestAgentHookRunRecordsStateDespiteBrokenConfig(t *testing.T) {
	kwtHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte("invalid = [\n"), 0o600))

	assert.Regexp(t, `^done [0-9]+ claude$`, recordStopThroughHook(t, kwtHome))
}

func TestAgentHookRunRecordsStateWhileConfigIsLocked(t *testing.T) {
	kwtHome := t.TempDir()
	lock := flock.New(filepath.Join(kwtHome, "config.toml.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { _ = lock.Unlock() })

	assert.Regexp(t, `^done [0-9]+ claude$`, recordStopThroughHook(t, kwtHome))
}

// recordStopThroughHook sends a Stop event through a kwt hook process inside
// a private tmux pane and returns the pane's recorded state. The hook must
// finish well within an agent's two-second hook timeout.
func recordStopThroughHook(t *testing.T, kwtHome string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tmux pane options are POSIX-only")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}
	socket := fmt.Sprintf("kwt-agenthook-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "tmux", "-L", socket, "kill-server").Run()
	})
	created, err := exec.CommandContext(
		t.Context(), "tmux", "-f", os.DevNull, "-L", socket,
		"new-session", "-d", "-P", "-F", "#{pane_id}\t#{socket_path},#{pid},0",
	).CombinedOutput()
	require.NoError(t, err, string(created))
	fields := strings.Split(strings.TrimSpace(string(created)), "\t")
	require.Len(t, fields, 2)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hook := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentHookRunHelperProcess$")
	hook.Env = append(os.Environ(),
		"KWT_TEST_AGENT_HOOK_RUN=1",
		"KWT_HOME="+kwtHome,
		"TMUX="+fields[1],
		"TMUX_PANE="+fields[0],
	)
	hook.Stdin = strings.NewReader(
		`{"session_id":"s-1","transcript_path":"/tmp/t.jsonl","cwd":"/tmp",` +
			`"permission_mode":"default","hook_event_name":"Stop"}`,
	)
	started := time.Now()
	output, err := hook.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Less(t, time.Since(started), 2*time.Second, "hook must not wait on kwt configuration")

	value, err := exec.CommandContext(
		t.Context(), "tmux", "-L", socket,
		"show-options", "-p", "-q", "-v", "-t", fields[0], "@kwt_agent",
	).CombinedOutput()
	require.NoError(t, err, string(value))
	return strings.TrimSpace(string(value))
}
