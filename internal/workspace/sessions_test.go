package workspace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

func TestCoreBatchUsesOneInventory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket fixture")
	}
	dir, err := os.MkdirTemp("/tmp", "kwt-batch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "core.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		count := 0
		defer func() { done <- count }()
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		scanner := bufio.NewScanner(connection)
		for scanner.Scan() {
			var request struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				return
			}
			var result any = map[string]any{"generation": 1, "framed": false}
			if request.Method == "terminal.list" {
				count++
				result = map[string]any{"terminals": []any{}}
			}
			if json.NewEncoder(connection).Encode(map[string]any{"id": request.ID, "result": result}) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	sessions := New(&models.Config{Workspace: models.WorkspaceConfig{Backend: "herdr-core", CoreSocket: socket}}, tmux.WorkspaceSessionsOptions{})
	requests := []tmux.WorkspaceEndpointRequest{{SessionName: "one", WorkspacePath: dir}, {SessionName: "two", WorkspacePath: filepath.Join(dir, "two")}}
	rows, err := sessions.ResolveAllBestEffort(t.Context(), requests)
	if err != nil || len(rows) != 2 || rows[0].Err != nil || rows[1].Err != nil {
		t.Fatalf("batch: %+v, %v", rows, err)
	}
	if count := <-done; count != 1 {
		t.Fatalf("inventory calls = %d, want 1", count)
	}
}

func TestCoreBatchCancellationIsNotPublishedAsRowErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sessions := New(&models.Config{Workspace: models.WorkspaceConfig{Backend: "herdr-core", CoreSocket: "/fixture/missing.sock"}}, tmux.WorkspaceSessionsOptions{})
	_, err := sessions.ResolveAllBestEffort(ctx, []tmux.WorkspaceEndpointRequest{{SessionName: "fixture", WorkspacePath: "/fixture"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled batch returned %v", err)
	}
}

func TestCoreExpectedSessionMismatchDoesNotLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	script := "#!/bin/sh\nif [ \"$1\" = ls ]; then printf '%s\\n' '{\"terminals\":[]}'; else : > \"$KWT_FAKE_MARKER\"; exit 1; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "hc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("KWT_FAKE_MARKER", marker)
	sessions := New(&models.Config{Workspace: models.WorkspaceConfig{Backend: "herdr-core", CoreSocket: filepath.Join(dir, "core.sock")}}, tmux.WorkspaceSessionsOptions{})
	_, err := sessions.EstablishExpectedWithGeneration(t.Context(), "new-branch", "old-branch", dir, "0123456789abcdef0123456789abcdef", models.Layout{Panes: []string{"echo should-not-run"}})
	if !errors.Is(err, tmux.ErrWorkspaceSessionChanged) {
		t.Fatalf("expected session change, got %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unexpected launch: %v", err)
	}
}

func TestCoreResolveLiveReturnsNotLiveForEmptyInventory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket fixture")
	}
	dir, err := os.MkdirTemp("/tmp", "kwt-empty-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "core.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		scanner := bufio.NewScanner(connection)
		for scanner.Scan() {
			var request struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				return
			}
			var result any = map[string]any{"generation": 1, "framed": false}
			if request.Method == "terminal.list" {
				result = map[string]any{"terminals": []any{}}
			}
			if json.NewEncoder(connection).Encode(map[string]any{"id": request.ID, "result": result}) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	sessions := New(&models.Config{Workspace: models.WorkspaceConfig{Backend: "herdr-core", CoreSocket: socket}}, tmux.WorkspaceSessionsOptions{})
	_, err = sessions.ResolveLive(t.Context(), tmux.WorkspaceEndpointRequest{SessionName: "fixture", WorkspacePath: dir})
	if !errors.Is(err, ErrWorkspaceNotLive) {
		t.Fatalf("expected no live workspace, got %v", err)
	}
}

// Inventory must never invoke hc: every read-only hc command can start Core.
func TestCoreInventoryDoesNotStartMissingConfiguredCore(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "hc-started")
	script := "#!/bin/sh\n: > '" + marker + "'\nprintf '%s\\n' '{\"terminals\":[]}'\n"
	if err := os.WriteFile(filepath.Join(root, "hc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	cfg := &models.Config{Workspace: models.WorkspaceConfig{Backend: "herdr-core", CoreSocket: filepath.Join(root, "missing.sock")}}
	sessions := New(cfg, tmux.WorkspaceSessionsOptions{})
	result, err := sessions.ResolveAllBestEffort(t.Context(), []tmux.WorkspaceEndpointRequest{{SessionName: "fixture", WorkspacePath: root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Err == nil || !strings.Contains(result[0].Err.Error(), "Core") {
		t.Fatalf("missing configured Core treated as absent: %+v", result)
	}
	if result[0].Session.Endpoint.SessionName != "fixture" || result[0].Session.Endpoint.Runtime != "herdr-core" {
		t.Fatalf("missing endpoint identity: %+v", result[0].Session.Endpoint)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("inventory invoked hc: %v", err)
	}
}
