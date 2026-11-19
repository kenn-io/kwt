package tmuxrestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	kwttmux "go.kenn.io/kwt/internal/tmux"
)

// Socket names one tmux server by the path of its socket. Name is the -L name
// when the socket lives in tmux's standard directory.
type Socket struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

// Key names the socket's files in the snapshot directory.
func (s Socket) Key() string {
	sum := sha256.Sum256([]byte(s.Path))
	return hex.EncodeToString(sum[:16])
}

func uidString() string { return strconv.Itoa(os.Getuid()) }

// DefaultSocketDir returns the directory tmux uses for -L sockets:
// $TMUX_TMPDIR or /tmp, then tmux-<uid>, with symlinks resolved as tmux does.
func DefaultSocketDir() (string, error) {
	base := os.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve tmux socket directory: %w", err)
	}
	return filepath.Join(resolved, "tmux-"+uidString()), nil
}

// NamedSocket returns the socket tmux -L name would use.
func NamedSocket(name string) (Socket, error) {
	dir, err := DefaultSocketDir()
	if err != nil {
		return Socket{}, err
	}
	return Socket{Path: filepath.Join(dir, name), Name: name}, nil
}

// SocketFromTMUX parses the socket path from a $TMUX value.
func SocketFromTMUX(value string) (Socket, bool) {
	path, _, _ := strings.Cut(value, ",")
	if path == "" {
		return Socket{}, false
	}
	socket := Socket{Path: path}
	if dir, err := DefaultSocketDir(); err == nil && filepath.Dir(path) == dir {
		socket.Name = filepath.Base(path)
	}
	return socket, true
}

// ServerPIDFromTMUX returns the server PID from a $TMUX value.
func ServerPIDFromTMUX(value string) string {
	fields := strings.Split(value, ",")
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// Tmux runs tmux against one socket.
type Tmux struct {
	Executable string
	Socket     Socket
}

// CommandError carries tmux's exit status and diagnostic.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("tmux %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *CommandError) Unwrap() error { return e.Err }

// Run runs one tmux command and returns its standard output.
func (t Tmux) Run(ctx context.Context, args ...string) (string, error) {
	executable := t.Executable
	if executable == "" {
		executable = "tmux"
	}
	full := append([]string{"-S", t.Socket.Path}, args...)
	cmd := exec.CommandContext(ctx, executable, full...)
	cmd.Env = kwttmux.SanitizedEnviron(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &CommandError{
			Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err,
		}
	}
	return stdout.String(), nil
}

// ServerState is what a probe learned about a socket's server.
type ServerState int

const (
	// ServerUnknown means the probe failed; nothing may be concluded.
	ServerUnknown ServerState = iota
	ServerRunning
	// ServerAbsent means tmux confirmed no server is listening.
	ServerAbsent
)

// ServerID identifies one tmux server instance.
type ServerID struct {
	PID       string `json:"pid"`
	StartTime string `json:"start_time"`
}

// Probe reports whether a server is running on the socket.
func (t Tmux) Probe(ctx context.Context) (ServerState, ServerID, error) {
	if _, err := os.Stat(t.Socket.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ServerAbsent, ServerID{}, nil
		}
		return ServerUnknown, ServerID{}, fmt.Errorf("inspect tmux socket: %w", err)
	}
	output, err := t.Run(ctx, "list-sessions", "-F", "#{pid} #{start_time}")
	if err != nil {
		if commandErr, ok := errors.AsType[*CommandError](err); ok && ctx.Err() == nil {
			if exitErr, ok := errors.AsType[*exec.ExitError](commandErr.Err); ok &&
				exitErr.ExitCode() == 1 &&
				kwttmux.IsExplicitlyAbsentDiagnostic(commandErr.Stderr) {
				return ServerAbsent, ServerID{}, nil
			}
		}
		return ServerUnknown, ServerID{}, fmt.Errorf("probe tmux server: %w", err)
	}
	line, _, _ := strings.Cut(output, "\n")
	pid, start, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok {
		// A server without sessions has nothing to save; treat it like one
		// that has exited.
		return ServerAbsent, ServerID{}, nil
	}
	return ServerRunning, ServerID{PID: pid, StartTime: start}, nil
}
