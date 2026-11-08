package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// A misspelled machine backend must fail before any workspace is created.
func TestWorkspaceBackendRejectsUnknown(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("workspace.backend", "herdr-croe")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("invalid backend accepted: %v", err)
	}
}

func TestWorkspaceBackendCannotBeChangedByTrustedCWD(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	root := t.TempDir()
	pinned := filepath.Join(t.TempDir(), "pinned.sock")
	attacker := filepath.Join(root, "attacker.sock")
	changeDir(t, root)
	t.Setenv("KWT_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, ".kwt.toml"), []byte(fmt.Sprintf("[workspace]\nbackend='herdr-core'\ncore_socket=%q\n", attacker)), 0600); err != nil {
		t.Fatal(err)
	}
	viper.Set("workspace.backend", "tmux")
	viper.Set("workspace.core_socket", pinned)
	if err := mergeLocalConfig(nil, trustingPrompter(), true); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString("workspace.core_socket"); got != pinned {
		t.Fatalf("repository changed machine Core route to %q", got)
	}
	if got := viper.GetString("workspace.backend"); got != "tmux" {
		t.Fatalf("repository changed machine backend to %q", got)
	}
}

func TestWorkspaceBackendCannotBeChangedByTrustedTarget(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	pinned := filepath.Join(t.TempDir(), "pinned.sock")
	viper.Set("workspace.backend", "tmux")
	viper.Set("workspace.core_socket", pinned)
	target := t.TempDir()
	path := filepath.Join(target, ".kwt.toml")
	data := []byte(fmt.Sprintf("[workspace]\nbackend='herdr-core'\ncore_socket=%q\n", filepath.Join(target, "attacker.sock")))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &TrustStore{path: defaultTrustStorePath()}
	if err := store.Add(normalized, computeSHA256(data)); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadForTarget(target, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	workspace, ok := fields["Workspace"].(map[string]any)
	if !ok || workspace["Backend"] != "tmux" || workspace["CoreSocket"] != pinned {
		t.Fatalf("target changed machine backend: %s", encoded)
	}
}

func TestWorkspaceBackendCoreRequiresCanonicalSocket(t *testing.T) {
	for _, socket := range []string{"", "relative.sock"} {
		t.Run(socket, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			viper.Set("workspace.backend", "herdr-core")
			viper.Set("workspace.core_socket", socket)
			if _, err := Load(); err == nil {
				t.Fatal("Core backend accepted an unpinned route")
			}
		})
	}
}
