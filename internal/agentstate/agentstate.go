// Package agentstate records coding-agent lifecycle state on the tmux pane
// the agent runs in, so terminal front ends such as Ghosthub can show which
// sessions need attention without a daemon or network listener.
//
// Agent hooks installed by Install invoke `kwt agent-hook run`, which maps
// each lifecycle event to a state and writes it to the pane user option
// Option as "<state> <unix-seconds> <agent>". SessionStart and SessionEnd
// clear the option. Outside tmux the hook does nothing.
package agentstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/agenthook"
)

// Option is the tmux pane user option that holds the recorded state.
const Option = "@kwt_agent"

// Marker identifies the hook commands kwt owns inside agent configs.
const Marker = "--source kwt-tmux-agent-state"

// State is the recorded agent lifecycle state.
type State string

const (
	Working  State = "working"
	Approval State = "approval"
	Input    State = "input"
	Done     State = "done"
)

// Recorder persists the state of the pane running the agent.
type Recorder interface {
	Record(ctx context.Context, value string) error
	Clear(ctx context.Context) error
}

// Handler maps agent hook events to pane state.
type Handler struct {
	agenthook.NoopHandler
	Agent    agenthook.Agent
	Recorder Recorder
	Now      func() time.Time
}

func (h Handler) SessionStart(
	ctx context.Context, input agenthook.SessionStartInput,
) (agenthook.SessionStartOutput, error) {
	return agenthook.SessionStartOutput{}, h.clear(ctx, input.CommonInput)
}

func (h Handler) UserPromptSubmit(
	ctx context.Context, input agenthook.UserPromptSubmitInput,
) (agenthook.UserPromptSubmitOutput, error) {
	return agenthook.UserPromptSubmitOutput{}, h.record(ctx, input.CommonInput, Working)
}

func (h Handler) PreToolUse(
	ctx context.Context, input agenthook.PreToolUseInput,
) (agenthook.PreToolUseOutput, error) {
	state := Working
	if isUserInputTool(input.ToolName) {
		state = Input
	}
	return agenthook.PreToolUseOutput{}, h.record(ctx, input.CommonInput, state)
}

func (h Handler) PostToolUse(
	ctx context.Context, input agenthook.PostToolUseInput,
) (agenthook.PostToolUseOutput, error) {
	return agenthook.PostToolUseOutput{}, h.record(ctx, input.CommonInput, Working)
}

func (h Handler) PostToolUseFailure(
	ctx context.Context, input agenthook.PostToolUseFailureInput,
) (agenthook.PostToolUseFailureOutput, error) {
	return agenthook.PostToolUseFailureOutput{}, h.record(ctx, input.CommonInput, Working)
}

func (h Handler) PermissionRequest(
	ctx context.Context, input agenthook.PermissionRequestInput,
) (agenthook.PermissionRequestOutput, error) {
	return agenthook.PermissionRequestOutput{}, h.record(ctx, input.CommonInput, Approval)
}

func (h Handler) Notification(
	ctx context.Context, input agenthook.NotificationInput,
) (agenthook.NotificationOutput, error) {
	var state State
	switch input.NotificationType {
	case agenthook.NotificationPermissionPrompt:
		state = Approval
	case agenthook.NotificationElicitationDialog, agenthook.NotificationAgentNeedsInput:
		state = Input
	case agenthook.NotificationAgentCompleted:
		state = Done
	default:
		// idle_prompt follows Stop by about a minute; recording it would
		// restamp a finished turn that the user may already have seen.
		return agenthook.NotificationOutput{}, nil
	}
	return agenthook.NotificationOutput{}, h.record(ctx, input.CommonInput, state)
}

func (h Handler) Stop(
	ctx context.Context, input agenthook.StopInput,
) (agenthook.StopOutput, error) {
	return agenthook.StopOutput{}, h.record(ctx, input.CommonInput, Done)
}

func (h Handler) SessionEnd(
	ctx context.Context, input agenthook.SessionEndInput,
) (agenthook.SessionEndOutput, error) {
	return agenthook.SessionEndOutput{}, h.clear(ctx, input.CommonInput)
}

func (h Handler) record(ctx context.Context, input agenthook.CommonInput, state State) error {
	if input.AgentID != "" || h.Recorder == nil {
		return nil
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	return h.Recorder.Record(ctx, fmt.Sprintf("%s %d %s", state, now().Unix(), h.Agent))
}

func (h Handler) clear(ctx context.Context, input agenthook.CommonInput) error {
	if input.AgentID != "" || h.Recorder == nil {
		return nil
	}
	return h.Recorder.Clear(ctx)
}

func isUserInputTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "askuserquestion", "request_user_input", "tool_user_input":
		return true
	default:
		return false
	}
}

// TmuxRecorder writes the pane option of the pane named by TMUX_PANE on the
// server named by TMUX.
type TmuxRecorder struct {
	pane string
}

// NewTmuxRecorder reads the calling process's tmux environment.
func NewTmuxRecorder() TmuxRecorder {
	if strings.TrimSpace(os.Getenv("TMUX")) == "" {
		return TmuxRecorder{}
	}
	return TmuxRecorder{pane: strings.TrimSpace(os.Getenv("TMUX_PANE"))}
}

func (r TmuxRecorder) Record(ctx context.Context, value string) error {
	return r.run(ctx, "set-option", "-p", "-t", r.pane, Option, value)
}

func (r TmuxRecorder) Clear(ctx context.Context) error {
	return r.run(ctx, "set-option", "-p", "-u", "-t", r.pane, Option)
}

func (r TmuxRecorder) run(ctx context.Context, args ...string) error {
	if !strings.HasPrefix(r.pane, "%") {
		return nil
	}
	tmux, err := tmuxExecutable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, tmux, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}

func tmuxExecutable() (string, error) {
	if path, err := exec.LookPath("tmux"); err == nil {
		return path, nil
	}
	for _, candidate := range []string{
		"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("tmux not found")
}

// Status reports one agent's hook installation.
type Status struct {
	Agent       agenthook.Agent `json:"agent"`
	DisplayName string          `json:"display_name"`
	ConfigPath  string          `json:"config_path"`
	// Available reports whether the agent's config directory exists.
	Available bool `json:"available"`
	// Installed reports whether any kwt hook command is present.
	Installed bool `json:"installed"`
	// Current reports whether the installed hooks match this executable
	// and event set exactly.
	Current bool `json:"current"`
}

// Install writes kwt's hooks into each agent's user config. With no agents,
// it targets every supported agent whose config directory already exists.
func Install(executable string, agents []agenthook.Agent) ([]agenthook.Result, error) {
	targets, err := installTargets(agents)
	if err != nil {
		return nil, err
	}
	results := make([]agenthook.Result, 0, len(targets))
	for _, agent := range targets {
		result, err := agenthook.Install(agent, installOptions(agent, executable))
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// Uninstall removes kwt's hooks. With no agents, it checks every supported
// agent and reports only configs that changed.
func Uninstall(agents []agenthook.Agent) ([]agenthook.Result, error) {
	if len(agents) == 0 {
		agents = allAgents()
	}
	results := []agenthook.Result{}
	for _, agent := range agents {
		result, err := agenthook.Uninstall(agent, "", Marker)
		if err != nil {
			return results, err
		}
		if result.Changed {
			results = append(results, result)
		}
	}
	return results, nil
}

// Statuses reports every supported agent's installation for executable.
func Statuses(executable string) ([]Status, error) {
	statuses := []Status{}
	for _, profile := range agenthook.Profiles() {
		path, err := agenthook.ConfigPath(profile.Agent)
		if err != nil {
			return nil, err
		}
		status := Status{
			Agent:       profile.Agent,
			DisplayName: profile.DisplayName,
			ConfigPath:  path,
			Available:   isDirectory(filepath.Dir(path)),
		}
		if status.Available {
			removal, err := agenthook.PlanUninstall(profile.Agent, "", Marker)
			if err != nil {
				return nil, err
			}
			status.Installed = removal.Changed
			if status.Installed {
				install, err := agenthook.PlanInstall(profile.Agent, installOptions(profile.Agent, executable))
				if err != nil {
					return nil, err
				}
				status.Current = !install.Changed
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func installOptions(agent agenthook.Agent, executable string) agenthook.InstallOptions {
	profile, _ := agenthook.LookupProfile(agent)
	hooks := make([]agenthook.Hook, 0, len(profile.SupportedEvents))
	for _, event := range profile.SupportedEvents {
		hooks = append(hooks, agenthook.Hook{Event: event, Timeout: 2 * time.Second})
	}
	return agenthook.InstallOptions{
		Executable: executable,
		Arguments: append(
			[]string{"agent-hook", "run", "--agent", string(agent)},
			strings.Fields(Marker)...,
		),
		Marker: Marker,
		Hooks:  hooks,
	}
}

func installTargets(agents []agenthook.Agent) ([]agenthook.Agent, error) {
	explicit := len(agents) > 0
	if !explicit {
		agents = allAgents()
	}
	targets := []agenthook.Agent{}
	for _, agent := range agents {
		path, err := agenthook.ConfigPath(agent)
		if err != nil {
			return nil, err
		}
		if isDirectory(filepath.Dir(path)) {
			targets = append(targets, agent)
			continue
		}
		if explicit {
			profile, _ := agenthook.LookupProfile(agent)
			return nil, fmt.Errorf(
				"%s config directory %s does not exist; run %s once first",
				profile.DisplayName, filepath.Dir(path), profile.DisplayName,
			)
		}
	}
	return targets, nil
}

func allAgents() []agenthook.Agent {
	profiles := agenthook.Profiles()
	agents := make([]agenthook.Agent, 0, len(profiles))
	for _, profile := range profiles {
		agents = append(agents, profile.Agent)
	}
	return agents
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
