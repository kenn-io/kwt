package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/tmuxrestore"
)

// TestKwtHelperProcess runs kwt with the arguments in KWT_TEST_ARGS,
// separated by newlines, through Execute as a real invocation would.
func TestKwtHelperProcess(t *testing.T) {
	args := os.Getenv("KWT_TEST_ARGS")
	if args == "" {
		t.Skip("helper process")
	}
	os.Args = append([]string{os.Args[0]}, strings.Split(args, "\n")...)
	Execute()
	// Exit before the test framework prints its result to stdout.
	os.Exit(0)
}

// sessionEnv isolates kwt home and tmux's socket directory. The socket
// directory is short because Unix socket paths are limited to about 100
// bytes.
type sessionEnv struct {
	home   string
	tmpdir string
}

func newSessionEnv(t *testing.T) sessionEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tmux is POSIX-only")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}
	tmpdir, err := os.MkdirTemp("/tmp", "kws")
	require.NoError(t, err)
	env := sessionEnv{home: t.TempDir(), tmpdir: tmpdir}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		for _, name := range []string{"default", "kwt"} {
			_ = env.tmux(ctx, "-L", name, "kill-server").Run()
		}
		_ = os.RemoveAll(tmpdir)
	})
	return env
}

func (e sessionEnv) environ(extra ...string) []string {
	env := append(os.Environ(), "KWT_HOME="+e.home, "TMUX_TMPDIR="+e.tmpdir)
	return append(env, extra...)
}

func (e sessionEnv) tmux(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-f", os.DevNull}, args...)...)
	cmd.Env = e.environ()
	return cmd
}

func (e sessionEnv) mustTmux(t *testing.T, args ...string) string {
	t.Helper()
	output, err := e.tmux(t.Context(), args...).CombinedOutput()
	require.NoError(t, err, string(output))
	return strings.TrimSpace(string(output))
}

func (e sessionEnv) kwt(t *testing.T, stdin string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKwtHelperProcess$")
	cmd.Env = e.environ(append(extraEnv, "KWT_TEST_ARGS="+strings.Join(args, "\n"))...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return stdout.String(), err
}

func (e sessionEnv) hook(t *testing.T, agent, pane, payload string) time.Duration {
	t.Helper()
	tmuxValue := e.mustTmux(t, "-L", "default", "display-message", "-p", "-t", pane, "#{socket_path},#{pid},0")
	started := time.Now()
	_, err := e.kwt(t, payload, []string{"TMUX=" + tmuxValue, "TMUX_PANE=" + pane},
		"agent-hook", "run", "--agent", agent, "--source", "kwt-tmux-agent-state")
	require.NoError(t, err)
	return time.Since(started)
}

func sessionStartPayload(conversation string) string {
	return `{"session_id":"` + conversation + `","transcript_path":"/tmp/` + conversation +
		`.jsonl","cwd":"/tmp","permission_mode":"plan","hook_event_name":"SessionStart","source":"startup"}`
}

func (e sessionEnv) snapshot(t *testing.T) []tmuxrestore.Listed {
	t.Helper()
	output, err := e.kwt(t, "", nil, "session", "snapshot", "--json")
	require.NoError(t, err, output)
	var listing struct {
		Entries []tmuxrestore.Listed `json:"entries"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &listing), output)
	return listing.Entries
}

func TestAgentHookRecordsTheConversationAndSaves(t *testing.T) {
	env := newSessionEnv(t)
	pane := env.mustTmux(t, "-L", "default", "new-session", "-d", "-s", "work", "-P", "-F", "#{pane_id}")

	elapsed := env.hook(t, "claude", pane, sessionStartPayload("c-1"))
	assert.Less(t, elapsed, 2*time.Second)

	value := env.mustTmux(t, "-L", "default", "show-options", "-p", "-q", "-v", "-t", pane, tmuxrestore.ResumeOption)
	record, err := tmuxrestore.DecodeRecord(value)
	require.NoError(t, err)
	assert.Equal(t, "c-1", record.ConversationID)
	assert.Equal(t, "plan", record.PermissionMode)

	files, err := filepath.Glob(filepath.Join(env.home, "tmux", "*.json"))
	require.NoError(t, err)
	assert.Len(t, files, 1, "the hook saved the live snapshot")
}

func TestSessionSnapshotListsSessionsFromAStoppedServer(t *testing.T) {
	env := newSessionEnv(t)
	pane := env.mustTmux(t, "-L", "default", "new-session", "-d", "-s", "work", "-P", "-F", "#{pane_id}")
	env.hook(t, "codex", pane, sessionStartPayload("x-1"))
	assert.Empty(t, env.snapshot(t), "nothing is restorable while the server runs")

	env.mustTmux(t, "-L", "default", "kill-server")
	entries := env.snapshot(t)
	require.Len(t, entries, 1)
	assert.Equal(t, "work", entries[0].Session)
	assert.Equal(t, "default", entries[0].Socket.Name)
	assert.Equal(t, []string{"codex"}, entries[0].Agents)
	assert.False(t, entries[0].NameTaken)
}
