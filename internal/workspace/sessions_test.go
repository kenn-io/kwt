package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

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
