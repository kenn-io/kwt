package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// TmuxInterface defines the contract for tmux operations
type TmuxInterface interface {
	NewSession(name, workDir string) error
	NewSessionContext(ctx context.Context, name, workDir string) error
	NewSessionWithCommandContext(ctx context.Context, name, workDir, command string) error
	SetOption(sessionName, option string, value any) error
	SetOptionContext(ctx context.Context, sessionName, option string, value any) error
	ListSessions() ([]string, error)
	ListSessionsDetailed() ([]*SessionInfo, error)
	KillSession(sessionName string) error
	AttachSession(sessionName string) error
	HasSession(sessionName string) bool
}

// SessionManagerInterface defines the contract for session management
type SessionManagerInterface interface {
	CreateSession(ctx context.Context, opts SessionOptions) (*Session, error)
	ListSessions() ([]*Session, error)
	GetSession(id string) (*Session, error)
	KillSession(id string) error
	KillSessionDirect(session *Session) error
	AttachSession(id string) error
	AttachSessionDirect(session *Session) error
	HasSession(sessionName string) bool
}

type TmuxCommand struct {
	command         string
	socketName      string
	extraStripNames map[string]bool
	attachProcess   attachProcessFunc
}

type attachProcessFunc func(*exec.Cmd) error

func NewTmuxCommand(command string) *TmuxCommand {
	return NewTmuxCommandWithStripNames(command, nil)
}

// NewTmuxCommandWithStripNames adds caller-owned credential names to the
// environment variables removed from every tmux subprocess.
func NewTmuxCommandWithStripNames(
	command string,
	names []string,
) *TmuxCommand {
	return NewTmuxCommandForSocketWithStripNames(command, "", names)
}

// NewTmuxCommandForSocketWithStripNames targets a named tmux socket for every
// invocation. A named socket gives protected workspaces a server boundary
// separate from the user's ordinary tmux server.
func NewTmuxCommandForSocketWithStripNames(
	command string,
	socketName string,
	names []string,
) *TmuxCommand {
	if command == "" {
		command = "tmux"
	}
	extra := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			extra[strings.ToLower(name)] = true
		}
	}
	return &TmuxCommand{
		command:         command,
		socketName:      strings.TrimSpace(socketName),
		extraStripNames: extra,
		attachProcess:   replaceAttachProcess,
	}
}

func (t *TmuxCommand) NewSession(name, workDir string) error {
	args := []string{"new-session", "-d", "-s", name}
	if workDir != "" {
		args = append(args, "-c", workDir)
	}
	return t.runCommand(args...)
}

func (t *TmuxCommand) NewSessionContext(ctx context.Context, name, workDir string) error {
	args := []string{"new-session", "-d", "-s", name}
	if workDir != "" {
		args = append(args, "-c", workDir)
	}
	return t.RunCommandContext(ctx, args...)
}

func (t *TmuxCommand) NewSessionWithCommandContext(ctx context.Context, name, workDir, command string) error {
	args := []string{"new-session", "-d", "-s", name}
	if workDir != "" {
		args = append(args, "-c", workDir)
	}
	if command != "" {
		args = append(args, command)
	}
	return t.RunCommandContext(ctx, args...)
}

func (t *TmuxCommand) SetOption(sessionName, option string, value any) error {
	args := []string{"set-option", "-t", sessionName, option, fmt.Sprintf("%v", value)}
	return t.runCommand(args...)
}

func (t *TmuxCommand) SetOptionContext(ctx context.Context, sessionName, option string, value any) error {
	args := []string{"set-option", "-t", sessionName, option, fmt.Sprintf("%v", value)}
	return t.RunCommandContext(ctx, args...)
}

func (t *TmuxCommand) ListSessions() ([]string, error) {
	args := []string{"list-sessions", "-F", "#{session_name}"}
	output, err := t.runCommandOutput(args...)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return []string{}, nil
		}
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	var sessions []string
	for _, line := range lines {
		if line != "" {
			sessions = append(sessions, line)
		}
	}
	return sessions, nil
}

func (t *TmuxCommand) ListSessionsDetailed() ([]*SessionInfo, error) {
	format := "#{session_name}:#{session_created}:#{session_activity}:#{session_attached}:#{pane_current_command}:#{pane_current_path}"
	args := []string{"list-sessions", "-F", format}
	output, err := t.runCommandOutput(args...)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return []*SessionInfo{}, nil
		}
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	var sessions []*SessionInfo
	for _, line := range lines {
		if line == "" {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) < 6 {
			continue
		}

		sessionInfo := &SessionInfo{
			Name:           parts[0],
			Created:        parts[1],
			Activity:       parts[2],
			Attached:       parts[3],
			CurrentCommand: parts[4],
			WorkingDir:     parts[5],
		}

		sessions = append(sessions, sessionInfo)
	}
	return sessions, nil
}

type SessionInfo struct {
	Name           string
	Created        string
	Activity       string
	Attached       string
	CurrentCommand string
	WorkingDir     string
}

func (t *TmuxCommand) KillSession(sessionName string) error {
	args := []string{"kill-session", "-t", sessionName}
	return t.runCommand(args...)
}

func (t *TmuxCommand) AttachSession(sessionName string) error {
	return t.runAttachProcess(
		context.Background(),
		t.attachSessionCmd(sessionName),
	)
}

func (t *TmuxCommand) AttachSessionWithoutEnvironment(
	ctx context.Context,
	sessionName string,
) error {
	return t.runAttachProcess(
		ctx,
		t.attachSessionWithoutEnvironmentCmd(ctx, sessionName),
	)
}

func (t *TmuxCommand) runAttachProcess(
	ctx context.Context,
	cmd *exec.Cmd,
) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.attachProcess(cmd)
}

func (t *TmuxCommand) attachSessionWithoutEnvironmentCmd(
	ctx context.Context,
	sessionName string,
) *exec.Cmd {
	cmd := t.newAttachCmd(
		ctx,
		[]string{"attach-session", "-E", "-t", sessionName},
	)
	// This command targets the protected workspace's isolated socket. A
	// parent TMUX value tells tmux it is already inside a client on another
	// server, which makes tmux reject the cross-server attachment as nested.
	cmd.Env = filteredEnviron(cmd.Env, func(name string) bool {
		return name == "TMUX" || name == "TMUX_PANE"
	})
	return cmd
}

// attachSessionCmd builds the attach-session invocation through the
// attach-class exec seam; split out so tests can pin that the attach path
// uses the full-strip sanitizer without needing a tty to run it.
func (t *TmuxCommand) attachSessionCmd(sessionName string) *exec.Cmd {
	return t.newAttachCmd(context.Background(), []string{"attach-session", "-t", sessionName})
}

func (t *TmuxCommand) HasSession(sessionName string) bool {
	args := []string{"has-session", "-t", sessionName}
	err := t.runCommand(args...)
	return err == nil
}

// SwitchClient switches the attached client to the given session (used when
// already inside tmux, where attach-session would nest). It is an
// attach-class command, so it uses the fully stripped attach environment.
func (t *TmuxCommand) SwitchClient(target string) error {
	cmd := t.switchClientCmd(target)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux command failed: %w, stderr: %s", err, stderr.String())
	}
	return nil
}

// switchClientCmd builds the switch-client invocation through the
// attach-class exec seam; split out so tests can pin the sanitizer choice.
func (t *TmuxCommand) switchClientCmd(target string) *exec.Cmd {
	return t.newAttachCmd(context.Background(), []string{"switch-client", "-t", target})
}

func (t *TmuxCommand) runCommand(args ...string) error {
	cmd := t.newCmd(context.Background(), args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("tmux command failed: %w, stderr: %s", err, stderr.String())
	}
	return nil
}

func (t *TmuxCommand) runCommandOutput(args ...string) (string, error) {
	cmd := t.newCmd(context.Background(), args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("tmux command failed: %w, stderr: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

func (t *TmuxCommand) RunCommandContext(ctx context.Context, args ...string) error {
	cmd := t.newCmd(ctx, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("tmux command failed: %w, stderr: %s", err, stderr.String())
	}
	return nil
}

// RunCommandOutputContext runs a tmux command with the given context and
// returns its stdout — used to capture the pane ID printed by
// new-session/split-window with -P -F '#{pane_id}'.
func (t *TmuxCommand) RunCommandOutputContext(ctx context.Context, args ...string) (string, error) {
	cmd := t.newCmd(ctx, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux command failed: %w, stderr: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

// GlobalEnvironment returns the tmux server's global environment table, one
// "NAME=VALUE" (or "-NAME") entry per line. start-server is a no-op for an
// existing server; on a fresh named socket it lets show-environment inspect
// the sanitized server state without interpreting platform-specific
// connection errors. The empty server exits after the command sequence.
func (t *TmuxCommand) GlobalEnvironment() (string, error) {
	return t.RunCommandOutputContext(
		context.Background(),
		"start-server",
		";",
		"show-environment",
		"-g",
	)
}

// SessionEnvironment returns a session's own environment table via
// show-environment -t <session>, one "NAME=VALUE" (or "-NAME") entry per
// line. This is distinct from the server-global table GlobalEnvironment
// reads: a session can hold launcher-state variables (e.g. an editor's
// VSCODE_*) directly in its own table, captured when the session was
// created, without those variables ever having been global. It fails when
// the session does not exist (e.g. kwt's create path, where the session is
// not up yet at the point the bootstrap set is derived); callers treat that
// as "nothing to inspect" and fall back to the other sources.
func (t *TmuxCommand) SessionEnvironment(session string) (string, error) {
	return t.RunCommandOutputContext(context.Background(), "show-environment", "-t", session)
}

// sessionOption reads a session-local user option without falling back to a
// global value. Missing options return the tmux command error to the caller.
func (t *TmuxCommand) sessionOption(session, option string) (string, error) {
	return t.RunCommandOutputContext(
		context.Background(),
		"show-options",
		"-v",
		"-t",
		session,
		option,
	)
}

func (t *TmuxCommand) globalOption(option string) (string, error) {
	return t.RunCommandOutputContext(
		context.Background(),
		"show-options",
		"-gv",
		option,
	)
}

// newCmd is the exec seam for every TmuxCommand method except the
// attach-class commands (see newAttachCmd). It builds the *exec.Cmd for a
// tmux invocation with Env set to a sanitized copy of the process environment
// (SanitizedEnviron(os.Environ())) rather than the default inherited
// environment, so a tmux server this invocation spawns does not capture
// launcher-specific integration state into its GLOBAL environment table —
// which would otherwise leak to every pane of every session on that server,
// not just the one kwt is currently building. SanitizedEnviron keeps
// EDITOR/VISUAL because commands routed here can START a server, and tmux
// reads them at server start for its default key mode. Call sites that
// predate context plumbing pass context.Background().
func (t *TmuxCommand) newCmd(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, t.command, t.socketArgs(args)...)
	cmd.Env = t.stripExtraNames(SanitizedEnviron(os.Environ()))
	return cmd
}

// newAttachCmd is the exec seam for attach-class commands (attach-session,
// switch-client), which connect a client to an existing server and can never
// start one. They use AttachSanitizedEnviron — the full launcher-state strip
// with no EDITOR/VISUAL carve-out — because tmux's update-environment option
// imports listed variables from the attaching client's environment into the
// session table, which would override the bootstrap's remove-markers if the
// client still carried them.
func (t *TmuxCommand) newAttachCmd(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, t.command, t.socketArgs(args)...)
	cmd.Env = t.stripExtraNames(AttachSanitizedEnviron(os.Environ()))
	return cmd
}

func (t *TmuxCommand) socketArgs(args []string) []string {
	if t.socketName == "" {
		return args
	}
	prefixed := make([]string, 0, len(args)+2)
	prefixed = append(prefixed, "-L", t.socketName)
	return append(prefixed, args...)
}

func (t *TmuxCommand) stripExtraNames(env []string) []string {
	if len(t.extraStripNames) == 0 {
		return env
	}
	return filteredEnviron(env, func(name string) bool {
		return t.extraStripNames[strings.ToLower(name)]
	})
}
