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

func TestSessionDismissAndForget(t *testing.T) {
	env := newSessionEnv(t)
	for _, name := range []string{"work", "ops"} {
		pane := env.mustTmux(t, "-L", "default", "new-session", "-d", "-s", name, "-P", "-F", "#{pane_id}")
		env.hook(t, "claude", pane, sessionStartPayload("c-"+name))
	}
	env.mustTmux(t, "-L", "default", "new-session", "-d", "-s", "scratch")
	identity := strings.Fields(env.mustTmux(t, "-L", "default", "display-message", "-p", "-t", "scratch",
		"#{socket_path} #{session_id} #{pid} #{session_created}"))
	require.Len(t, identity, 4)
	env.snapshot(t)

	_, err := env.kwt(t, "", nil, "session", "forget", "--socket", identity[0],
		"--session-id", identity[1], "--server-pid", identity[2], "--created", identity[3])
	require.NoError(t, err)
	env.mustTmux(t, "-L", "default", "kill-server")
	entries := env.snapshot(t)
	require.Len(t, entries, 2, "a forgotten session is not restorable")

	_, err = env.kwt(t, "", nil, "session", "dismiss", "nope")
	require.Error(t, err)
	_, err = env.kwt(t, "", nil, "session", "dismiss", entries[0].ID)
	require.NoError(t, err)
	remaining := env.snapshot(t)
	require.Len(t, remaining, 1)
	assert.Equal(t, entries[1].ID, remaining[0].ID)

	_, err = env.kwt(t, "", nil, "session", "dismiss", "--all")
	require.NoError(t, err)
	assert.Empty(t, env.snapshot(t))
}

func TestSessionExecRunsTheClaimedLaunchOnce(t *testing.T) {
	env := newSessionEnv(t)
	pane := env.mustTmux(t, "-L", "default", "new-session", "-d", "-s", "work", "-P", "-F", "#{pane_id}")
	fields := strings.Fields(env.mustTmux(t, "-L", "default", "display-message", "-p", "-t", pane, "#{socket_path} #{pid}"))
	require.Len(t, fields, 2)
	socket := tmuxrestore.Socket{Path: fields[0], Name: "default"}
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "ran")
	var id string
	store := tmuxrestore.OpenStore(env.home)
	require.NoError(t, store.WithLock(t.Context(), socket, func(f *tmuxrestore.Files) error {
		f.Live = &tmuxrestore.Snapshot{Socket: socket, Server: tmuxrestore.ServerID{PID: "1"}, Sessions: []tmuxrestore.Session{{Name: "work"}}}
		f.Promote()
		entry := &f.Restorable.Entries[0]
		id = entry.ID
		entry.State, entry.LiveServerPID = tmuxrestore.StateLaunching, fields[1]
		entry.Launches = []tmuxrestore.PaneLaunch{{
			PaneID: pane, Status: tmuxrestore.LaunchWaiting, Dir: dir,
			Argv: []string{"/bin/sh", "-c", "pwd -P > " + out},
		}}
		return nil
	}))
	tmuxEnv := []string{"TMUX=" + fields[0] + "," + fields[1] + ",0", "TMUX_PANE=" + pane}

	_, err := env.kwt(t, "", tmuxEnv, "session", "exec", id, "0")
	require.NoError(t, err)
	ran, err := os.ReadFile(out)
	require.NoError(t, err)
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, resolved, strings.TrimSpace(string(ran)), "the program runs in the saved directory")

	_, err = env.kwt(t, "", tmuxEnv, "session", "exec", id, "0")
	assert.Error(t, err, "a launch runs once")
}
