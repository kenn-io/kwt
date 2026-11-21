package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/tmuxrestore"
)

// restoreAcceptance runs a built kwt, as front ends and restored panes do.
// Restored panes type kwt's own path, so the test binary cannot stand in.
type restoreAcceptance struct {
	sessionEnv
	kwtPath string
	logs    string
}

func newRestoreAcceptance(t *testing.T) restoreAcceptance {
	t.Helper()
	env := newSessionEnv(t)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	a := restoreAcceptance{sessionEnv: env, kwtPath: filepath.Join(t.TempDir(), "kwt"), logs: t.TempDir()}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", a.kwtPath, "./cmd/kwt")
	build.Dir = root
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))

	// Agents are configured by absolute path: a login shell's PATH setup
	// could otherwise find a real claude or codex first.
	script, err := filepath.Abs(filepath.Join("testdata", "fake-agent.sh"))
	require.NoError(t, err)
	bin := t.TempDir()
	config := "[agents]\n"
	for _, agent := range []string{"claude", "codex"} {
		require.NoError(t, os.Symlink(script, filepath.Join(bin, agent)))
		config += agent + " = \"" + filepath.Join(bin, agent) + "\"\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(env.home, "config.toml"), []byte(config), 0o600))

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("FAKE_AGENT_LOG", a.logs)
	return a
}

func (a restoreAcceptance) run(t *testing.T, stdin string, extraEnv []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.kwtPath, args...)
	cmd.Env = a.environ(extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run(), "kwt %v", args)
	return stdout.String()
}

// startAgent records an agent conversation in pane. kwt records only a hook
// that runs inside its pane, so the hook runs there, as an agent's would.
func (a restoreAcceptance) startAgent(t *testing.T, socket, pane, agent, conversation string) {
	t.Helper()
	args := []string{"agent-hook", "run", "--agent", agent, "--source", "kwt-tmux-agent-state"}
	tmux := func(args ...string) *exec.Cmd {
		return a.tmux(t.Context(), append([]string{"-L", socket}, args...)...)
	}
	runInPane(t, tmux, pane, "TestKwtHelperProcess", []string{
		"KWT_HOME=" + a.home, "TMUX_TMPDIR=" + a.tmpdir, "KWT_TEST_ARGS=" + strings.Join(args, "\n"),
	}, sessionStartPayload(conversation))
}

func (a restoreAcceptance) entries(t *testing.T) []tmuxrestore.Listed {
	t.Helper()
	var listing struct {
		Entries []tmuxrestore.Listed `json:"entries"`
	}
	output := a.run(t, "", nil, "session", "snapshot", "--json")
	require.NoError(t, json.Unmarshal([]byte(output), &listing), output)
	return listing.Entries
}

// agentLog waits for the fake agent to record its start in pane.
func (a restoreAcceptance) agentLog(t *testing.T, pane string) map[string]string {
	t.Helper()
	path := filepath.Join(a.logs, strings.TrimPrefix(pane, "%"))
	var data []byte
	require.Eventually(t, func() bool {
		var err error
		data, err = os.ReadFile(path)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "no agent started in pane %s", pane)
	fields := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		fields[key] = value
	}
	return fields
}

func resolved(t *testing.T, dir string) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return path
}

func TestColdRestoreOfTwoSameNamedSessions(t *testing.T) {
	a := newRestoreAcceptance(t)
	left, right, other := t.TempDir(), t.TempDir(), t.TempDir()

	a.mustTmux(t, "-L", "default", "new-session", "-d", "-s", "work", "-c", left, "-x", "120", "-y", "30")
	a.mustTmux(t, "-L", "default", "split-window", "-h", "-l", "40", "-t", "work", "-c", right)
	panes := strings.Fields(a.mustTmux(t, "-L", "default", "list-panes", "-t", "work", "-F", "#{pane_id}"))
	require.Len(t, panes, 2)
	a.startAgent(t, "default", panes[1], "claude", "c-1")
	geometry := "#{pane_index} #{pane_left} #{pane_top} #{pane_width} #{pane_height} #{pane_active}"
	saved := a.mustTmux(t, "-L", "default", "list-panes", "-t", "work", "-F", geometry)

	otherPane := a.mustTmux(t, "-L", "other", "new-session", "-d", "-s", "work", "-c", other, "-P", "-F", "#{pane_id}")
	a.startAgent(t, "other", otherPane, "codex", "x-1")

	a.mustTmux(t, "-L", "default", "kill-server")
	a.mustTmux(t, "-L", "other", "kill-server")

	entries := a.entries(t)
	require.Len(t, entries, 2, "both servers' sessions survive with the same name")
	bySocket := map[string]tmuxrestore.Listed{}
	for _, entry := range entries {
		assert.Equal(t, "work", entry.Session)
		bySocket[entry.Socket.Name] = entry
	}
	require.Contains(t, bySocket, "default")
	require.Contains(t, bySocket, "other")
	assert.Equal(t, []string{"claude"}, bySocket["default"].Agents)
	assert.Equal(t, []string{"codex"}, bySocket["other"].Agents)

	var restored struct {
		Entries []tmuxrestore.EntryReport `json:"entries"`
	}
	output := a.run(t, "", nil, "session", "restore", bySocket["default"].ID, "--json")
	require.NoError(t, json.Unmarshal([]byte(output), &restored), output)
	require.Len(t, restored.Entries, 1)
	report := restored.Entries[0]
	assert.Equal(t, tmuxrestore.OutcomeRestored, report.Outcome, output)
	require.NotNil(t, report.Live)
	assert.Equal(t, "default", report.Live.Socket.Name)

	assert.Equal(t, saved, a.mustTmux(t, "-L", "default", "list-panes", "-t", "work", "-F", geometry),
		"pane geometry and the active pane come back by index")
	restoredPanes := strings.Fields(a.mustTmux(t, "-L", "default", "list-panes", "-t", "work", "-F", "#{pane_id}"))
	require.Len(t, restoredPanes, 2)
	assert.Equal(t, resolved(t, left), resolved(t, a.mustTmux(t, "-L", "default", "display-message", "-p", "-t", restoredPanes[0], "#{pane_current_path}")))

	started := a.agentLog(t, restoredPanes[1])
	assert.Equal(t, "claude", started["name"])
	assert.Equal(t, resolved(t, right), started["cwd"])
	assert.True(t, strings.HasSuffix(started["args"], "--resume c-1"), started["args"])
	_, err := os.Stat(filepath.Join(a.logs, strings.TrimPrefix(restoredPanes[0], "%")))
	assert.ErrorIs(t, err, os.ErrNotExist, "a pane with no conversation gets a shell only")

	remaining := a.entries(t)
	require.Len(t, remaining, 1, "the other socket's session is still restorable")
	assert.Equal(t, "other", remaining[0].Socket.Name)
}
