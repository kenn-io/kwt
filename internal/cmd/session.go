package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/agentstate"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	kwttmux "go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/tmuxrestore"
	"go.kenn.io/kwt/pkg/models"
	shared "go.kenn.io/kwt/worktree"
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

Worktree and directory workspace sessions are created the way "kwt open"
creates them; one whose worktree now holds a different checkout, or whose
directory is no longer registered, is skipped. A session whose name is
in use is skipped. Get IDs from "kwt session snapshot".`,
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
	socket, socketName, sessionID, serverPID, created string
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
	flags.StringVar(&sessionForget.socketName, "socket-name", "", "tmux socket name, as given to tmux -L")
	flags.StringVar(&sessionForget.sessionID, "session-id", "", "tmux session ID, such as $3")
	flags.StringVar(&sessionForget.serverPID, "server-pid", "", "tmux server PID")
	flags.StringVar(&sessionForget.created, "created", "", "session creation time from #{session_created}")
	for _, name := range []string{"session-id", "server-pid", "created"} {
		_ = sessionForgetCmd.MarkFlagRequired(name)
	}
	sessionForgetCmd.MarkFlagsOneRequired("socket", "socket-name")
	sessionForgetCmd.MarkFlagsMutuallyExclusive("socket", "socket-name")
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
	home, err := config.EnsureHome()
	if err != nil {
		return tmuxrestore.Store{}, fmt.Errorf("resolve kwt home: %w", err)
	}
	return tmuxrestore.OpenStore(home), nil
}

// sessionSockets returns the default socket, kwt's socket, and every socket
// with saved files. kwt runs its server with TMUX_TMPDIR unset, so its socket
// does not follow TMUX_TMPDIR the way the default socket does.
func sessionSockets(store tmuxrestore.Store) ([]tmuxrestore.Socket, error) {
	defaultSocket, err := tmuxrestore.NamedSocket("default")
	if err != nil {
		return nil, err
	}
	kwtSocket, err := tmuxrestore.SocketIn("", kwttmux.KWTServerSocketName)
	if err != nil {
		return nil, err
	}
	saved, err := store.Sockets()
	if err != nil {
		return nil, err
	}
	return append([]tmuxrestore.Socket{defaultSocket, kwtSocket}, saved...), nil
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
		panes := "panes"
		if entry.Panes == 1 {
			panes = "pane"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %d %s%s\n", entry.ID, entry.Session, entry.Panes, panes, note)
	}
	return nil
}

// sessionClaimWait is how long restore waits for panes to start their
// programs before reporting them as still waiting.
const sessionClaimWait = 3 * time.Second

// sessionRestoreDeps returns the inputs every restore shares.
func sessionRestoreDeps(cfg *models.Config) (tmuxrestore.RestoreDeps, error) {
	store, err := sessionStore()
	if err != nil {
		return tmuxrestore.RestoreDeps{}, err
	}
	executable, err := agentstate.TmuxExecutable()
	if err != nil {
		return tmuxrestore.RestoreDeps{}, err
	}
	kwtPath, err := os.Executable()
	if err != nil {
		return tmuxrestore.RestoreDeps{}, fmt.Errorf("locate kwt: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return tmuxrestore.RestoreDeps{}, fmt.Errorf("locate home directory: %w", err)
	}
	return tmuxrestore.RestoreDeps{
		Store:      store,
		Executable: executable,
		Kwt:        kwtPath,
		Agents:     cfg.Agents,
		StripNames: credentials.ProtectedNames(cfg),
		Home:       home,
		Protection: tmuxrestore.ProvenanceProtection(prStorePath()),
	}, nil
}

// restoreSavedWorktreeSession brings a missing worktree session back from
// its saved state when kwt has one for it. Opening does not wait for the
// panes to start their programs.
func restoreSavedWorktreeSession(ctx context.Context, req kwttmux.SkeletonRequest) (bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	deps, err := sessionRestoreDeps(cfg)
	if err != nil {
		return false, err
	}
	_, restored, err := tmuxrestore.RestoreWorkspace(ctx, deps, req)
	return restored, err
}

// newSavedWorktreeSessions returns the worktree sessions restore opens a
// saved worktree session through. Tests replace it to keep off kwt's server.
var newSavedWorktreeSessions = func(names []string, restore kwttmux.SkeletonRestorer) openWorkspaceRunner {
	return kwttmux.NewWorkspaceSessions(kwttmux.WorkspaceSessionsOptions{
		StripNames:       names,
		ReportDiagnostic: tmuxDiagnosticReporter(os.Stderr),
		Restore:          restore,
	})
}

// openSavedWorktreeSession opens a saved worktree session the way
// `kwt open --start-session` does, under the same project and worktree
// identity checks, with restore in place of the layout preset. A directory
// workspace must still be registered under the name its session was made for.
func openSavedWorktreeSession(names []string, workspaces []models.Workspace) func(context.Context, tmuxrestore.Entry, kwttmux.SkeletonRestorer) error {
	return func(ctx context.Context, entry tmuxrestore.Entry, restore kwttmux.SkeletonRestorer) error {
		path, generation := entry.Session.Path, entry.Session.WorkspaceGeneration
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return tmuxrestore.ErrWorktreeChanged
		}
		sessions := newSavedWorktreeSessions(names, restore)
		if generation == "" {
			// Only a registered directory workspace has kwt's markers
			// without a checkout generation. It must still be registered
			// under the name its session was made for.
			identity := kwttmux.WorkspacePathIdentity(path)
			if !slices.ContainsFunc(workspaces, func(w models.Workspace) bool {
				return kwttmux.WorkspacePathIdentity(w.Path) == identity &&
					kwttmux.DirWorkspaceSessionName(w.Name, w.Path) == entry.Session.Name
			}) {
				return tmuxrestore.ErrWorktreeChanged
			}
			_, err := sessions.EstablishWithGeneration(ctx, entry.Session.Name, path, "", kwttmux.BlankLayout())
			return err
		}
		_, err := runWorktreeSessionEstablishment(ctx, path, generation, names, func(session string) error {
			if session != entry.Session.Name {
				return tmuxrestore.ErrWorktreeChanged
			}
			_, err := sessions.EstablishWithGeneration(ctx, session, path, generation, kwttmux.BlankLayout())
			return err
		})
		if condition, ok := errors.AsType[*shared.ConditionError](err); ok && condition.Reason == shared.ReasonGenerationChanged {
			return tmuxrestore.ErrWorktreeChanged
		}
		return err
	}
}

func runSessionRestore(cmd *cobra.Command, args []string) error {
	return ExecuteWithContext(false, func(appCtx *CommandContext) error {
		deps, err := sessionRestoreDeps(appCtx.Config)
		if err != nil {
			return err
		}
		// A save first turns sessions from a server that is now gone into
		// restorable entries.
		if _, err := saveSessions(cmd, deps.Store, deps.Executable); err != nil {
			return err
		}
		deps.ClaimWait = sessionClaimWait
		deps.Workspace = openSavedWorktreeSession(credentials.ProtectedNames(appCtx.Config), appCtx.Config.Workspaces)
		reports, restoreErr := tmuxrestore.Restore(commandContext(cmd), deps, args, sessionAll)
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
		switch {
		case report.Outcome == tmuxrestore.OutcomeRestored && report.Reason == tmuxrestore.ReasonAlreadyRestored:
			line = name + ": " + report.Reason
		case report.Reason != "":
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
			status := pane.Status
			if status == tmuxrestore.LaunchNone {
				status = "not started"
			}
			detail := fmt.Sprintf("  window %d pane %d: %s", pane.Window, pane.Pane, strings.TrimSpace(pane.Agent+" "+status))
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
	socket := tmuxrestore.Socket{Path: sessionForget.socket}
	if name := sessionForget.socketName; name != "" {
		// kwt's own server lives under /tmp whatever TMUX_TMPDIR says, as
		// when kwt saves it.
		if name == kwttmux.KWTServerSocketName {
			socket, err = tmuxrestore.SocketIn("", name)
		} else {
			socket, err = tmuxrestore.NamedSocket(name)
		}
		if err != nil {
			return err
		}
	}
	return store.WithLock(commandContext(cmd), socket, func(f *tmuxrestore.Files) error {
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
	return tmuxrestore.Launch(ctx, store, socket, args[0], launch,
		os.Getenv("TMUX_PANE"), tmuxrestore.ServerPIDFromTMUX(tmuxValue))
}
