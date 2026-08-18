package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/table"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/ui"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

var (
	tmuxListJSON  bool
	tmuxListCSV   bool
	tmuxListWatch bool
)

var tmuxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active tmux sessions",
	Long: `List active tmux sessions with their information.

Shows running tmux sessions with context, identifier, duration and working directory.
Supports various output formats and real-time monitoring.`,
	Example: `  # List all sessions
  kwt tmux list

  # JSON output for scripting  
  kwt tmux list --json

  # Real-time monitoring
  kwt tmux list --watch`,
	RunE: runTmuxList,
}

func init() {
	tmuxCmd.AddCommand(tmuxListCmd)

	tmuxListCmd.Flags().BoolVar(&tmuxListJSON, "json", false, "Output as JSON")
	tmuxListCmd.Flags().BoolVar(&tmuxListCSV, "csv", false, "Output as CSV")
	tmuxListCmd.Flags().BoolVarP(&tmuxListWatch, "watch", "w", false, "Real-time monitoring")
}

func runTmuxList(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	sessionManager := tmux.NewSessionManager(nil)

	if tmuxListWatch {
		return runTmuxListWatch(sessionManager, cfg)
	}

	return runTmuxListOnce(sessionManager, cfg)
}

func runTmuxListOnce(sessionManager *tmux.SessionManager, cfg *models.Config) error {
	sessions, err := sessionManager.ListSessions()
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	switch {
	case tmuxListJSON:
		return outputSessionsJSON(sessions)
	case tmuxListCSV:
		return outputSessionsCSV(sessions)
	default:
		printer := ui.New(&cfg.UI)
		return outputSessionsTable(sessions, printer)
	}
}

func runTmuxListWatch(sessionManager *tmux.SessionManager, cfg *models.Config) error {
	printer := ui.New(&cfg.UI)

	hideCursor := "\033[?25l"
	showCursor := "\033[?25h"
	clearScreen := "\033[H\033[2J"

	fmt.Print(hideCursor)
	defer fmt.Print(showCursor)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	refresh := func() error {
		fmt.Print(clearScreen)

		sessions, err := sessionManager.ListSessions()
		if err != nil {
			return fmt.Errorf("failed to list sessions: %w", err)
		}

		fmt.Printf("tmux Sessions - Updated: %s\n", time.Now().Format("15:04:05"))
		fmt.Printf("Total: %d sessions\n\n", len(sessions))

		if err := outputSessionsTable(sessions, printer); err != nil {
			return err
		}

		fmt.Println("\n[Press Ctrl+C to exit]")
		return nil
	}

	if err := refresh(); err != nil {
		return err
	}

	for range ticker.C {
		if err := refresh(); err != nil {
			fmt.Printf("Error: %v\n", err)
		}
	}

	return nil
}

func outputSessionsJSON(sessions []*tmux.Session) error {
	records := make([]tmuxSessionRecord, 0, len(sessions))
	for _, session := range sessions {
		records = append(records, tmuxSessionRecordFromSession(session))
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(records)
}

type tmuxSessionRecord struct {
	ID             string                `json:"id"`
	SessionName    string                `json:"session_name"`
	Context        string                `json:"context"`
	Identifier     string                `json:"identifier"`
	WorkingDir     string                `json:"working_dir"`
	Command        string                `json:"command"`
	StartTime      time.Time             `json:"start_time"`
	HistorySize    int                   `json:"history_size"`
	Metadata       map[string]string     `json:"metadata,omitempty"`
	TmuxSocketName string                `json:"tmux_socket_name,omitempty"`
	TmuxAttachMode models.TmuxAttachMode `json:"tmux_attach_mode"`
}

func tmuxSessionRecordFromSession(session *tmux.Session) tmuxSessionRecord {
	return tmuxSessionRecord{
		ID:             session.ID,
		SessionName:    session.SessionName,
		Context:        session.Context,
		Identifier:     session.Identifier,
		WorkingDir:     session.WorkingDir,
		Command:        session.Command,
		StartTime:      session.StartTime,
		HistorySize:    session.HistorySize,
		Metadata:       session.Metadata,
		TmuxSocketName: session.SocketName,
		TmuxAttachMode: models.TmuxAttachDirect,
	}
}

func outputSessionsCSV(sessions []*tmux.Session) error {
	t := table.New().Headers("context", "identifier", "endpoint", "duration", "command", "working_dir", "session_name")

	// Write data
	for _, session := range sessions {
		duration := time.Since(session.StartTime).Round(time.Second).String()
		t.Row(
			session.Context,
			session.Identifier,
			tmux.SessionEndpointLabel(session),
			duration,
			session.Command,
			session.WorkingDir,
			session.SessionName,
		)
	}

	return t.WriteCSV()
}

func outputSessionsTable(sessions []*tmux.Session, printer *ui.Printer) error {
	if len(sessions) == 0 {
		printer.PrintInfo("No tmux sessions found")
		return nil
	}

	t := table.New().Headers("SESSION", "DURATION", "WORKING_DIR")

	for _, session := range sessions {
		sessionIdentifier := formatTmuxSessionLabel(session)
		duration := formatSessionDuration(session.StartTime)
		workdir := formatWorkingDir(session.WorkingDir, printer)

		t.Row(sessionIdentifier, duration, workdir)
	}

	return t.Println()
}

func formatTmuxSessionLabel(session *tmux.Session) string {
	return fmt.Sprintf(
		"%s/%s [%s]",
		session.Context,
		session.Identifier,
		tmux.SessionEndpointLabel(session),
	)
}

func formatSessionDuration(startTime time.Time) string {
	duration := time.Since(startTime)
	switch {
	case duration < time.Minute:
		return "just now"
	case duration < time.Hour:
		mins := int(duration.Minutes())
		if mins == 1 {
			return "1 min"
		}
		return fmt.Sprintf("%d mins", mins)
	case duration < 24*time.Hour:
		hours := int(duration.Hours())
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	case duration < 7*24*time.Hour:
		days := int(duration.Hours() / 24)
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	default:
		weeks := int(duration.Hours() / 24 / 7)
		if weeks == 1 {
			return "1 week"
		}
		return fmt.Sprintf("%d weeks", weeks)
	}
}

func formatWorkingDir(workdir string, printer *ui.Printer) string {
	// Apply tilde home replacement first if enabled
	if printer != nil && printer.UseTildeHome() {
		workdir = utils.TildePath(workdir)
	}

	// Then apply truncation if needed
	if len(workdir) > 30 {
		return "..." + workdir[len(workdir)-27:]
	}
	return workdir
}
