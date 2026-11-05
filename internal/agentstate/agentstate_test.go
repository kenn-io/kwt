package agentstate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestHookEventsRecordPaneState(t *testing.T) {
	pane := startPrivateTmuxPane(t)
	clock := time.Unix(1_800_000_000, 0)
	handler := Handler{
		Agent:    agenthook.AgentClaude,
		Recorder: NewTmuxRecorder(),
		Now:      func() time.Time { return clock },
	}

	steps := []struct {
		payload string
		want    string
	}{
		{claudePayload("UserPromptSubmit", `"prompt":"fix it"`), "working 1800000000 claude"},
		{claudePayload("PermissionRequest", `"tool_name":"Bash","tool_input":{"command":"ls"}`), "approval 1800000000 claude"},
		{claudePayload("PostToolUse", `"tool_name":"Bash","tool_input":{},"tool_response":{}`), "working 1800000000 claude"},
		{claudePayload("PreToolUse", `"tool_name":"AskUserQuestion","tool_input":{}`), "input 1800000000 claude"},
		{claudePayload("Notification", `"message":"Claude needs your permission","notification_type":"permission_prompt"`), "approval 1800000000 claude"},
		{claudePayload("Stop", ""), "done 1800000000 claude"},
		// Claude raises idle_prompt a minute after Stop; it must not
		// replace the finished turn.
		{claudePayload("Notification", `"message":"waiting","notification_type":"idle_prompt"`), "done 1800000000 claude"},
		// A subagent finishing is not the session handing control back.
		{claudePayload("UserPromptSubmit", `"prompt":"again"`), "working 1800000000 claude"},
		{claudePayload("Stop", `"agent_id":"sub-1","agent_type":"Explore"`), "working 1800000000 claude"},
		{claudePayload("SessionEnd", `"reason":"prompt_input_exit"`), ""},
	}
	for _, step := range steps {
		err := agenthook.Handle(
			t.Context(),
			agenthook.AgentClaude,
			strings.NewReader(step.payload),
			io.Discard,
			handler,
		)
		require.NoError(t, err, step.payload)
		assert.Equal(t, step.want, pane.option(t), step.payload)
	}
}

func TestRecorderOutsideTmuxDoesNothing(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	handler := Handler{
		Agent:    agenthook.AgentClaude,
		Recorder: NewTmuxRecorder(),
	}

	err := agenthook.Handle(
		t.Context(),
		agenthook.AgentClaude,
		strings.NewReader(claudePayload("Stop", "")),
		io.Discard,
		handler,
	)

	require.NoError(t, err)
}

func TestInstallTargetsAgentsPresentOnHost(t *testing.T) {
	home := isolateAgentConfig(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o700))

	results, err := Install("/opt/kwt/bin/kwt", nil)
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.Equal(t, agenthook.AgentClaude, results[0].Agent)
	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	require.NoError(t, err)
	assert.Contains(t, string(settings), "/opt/kwt/bin/kwt agent-hook run --agent claude "+Marker)

	statuses, err := Statuses("/opt/kwt/bin/kwt")
	require.NoError(t, err)
	claude := findStatus(t, statuses, agenthook.AgentClaude)
	assert.True(t, claude.Available)
	assert.True(t, claude.Installed)
	assert.True(t, claude.Current)
	codex := findStatus(t, statuses, agenthook.AgentCodex)
	assert.False(t, codex.Available)
	assert.False(t, codex.Installed)

	moved, err := Statuses("/opt/kwt-next/bin/kwt")
	require.NoError(t, err)
	claude = findStatus(t, moved, agenthook.AgentClaude)
	assert.True(t, claude.Installed)
	assert.False(t, claude.Current, "a moved executable needs reinstalling")

	removed, err := Uninstall(nil)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	statuses, err = Statuses("/opt/kwt/bin/kwt")
	require.NoError(t, err)
	assert.False(t, findStatus(t, statuses, agenthook.AgentClaude).Installed)
}

func TestInstallRejectsNamedAgentWithoutConfigDirectory(t *testing.T) {
	isolateAgentConfig(t)

	_, err := Install("/opt/kwt/bin/kwt", []agenthook.Agent{agenthook.AgentCodex})

	require.ErrorContains(t, err, "Codex")
}

type tmuxPane struct {
	socket string
	id     string
}

func (p tmuxPane) option(t *testing.T) string {
	t.Helper()
	output, err := exec.CommandContext(
		t.Context(),
		"tmux", "-L", p.socket,
		"show-options", "-p", "-q", "-v", "-t", p.id, Option,
	).CombinedOutput()
	require.NoError(t, err, string(output))
	return strings.TrimSpace(string(output))
}

func startPrivateTmuxPane(t *testing.T) tmuxPane {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tmux pane options are POSIX-only")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}
	socket := fmt.Sprintf("kwt-agentstate-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "tmux", "-L", socket, "kill-server").Run()
	})
	output, err := exec.CommandContext(
		t.Context(),
		"tmux", "-f", os.DevNull, "-L", socket,
		"new-session", "-d", "-P", "-F",
		"#{pane_id}\t#{socket_path},#{pid},#{session_id}",
	).CombinedOutput()
	require.NoError(t, err, string(output))
	fields := strings.Split(strings.TrimSpace(string(output)), "\t")
	require.Len(t, fields, 2)
	t.Setenv("TMUX", strings.Replace(fields[1], ",$", ",", 1))
	t.Setenv("TMUX_PANE", fields[0])
	return tmuxPane{socket: socket, id: fields[0]}
}

func claudePayload(event, fields string) string {
	payload := `{"session_id":"s-1","transcript_path":"/tmp/t.jsonl","cwd":"/tmp",` +
		`"permission_mode":"default","hook_event_name":"` + event + `"`
	if fields != "" {
		payload += "," + fields
	}
	return payload + "}"
}

func isolateAgentConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, profile := range agenthook.Profiles() {
		if profile.ConfigEnvironment != "" {
			t.Setenv(profile.ConfigEnvironment, "")
		}
	}
	return home
}

func findStatus(t *testing.T, statuses []Status, agent agenthook.Agent) Status {
	t.Helper()
	for _, status := range statuses {
		if status.Agent == agent {
			return status
		}
	}
	t.Fatalf("no status for %s", agent)
	return Status{}
}
