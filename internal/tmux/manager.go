package tmux

import (
	"context"
	"fmt"
	"os"
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
	config        *SessionConfig
	kwtServer     sessionManagerCommand
	defaultServer sessionManagerCommand
	// stripNames also drive session removal markers; filtering the tmux
	// client alone cannot mask values retained by an existing server.
	stripNames      []string
	attachEndpoint  func(context.Context, SessionEndpoint, workspaceAttachCommand, string) error
	tmuxEnvironment func() string
}

type sessionManagerCommand interface {
	sessionEnvironmentReader
	RunCommandContext(ctx context.Context, args ...string) error
	RunCommandOutputContext(ctx context.Context, args ...string) (string, error)
	SetOptionContext(context.Context, string, string, any) error
	ListSessionsDetailed() ([]*SessionInfo, error)
	KillSession(string) error
	HasSession(string) bool
	AttachSession(string) error
	SwitchClient(string) error
	AttachSessionNested(context.Context, string) error
	ServerPIDContext(context.Context) (string, error)
}

func NewSessionManager(
	config *SessionConfig,
	stripNames ...string,
) *SessionManager {
	if config == nil {
		config = DefaultSessionConfig()
	}

	servers := newServerCommands(WorkspaceSessionsOptions{
		Command: config.TmuxCommand, StripNames: stripNames,
	})
	return newSessionManagerWithCommands(
		config,
		servers.kwtServer(),
		servers.defaultServer(),
		stripNames...,
	)
}

// NewSessionManagerWithTempDir constructs a manager whose KWT and default
// tmux servers are rooted in tempDir. It is intended for isolated integration
// tests that exercise both server endpoints.
func NewSessionManagerWithTempDir(
	config *SessionConfig,
	tempDir string,
	stripNames ...string,
) *SessionManager {
	if config == nil {
		config = DefaultSessionConfig()
	}
	servers := newServerCommands(WorkspaceSessionsOptions{
		Command:              config.TmuxCommand,
		KWTServerTempDir:     tempDir,
		DefaultServerTempDir: tempDir,
		StripNames:           stripNames,
	})
	return newSessionManagerWithCommands(
		config,
		servers.kwtServer(),
		servers.defaultServer(),
		stripNames...,
	)
}

func newSessionManagerWithCommands(
	config *SessionConfig,
	kwtServer sessionManagerCommand,
	defaultServer sessionManagerCommand,
	stripNames ...string,
) *SessionManager {
	return &SessionManager{
		config: config, kwtServer: kwtServer, defaultServer: defaultServer,
		stripNames:      cleanNames(stripNames),
		attachEndpoint:  attachWorkspaceEndpoint,
		tmuxEnvironment: func() string { return os.Getenv("TMUX") },
	}
}

func (sm *SessionManager) CreateSession(ctx context.Context, opts SessionOptions) (*Session, error) {
	sessionName := sanitizeTmuxName(fmt.Sprintf(
		"kwt-%s-%s-%s",
		opts.Context,
		opts.Identifier,
		time.Now().Format("20060102150405"),
	))
	if isKWTSessionName(sessionName) {
		return nil, fmt.Errorf(
			"tmux context %q is reserved for workspace sessions",
			opts.Context,
		)
	}

	stripNames, err := deriveSessionStripNames(
		ctx, sm.kwtServer, sessionName, false, sm.stripNames,
	)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect tmux bootstrap environment: %w", err)
	}

	createCmd := BuildSessionCreateCommand(sessionName, opts.WorkingDir)
	firstPaneOut, err := sm.kwtServer.RunCommandOutputContext(ctx, createCmd...)
	if err != nil {
		return nil, fmt.Errorf("failed to create tmux session: %w", wrapTmuxErr(createCmd, err))
	}
	firstPane := strings.TrimSpace(firstPaneOut)
	if firstPane == "" {
		return nil, sm.abortCreate(
			sessionName,
			createCmd,
			fmt.Errorf("tmux returned an empty first-pane ID"),
		)
	}

	bootstrapCmd := BuildSessionBootstrapCommand(sessionName, stripNames)
	if err := sm.kwtServer.RunCommandContext(ctx, bootstrapCmd...); err != nil {
		return nil, sm.abortCreate(sessionName, bootstrapCmd, err)
	}
	if err := sm.kwtServer.SetOptionContext(
		ctx, sessionName, "history-limit", sm.config.HistoryLimit,
	); err != nil {
		return nil, sm.abortCreate(
			sessionName,
			[]string{"set-option", "-t", sessionName, "history-limit"},
			err,
		)
	}

	respawnCmd, err := sm.sessionRespawnCommand(
		ctx, sessionName, firstPane, opts.WorkingDir, opts.Command,
	)
	if err != nil {
		return nil, sm.abortCreate(sessionName, nil, err)
	}
	if err := sm.kwtServer.RunCommandContext(ctx, respawnCmd...); err != nil {
		return nil, sm.abortCreate(sessionName, respawnCmd, err)
	}

	session := &Session{
		SessionName: sessionName,
		SocketName:  KWTServerSocketName,
		ID:          utils.GenerateID(),
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

func (sm *SessionManager) sessionRespawnCommand(
	ctx context.Context,
	sessionName string,
	firstPane string,
	workingDir string,
	command string,
) ([]string, error) {
	if command != "" {
		return buildFirstPaneCommandRespawnCommand(
			firstPane, workingDir, command,
		), nil
	}

	defaultShellCmd := []string{
		"show-options", "-v", "-t", sessionName, "default-shell",
	}
	defaultShellOut, err := sm.kwtServer.RunCommandOutputContext(
		ctx, defaultShellCmd...,
	)
	if err != nil {
		return nil, wrapTmuxErr(defaultShellCmd, err)
	}
	defaultShell := strings.TrimSpace(defaultShellOut)
	if defaultShell == "" {
		defaultShellCmd = []string{"show-options", "-gv", "default-shell"}
		defaultShellOut, err = sm.kwtServer.RunCommandOutputContext(
			ctx, defaultShellCmd...,
		)
		if err != nil {
			return nil, wrapTmuxErr(defaultShellCmd, err)
		}
		defaultShell = strings.TrimSpace(defaultShellOut)
	}
	if defaultShell == "" {
		return nil, fmt.Errorf("tmux returned an empty default-shell")
	}
	return BuildFirstPaneRespawnCommand(
		firstPane, workingDir, defaultShell,
	), nil
}

func (sm *SessionManager) abortCreate(
	sessionName string,
	args []string,
	err error,
) error {
	if len(args) > 0 {
		err = wrapTmuxErr(args, err)
	}
	if killErr := sm.kwtServer.KillSession(sessionName); killErr != nil {
		return fmt.Errorf(
			"%w (also failed to kill partial session: %v)", err, killErr,
		)
	}
	return err
}

func (sm *SessionManager) ListSessions() ([]*Session, error) {
	kwtServer, err := sm.listEndpoint(sm.kwtServer, KWTServerSocketName)
	if err != nil {
		return nil, err
	}
	// compat(kag1): default-server adoption
	defaultServer, err := sm.listEndpoint(sm.defaultServer, "")
	if err != nil {
		return nil, fmt.Errorf("list default-server tmux sessions: %w", err)
	}
	return append(kwtServer, defaultServer...), nil
}

func (sm *SessionManager) listEndpoint(
	command sessionManagerCommand,
	socketName string,
) ([]*Session, error) {
	tmuxSessions, err := command.ListSessionsDetailed()
	if err != nil {
		return nil, err
	}
	var sessions []*Session
	for _, tmuxSession := range tmuxSessions {
		// Only show kwt-managed sessions
		if !strings.HasPrefix(tmuxSession.Name, "kwt-") {
			continue
		}

		session := sm.parseSessionFromEndpoint(tmuxSession, socketName)
		if session != nil {
			sessions = append(sessions, session)
		}
	}

	return sessions, nil
}

func (sm *SessionManager) parseSessionFromTmux(info *SessionInfo) *Session {
	return sm.parseSessionFromEndpoint(info, KWTServerSocketName)
}

func (sm *SessionManager) parseSessionFromEndpoint(
	info *SessionInfo,
	socketName string,
) *Session {
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
			SessionName: info.Name,
			SocketName:  socketName,
			ID:          utils.GenerateShortID(),
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
			SessionName: info.Name,
			SocketName:  socketName,
			ID:          utils.GenerateShortID(),
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
	command, err := sm.commandForSession(session)
	if err != nil {
		return err
	}
	if command.HasSession(session.SessionName) {
		if err := command.KillSession(session.SessionName); err != nil {
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
	command, err := sm.commandForSession(session)
	if err != nil {
		return err
	}
	if !command.HasSession(session.SessionName) {
		return fmt.Errorf("tmux session %s no longer exists", session.SessionName)
	}
	return sm.attachEndpoint(
		context.Background(),
		sessionEndpoint(session),
		command,
		sm.tmuxEnvironment(),
	)
}

// HasSession checks if a session exists
func (sm *SessionManager) HasSession(sessionName string) bool {
	// compat(kag1): default-server adoption
	return sm.kwtServer.HasSession(sessionName) || sm.defaultServer.HasSession(sessionName)
}

func (sm *SessionManager) commandForSession(
	session *Session,
) (sessionManagerCommand, error) {
	if session == nil {
		return nil, fmt.Errorf("tmux session is required")
	}
	switch session.SocketName {
	case KWTServerSocketName:
		return sm.kwtServer, nil
	case "":
		// compat(kag1): default-server adoption
		return sm.defaultServer, nil
	default:
		return nil, fmt.Errorf("socket %q is not a KWT workspace endpoint", session.SocketName)
	}
}

func sessionEndpoint(session *Session) SessionEndpoint {
	return SessionEndpoint{
		SessionName: session.SessionName,
		SocketName:  session.SocketName,
	}
}
