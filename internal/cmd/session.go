package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/agentstate"
	"go.kenn.io/kwt/internal/config"
	kwttmux "go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/tmuxrestore"
)

var sessionJSON bool

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Save and restore tmux sessions across reboots",
	Long: `kwt saves every tmux session's windows, pane layout, and directories,
along with the Claude Code or Codex conversation each pane was running.
After a reboot or a tmux kill-server, restore brings the sessions back and
resumes those conversations in the panes where they ran.

Agent hooks save as agents work, and "kwt session snapshot" saves on demand.`,
	Args: cobra.NoArgs,
	// Front ends call these commands often, and a restored pane runs
	// "session exec", so they skip the root config check.
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var sessionSnapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Save tmux sessions now and list the ones that can be restored",
	Long: `Save the default tmux server, kwt's server, and every server kwt has
saved before, then list the sessions that can be restored. A session can be
restored once the server it ran on is gone.`,
	Args: cobra.NoArgs,
	RunE: runSessionSnapshot,
}

func init() {
	rootCmd.AddCommand(sessionCmd)
	sessionCmd.AddCommand(sessionSnapshotCmd)
	sessionSnapshotCmd.Flags().BoolVar(&sessionJSON, "json", false, "Print machine-readable JSON")
}

// sessionSkipsConfig reports session subcommands that must not wait on kwt
// configuration.
func sessionSkipsConfig(cmd *cobra.Command) bool {
	return cmd == sessionSnapshotCmd
}

func sessionStore() (tmuxrestore.Store, error) {
	home, err := config.CanonicalHome()
	if err != nil {
		return tmuxrestore.Store{}, fmt.Errorf("resolve kwt home: %w", err)
	}
	return tmuxrestore.OpenStore(home), nil
}

// sessionSockets returns the default socket, kwt's socket, and every socket
// with saved files.
func sessionSockets(store tmuxrestore.Store) ([]tmuxrestore.Socket, error) {
	var sockets []tmuxrestore.Socket
	for _, name := range []string{"default", kwttmux.KWTServerSocketName} {
		socket, err := tmuxrestore.NamedSocket(name)
		if err != nil {
			return nil, err
		}
		sockets = append(sockets, socket)
	}
	saved, err := store.Sockets()
	if err != nil {
		return nil, err
	}
	return append(sockets, saved...), nil
}

func runSessionSnapshot(cmd *cobra.Command, _ []string) error {
	ctx := commandContext(cmd)
	store, err := sessionStore()
	if err != nil {
		return err
	}
	sockets, err := sessionSockets(store)
	if err != nil {
		return err
	}
	executable, err := agentstate.TmuxExecutable()
	if err != nil {
		return err
	}
	listed, errs := tmuxrestore.SaveAndList(ctx, tmuxrestore.ListDeps{
		Executable: executable,
		Store:      store,
		Processes:  tmuxrestore.SystemProcesses(),
		Protection: tmuxrestore.ProvenanceProtection(prStorePath()),
	}, sockets)
	for _, err := range errs {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", err)
	}
	if sessionJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			Entries []tmuxrestore.Listed `json:"entries"`
		}{Entries: listed})
	}
	if len(listed) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions to restore.")
		return nil
	}
	for _, entry := range listed {
		note := ""
		if entry.NameTaken {
			note = " (name in use)"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %d panes%s\n", entry.ID, entry.Session, entry.Panes, note)
	}
	return nil
}
