package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrNotInstalled reports that no rex command was found.
	ErrNotInstalled = errors.New("rex is not installed")
	// ErrNotRunning reports that rex is installed but its server is not running.
	ErrNotRunning = errors.New("the Rex server is not running")
)

// Server is a running Rex server.
type Server struct {
	// Endpoint is where the server answers: unix://PATH on this machine, or
	// http:// or https:// with a host, such as a server's tailnet address.
	Endpoint    string `json:"endpoint"`
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

// FindServer locates the account's running Rex server. REX_SERVER, an
// endpoint as Server describes it, takes precedence. Otherwise the first rex command found
// in executables reports its server. A missing server is never started.
func FindServer(ctx context.Context, executables []string) (Server, error) {
	if endpoint := os.Getenv("REX_SERVER"); endpoint != "" {
		if err := checkEndpoint(endpoint); err != nil {
			return Server{}, err
		}
		return Server{Endpoint: endpoint}, nil
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
		if err := checkEndpoint(who.Endpoint); err != nil {
			return Server{}, err
		}
		return Server{Endpoint: who.Endpoint, Version: who.Version, APIRevision: who.APIRevision}, nil
	}
	return Server{}, ErrNotInstalled
}

func checkEndpoint(endpoint string) error {
	_, _, err := webSocketTarget(endpoint, "")
	return err
}

// webSocketTarget is the WebSocket URL for path on the server at endpoint
// and the HTTP client that reaches it.
func webSocketTarget(endpoint, path string) (string, *http.Client, error) {
	unsupported := fmt.Errorf("the Rex server endpoint %q is not unix://PATH, http://HOST[:PORT], or https://HOST[:PORT]", endpoint)
	if rest, ok := strings.CutPrefix(endpoint, "unix://"); ok {
		socket, err := url.PathUnescape(rest)
		if err != nil || socket == "" {
			return "", nil, unsupported
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}
		return "ws://rex" + path, &http.Client{Transport: transport}, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", nil, unsupported
	}
	base := strings.TrimSuffix(parsed.Host+parsed.Path, "/")
	switch parsed.Scheme {
	case "http":
		return "ws://" + base + path, networkClient, nil
	case "https":
		return "wss://" + base + path, networkClient, nil
	default:
		return "", nil, unsupported
	}
}

// networkClient gives up on a server that does not answer within the time
// the rex command allows each connection.
var networkClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // Always an *http.Transport.
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	return &http.Client{Transport: transport}
}()
