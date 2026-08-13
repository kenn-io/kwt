package tmux

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kwt/internal/utils"
)

var (
	workspaceSessionRe    = regexp.MustCompile(`^kwt-wt-(.+)-([0-9a-f]{8})$`)
	dirWorkspaceSessionRe = regexp.MustCompile(`^kwt-workspace-(dir-.+)-([0-9a-f]{8})$`)
	legacySessionRe       = regexp.MustCompile(`^kwt-([^-]+)-(.+)-(\d{14})$`)
)

type SessionManager struct {
	config  *SessionConfig
	tmuxCmd TmuxInterface
}

func NewSessionManager(config *SessionConfig) *SessionManager {
	if config == nil {
		config = DefaultSessionConfig()
	}

	return &SessionManager{
		config:  config,
		tmuxCmd: NewTmuxCommand(config.TmuxCommand),
	}
}

func (sm *SessionManager) CreateSession(ctx context.Context, opts SessionOptions) (*Session, error) {
	sessionName := fmt.Sprintf("kwt-%s-%s-%s", opts.Context, opts.Identifier, time.Now().Format("20060102150405"))

	// Create session with or without command
	if opts.Command != "" {
		// Create session with command - when command finishes, session will automatically terminate
		if err := sm.tmuxCmd.NewSessionWithCommandContext(ctx, sessionName, opts.WorkingDir, opts.Command); err != nil {
			return nil, fmt.Errorf("failed to create tmux session with command: %w", err)
		}
	} else {
		// Create session without command (traditional behavior)
		if err := sm.tmuxCmd.NewSessionContext(ctx, sessionName, opts.WorkingDir); err != nil {
			return nil, fmt.Errorf("failed to create tmux session: %w", err)
		}
	}

	if err := sm.tmuxCmd.SetOptionContext(ctx, sessionName, "history-limit", sm.config.HistoryLimit); err != nil {
		_ = sm.tmuxCmd.KillSession(sessionName)
		return nil, fmt.Errorf("failed to set history limit: %w", err)
	}

	session := &Session{
		ID:          utils.GenerateID(),
		SessionName: sessionName,
		Context:     opts.Context,
		Identifier:  opts.Identifier,
		WorkingDir:  opts.WorkingDir,
		Command:     opts.Command,
		StartTime:   time.Now(),
		HistorySize: sm.config.HistoryLimit,
		Metadata:    opts.Metadata,
	}

	return session, nil
}

func (sm *SessionManager) ListSessions() ([]*Session, error) {
	tmuxSessions, err := sm.tmuxCmd.ListSessionsDetailed()
	if err != nil {
		return nil, err
	}

	var sessions []*Session
	for _, tmuxSession := range tmuxSessions {
		// Only show kwt-managed sessions
		if !strings.HasPrefix(tmuxSession.Name, "kwt-") {
			continue
		}

		session := sm.parseSessionFromTmux(tmuxSession)
		if session != nil {
			sessions = append(sessions, session)
		}
	}

	return sessions, nil
}

func (sm *SessionManager) parseSessionFromTmux(info *SessionInfo) *Session {
	// Parse the timestamped run format before the broad eight-hex workspace
	// suffix, because the last eight decimal timestamp digits are also hex.
	if matches := legacySessionRe.FindStringSubmatch(info.Name); len(matches) == 4 {
		startTime, err := time.Parse("20060102150405", matches[3])
		if err != nil {
			startTime = time.Now()
		}
		command := info.CurrentCommand
		if command == "bash" || command == "zsh" || command == "sh" {
			command = "Shell session (original command completed)"
		}
		return &Session{
			ID:          utils.GenerateShortID(),
			SessionName: info.Name,
			Context:     matches[1],
			Identifier:  matches[2],
			WorkingDir:  info.WorkingDir,
			Command:     command,
			StartTime:   startTime,
			HistorySize: sm.config.HistoryLimit,
			Metadata:    map[string]string{},
		}
	}

	// Preserve the directory-workspace identifier before parsing the broad
	// standard-worktree format.
	workspaceMatch := dirWorkspaceSessionRe.FindStringSubmatch(info.Name)
	if len(workspaceMatch) != 3 {
		workspaceMatch = workspaceSessionRe.FindStringSubmatch(info.Name)
	}
	if len(workspaceMatch) == 3 {
		return &Session{
			ID:          utils.GenerateShortID(),
			SessionName: info.Name,
			Context:     "workspace",
			Identifier:  workspaceMatch[1],
			WorkingDir:  info.WorkingDir,
			Command:     info.CurrentCommand,
			StartTime:   parseCreated(info.Created),
			HistorySize: sm.config.HistoryLimit,
			Metadata:    map[string]string{},
		}
	}
	return nil
}

// parseCreated converts a tmux #{session_created} unix-seconds string to a
// time; on any parse failure it falls back to the current time.
func parseCreated(created string) time.Time {
	secs, err := strconv.ParseInt(strings.TrimSpace(created), 10, 64)
	if err != nil {
		return time.Now()
	}
	return time.Unix(secs, 0)
}

func (sm *SessionManager) GetSession(id string) (*Session, error) {
	sessions, err := sm.ListSessions()
	if err != nil {
		return nil, err
	}

	for _, session := range sessions {
		if session.ID == id ||
			strings.Contains(session.SessionName, id) ||
			session.Identifier == id ||
			strings.Contains(session.Identifier, id) ||
			strings.Contains(session.Context, id) {
			return session, nil
		}
	}

	return nil, fmt.Errorf("session not found: %s", id)
}

func (sm *SessionManager) KillSession(id string) error {
	session, err := sm.GetSession(id)
	if err != nil {
		return err
	}

	return sm.KillSessionDirect(session)
}

func (sm *SessionManager) KillSessionDirect(session *Session) error {
	if sm.tmuxCmd.HasSession(session.SessionName) {
		if err := sm.tmuxCmd.KillSession(session.SessionName); err != nil {
			return fmt.Errorf("failed to kill tmux session: %w", err)
		}
	}

	return nil
}

func (sm *SessionManager) AttachSession(id string) error {
	session, err := sm.GetSession(id)
	if err != nil {
		return err
	}

	return sm.AttachSessionDirect(session)
}

func (sm *SessionManager) AttachSessionDirect(session *Session) error {
	if !sm.tmuxCmd.HasSession(session.SessionName) {
		return fmt.Errorf("tmux session %s no longer exists", session.SessionName)
	}

	return sm.tmuxCmd.AttachSession(session.SessionName)
}

// HasSession checks if a session exists
func (sm *SessionManager) HasSession(sessionName string) bool {
	return sm.tmuxCmd.HasSession(sessionName)
}
