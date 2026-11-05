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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentHookRunRecordsStateDespiteBrokenConfig(t *testing.T) {
	if os.Getenv("KWT_TEST_AGENT_HOOK_RUN") == "1" {
		rootCmd.SetArgs([]string{
			"agent-hook", "run", "--agent", "claude", "--source", "kwt-tmux-agent-state",
		})
		Execute()
		return
	}
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
	kwtHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte("invalid = [\n"), 0o600))

	hook := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAgentHookRunRecordsStateDespiteBrokenConfig$")
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
	output, err := hook.CombinedOutput()
	require.NoError(t, err, string(output))

	value, err := exec.CommandContext(
		t.Context(), "tmux", "-L", socket,
		"show-options", "-p", "-q", "-v", "-t", fields[0], "@kwt_agent",
	).CombinedOutput()
	require.NoError(t, err, string(value))
	assert.Regexp(t, `^done [0-9]+ claude$`, strings.TrimSpace(string(value)))
}
