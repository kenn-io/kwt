package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/table"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

var (
	workspaceAddName string
	workspaceJSON    bool

	registerWorkspace     = config.RegisterWorkspace
	unregisterWorkspace   = config.UnregisterWorkspace
	loadWorkspaceConfig   = config.Load
	listWorkspaceSessions = func() ([]string, error) {
		return tmux.NewTmuxCommand("").ListSessions()
	}
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage directory workspaces not bound to a git worktree",
	// Isolation: workspace commands manage machine-level state in the global
	// config and must not merge the caller's cwd .kwt.toml. Skipping the root
	// cwd merge keeps the global config pristine while still
	// propagating global config initialization failures.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return requireConfigInitialization() },
}

var workspaceAddCmd = &cobra.Command{
	Use:   "add [path]",
	Short: "Register a directory as a workspace",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorkspaceAdd,
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered directory workspaces",
	Args:  cobra.NoArgs,
	RunE:  runWorkspaceList,
}

var workspaceRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Unregister a directory workspace (never deletes the directory)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceRemove,
}

func init() {
	workspaceAddCmd.Flags().StringVar(&workspaceAddName, "name", "", "workspace name (defaults to the directory base name)")
	workspaceListCmd.Flags().BoolVar(&workspaceJSON, "json", false, "Output in JSON format")
	workspaceCmd.AddCommand(workspaceAddCmd, workspaceListCmd, workspaceRemoveCmd)
	rootCmd.AddCommand(workspaceCmd)
}

func runWorkspaceAdd(cmd *cobra.Command, args []string) error {
	path := ""
	if len(args) == 1 {
		path = args[0]
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to resolve current directory: %w", err)
		}
		path = cwd
	}
	stored, err := registerWorkspace(models.Workspace{Name: workspaceAddName, Path: path})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "registered workspace %s at %s\n", stored.Name, stored.Path)
	return nil
}

func runWorkspaceList(cmd *cobra.Command, args []string) error {
	cfg, err := loadWorkspaceConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if len(cfg.Workspaces) == 0 {
		if workspaceJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(
				[]directoryWorkspaceRecord{},
			)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no workspaces registered")
		return nil
	}
	sessions, err := listWorkspaceSessions()
	if err != nil {
		return fmt.Errorf("failed to list tmux sessions: %w", err)
	}
	records := directoryWorkspaceRecords(cfg.Workspaces, sessions)
	if workspaceJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(records)
	}
	t := table.New().SetOutput(cmd.OutOrStdout()).Headers("NAME", "PATH", "SESSION")
	for _, record := range records {
		state := "stopped"
		if record.SessionLive {
			state = "live"
		}
		t.Row(record.Name, record.Path, state)
	}
	return t.Println()
}

func runWorkspaceRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	cfg, cfgErr := loadWorkspaceConfig()
	livePath := ""
	if cfgErr == nil {
		for _, workspace := range cfg.Workspaces {
			if strings.EqualFold(workspace.Name, name) {
				livePath = workspace.Path
			}
		}
	}
	if err := unregisterWorkspace(name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "unregistered workspace %s\n", name)
	if livePath != "" {
		sessions, err := listWorkspaceSessions()
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not check for a live session: %v\n", err)
		} else if session, ok := tmux.MatchDirWorkspaceSession(sessions, livePath); ok {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"its tmux session %s is still running; kill it with: tmux kill-session -t %s\n",
				session, session)
		}
	}
	return nil
}
