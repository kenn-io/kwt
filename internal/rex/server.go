package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	// ErrNotInstalled reports that no rex command was found.
	ErrNotInstalled = errors.New("rex is not installed")
	// ErrNotRunning reports that rex is installed but its server is not running.
	ErrNotRunning = errors.New("the Rex server is not running")
)

// Server is a running Rex server reached through its Unix socket.
type Server struct {
	Socket      string `json:"socket"`
	Version     string `json:"version,omitempty"`
	APIRevision int    `json:"api_revision,omitempty"`
}

// Executables lists where the rex command may be, in the order to try.
// Rex for Mac keeps its command inside the server application it installs.
func Executables() []string {
	var found []string
	if path, err := exec.LookPath("rex"); err == nil {
		found = append(found, path)
	}
	if home, err := os.UserHomeDir(); err == nil {
		found = append(found, filepath.Join(home,
			"Library/Application Support/com.superlogical.rex/RexServer/Versions/Current/Rex Server.app/Contents/MacOS/rex"))
	}
	return found
}

// FindServer locates the account's running Rex server. REX_SERVER, a
// unix:// endpoint, takes precedence. Otherwise the first rex command found
// in executables reports its server. A missing server is never started.
func FindServer(ctx context.Context, executables []string) (Server, error) {
	if endpoint := os.Getenv("REX_SERVER"); endpoint != "" {
		socket, err := unixSocket(endpoint)
		return Server{Socket: socket}, err
	}
	for _, rex := range executables {
		info, err := os.Stat(rex)
		if err != nil || info.IsDir() {
			continue
		}
		cmd := exec.CommandContext(ctx, rex, "whoami", "--json", "--autostart=false")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return Server{}, ctx.Err()
			}
			return Server{}, fmt.Errorf("%w: %s", ErrNotRunning, strings.TrimSpace(stderr.String()))
		}
		var who struct {
			Endpoint    string `json:"endpoint"`
			Version     string `json:"rex_version"`
			APIRevision int    `json:"api_revision"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &who); err != nil {
			return Server{}, fmt.Errorf("rex whoami printed unreadable JSON: %w", err)
		}
		socket, err := unixSocket(who.Endpoint)
		if err != nil {
			return Server{}, err
		}
		return Server{Socket: socket, Version: who.Version, APIRevision: who.APIRevision}, nil
	}
	return Server{}, ErrNotInstalled
}

func unixSocket(endpoint string) (string, error) {
	rest, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok {
		return "", fmt.Errorf("only local Rex servers (unix://) are supported, not %q", endpoint)
	}
	path, err := url.PathUnescape(rest)
	if err != nil {
		return "", fmt.Errorf("unreadable Rex endpoint %q: %w", endpoint, err)
	}
	return path, nil
}
