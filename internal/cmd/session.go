package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

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

var sessionRestoreCmd = &cobra.Command{
	Use:   "restore (--all | <id>...)",
	Short: "Restore saved tmux sessions and resume their conversations",
	Long: `Recreate each saved session's windows, panes, and layout on the server
it came from, then resume each pane's Claude Code or Codex conversation with
the settings it was started with. Panes that ran a layout preset's agent run
its configured command again. Other panes open a shell in their directory.

A session whose name is in use is skipped. Get IDs from
"kwt session snapshot".`,
	Args:    sessionTargets,
	PreRunE: globalOnlyPreRun,
	RunE:    runSessionRestore,
}

var sessionDismissCmd = &cobra.Command{
	Use:   "dismiss (--all | <id>...)",
	Short: "Delete saved sessions without restoring them",
	Args:  sessionTargets,
	RunE:  runSessionDismiss,
}

var sessionForget struct {
	socket, sessionID, serverPID, created string
}

var sessionForgetCmd = &cobra.Command{
	Use:   "forget",
	Short: "Stop saving one tmux session after it was killed on purpose",
	Long: `Remove one exact session from kwt's saved state, so a session killed
on purpose is not offered for restore. Front ends run this after they kill a
session; the server PID and creation time keep a reused session ID from
matching.`,
	Args: cobra.NoArgs,
	RunE: runSessionForget,
}

var sessionExecCmd = &cobra.Command{
	Use:    "exec <entry-id> <launch>",
	Short:  "Start a restored pane's program",
	Long:   `kwt session restore types this into each restored pane. It is not run by people.`,
	Args:   cobra.ExactArgs(2),
	Hidden: true,
	RunE:   runSessionExec,
}

var sessionAll bool

func init() {
	rootCmd.AddCommand(sessionCmd)
	sessionCmd.AddCommand(sessionSnapshotCmd, sessionRestoreCmd, sessionDismissCmd, sessionForgetCmd, sessionExecCmd)
	sessionSnapshotCmd.Flags().BoolVar(&sessionJSON, "json", false, "Print machine-readable JSON")
	sessionRestoreCmd.Flags().BoolVar(&sessionJSON, "json", false, "Print machine-readable JSON")
	for _, cmd := range []*cobra.Command{sessionRestoreCmd, sessionDismissCmd} {
		cmd.Flags().BoolVar(&sessionAll, "all", false, "Act on every saved session")
	}
	flags := sessionForgetCmd.Flags()
	flags.StringVar(&sessionForget.socket, "socket", "", "tmux socket path")
	flags.StringVar(&sessionForget.sessionID, "session-id", "", "tmux session ID, such as $3")
	flags.StringVar(&sessionForget.serverPID, "server-pid", "", "tmux server PID")
	flags.StringVar(&sessionForget.created, "created", "", "session creation time from #{session_created}")
	for _, name := range []string{"socket", "session-id", "server-pid", "created"} {
		_ = sessionForgetCmd.MarkFlagRequired(name)
	}
}

// sessionSkipsConfig reports session subcommands that must not wait on kwt
// configuration.
func sessionSkipsConfig(cmd *cobra.Command) bool {
	switch cmd {
	case sessionSnapshotCmd, sessionDismissCmd, sessionForgetCmd, sessionExecCmd:
		return true
	}
	return false
}

func sessionTargets(_ *cobra.Command, args []string) error {
	if sessionAll == (len(args) > 0) {
		return errors.New("give either --all or one or more IDs")
	}
	return nil
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

// saveSessions saves every known socket and returns what can be restored.
func saveSessions(cmd *cobra.Command, store tmuxrestore.Store, executable string) ([]tmuxrestore.Listed, error) {
	sockets, err := sessionSockets(store)
	if err != nil {
		return nil, err
	}
	listed, errs := tmuxrestore.SaveAndList(commandContext(cmd), tmuxrestore.ListDeps{
		Executable: executable,
		Store:      store,
		Processes:  tmuxrestore.SystemProcesses(),
		Protection: tmuxrestore.ProvenanceProtection(prStorePath()),
	}, sockets)
	for _, err := range errs {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", err)
	}
	return listed, nil
}

func runSessionSnapshot(cmd *cobra.Command, _ []string) error {
	store, err := sessionStore()
	if err != nil {
		return err
	}
	executable, err := agentstate.TmuxExecutable()
	if err != nil {
		return err
	}
	listed, err := saveSessions(cmd, store, executable)
	if err != nil {
		return err
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

// sessionClaimWait is how long restore waits for panes to start their
// programs before reporting them as still waiting.
const sessionClaimWait = 3 * time.Second

func runSessionRestore(cmd *cobra.Command, args []string) error {
	return ExecuteWithContext(false, func(appCtx *CommandContext) error {
		store, err := sessionStore()
		if err != nil {
			return err
		}
		executable, err := agentstate.TmuxExecutable()
		if err != nil {
			return err
		}
		kwtPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate kwt: %w", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home directory: %w", err)
		}
		// A save first turns sessions from a server that is now gone into
		// restorable entries.
		if _, err := saveSessions(cmd, store, executable); err != nil {
			return err
		}
		reports, restoreErr := tmuxrestore.Restore(commandContext(cmd), tmuxrestore.RestoreDeps{
			Store:      store,
			Executable: executable,
			Kwt:        kwtPath,
			Agents:     appCtx.Config.Agents,
			Home:       home,
			Protection: tmuxrestore.ProvenanceProtection(prStorePath()),
			ClaimWait:  sessionClaimWait,
		}, args, sessionAll)
		if err := printRestoreReports(cmd, reports); err != nil {
			return err
		}
		if restoreErr != nil {
			return restoreErr
		}
		for _, report := range reports {
			if report.Outcome == tmuxrestore.OutcomeFailed || report.Outcome == tmuxrestore.OutcomeNotFound {
				return errors.New("some sessions were not restored")
			}
		}
		return nil
	})(cmd, args)
}

func printRestoreReports(cmd *cobra.Command, reports []tmuxrestore.EntryReport) error {
	if reports == nil {
		reports = []tmuxrestore.EntryReport{}
	}
	if sessionJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			Entries []tmuxrestore.EntryReport `json:"entries"`
		}{Entries: reports})
	}
	out := cmd.OutOrStdout()
	if len(reports) == 0 {
		fmt.Fprintln(out, "No sessions to restore.")
	}
	for _, report := range reports {
		name := report.Session
		if name == "" {
			name = report.ID
		}
		line := name + ": " + report.Outcome
		if report.Reason != "" {
			line += " (" + report.Reason + ")"
		}
		fmt.Fprintln(out, line)
		for _, note := range report.Notes {
			fmt.Fprintln(out, "  "+note)
		}
		for _, pane := range report.Panes {
			if pane.Agent == "" && pane.Note == "" {
				continue
			}
			detail := fmt.Sprintf("  window %d pane %d: %s %s", pane.Window, pane.Pane, pane.Agent, pane.Status)
			if pane.Note != "" {
				detail += " (" + pane.Note + ")"
			}
			if len(pane.Dropped) > 0 {
				detail += "; launch flags dropped: " + strings.Join(pane.Dropped, " ")
			}
			fmt.Fprintln(out, detail)
		}
	}
	return nil
}

func runSessionDismiss(cmd *cobra.Command, args []string) error {
	store, err := sessionStore()
	if err != nil {
		return err
	}
	sockets, err := store.Sockets()
	if err != nil {
		return err
	}
	remaining := map[string]bool{}
	for _, id := range args {
		remaining[id] = true
	}
	for _, socket := range sockets {
		err := store.WithLock(commandContext(cmd), socket, func(f *tmuxrestore.Files) error {
			for _, entry := range slices.Clone(f.Restorable.Entries) {
				if sessionAll || remaining[entry.ID] {
					f.Dismiss([]string{entry.ID})
					delete(remaining, entry.ID)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(remaining) > 0 {
		return fmt.Errorf("no saved session with ID %s", strings.Join(slices.Sorted(maps.Keys(remaining)), ", "))
	}
	return nil
}

func runSessionForget(cmd *cobra.Command, _ []string) error {
	store, err := sessionStore()
	if err != nil {
		return err
	}
	return store.WithLock(commandContext(cmd), tmuxrestore.Socket{Path: sessionForget.socket}, func(f *tmuxrestore.Files) error {
		f.Forget(tmuxrestore.ServerID{PID: sessionForget.serverPID}, sessionForget.sessionID, sessionForget.created)
		return nil
	})
}

// sessionExecLockWait bounds how long a restored pane waits for the restore
// that typed its handoff to release the lock.
const sessionExecLockWait = 30 * time.Second

func runSessionExec(cmd *cobra.Command, args []string) error {
	launch, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("launch %q is not a number", args[1])
	}
	tmuxValue := os.Getenv("TMUX")
	socket, ok := tmuxrestore.SocketFromTMUX(tmuxValue)
	if !ok {
		return errors.New("kwt session exec runs only inside a restored tmux pane")
	}
	store, err := sessionStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(commandContext(cmd), sessionExecLockWait)
	defer cancel()
	target, err := tmuxrestore.Claim(ctx, store, socket, args[0], launch,
		os.Getenv("TMUX_PANE"), tmuxrestore.ServerPIDFromTMUX(tmuxValue))
	if err != nil {
		return err
	}
	return tmuxrestore.Exec(target)
}
