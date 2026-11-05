package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kwt/internal/agentstate"
)

var (
	agentHookAgents     []string
	agentHookExecutable string
	agentHookJSON       bool
	agentHookRunAgent   string
	agentHookRunSource  string
)

var agentHookCmd = &cobra.Command{
	Use:   "agent-hook",
	Short: "Record coding-agent attention state on tmux panes",
	Long: `Install lifecycle hooks into coding agents so each agent records its
state on the tmux pane it runs in. Terminal front ends such as Ghosthub read
the pane option @kwt_agent to show which sessions are waiting for
approval or input, or have finished a turn.

Hooks do nothing when the agent is not running inside tmux.`,
	Args: cobra.NoArgs,
	// Hooks must never fail because of kwt configuration or the agent's
	// working directory, so skip the root config and project merge.
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var agentHookInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install agent hooks for this user",
	Long: `Install kwt's hooks into each supported agent's user config. Without
--agent, every supported agent whose config directory exists is updated.
Reinstalling replaces kwt's earlier hooks and keeps hooks owned by others.`,
	Example: `  kwt agent-hook install
  kwt agent-hook install --agent claude --agent codex`,
	Args: cobra.NoArgs,
	RunE: runAgentHookInstall,
}

var agentHookUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove kwt's agent hooks",
	Args:  cobra.NoArgs,
	RunE:  runAgentHookUninstall,
}

var agentHookStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show which agents have kwt's hooks installed",
	Args:  cobra.NoArgs,
	RunE:  runAgentHookStatus,
}

var agentHookRunCmd = &cobra.Command{
	Use:    "run",
	Short:  "Handle one agent hook event (invoked by agents)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runAgentHookRun,
}

func init() {
	rootCmd.AddCommand(agentHookCmd)
	agentHookCmd.AddCommand(
		agentHookInstallCmd,
		agentHookUninstallCmd,
		agentHookStatusCmd,
		agentHookRunCmd,
	)
	for _, command := range []*cobra.Command{agentHookInstallCmd, agentHookUninstallCmd} {
		command.Flags().StringSliceVar(&agentHookAgents, "agent", nil, "Agent to update (repeatable)")
	}
	for _, command := range []*cobra.Command{agentHookInstallCmd, agentHookStatusCmd} {
		command.Flags().StringVar(&agentHookExecutable, "executable", "", "kwt path written into hooks (default: this executable)")
	}
	for _, command := range []*cobra.Command{agentHookInstallCmd, agentHookUninstallCmd, agentHookStatusCmd} {
		command.Flags().BoolVar(&agentHookJSON, "json", false, "Output as JSON")
	}
	agentHookRunCmd.Flags().StringVar(&agentHookRunAgent, "agent", "", "Agent that invoked the hook")
	agentHookRunCmd.Flags().StringVar(&agentHookRunSource, "source", "", "Hook owner")
}

func runAgentHookInstall(cmd *cobra.Command, _ []string) error {
	agents, err := parseHookAgents(agentHookAgents)
	if err != nil {
		return err
	}
	executable, err := hookExecutable()
	if err != nil {
		return err
	}
	results, err := agentstate.Install(executable, agents)
	if err != nil {
		return err
	}
	if agentHookJSON {
		return writeAgentHookStatuses(cmd, executable)
	}
	if len(results) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No supported agent config directories found.")
		return err
	}
	for _, result := range results {
		profile, _ := agenthook.LookupProfile(result.Agent)
		verb := "Installed"
		if !result.Changed {
			verb = "Already current:"
		}
		if _, err := fmt.Fprintf(
			cmd.OutOrStdout(), "%s %s hooks in %s\n", verb, profile.DisplayName, result.ConfigPath,
		); err != nil {
			return err
		}
		if result.Agent == agenthook.AgentCodex && result.Changed {
			if _, err := fmt.Fprintln(
				cmd.OutOrStdout(), "Open /hooks in Codex once to review and trust the new hook commands.",
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func runAgentHookUninstall(cmd *cobra.Command, _ []string) error {
	agents, err := parseHookAgents(agentHookAgents)
	if err != nil {
		return err
	}
	results, err := agentstate.Uninstall(agents)
	if err != nil {
		return err
	}
	if agentHookJSON {
		executable, err := hookExecutable()
		if err != nil {
			return err
		}
		return writeAgentHookStatuses(cmd, executable)
	}
	for _, result := range results {
		profile, _ := agenthook.LookupProfile(result.Agent)
		if _, err := fmt.Fprintf(
			cmd.OutOrStdout(), "Removed %s hooks from %s\n", profile.DisplayName, result.ConfigPath,
		); err != nil {
			return err
		}
	}
	return nil
}

func runAgentHookStatus(cmd *cobra.Command, _ []string) error {
	executable, err := hookExecutable()
	if err != nil {
		return err
	}
	if agentHookJSON {
		return writeAgentHookStatuses(cmd, executable)
	}
	statuses, err := agentstate.Statuses(executable)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		state := "not set up"
		switch {
		case status.Current:
			state = "installed"
		case status.Installed:
			state = "installed for another kwt; reinstall to update"
		case status.Available:
			state = "not installed"
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s\n", status.DisplayName, state); err != nil {
			return err
		}
	}
	return nil
}

func writeAgentHookStatuses(cmd *cobra.Command, executable string) error {
	statuses, err := agentstate.Statuses(executable)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(struct {
		Agents []agentstate.Status `json:"agents"`
	}{Agents: statuses})
}

// runAgentHookRun always succeeds: a hook failure must never interrupt the
// agent, and missing state only hides an attention indicator.
func runAgentHookRun(cmd *cobra.Command, _ []string) error {
	if agentHookRunSource != strings.TrimPrefix(agentstate.Marker, "--source ") {
		return nil
	}
	agent, err := agenthook.ParseAgent(agentHookRunAgent)
	if err != nil {
		return nil
	}
	_ = agenthook.Handle(
		cmd.Context(),
		agent,
		cmd.InOrStdin(),
		cmd.OutOrStdout(),
		agentstate.Handler{Agent: agent, Recorder: agentstate.NewTmuxRecorder()},
	)
	return nil
}

func parseHookAgents(names []string) ([]agenthook.Agent, error) {
	agents := make([]agenthook.Agent, 0, len(names))
	for _, name := range names {
		agent, err := agenthook.ParseAgent(name)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

func hookExecutable() (string, error) {
	if path := strings.TrimSpace(agentHookExecutable); path != "" {
		return path, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve kwt executable: %w", err)
	}
	return path, nil
}
