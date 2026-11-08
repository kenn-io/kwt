package herdrcore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type managedObserver interface {
	ObserveManaged(context.Context) ([]Terminal, error)
}

func syntheticCoreSocket(t *testing.T, reply func(string, json.RawMessage) any) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket fixture")
	}
	root, err := os.MkdirTemp("/tmp", "kwt-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "core.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("synthetic RPC listener did not retire")
		}
	})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		scanner := bufio.NewScanner(connection)
		for scanner.Scan() {
			var request struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				return
			}
			result := reply(request.Method, request.Params)
			response := map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}
			if err := json.NewEncoder(connection).Encode(response); err != nil {
				return
			}
		}
	}()
	return path
}

func TestObserveManagedReadsRPCWithoutHC(t *testing.T) {
	terminal := Terminal{Address: "t91", Directory: "/fixture/repo", Argv: []string{"/fixture/kwt", "herdr", "_run-command"}, State: "live", CreatedMS: 123, Labels: map[string]string{"kwt.managed": "true"}}
	path := syntheticCoreSocket(t, func(method string, params json.RawMessage) any {
		switch method {
		case "hello":
			return map[string]any{"version": "0.1.18", "generation": 1, "framed": false}
		case "terminal.list":
			return map[string]any{"terminals": []Terminal{terminal}}
		case "terminal.get":
			return terminal
		default:
			t.Errorf("read-only snapshot issued %q", method)
			return nil
		}
	})
	t.Setenv("HC_SOCKET", path)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &Client{run: func(context.Context, ...string) ([]byte, error) {
		t.Error("observer invoked hc and could start Core")
		return nil, errors.New("hc forbidden")
	}}
	observer, ok := any(client).(managedObserver)
	if !ok {
		t.Fatal("Core has no non-starting observer")
	}
	terminals, err := observer.ObserveManaged(t.Context())
	if err != nil || len(terminals) != 1 || terminals[0].Address != "t91" || terminals[0].CreatedMS != 123 {
		t.Fatalf("read-only Core snapshot: terminals=%+v err=%v", terminals, err)
	}
}

func TestObserveManagedDoesNotStartAbsentCore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket discovery")
	}
	// The runner's TMPDIR plus the test name can exceed Linux's Unix-socket
	// path limit before absence is observed. Keep only this private RPC fixture short.
	root, err := os.MkdirTemp("/tmp", "kwt-abs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(name, filepath.Join(root, name))
	}
	t.Setenv("HC_SOCKET", "")
	t.Setenv("HC_SESSION", "fixture")
	t.Setenv("HC_TERMINAL_SOCKET", "/not-authoritative.sock")
	client := &Client{run: func(context.Context, ...string) ([]byte, error) {
		t.Error("absence check invoked hc")
		return nil, errors.New("hc forbidden")
	}}
	observer, ok := any(client).(managedObserver)
	if !ok {
		t.Fatal("Core has no non-starting observer")
	}
	terminals, err := observer.ObserveManaged(t.Context())
	if err != nil || len(terminals) != 0 {
		t.Fatalf("missing socket/state should be absent: terminals=%+v err=%v", terminals, err)
	}
}

func TestObserveManagedRejectsMalformedRPC(t *testing.T) {
	path := syntheticCoreSocket(t, func(string, json.RawMessage) any { return "not a terminal inventory" })
	t.Setenv("HC_SOCKET", path)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	observer, ok := any(NewClient()).(managedObserver)
	if !ok {
		t.Fatal("Core has no non-starting observer")
	}
	if _, err := observer.ObserveManaged(t.Context()); err == nil {
		t.Fatal("malformed snapshot treated as absence")
	}
}

func TestObserveManagedPreservesPossibleOrphanState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket discovery")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("HC_SOCKET", "")
	t.Setenv("HC_SESSION", "fixture")
	if err := os.MkdirAll(filepath.Join(root, "state", "herdr-core", "fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	observer, ok := any(NewClient()).(managedObserver)
	if !ok {
		t.Fatal("Core has no non-starting observer")
	}
	if _, err := observer.ObserveManaged(t.Context()); err == nil {
		t.Fatal("missing socket erased authority of possibly surviving holders")
	}
}

func TestObserveManagedExplicitSocketOverridesConfig(t *testing.T) {
	expected := Terminal{Address: "t91", State: "live", CreatedMS: 123, Labels: map[string]string{"kwt.managed": "true"}}
	path := syntheticCoreSocket(t, func(method string, _ json.RawMessage) any {
		switch method {
		case "hello":
			return map[string]any{"version": "0.1.18", "generation": 1, "framed": false}
		case "terminal.list":
			return map[string]any{"terminals": []Terminal{expected}}
		case "terminal.get":
			return expected
		}
		return nil
	})
	config := t.TempDir()
	if err := os.MkdirAll(filepath.Join(config, "herdr-core"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "herdr-core", "config.toml"), []byte("socket = '/must-not-use.sock'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HC_SOCKET", path)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("HC_TERMINAL_SOCKET", "/also-wrong.sock")
	t.Setenv("HC_SESSION", "different")
	observer, ok := any(NewClient()).(managedObserver)
	if !ok {
		t.Fatal("Core has no non-starting observer")
	}
	terminals, err := observer.ObserveManaged(t.Context())
	if err != nil || len(terminals) != 1 || terminals[0].Address != "t91" {
		t.Fatalf("wrong Core selector precedence: terminals=%+v err=%v", terminals, err)
	}
}
