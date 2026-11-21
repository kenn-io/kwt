package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

	state, _ := recordStopThroughHook(t, kwtHome, true)
	assert.Regexp(t, `^done [0-9]+ claude$`, state)
}

func TestAgentHookRunRecordsStateWhileConfigIsLocked(t *testing.T) {
	kwtHome := t.TempDir()
	lock := flock.New(filepath.Join(kwtHome, "config.toml.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { _ = lock.Unlock() })

	state, _ := recordStopThroughHook(t, kwtHome, true)
	assert.Regexp(t, `^done [0-9]+ claude$`, state)
}

func TestAgentHookRunRecordsNothingFromOutsideItsPane(t *testing.T) {
	// Codex's shared background server runs every session's hooks with the
	// tmux environment of the pane that started the server.
	state, resume := recordStopThroughHook(t, t.TempDir(), false)
	assert.Empty(t, state)
	assert.Empty(t, resume)
}

// recordStopThroughHook sends a Stop event through a kwt hook process whose
// tmux environment names a private pane, and returns the pane's recorded
// state and resume record. With inPane, the hook runs in that pane;
// otherwise it runs from the test. The hook must finish well within an
// agent's two-second hook timeout.
func recordStopThroughHook(t *testing.T, kwtHome string, inPane bool) (string, string) {
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
	tmux := func(args ...string) *exec.Cmd {
		return exec.CommandContext(t.Context(), "tmux", append([]string{"-f", os.DevNull, "-L", socket}, args...)...)
	}
	created, err := tmux("new-session", "-d", "-P", "-F", "#{pane_id}\t#{socket_path},#{pid},0").CombinedOutput()
	require.NoError(t, err, string(created))
	fields := strings.Split(strings.TrimSpace(string(created)), "\t")
	require.Len(t, fields, 2)

	env := []string{"KWT_TEST_AGENT_HOOK_RUN=1", "KWT_HOME=" + kwtHome}
	payload := `{"session_id":"s-1","transcript_path":"/tmp/t.jsonl","cwd":"/tmp",` +
		`"permission_mode":"default","hook_event_name":"Stop"}`
	var elapsed time.Duration
	if inPane {
		elapsed = runInPane(t, tmux, fields[0], "TestAgentHookRunHelperProcess", env, payload)
	} else {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		hook := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentHookRunHelperProcess$")
		hook.Env = append(os.Environ(), append(env,
			"HOST_PROC=/proc", "TMUX="+fields[1], "TMUX_PANE="+fields[0])...)
		hook.Stdin = strings.NewReader(payload)
		started := time.Now()
		output, err := hook.CombinedOutput()
		require.NoError(t, err, string(output))
		elapsed = time.Since(started)
	}
	assert.Less(t, elapsed, 2*time.Second, "hook must not wait on kwt configuration")

	option := func(name string) string {
		value, err := tmux("show-options", "-p", "-q", "-v", "-t", fields[0], name).CombinedOutput()
		require.NoError(t, err, string(value))
		return strings.TrimSpace(string(value))
	}
	return option("@kwt_agent"), option("@kwt_agent_resume")
}

// runInPane runs the test binary's helper test in pane, below the pane's
// shell as a hook runs below an agent, and returns how long it took. The
// helper reads stdin and sees env on top of the pane's tmux environment.
// Afterwards the pane sleeps, which keeps its server running.
func runInPane(
	t *testing.T, tmux func(...string) *exec.Cmd, pane, helper string, env []string, stdin string,
) time.Duration {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "output")
	status := filepath.Join(dir, "status")
	require.NoError(t, os.WriteFile(input, []byte(stdin), 0o600))
	var script strings.Builder
	// kwt's test harness hides the process table on Linux; the hook reads it
	// to find its pane.
	for _, entry := range append(slices.Clone(env), "HOST_PROC=/proc") {
		name, value, _ := strings.Cut(entry, "=")
		fmt.Fprintf(&script, "export %s=%s\n", name, shellQuote(value))
	}
	// The commands after the helper keep the shell from exec'ing it.
	fmt.Fprintf(&script, "%s -test.run=%s <%s >%s 2>&1\necho $? >%s\nexec sleep 600\n",
		shellQuote(os.Args[0]), shellQuote("^"+helper+"$"),
		shellQuote(input), shellQuote(output), shellQuote(status))
	path := filepath.Join(dir, "run.sh")
	require.NoError(t, os.WriteFile(path, []byte(script.String()), 0o600))

	started := time.Now()
	respawned, err := tmux("respawn-pane", "-k", "-t", pane, "/bin/sh "+shellQuote(path)).CombinedOutput()
	require.NoError(t, err, string(respawned))
	require.Eventually(t, func() bool {
		code, err := os.ReadFile(status)
		return err == nil && len(code) > 0
	}, 20*time.Second, 10*time.Millisecond, "helper did not finish in its pane")
	elapsed := time.Since(started)
	code, err := os.ReadFile(status)
	require.NoError(t, err)
	logged, _ := os.ReadFile(output)
	require.Equal(t, "0", strings.TrimSpace(string(code)), string(logged))
	return elapsed
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
