package tmux

import (
	"fmt"
	"strings"
)

// KWTServerSocketName is the fixed named socket shared by KWT workspaces and
// long-running KWT sessions.
const KWTServerSocketName = "kwt"

// SessionEndpoint is the durable tmux address for one session.
type SessionEndpoint struct {
	SessionName string
	SocketName  string
}

// WorkspaceSession pairs a durable endpoint with its observed liveness.
type WorkspaceSession struct {
	Endpoint SessionEndpoint
	Live     bool
}

func canonicalEndpoint(session string) SessionEndpoint {
	return SessionEndpoint{
		SessionName: session,
		SocketName:  KWTServerSocketName,
	}
}

// KillCommandHint formats a manual command for the durable endpoint address.
// It does not infer attachment policy from the socket name.
func KillCommandHint(endpoint SessionEndpoint) string {
	if endpoint.SocketName != "" {
		return fmt.Sprintf(
			"env -u TMUX_TMPDIR tmux -L %s kill-session -t %s",
			endpoint.SocketName,
			exactSessionTarget(endpoint.SessionName),
		)
	}
	return fmt.Sprintf(
		"env -u TMUX -u TMUX_PANE tmux kill-session -t %s",
		exactSessionTarget(endpoint.SessionName),
	)
}

// WorkspaceSessionsOptions configures workspace session coordination. The
// explicit directories are dependency seams for daemon request context and
// isolated real-tmux tests.
type WorkspaceSessionsOptions struct {
	Command              string
	StripNames           []string
	KWTServerTempDir     string
	DefaultServerTempDir string
	// DefaultServerTempDirSet makes an empty directory override ambient
	// TMUX_TMPDIR instead of inheriting it.
	DefaultServerTempDirSet bool
	ReportDiagnostic        func(error)
}

type serverCommands struct {
	command                 string
	kwtServerTempDir        string
	defaultServerTempDir    string
	defaultServerTempDirSet bool
	stripNames              []string
}

func newServerCommands(options WorkspaceSessionsOptions) serverCommands {
	defaultServerTempDir := strings.TrimSpace(options.DefaultServerTempDir)
	return serverCommands{
		command:                 options.Command,
		kwtServerTempDir:        strings.TrimSpace(options.KWTServerTempDir),
		defaultServerTempDir:    defaultServerTempDir,
		defaultServerTempDirSet: options.DefaultServerTempDirSet || defaultServerTempDir != "",
		stripNames:              append([]string(nil), options.StripNames...),
	}
}

func (f serverCommands) kwtServer() *TmuxCommand {
	if f.kwtServerTempDir != "" {
		return NewTmuxCommandForSocketInTempDirWithStripNames(
			f.command,
			KWTServerSocketName,
			f.kwtServerTempDir,
			f.stripNames,
		)
	}
	return NewTmuxCommandForSocketWithStripNames(
		f.command,
		KWTServerSocketName,
		f.stripNames,
	)
}

// compat(kag1): default-server adoption
func (f serverCommands) defaultServer() *TmuxCommand {
	var command *TmuxCommand
	if f.defaultServerTempDir != "" {
		command = NewTmuxCommandInTempDirWithStripNames(
			f.command,
			f.defaultServerTempDir,
			f.stripNames,
		)
	} else {
		command = NewTmuxCommandWithStripNames(f.command, f.stripNames)
		command.clearSocketTempDir = f.defaultServerTempDirSet
	}
	command.stripParentTmux = true
	return command
}

func (f serverCommands) commandForEndpoint(
	endpoint SessionEndpoint,
) (*TmuxCommand, error) {
	switch endpoint.SocketName {
	case KWTServerSocketName:
		return f.kwtServer(), nil
	case "":
		return f.defaultServer(), nil
	default:
		return nil, fmt.Errorf(
			"socket %q is not an ordinary KWT tmux server",
			endpoint.SocketName,
		)
	}
}
