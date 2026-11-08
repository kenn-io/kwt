package herdrcore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in because this starts an isolated Core. Never point it at a personal
// session, account configuration or archive. The caller builds the helper first.
func TestCoreLifecycleIntegration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lifecycle fixture uses POSIX shell commands")
	}
	binary := os.Getenv("KWT_HERDR_TEST_BINARY")
	if binary == "" {
		t.Skip("set KWT_HERDR_TEST_BINARY to the built kwt binary for isolated Core integration")
	}
	hc, err := exec.LookPath("hc")
	if err != nil {
		t.Skip("hc not installed")
	}
	fixture := os.Getenv("KWT_HERDR_TEST_ROOT")
	if fixture == "" {
		fixture, err = os.MkdirTemp("/tmp", "kwt-core-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(fixture) })
	} else if err := os.MkdirAll(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	// A separate short, newly created directory prevents inherited routes or a
	// caller-supplied fixture root from directing cleanup at another server.
	socketDir, err := os.MkdirTemp("/tmp", "kwt-hc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("HC_SOCKET", filepath.Join(socketDir, "core.sock"))
	t.Setenv("HC_TERMINAL_SOCKET", "")
	session := os.Getenv("KWT_HERDR_TEST_SESSION")
	if session == "" {
		session = "kwt-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	t.Setenv("HC_SESSION", session)
	t.Setenv("KWT_HERDR_FIXTURE_TOKEN", "synthetic-token")
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "KWT_HOME"} {
		path := filepath.Join(fixture, key)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "HC_TERMINAL_ID", "KWT_CONFIG", "BASH_ENV", "ENV", "ZDOTDIR"} {
		t.Setenv(key, "")
	}
	// The hidden helper must not read even a malformed kwt config before the shell.
	if err := os.WriteFile(filepath.Join(os.Getenv("KWT_HOME"), "config.toml"), []byte("invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewClient()
	t.Cleanup(func() {
		// The fixture Core belongs to this test, and cleanup must outlive cancellation.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if receipt := os.Getenv("KWT_HERDR_TEST_CLEANUP_RECEIPT"); receipt != "" {
			if err := os.WriteFile(receipt, []byte(`{"stop_attempted":true}`), 0600); err != nil {
				t.Error(err)
			}
		}
		output, err := exec.CommandContext(ctx, hc, "server", "stop").CombinedOutput()
		if receipt := os.Getenv("KWT_HERDR_TEST_CLEANUP_RECEIPT"); receipt != "" {
			data, _ := json.Marshal(map[string]any{"stop_attempted": true, "stop_ok": err == nil})
			if writeErr := os.WriteFile(receipt, data, 0600); writeErr != nil {
				t.Error(writeErr)
			}
		}
		if err != nil {
			t.Errorf("stop isolated Core: %v: %s", err, output)
		}
	})
	completed, err := c.Run(t.Context(), RunOptions{Executable: binary, Directory: fixture, Identifier: "completed", Context: "test", Command: `test -z "${KWT_HERDR_FIXTURE_TOKEN+x}" || exit 42; echo core-fixture-ready; exit 23`, ProtectedNames: []string{"KWT_HERDR_FIXTURE_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	if receipt := os.Getenv("KWT_HERDR_TEST_PROCESS_RECEIPT"); receipt != "" {
		status, err := c.run(t.Context(), "server", "status", "--json")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receipt, status, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.run(t.Context(), "wait", "--json", "--timeout", "15", "--", completed.Address); err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 23 {
			t.Fatal(err)
		}
	}
	finished, err := c.List(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(finished) != 1 || finished[0].State != "finished" || finished[0].ExitCode == nil || *finished[0].ExitCode != 23 {
		t.Fatalf("finished command record: %+v", finished)
	}
	output, err := c.run(t.Context(), "read", "--lines", "0", "--", completed.Address)
	if err != nil || !strings.Contains(string(output), "core-fixture-ready") {
		t.Fatalf("finished output %q: %v", output, err)
	}
	if err := c.Kill(t.Context(), completed.Address); err != nil {
		t.Fatal(err)
	}
	terminal, err := c.Run(t.Context(), RunOptions{Executable: binary, Directory: fixture, Identifier: "lifecycle", Context: "test", Command: "sleep 60", ProtectedNames: []string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_ = c.Kill(ctx, terminal.Address)
	})
	sessions, err := c.List(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Address != terminal.Address || sessions[0].Directory != fixture || sessions[0].Identifier() != "lifecycle" {
		t.Fatalf("unexpected terminals: %+v", sessions)
	}
	if err := c.Kill(t.Context(), terminal.Address); err != nil {
		t.Fatal(err)
	}
	sessions, err = c.List(t.Context(), true)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("cleanup list %+v: %v", sessions, err)
	}
	automatic, err := c.Run(t.Context(), RunOptions{Executable: binary, Directory: fixture, Identifier: "automatic", Context: "test", Command: "echo complete", AutoCleanup: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.run(t.Context(), "wait", "--json", "--timeout", "15", "--", automatic.Address)
	var failure *commandError
	if err != nil && !(errors.As(err, &failure) && failure.kind == "not_found") {
		t.Fatal(err)
	}
	sessions, err = c.List(t.Context(), true)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("automatic cleanup list %+v: %v", sessions, err)
	}
}
