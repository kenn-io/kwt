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
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("inventory invoked hc: %v", err)
	}
}
