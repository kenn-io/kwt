package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

// changeDir changes to the specified directory and returns a cleanup function
// that restores the original working directory.
func changeDir(t *testing.T, dir string) {
	t.Helper()
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Failed to change directory: %v", err)
	}
}

// stubPrompter is a trustPrompter for tests. response controls what PromptTrust
// returns; callCount records how many times it was invoked.
type stubPrompter struct {
	response  bool
	callCount int
}

func (s *stubPrompter) PromptTrust(string, []byte) (bool, error) {
	s.callCount++
	return s.response, nil
}

func trustingPrompter() trustPrompter {
	return &stubPrompter{response: true}
}

func TestRepositorySettingsParsing(t *testing.T) {
	viper.Reset()
	viper.SetConfigType("toml")
	configTOML := `
[[repository_settings]]
repository = "/tmp/repository1"
copy_files = ["templates/.env.example", "config/*.json"]
setup_commands = ["npm install", "echo done"]

[[repository_settings]]
repository = "/tmp/repository2"
copy_files = ["foo.txt"]
setup_commands = ["touch bar"]
`
	err := viper.ReadConfig(strings.NewReader(configTOML))
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if len(cfg.RepositorySettings) != 2 {
		t.Fatalf("Expected 2 repository_settings, got %d", len(cfg.RepositorySettings))
	}
	if cfg.RepositorySettings[0].Repository != "/tmp/repository1" {
		t.Errorf("First repository mismatch: %s", cfg.RepositorySettings[0].Repository)
	}
	if len(cfg.RepositorySettings[0].CopyFiles) != 2 || cfg.RepositorySettings[0].CopyFiles[0] != "templates/.env.example" {
		t.Errorf("First repository copy_files mismatch: %+v", cfg.RepositorySettings[0].CopyFiles)
	}
	if len(cfg.RepositorySettings[0].SetupCommands) != 2 || cfg.RepositorySettings[0].SetupCommands[0] != "npm install" {
		t.Errorf("First repository setup_commands mismatch: %+v", cfg.RepositorySettings[0].SetupCommands)
	}
	if cfg.RepositorySettings[1].Repository != "/tmp/repository2" {
		t.Errorf("Second repository mismatch: %s", cfg.RepositorySettings[1].Repository)
	}
	if len(cfg.RepositorySettings[1].CopyFiles) != 1 || cfg.RepositorySettings[1].CopyFiles[0] != "foo.txt" {
		t.Errorf("Second repository copy_files mismatch: %+v", cfg.RepositorySettings[1].CopyFiles)
	}
	if len(cfg.RepositorySettings[1].SetupCommands) != 1 || cfg.RepositorySettings[1].SetupCommands[0] != "touch bar" {
		t.Errorf("Second repository setup_commands mismatch: %+v", cfg.RepositorySettings[1].SetupCommands)
	}
}

func TestRepositorySettingsGlobPatternPreserved(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })
	viper.SetConfigType("toml")
	configTOML := `
[[repository_settings]]
repository = "**/cerebras/monolith"
setup_commands = ["make_vscode"]

[[repository_settings]]
repository = "/tmp/exact-path"
setup_commands = ["echo hi"]
`
	if err := viper.ReadConfig(strings.NewReader(configTOML)); err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Glob pattern should be preserved as-is (not expanded to absolute path)
	if cfg.RepositorySettings[0].Repository != "**/cerebras/monolith" {
		t.Errorf("Glob pattern was mangled: got %q, want %q",
			cfg.RepositorySettings[0].Repository, "**/cerebras/monolith")
	}

	// Exact path should still be expanded normally
	if cfg.RepositorySettings[1].Repository != "/tmp/exact-path" {
		t.Errorf("Exact path was changed: got %q, want %q",
			cfg.RepositorySettings[1].Repository, "/tmp/exact-path")
	}
}

func TestLoadIgnoresLegacyClaudeSettings(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})
	viper.SetConfigType("toml")
	configTOML := `
[worktree]
basedir = "/tmp/worktrees"
auto_mkdir = true

[claude]
executable = "claude"
config_dir = "~/.config/kwt/claude"
max_parallel = 4

[claude.queue]
queue_dir = "~/.config/kwt/claude/queue"
`
	if err := viper.ReadConfig(strings.NewReader(configTOML)); err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Worktree.BaseDir != "/tmp/worktrees" {
		t.Errorf("Worktree.BaseDir = %s, want /tmp/worktrees", cfg.Worktree.BaseDir)
	}
	if !cfg.Worktree.AutoMkdir {
		t.Error("Worktree.AutoMkdir should be true")
	}
}

func TestLoadFleetDefaultsDisabled(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })
	viper.SetConfigType("toml")
	require.NoError(t, viper.ReadConfig(strings.NewReader(`
[worktree]
basedir = "/tmp/worktrees"
auto_mkdir = true
`)))

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.Fleet.Enabled)
	assert.Empty(t, cfg.Fleet.HostID)
	assert.Empty(t, cfg.Fleet.HubURL)
	assert.Empty(t, cfg.Fleet.TokenFile)
	assert.Empty(t, cfg.Fleet.TokenEnv)
	assert.Empty(t, cfg.Fleet.Hub.ListenAddr)
	assert.Empty(t, cfg.Fleet.Hub.StorePath)
}

func TestLoadFleetConfigExpandsPaths(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })
	home := t.TempDir()
	t.Setenv("HOME", home)
	viper.SetConfigType("toml")
	require.NoError(t, viper.ReadConfig(strings.NewReader(`
[worktree]
basedir = "/tmp/worktrees"
auto_mkdir = true

[fleet]
enabled = true
host_id = "Host-A"
hub_url = "https://host-a.example"
token_file = "~/kwt/fleet.token"
token_env = "KWT_FLEET_TOKEN"

[fleet.hub]
listen_addr = "127.0.0.1:8787"
store_path = "~/kwt/fleet/state.json"
`)))

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.Fleet.Enabled)
	assert.Equal(t, "Host-A", cfg.Fleet.HostID)
	assert.Equal(t, "https://host-a.example", cfg.Fleet.HubURL)
	assert.Equal(t, filepath.Join(home, "kwt", "fleet.token"), cfg.Fleet.TokenFile)
	assert.Equal(t, filepath.Join(home, "kwt", "fleet", "state.json"), cfg.Fleet.Hub.StorePath)
}

func TestGetConfigDir(t *testing.T) {
	t.Setenv("KWT_HOME", "")

	// Test without XDG_CONFIG_HOME
	t.Run("WithoutXDGConfigHome", func(t *testing.T) {
		origXDG := os.Getenv("XDG_CONFIG_HOME")
		defer func() { _ = os.Setenv("XDG_CONFIG_HOME", origXDG) }()

		_ = os.Unsetenv("XDG_CONFIG_HOME")

		dir := getConfigDir()
		if !filepath.IsAbs(dir) {
			t.Errorf("getConfigDir() should return absolute path, got %s", dir)
		}
		if filepath.Base(dir) != "kwt" {
			t.Errorf("getConfigDir() should end with 'kwt', got %s", dir)
		}
	})

	// getConfigDir uses os.UserConfigDir which doesn't respect XDG_CONFIG_HOME on macOS
	// So we just verify the basic behavior
}

func TestGetConfigDirUsesKWTHome(t *testing.T) {
	kwtHome := filepath.Join(t.TempDir(), "kwt-home")
	t.Setenv("KWT_HOME", kwtHome)

	assert.Equal(t, kwtHome, getConfigDir())
}

func TestInit(t *testing.T) {
	// Create temporary directory for config
	tmpDir := t.TempDir()

	// Set test environment
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpDir, ".config"))

	// Reset viper to clean state
	viper.Reset()

	// Test initialization
	if err := Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Verify defaults are set
	if viper.GetString("worktree.basedir") != "~/.kwt/worktrees" {
		t.Errorf("Default worktree.basedir not set correctly")
	}
	if !viper.GetBool("worktree.auto_mkdir") {
		t.Errorf("Default worktree.auto_mkdir should be true")
	}
	if !viper.GetBool("finder.preview") {
		t.Errorf("Default finder.preview should be true")
	}
	if !viper.GetBool("ui.icons") {
		t.Errorf("Default ui.icons should be true")
	}
	if !viper.GetBool("cd.launch_shell") {
		t.Errorf("Default cd.launch_shell should be true")
	}

	// Cleanup viper for other tests
	t.Cleanup(func() {
		viper.Reset()
	})
}

func TestLoad(t *testing.T) {
	// Setup viper with test values
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})
	viper.Set("worktree.basedir", "~/test-worktrees")
	viper.Set("worktree.auto_mkdir", false)
	viper.Set("finder.preview", false)
	viper.Set("ui.icons", false)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify loaded values
	if cfg.Worktree.AutoMkdir {
		t.Errorf("WorktreeConfig.AutoMkdir = %v, want false", cfg.Worktree.AutoMkdir)
	}
	if cfg.Finder.Preview {
		t.Errorf("FinderConfig.Preview = %v, want false", cfg.Finder.Preview)
	}
	if cfg.UI.Icons {
		t.Errorf("UIConfig.Icons = %v, want false", cfg.UI.Icons)
	}
}

func TestPathExpansion(t *testing.T) {
	// Test home directory expansion
	t.Run("HomeDirectoryExpansion", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() {
			viper.Reset()
		})
		viper.Set("worktree.basedir", "~/worktrees")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		if filepath.IsAbs(cfg.Worktree.BaseDir) && cfg.Worktree.BaseDir != "~/worktrees" {
			// Path was expanded
			if !filepath.IsAbs(cfg.Worktree.BaseDir) {
				t.Errorf("Expanded path should be absolute, got %s", cfg.Worktree.BaseDir)
			}
		}
	})

	// Test environment variable expansion
	t.Run("EnvironmentVariableExpansion", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() {
			viper.Reset()
		})
		_ = os.Setenv("TEST_WORKTREE_DIR", "/test/path")
		defer func() { _ = os.Unsetenv("TEST_WORKTREE_DIR") }()

		viper.Set("worktree.basedir", "$TEST_WORKTREE_DIR/worktrees")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		expected := "/test/path/worktrees"
		if cfg.Worktree.BaseDir != expected {
			t.Errorf("BaseDir = %s, want %s", cfg.Worktree.BaseDir, expected)
		}
	})
}

func TestGettersAndSetters(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})

	// Test Set and Get
	testKey := "test.key"
	testValue := "test-value"

	// Note: In real usage, Set would write to config file
	// For testing, we'll just verify viper operations
	viper.Set(testKey, testValue)

	if got := GetValue(testKey); got != testValue {
		t.Errorf("GetValue(%s) = %v, want %v", testKey, got, testValue)
	}

}

func TestAllSettings(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})
	viper.Set("test.key1", "value1")
	viper.Set("test.key2", 123)
	viper.Set("test.key3", true)

	settings := AllSettings()
	if len(settings) == 0 {
		t.Error("AllSettings() returned empty map")
	}

	// Check if our test settings are included
	if testSection, ok := settings["test"].(map[string]any); ok {
		if testSection["key1"] != "value1" {
			t.Errorf("AllSettings() missing or incorrect test.key1")
		}
		if testSection["key2"] != 123 {
			t.Errorf("AllSettings() missing or incorrect test.key2")
		}
		if testSection["key3"] != true {
			t.Errorf("AllSettings() missing or incorrect test.key3")
		}
	} else {
		t.Error("AllSettings() missing 'test' section")
	}
}

func TestConfigStructureIntegrity(t *testing.T) {
	t.Cleanup(func() {
		viper.Reset()
	})
	// This test ensures that the Config structure can be properly marshaled/unmarshaled
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   "/test/worktrees",
			AutoMkdir: true,
		},
		Finder: models.FinderConfig{
			Preview: true,
		},
		UI: models.UIConfig{
			Icons: false,
		},
	}

	// Set values in viper
	viper.Reset()
	viper.Set("worktree.basedir", cfg.Worktree.BaseDir)
	viper.Set("worktree.auto_mkdir", cfg.Worktree.AutoMkdir)
	viper.Set("finder.preview", cfg.Finder.Preview)
	viper.Set("ui.icons", cfg.UI.Icons)

	// Load and verify
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Compare loaded config with original
	if loaded.Worktree.BaseDir != cfg.Worktree.BaseDir {
		t.Errorf("Worktree.BaseDir mismatch")
	}
	if loaded.Worktree.AutoMkdir != cfg.Worktree.AutoMkdir {
		t.Errorf("Worktree.AutoMkdir mismatch")
	}
	if loaded.Finder.Preview != cfg.Finder.Preview {
		t.Errorf("Finder.Preview mismatch")
	}
	if loaded.UI.Icons != cfg.UI.Icons {
		t.Errorf("UI.Icons mismatch")
	}
}

func TestMergeLocalConfig(t *testing.T) {
	t.Run("NoLocalConfig", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		// Change to temp directory (no local config)
		tmpDir := t.TempDir()
		changeDir(t, tmpDir)

		// Load global settings
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")

		// Merge local config (no file exists)
		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}

		// Verify global config is preserved
		if viper.GetString("worktree.basedir") != "~/global-worktrees" {
			t.Errorf("Global config should be preserved, got %s", viper.GetString("worktree.basedir"))
		}
	})

	t.Run("WithLocalConfig", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		// Create local config in temp directory
		tmpDir := t.TempDir()
		localConfigPath := filepath.Join(tmpDir, ".kwt.toml")
		localConfig := []byte(`
[worktree]
basedir = "~/local-worktrees"

[finder]
preview = false
`)
		if err := os.WriteFile(localConfigPath, localConfig, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		// Load global settings
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")
		viper.Set("worktree.auto_mkdir", true)
		viper.Set("finder.preview", true)

		// Merge local config
		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}

		// Verify local config overrides global
		if viper.GetString("worktree.basedir") != "~/local-worktrees" {
			t.Errorf("Local config should override global: got %s", viper.GetString("worktree.basedir"))
		}
		if viper.GetBool("finder.preview") != false {
			t.Errorf("Local config should override finder.preview to false")
		}
		// Verify global-only settings are preserved
		if viper.GetBool("worktree.auto_mkdir") != true {
			t.Errorf("Global-only config should be preserved")
		}
	})

	t.Run("IgnoresWorkspaces", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		tmpDir := t.TempDir()
		localConfig := []byte(`
[[workspaces]]
name = "evil"
path = "/tmp/evil"
`)
		if err := os.WriteFile(filepath.Join(tmpDir, ".kwt.toml"), localConfig, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		viper.SetConfigType("toml")

		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}
		if viper.IsSet("workspaces") {
			t.Errorf("workspaces must not be settable from local config")
		}
	})

	t.Run("IgnoresWorkspacesTable", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		tmpDir := t.TempDir()
		localConfig := []byte(`
[workspaces]
name = "evil"
path = "/tmp/evil"
`)
		if err := os.WriteFile(filepath.Join(tmpDir, ".kwt.toml"), localConfig, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		viper.SetConfigType("toml")

		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}
		if viper.IsSet("workspaces.name") {
			t.Errorf("workspaces.name must not be settable from local config")
		}
		if viper.IsSet("workspaces") {
			t.Errorf("workspaces must not be settable from local config")
		}
	})

	t.Run("PartialOverride", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		// Create partial local config in temp directory
		tmpDir := t.TempDir()
		localConfigPath := filepath.Join(tmpDir, ".kwt.toml")
		localConfig := []byte(`
[naming]
template = "{{.Repository}}/{{.Branch}}"
`)
		if err := os.WriteFile(localConfigPath, localConfig, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		// Load global settings
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")
		viper.Set("naming.template", "{{.Host}}/{{.Owner}}/{{.Repository}}/{{.Branch}}")
		viper.Set("naming.sanitize_chars", map[string]string{"/": "-"})

		// Merge local config
		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}

		// Verify only naming.template is overridden
		if viper.GetString("naming.template") != "{{.Repository}}/{{.Branch}}" {
			t.Errorf("naming.template should be overridden, got %s", viper.GetString("naming.template"))
		}
		// Verify worktree.basedir is preserved
		if viper.GetString("worktree.basedir") != "~/global-worktrees" {
			t.Errorf("worktree.basedir should be preserved, got %s", viper.GetString("worktree.basedir"))
		}
	})

	t.Run("IgnoresFleetSettings", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		tmpDir := t.TempDir()
		localConfigPath := filepath.Join(tmpDir, ".kwt.toml")
		localConfig := []byte(`
[worktree]
basedir = "~/local-worktrees"

[fleet]
enabled = true
hub_url = "https://attacker.example"
token_env = "AWS_SECRET_ACCESS_KEY"
token_file = "/home/user/.ssh/id_ed25519"

[fleet.hub]
listen_addr = "0.0.0.0:9999"
`)
		if err := os.WriteFile(localConfigPath, localConfig, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		viper.SetConfigType("toml")
		viper.Set("fleet.hub_url", "https://hub.internal")

		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}

		if viper.GetBool("fleet.enabled") {
			t.Errorf("fleet.enabled must not be settable from local config")
		}
		if got := viper.GetString("fleet.hub_url"); got != "https://hub.internal" {
			t.Errorf("fleet.hub_url must not be overridden by local config, got %s", got)
		}
		if got := viper.GetString("fleet.token_env"); got != "" {
			t.Errorf("fleet.token_env must not be settable from local config, got %s", got)
		}
		if got := viper.GetString("fleet.token_file"); got != "" {
			t.Errorf("fleet.token_file must not be settable from local config, got %s", got)
		}
		if got := viper.GetString("fleet.hub.listen_addr"); got != "" {
			t.Errorf("fleet.hub.listen_addr must not be settable from local config, got %s", got)
		}
		if viper.GetString("worktree.basedir") != "~/local-worktrees" {
			t.Errorf("non-fleet local settings should still merge")
		}
	})

	t.Run("IgnoresDaemonSettings", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(viper.Reset)
		applyGlobalDefaults(viper.GetViper())
		globalGrace := viper.GetDuration("daemon.replacement_grace")

		tmpDir := t.TempDir()
		localConfig := []byte(`[daemon]
idle_timeout = "-1s"
auto_restart = "sometimes"
replacement_grace = "0s"
`)
		require.NoError(t, os.WriteFile(
			filepath.Join(tmpDir, ".kwt.toml"),
			localConfig,
			0o600,
		))
		changeDir(t, tmpDir)

		require.NoError(t, mergeLocalConfig(
			&TrustStore{},
			trustingPrompter(),
			true,
		))
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, 2*time.Hour, cfg.Daemon.IdleTimeout)
		assert.Equal(t, "newer", cfg.Daemon.AutoRestart)
		assert.Equal(t, globalGrace, cfg.Daemon.ReplacementGrace)
	})
}

func TestMergeLocalConfigRepositorySettings(t *testing.T) {
	t.Run("MergeByRepositoryKey", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		// Create local config in temp directory
		tmpDir := t.TempDir()
		localConfigPath := filepath.Join(tmpDir, ".kwt.toml")
		localConfig := []byte(`
[[repository_settings]]
repository = "/shared/repo"
setup_commands = ["npm install"]
copy_files = [".env.local"]

[[repository_settings]]
repository = "/local/repo"
setup_commands = ["make build"]
`)
		if err := os.WriteFile(localConfigPath, localConfig, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		// Load global settings (including repository_settings)
		viper.SetConfigType("toml")
		globalConfig := `
[[repository_settings]]
repository = "/global/repo"
setup_commands = ["yarn install"]
copy_files = [".env.global"]

[[repository_settings]]
repository = "/shared/repo"
setup_commands = ["old command"]
copy_files = [".env.old"]
`
		if err := viper.ReadConfig(strings.NewReader(globalConfig)); err != nil {
			t.Fatalf("Failed to read global config: %v", err)
		}

		// Merge local config
		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}

		// Load as struct
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		// Verify repository settings are merged by repository key:
		// - Global only: /global/repo
		// - Local only: /local/repo
		// - Both: /shared/repo -> overridden by local
		if len(cfg.RepositorySettings) != 3 {
			t.Errorf("Expected 3 repository_settings (merged), got %d", len(cfg.RepositorySettings))
			for _, rs := range cfg.RepositorySettings {
				t.Logf("  - %s", rs.Repository)
			}
		}

		// Verify each setting
		found := make(map[string]bool)
		for _, rs := range cfg.RepositorySettings {
			found[rs.Repository] = true

			switch rs.Repository {
			case "/global/repo":
				// Global-only settings are preserved
				if len(rs.SetupCommands) != 1 || rs.SetupCommands[0] != "yarn install" {
					t.Errorf("/global/repo setup_commands should be preserved: %v", rs.SetupCommands)
				}
			case "/local/repo":
				// Local-only settings are added
				if len(rs.SetupCommands) != 1 || rs.SetupCommands[0] != "make build" {
					t.Errorf("/local/repo setup_commands mismatch: %v", rs.SetupCommands)
				}
			case "/shared/repo":
				// Same repository is overridden by local
				if len(rs.SetupCommands) != 1 || rs.SetupCommands[0] != "npm install" {
					t.Errorf("/shared/repo should be overridden by local: %v", rs.SetupCommands)
				}
				if len(rs.CopyFiles) != 1 || rs.CopyFiles[0] != ".env.local" {
					t.Errorf("/shared/repo copy_files should be overridden: %v", rs.CopyFiles)
				}
			}
		}

		if !found["/global/repo"] {
			t.Error("/global/repo should be present (from global)")
		}
		if !found["/local/repo"] {
			t.Error("/local/repo should be present (from local)")
		}
		if !found["/shared/repo"] {
			t.Error("/shared/repo should be present (overridden by local)")
		}
	})

	t.Run("NoLocalRepositorySettingsKeepsGlobal", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })

		// Create local config without repository_settings in temp directory
		tmpDir := t.TempDir()
		localConfigPath := filepath.Join(tmpDir, ".kwt.toml")
		localConfig := []byte(`
[worktree]
basedir = "~/local-worktrees"
`)
		if err := os.WriteFile(localConfigPath, localConfig, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		// Load global settings
		viper.SetConfigType("toml")
		globalConfig := `
[[repository_settings]]
repository = "/global/repo1"
setup_commands = ["yarn install"]
`
		if err := viper.ReadConfig(strings.NewReader(globalConfig)); err != nil {
			t.Fatalf("Failed to read global config: %v", err)
		}

		// Merge local config
		if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
			t.Fatalf("mergeLocalConfig() error = %v", err)
		}

		// Load as struct
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		// Verify global repository_settings is preserved
		if len(cfg.RepositorySettings) != 1 {
			t.Errorf("Expected 1 repository_settings (global), got %d", len(cfg.RepositorySettings))
		}
		if len(cfg.RepositorySettings) > 0 && cfg.RepositorySettings[0].Repository != "/global/repo1" {
			t.Errorf("Global repository_settings should be preserved, got %s", cfg.RepositorySettings[0].Repository)
		}

		// Verify worktree.basedir is overridden by local
		// (Check viper directly since Load() expands paths)
		if viper.GetString("worktree.basedir") != "~/local-worktrees" {
			t.Errorf("Local worktree.basedir should override global, got %s", viper.GetString("worktree.basedir"))
		}
	})
}

func TestGetLocalConfigPath(t *testing.T) {
	t.Run("FileExists", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Resolve symlinks for macOS where /var -> /private/var
		tmpDir, err := filepath.EvalSymlinks(tmpDir)
		if err != nil {
			t.Fatalf("Failed to resolve symlinks: %v", err)
		}

		localConfigPath := filepath.Join(tmpDir, ".kwt.toml")
		if err := os.WriteFile(localConfigPath, []byte{}, 0644); err != nil {
			t.Fatalf("Failed to create local config: %v", err)
		}
		changeDir(t, tmpDir)

		path := getLocalConfigPath()
		if path != localConfigPath {
			t.Errorf("getLocalConfigPath() = %s, want %s", path, localConfigPath)
		}
	})

	t.Run("FileNotExists", func(t *testing.T) {
		tmpDir := t.TempDir()
		changeDir(t, tmpDir)

		path := getLocalConfigPath()
		if path != "" {
			t.Errorf("getLocalConfigPath() = %s, want empty string", path)
		}
	})
}

func TestSetGlobalDoesNotWriteLocalSettings(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	// Create temp directory and isolate from real user config
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	localDir := filepath.Join(tmpDir, "local")

	// Create isolated HOME directory structure
	if err := os.MkdirAll(filepath.Join(homeDir, ".config", "kwt"), 0755); err != nil {
		t.Fatalf("Failed to create config directory: %v", err)
	}
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("Failed to create local directory: %v", err)
	}
	// Set HOME for Unix and USERPROFILE for Windows to ensure os.UserHomeDir() is isolated
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	// Create global config in isolated HOME
	globalConfigPath := filepath.Join(homeDir, ".config", "kwt", "config.toml")
	globalConfig := []byte(`
[finder]
preview = true

[ui]
icons = true
`)
	if err := os.WriteFile(globalConfigPath, globalConfig, 0644); err != nil {
		t.Fatalf("Failed to create global config: %v", err)
	}

	// Create local config with different value
	localConfigPath := filepath.Join(localDir, ".kwt.toml")
	localConfig := []byte(`
[finder]
preview = false
`)
	if err := os.WriteFile(localConfigPath, localConfig, 0644); err != nil {
		t.Fatalf("Failed to create local config: %v", err)
	}
	changeDir(t, localDir)

	// Setup viper with global config and merge local
	viper.SetConfigType("toml")
	viper.SetConfigFile(globalConfigPath)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read global config: %v", err)
	}

	// Merge local config (simulating Init behavior)
	if err := mergeLocalConfig(&TrustStore{}, trustingPrompter(), true); err != nil {
		t.Fatalf("Failed to merge local config: %v", err)
	}

	// Verify local config is merged (finder.preview = false)
	if viper.GetBool("finder.preview") != false {
		t.Fatal("Local config should have been merged")
	}

	// Call SetGlobal with a new value (writes to isolated HOME, not real user config)
	if err := SetGlobal("ui.tilde_home", true); err != nil {
		t.Fatalf("SetGlobal() error = %v", err)
	}

	// Read the global config file directly to verify it wasn't polluted
	globalViper := viper.New()
	globalViper.SetConfigFile(globalConfigPath)
	globalViper.SetConfigType("toml")
	if err := globalViper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read global config after SetGlobal: %v", err)
	}

	// The global config should NOT have finder.preview = false (local setting)
	// It should keep its original value of true
	if globalViper.GetBool("finder.preview") != true {
		t.Error("Global config should NOT contain local settings (finder.preview should be true)")
	}

	// Verify the new value was written
	if globalViper.GetBool("ui.tilde_home") != true {
		t.Error("SetGlobal should have written the new value")
	}
}

func TestRegisterProjectWritesGlobalProjects(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	require.NoError(t, Init())

	repoRoot := t.TempDir()
	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/example/kwt",
		Name:       "kwt",
		Path:       repoRoot,
	}))
	wantRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)

	cfg, err := Load()
	require.NoError(t, err)
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "github.com/example/kwt", cfg.Projects[0].Repository)
	assert.Equal(t, "kwt", cfg.Projects[0].Name)
	assert.Equal(t, wantRepoRoot, cfg.Projects[0].Path)
	assert.NotEmpty(t, cfg.Projects[0].LastTouched)
	assert.Empty(t, cfg.RepositorySettings)

	globalViper := viper.New()
	globalViper.SetConfigFile(filepath.Join(kwtHome, "config.toml"))
	globalViper.SetConfigType("toml")
	require.NoError(t, globalViper.ReadInConfig())

	var persisted []models.Project
	require.NoError(t, globalViper.UnmarshalKey("projects", &persisted))
	require.Len(t, persisted, 1)
	assert.Equal(t, cfg.Projects[0], persisted[0])
}

func TestRegisterProjectUpdatesExistingProject(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	t.Setenv("KWT_HOME", t.TempDir())
	require.NoError(t, Init())

	firstPath := t.TempDir()
	secondPath := t.TempDir()
	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/example/kwt",
		Name:       "kwt",
		Path:       firstPath,
	}))
	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/example/kwt",
		Name:       "kwt-renamed",
		Path:       secondPath,
	}))
	wantSecondPath, err := filepath.EvalSymlinks(secondPath)
	require.NoError(t, err)

	cfg, err := Load()
	require.NoError(t, err)
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "kwt-renamed", cfg.Projects[0].Name)
	assert.Equal(t, wantSecondPath, cfg.Projects[0].Path)
}

func TestRegisterProjectPreservesUnknownProjectFields(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	firstPath := t.TempDir()
	secondPath := t.TempDir()
	replacementPath := t.TempDir()
	configPath := filepath.Join(kwtHome, "config.toml")
	contents := fmt.Sprintf(`[[projects]]
repository = "github.com/example/widget"
name = "widget"
path = %q
last_touched = "2026-08-01T00:00:00Z"
future_policy = "retain"

[[projects]]
repository = "github.com/example/other"
name = "other"
path = %q
last_touched = "2026-08-01T00:00:00Z"
future_policy = "untouched"
`, firstPath, secondPath)
	require.NoError(t, os.WriteFile(configPath, []byte(contents), 0o600))

	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/example/widget",
		Name:       "widget-renamed",
		Path:       replacementPath,
	}))
	wantReplacementPath, err := filepath.EvalSymlinks(replacementPath)
	require.NoError(t, err)

	stored, err := readGlobalViper(configPath)
	require.NoError(t, err)
	projects, err := rawProjectEntries(stored)
	require.NoError(t, err)
	require.Len(t, projects, 2)
	assert.Equal(t, "retain", projects[0]["future_policy"])
	assert.Equal(t, "widget-renamed", projects[0]["name"])
	assert.Equal(t, wantReplacementPath, projects[0]["path"])
	assert.Equal(t, "untouched", projects[1]["future_policy"])
}

func TestRegisterProjectAppendsWithoutDiscardingUnknownFields(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	existingPath := t.TempDir()
	newPath := t.TempDir()
	configPath := filepath.Join(kwtHome, "config.toml")
	contents := fmt.Sprintf(`[[projects]]
repository = "github.com/example/existing"
name = "existing"
path = %q
last_touched = "2026-08-01T00:00:00Z"
future_policy = "retain"
`, existingPath)
	require.NoError(t, os.WriteFile(configPath, []byte(contents), 0o600))

	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/example/new",
		Name:       "new",
		Path:       newPath,
	}))

	stored, err := readGlobalViper(configPath)
	require.NoError(t, err)
	projects, err := rawProjectEntries(stored)
	require.NoError(t, err)
	require.Len(t, projects, 2)
	assert.Equal(t, "retain", projects[0]["future_policy"])
	assert.Equal(t, "github.com/example/new", projects[1]["repository"])
}

func TestRegisterProjectMatchesCaseInsensitiveRepositoryIdentity(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	t.Setenv("KWT_HOME", t.TempDir())
	require.NoError(t, Init())

	firstPath := t.TempDir()
	secondPath := t.TempDir()
	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/Acme/Widget",
		Name:       "widget",
		Path:       firstPath,
	}))
	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/acme/widget",
		Name:       "widget",
		Path:       secondPath,
	}))
	wantSecondPath, err := filepath.EvalSymlinks(secondPath)
	require.NoError(t, err)

	cfg, err := Load()
	require.NoError(t, err)
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, wantSecondPath, cfg.Projects[0].Path)
}

func TestRegisterProjectUpgradesPathFallbackByPath(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	require.NoError(t, Init())

	repoRoot := t.TempDir()
	require.NoError(t, RegisterProject(models.Project{
		Repository: repoRoot,
		Name:       "service-local",
		Path:       repoRoot,
	}))
	require.NoError(t, RegisterProject(models.Project{
		Repository: "github.com/example/service-api",
		Name:       "service",
		Path:       repoRoot,
	}))
	wantRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)

	cfg, err := Load()
	require.NoError(t, err)
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "github.com/example/service-api", cfg.Projects[0].Repository)
	assert.Equal(t, "service", cfg.Projects[0].Name)
	assert.Equal(t, wantRepoRoot, cfg.Projects[0].Path)

	globalViper := viper.New()
	globalViper.SetConfigFile(filepath.Join(kwtHome, "config.toml"))
	globalViper.SetConfigType("toml")
	require.NoError(t, globalViper.ReadInConfig())

	var persisted []models.Project
	require.NoError(t, globalViper.UnmarshalKey("projects", &persisted))
	require.Len(t, persisted, 1)
	assert.Equal(t, cfg.Projects[0], persisted[0])
}

func TestRegisterProjectDoesNotOverwriteUnreadableConfig(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	configPath := filepath.Join(kwtHome, "config.toml")
	original := []byte("[worktree\nbasedir = \"~/worktrees\"\n")
	require.NoError(t, os.WriteFile(configPath, original, 0o600))

	err := RegisterProject(models.Project{
		Repository: "github.com/example/service-api",
		Name:       "service",
		Path:       t.TempDir(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read config")
	data, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	assert.Equal(t, original, data)
}

func TestMergeLocalConfigDoesNotOverrideGlobalProjects(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })
	viper.Set("worktree.basedir", t.TempDir())
	viper.Set("projects", []models.Project{{
		Repository: "github.com/example/kwt",
		Name:       "kwt",
		Path:       "/global/kwt",
	}})

	writeLocalConfig(t, t.TempDir(), []byte(`
[[projects]]
repository = "github.com/evil/repo"
name = "repo"
path = "/evil/repo"
`))
	store := &TrustStore{}
	prompter := &stubPrompter{response: true}

	require.NoError(t, mergeLocalConfig(store, prompter, true))
	cfg, err := Load()
	require.NoError(t, err)

	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "github.com/example/kwt", cfg.Projects[0].Repository)
	assert.Equal(t, "kwt", cfg.Projects[0].Name)
}

func TestSetLocal(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	// Create temp directory for local config
	tmpDir := t.TempDir()
	changeDir(t, tmpDir)

	// Call SetLocal to create local config
	if err := SetLocal("finder.preview", false); err != nil {
		t.Fatalf("SetLocal() error = %v", err)
	}

	// Verify local config file was created
	localConfigPath := filepath.Join(tmpDir, ".kwt.toml")
	if _, err := os.Stat(localConfigPath); os.IsNotExist(err) {
		t.Fatal("Local config file should have been created")
	}

	// Read local config and verify the value
	localViper := viper.New()
	localViper.SetConfigFile(localConfigPath)
	localViper.SetConfigType("toml")
	if err := localViper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read local config: %v", err)
	}

	if localViper.GetBool("finder.preview") != false {
		t.Error("Local config should contain the set value (finder.preview = false)")
	}
}

// writeLocalConfig places a .kwt.toml in tmpDir and chdir's into it.
// Returns the canonical absolute path (symlinks resolved).
func writeLocalConfig(t *testing.T, tmpDir string, content []byte) (absPath string, data []byte) {
	t.Helper()
	realDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	path := filepath.Join(realDir, ".kwt.toml")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	changeDir(t, realDir)
	return path, content
}

func TestMergeLocalConfigTrust(t *testing.T) {
	localTOML := []byte(`
[worktree]
basedir = "~/local-worktrees"

[[repository_settings]]
repository = "/shared/repo"
setup_commands = ["echo from-local"]
copy_files = [".env.local"]
`)

	t.Run("trusted: preloaded entry skips prompt and merges", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")

		absPath, data := writeLocalConfig(t, t.TempDir(), localTOML)
		store := &TrustStore{
			entries: []trustEntry{{Path: absPath, SHA256: computeSHA256(data)}},
		}
		prompter := &stubPrompter{response: false} // would deny if called

		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("mergeLocalConfig: %v", err)
		}
		if prompter.callCount != 0 {
			t.Errorf("prompter should not be called for trusted file, got %d calls", prompter.callCount)
		}
		if viper.GetString("worktree.basedir") != "~/local-worktrees" {
			t.Errorf("merge did not run: worktree.basedir = %s", viper.GetString("worktree.basedir"))
		}
	})

	t.Run("sha256 mismatch denied: merge skipped, copy_files/basedir not applied", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")

		absPath, _ := writeLocalConfig(t, t.TempDir(), localTOML)
		// Registered but with a stale hash
		store := &TrustStore{
			entries: []trustEntry{{Path: absPath, SHA256: "stale-hash"}},
		}
		prompter := &stubPrompter{response: false}

		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("mergeLocalConfig: %v", err)
		}
		if prompter.callCount != 1 {
			t.Errorf("prompter should be called once for stale hash, got %d", prompter.callCount)
		}
		if viper.GetString("worktree.basedir") != "~/global-worktrees" {
			t.Errorf("merge should be skipped, basedir = %s", viper.GetString("worktree.basedir"))
		}
		// repository_settings from local config should NOT be in viper
		if rs, ok := viper.Get("repository_settings").([]any); ok && len(rs) > 0 {
			t.Errorf("repository_settings should not be applied on denial, got %v", rs)
		}
	})

	t.Run("new file accepted: merge runs and store is updated", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")

		absPath, data := writeLocalConfig(t, t.TempDir(), localTOML)
		store := &TrustStore{} // empty in-memory
		prompter := &stubPrompter{response: true}

		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("mergeLocalConfig: %v", err)
		}
		if prompter.callCount != 1 {
			t.Errorf("prompter should be called once, got %d", prompter.callCount)
		}
		if viper.GetString("worktree.basedir") != "~/local-worktrees" {
			t.Errorf("merge should run: basedir = %s", viper.GetString("worktree.basedir"))
		}
		if !store.IsTrusted(absPath, computeSHA256(data)) {
			t.Error("store should be updated after accept")
		}
	})

	t.Run("non-interactive: untrusted file is skipped without prompt", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")

		writeLocalConfig(t, t.TempDir(), localTOML)
		store := &TrustStore{}
		prompter := &stubPrompter{response: true} // would accept if called

		if err := mergeLocalConfig(store, prompter, false); err != nil {
			t.Fatalf("mergeLocalConfig: %v", err)
		}
		if prompter.callCount != 0 {
			t.Errorf("prompter must not be called in non-interactive mode, got %d calls", prompter.callCount)
		}
		if viper.GetString("worktree.basedir") != "~/global-worktrees" {
			t.Errorf("merge must be skipped: basedir = %s", viper.GetString("worktree.basedir"))
		}
	})

	t.Run("store write failure: merge proceeds with warning", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")

		writeLocalConfig(t, t.TempDir(), localTOML)
		// Store path under a nonexistent parent → MkdirAll inside a non-writable
		// parent fails. Use a path whose parent is a file, which will fail.
		badParent := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(badParent, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		store := &TrustStore{path: filepath.Join(badParent, "child", "trusted.json")}
		prompter := &stubPrompter{response: true}

		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("mergeLocalConfig should not error on store write failure: %v", err)
		}
		if viper.GetString("worktree.basedir") != "~/local-worktrees" {
			t.Errorf("merge should still run: basedir = %s", viper.GetString("worktree.basedir"))
		}
	})

	t.Run("not regular file: merge skipped, no error", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")
		viper.Set("worktree.basedir", "~/global-worktrees")

		tmpDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		// Create .kwt.toml as a directory
		if err := os.Mkdir(filepath.Join(tmpDir, ".kwt.toml"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		changeDir(t, tmpDir)

		store := &TrustStore{}
		prompter := &stubPrompter{response: true}
		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("expected non-error on non-regular file, got %v", err)
		}
		if prompter.callCount != 0 {
			t.Errorf("prompter should not be called for non-regular file, got %d", prompter.callCount)
		}
		if viper.GetString("worktree.basedir") != "~/global-worktrees" {
			t.Errorf("merge should be skipped: basedir = %s", viper.GetString("worktree.basedir"))
		}
	})

	t.Run("SetLocal invalidates trust: rewriting file changes sha256 and re-prompts", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")

		absPath, data := writeLocalConfig(t, t.TempDir(), []byte("[worktree]\nbasedir = \"/v1\"\n"))
		store := &TrustStore{
			entries: []trustEntry{{Path: absPath, SHA256: computeSHA256(data)}},
		}
		prompter := &stubPrompter{response: true}

		// First merge: trusted, no prompt
		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("first merge: %v", err)
		}
		if prompter.callCount != 0 {
			t.Errorf("first merge should not prompt, got %d calls", prompter.callCount)
		}

		// Simulate SetLocal (which edits the file on disk).
		if err := SetLocal("worktree.basedir", "/v2"); err != nil {
			t.Fatalf("SetLocal: %v", err)
		}

		// Second merge: hash changed → re-prompt
		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("second merge: %v", err)
		}
		if prompter.callCount != 1 {
			t.Errorf("second merge should prompt after SetLocal changed file, got %d calls", prompter.callCount)
		}
	})

	// Denial must not leak repository_settings into the *models.Config returned
	// by Load(), which is the struct the worktree Manager consumes to decide
	// whether to run setup_commands.
	t.Run("denied local config: Load() returns no leaked repository_settings", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")

		writeLocalConfig(t, t.TempDir(), []byte(`
[[repository_settings]]
repository = "/mock/repo/path"
setup_commands = ["sh -c 'touch /tmp/kwt-should-not-run'"]
copy_files = [".env.evil"]
`))
		store := &TrustStore{}
		prompter := &stubPrompter{response: false}

		if err := mergeLocalConfig(store, prompter, true); err != nil {
			t.Fatalf("mergeLocalConfig: %v", err)
		}

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.RepositorySettings) != 0 {
			t.Errorf("denied local config leaked %d repository_settings into Config: %+v",
				len(cfg.RepositorySettings), cfg.RepositorySettings)
		}
	})

	// Init() passes store=nil so the trust store is loaded lazily from disk
	// only when a local .kwt.toml exists. Verify the lazy load runs and uses
	// the on-disk store.
	t.Run("nil store: lazy-load from disk", func(t *testing.T) {
		viper.Reset()
		t.Cleanup(func() { viper.Reset() })
		viper.SetConfigType("toml")

		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		absPath, data := writeLocalConfig(t, t.TempDir(), []byte("[worktree]\nbasedir = \"/from-local\"\n"))

		// Seed trust store on disk so lazy-load finds it trusted.
		onDisk := &TrustStore{path: defaultTrustStorePath()}
		if err := onDisk.Add(absPath, computeSHA256(data)); err != nil {
			t.Fatalf("seed trust store: %v", err)
		}

		prompter := &stubPrompter{response: false}
		if err := mergeLocalConfig(nil, prompter, true); err != nil {
			t.Fatalf("mergeLocalConfig: %v", err)
		}
		if prompter.callCount != 0 {
			t.Errorf("lazy-loaded trust store should have trusted the file, got %d prompt calls", prompter.callCount)
		}
		if viper.GetString("worktree.basedir") != "/from-local" {
			t.Errorf("merge should run: basedir = %s", viper.GetString("worktree.basedir"))
		}
	})
}

func TestMergeLocalConfigRejectsEnvironmentReferencesInNaming(t *testing.T) {
	tests := []struct {
		name  string
		local string
	}{
		{
			name: "template",
			local: `
[naming]
template = "$KWT_GITHUB_TOKEN/{{.Branch}}"
`,
		},
		{
			name: "replacement",
			local: `
[naming.sanitize_chars]
"/" = "$KWT_GITHUB_TOKEN"
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			absPath, data := writeLocalConfig(
				t,
				t.TempDir(),
				[]byte(tt.local),
			)
			store := &TrustStore{
				entries: []trustEntry{{
					Path:   absPath,
					SHA256: computeSHA256(data),
				}},
			}

			err := mergeLocalConfig(store, trustingPrompter(), true)

			if err == nil ||
				!strings.Contains(err.Error(), "environment variable references") {
				t.Fatalf("mergeLocalConfig() error = %v, want environment rejection", err)
			}
		})
	}
}

func TestMergeLocalConfigAllowsTemplateVariablesNamedWithDollar(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	absPath, data := writeLocalConfig(
		t,
		t.TempDir(),
		[]byte(`
[naming]
template = "{{$branch := .Branch}}/{{$branch}}"
`),
	)
	store := &TrustStore{
		entries: []trustEntry{{
			Path:   absPath,
			SHA256: computeSHA256(data),
		}},
	}

	require.NoError(t, mergeLocalConfig(store, trustingPrompter(), true))
}

func TestMergeLocalConfigTracksTemplateProvenanceSeparately(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	absPath, data := writeLocalConfig(
		t,
		t.TempDir(),
		[]byte(`
[naming]
template = "{{.Repository}}/{{.Branch}}"
`),
	)
	store := &TrustStore{
		entries: []trustEntry{{
			Path:   absPath,
			SHA256: computeSHA256(data),
		}},
	}

	if err := mergeLocalConfig(store, trustingPrompter(), true); err != nil {
		t.Fatalf("mergeLocalConfig() error = %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	assert.True(t, cfg.Naming.TemplateRepositoryLocal)
	assert.False(t, cfg.Naming.SanitizeCharsRepositoryLocal)
}

func TestMergeLocalConfigTracksSanitizationProvenanceSeparately(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("naming.template", "$KWT_GROUP/{{.Branch}}")
	absPath, data := writeLocalConfig(
		t,
		t.TempDir(),
		[]byte(`
[naming.sanitize_chars]
"/" = "-"
`),
	)
	store := &TrustStore{
		entries: []trustEntry{{
			Path:   absPath,
			SHA256: computeSHA256(data),
		}},
	}

	require.NoError(t, mergeLocalConfig(store, trustingPrompter(), true))
	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.Naming.TemplateRepositoryLocal)
	assert.True(t, cfg.Naming.SanitizeCharsRepositoryLocal)
}

func TestDefaultLayoutsConfig(t *testing.T) {
	// Isolate HOME so Init() reads and writes a throwaway config dir,
	// never the real ~/.config/kwt (Init calls viper.SafeWriteConfig).
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "codex", cfg.Agents["codex"])
	assert.Equal(t, "claude", cfg.Agents["claude"])
	assert.Equal(t, "roborev tui", cfg.Agents["roborev"])
	assert.Equal(t, "{{.FullPath}}/{{.Branch}}", cfg.Naming.Template)
	assert.Empty(t, cfg.Layouts.Default, "fresh config must default to a blank session")
	assert.True(t, cfg.Layouts.AutoLaunchOnAdd)
	require.NotEmpty(t, cfg.Layouts.Presets)

	byName := make(map[string]models.Layout, len(cfg.Layouts.Presets))
	for _, p := range cfg.Layouts.Presets {
		byName[p.Name] = p
	}
	wantArrange := map[string]string{
		"quad":  "even-horizontal",
		"grid":  "tiled",
		"focus": "main-vertical",
		"stack": "even-vertical",
	}
	wantPanes := []string{"agent:codex", "agent:claude", "agent:roborev", ""}
	for name, arrange := range wantArrange {
		p, ok := byName[name]
		require.True(t, ok, "missing built-in preset %q", name)
		assert.Equal(t, arrange, p.Arrange, "preset %q arrange", name)
		assert.Equal(t, wantPanes, p.Panes, "preset %q panes", name)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".config", "kwt", "config.toml"))
	require.NoError(t, err)
	text := string(data)
	assert.Contains(t, text, "template = \"{{.FullPath}}/{{.Branch}}\"")
	assert.Contains(t, text, "[agents]")
	assert.Contains(t, text, "codex = \"codex\"")
	assert.Contains(t, text, "claude = \"claude\"")
	assert.NotContains(t, text, "dangerously-bypass-approvals-and-sandbox")
	assert.NotContains(t, text, "dangerously-skip-permissions")
	assert.Contains(t, text, "[[layouts.presets]]")
	assert.Contains(t, text, "panes = [\"agent:codex\", \"agent:claude\", \"agent:roborev\", \"\"]")
}

func TestInitBackfillsWorkspaceConfigIntoExistingConfig(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte(`
[worktree]
basedir = "~/worktrees"
auto_mkdir = true
`), 0o600))

	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.Agents["codex"])
	assert.Equal(t, "claude", cfg.Agents["claude"])
	assert.Equal(t, "roborev tui", cfg.Agents["roborev"])
	assert.Empty(t, cfg.Layouts.Default, "migration must not write a default layout")
	assert.True(t, cfg.Layouts.AutoLaunchOnAdd, "auto_launch_on_add backfill stays")
	assert.Empty(t, cfg.Layouts.Presets, "migration must not seed presets")

	data, err := os.ReadFile(filepath.Join(kwtHome, "config.toml"))
	require.NoError(t, err)
	text := string(data)
	assert.Contains(t, text, "[agents]")
	assert.NotContains(t, text, "[[layouts.presets]]")
	assert.Contains(t, text, "codex")
	assert.NotContains(t, text, "dangerously-bypass-approvals-and-sandbox")
	assert.NotContains(t, text, "dangerously-skip-permissions")
}

func TestInitRepairsStringEncodedEmptyProjectRegistry(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte(`
projects = '[]'

[agents]
codex = "codex"
claude = "claude"
roborev = "roborev tui"

[layouts]
auto_launch_on_add = true

[naming]
template = "{{.FullPath}}/{{.Branch}}"

[custom]
sentinel = "preserved"
`), 0o600))

	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.Projects)
	stored, err := readGlobalViper(filepath.Join(kwtHome, "config.toml"))
	require.NoError(t, err)
	assert.IsType(t, []any{}, stored.Get("projects"))
	assert.Equal(t, "preserved", stored.GetString("custom.sentinel"))
	projects, err := rawProjectEntries(stored)
	require.NoError(t, err)
	assert.Empty(t, projects)
	migrated, err := backfillWorkspaceConfig(filepath.Join(kwtHome, "config.toml"))
	require.NoError(t, err)
	assert.False(t, migrated)
}

func TestInitDoesNotRepairOtherMalformedProjectRegistries(t *testing.T) {
	for _, value := range []string{"' [] '", "'other'", "['not-a-project']"} {
		t.Run(value, func(t *testing.T) {
			kwtHome := t.TempDir()
			t.Setenv("KWT_HOME", kwtHome)
			require.NoError(t, os.WriteFile(
				filepath.Join(kwtHome, "config.toml"),
				[]byte("projects = "+value+"\n"),
				0o600,
			))

			viper.Reset()
			t.Cleanup(viper.Reset)
			require.NoError(t, Init())

			_, err := Load()
			require.ErrorContains(t, err, "projects")
		})
	}
}

func TestInitLeavesExistingLayoutsUntouched(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte(`
[layouts]
default = "quad"
auto_launch_on_add = false

[[layouts.presets]]
name = "quad"
arrange = "tiled"
panes = [""]
`), 0o600))

	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "quad", cfg.Layouts.Default)
	assert.False(t, cfg.Layouts.AutoLaunchOnAdd)
	require.Len(t, cfg.Layouts.Presets, 1)
	assert.Equal(t, "quad", cfg.Layouts.Presets[0].Name)
}

func TestInitMigratesLegacyDefaultNamingTemplate(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte(`
[worktree]
basedir = "~/worktrees"
auto_mkdir = true

[naming]
template = "{{.Host}}/{{.Owner}}/{{.Repository}}/{{.Branch}}"
`), 0o600))

	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, DefaultNamingTemplate, cfg.Naming.Template)

	data, err := os.ReadFile(filepath.Join(kwtHome, "config.toml"))
	require.NoError(t, err)
	text := string(data)
	assert.Contains(t, text, DefaultNamingTemplate)
	assert.NotContains(t, text, legacyDefaultNamingTemplate)
}

func TestInitIsGlobalOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Isolate the global config dir (getConfigDir uses $HOME) so Init reads
	// defaults, not the developer's real ~/.config/kwt.
	t.Setenv("HOME", dir)
	localTOML := []byte("[layouts]\ndefault = \"from-cwd\"\n")
	localConfigPath := filepath.Join(dir, ".kwt.toml")
	require.NoError(t, os.WriteFile(localConfigPath, localTOML, 0o644))

	// Pre-trust the local config in the on-disk trust store that Init (via
	// mergeLocalConfig's nil-store lazy load) would consult. Without this,
	// go test's non-interactive stdin makes mergeLocalConfig skip any
	// untrusted file silently regardless of whether Init still merges, which
	// would make the assertion below pass vacuously. Pre-trusting removes
	// that false pass and forces the test to exercise the merge path itself.
	absPath, err := filepath.EvalSymlinks(localConfigPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(localTOML)))

	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.Layouts.Default, "Init must not merge cwd .kwt.toml")
}

func TestLoadRepoLayoutDefaultUntrustedSkipped(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".kwt.toml"),
		[]byte("[layouts]\ndefault = \"focus\"\n"), 0o644))

	// Non-interactive: untrusted config is skipped, returns "".
	got, err := LoadRepoLayoutDefault(repo, false)
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestLoadRepoLayoutDefaultMissingFile(t *testing.T) {
	got, err := LoadRepoLayoutDefault(t.TempDir(), false)
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// TestLoadRepoLayoutDefaultTrustedReturnsDefault exercises the trusted read
// path (trust check -> viper.New() parse -> layouts.default), which the
// skip-path tests above never reach. It isolates HOME/USERPROFILE so
// defaultTrustStorePath() resolves inside a throwaway directory, then
// pre-trusts the target repo's .kwt.toml the same way TestInitIsGlobalOnly
// and the "nil store: lazy-load from disk" case in TestMergeLocalConfigTrust
// do: write the file, resolve its canonical path, and persist a trust entry
// via TrustStore.Add before calling the function under test.
func TestLoadRepoLayoutDefaultTrustedReturnsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repo := t.TempDir()
	localTOML := []byte("[layouts]\ndefault = \"focus\"\n")
	localConfigPath := filepath.Join(repo, ".kwt.toml")
	require.NoError(t, os.WriteFile(localConfigPath, localTOML, 0o644))

	absPath, err := filepath.EvalSymlinks(localConfigPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(localTOML)))

	got, err := LoadRepoLayoutDefault(repo, false)
	require.NoError(t, err)
	assert.Equal(t, "focus", got)
}

func TestLoadForTargetMergesTrustedRepositorySettings(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())
	viper.Set("fleet.token_env", "GLOBAL_FLEET_TOKEN")

	target := t.TempDir()
	localPath := filepath.Join(target, ".kwt.toml")
	targetWorktrees := filepath.Join(target, "target-worktrees")
	local := []byte("[worktree]\nbasedir = '" + targetWorktrees + "'\n[fleet]\ntoken_env = 'ATTACKER_TOKEN'\n[daemon]\nidle_timeout = '-1s'\nauto_restart = 'sometimes'\nreplacement_grace = '0s'\n[ssh]\nidle_timeout = '-1s'\n[[repository_settings]]\nrepository = '" + target + "'\nsetup_commands = ['echo trusted']\n")
	require.NoError(t, os.WriteFile(localPath, local, 0o600))
	absPath, err := normalizeConfigPath(localPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(local)))

	cfg, err := LoadForTarget(target, false)

	require.NoError(t, err)
	assert.Equal(t, targetWorktrees, cfg.Worktree.BaseDir)
	assert.Equal(t, "GLOBAL_FLEET_TOKEN", cfg.Fleet.TokenEnv)
	assert.Equal(t, 2*time.Hour, cfg.Daemon.IdleTimeout)
	assert.Equal(t, "newer", cfg.Daemon.AutoRestart)
	assert.Equal(t, 5*time.Minute, cfg.Daemon.ReplacementGrace)
	assert.Equal(t, time.Hour, cfg.SSH.IdleTimeout)
	require.Len(t, cfg.RepositorySettings, 1)
	assert.Equal(t, []string{"echo trusted"}, cfg.RepositorySettings[0].SetupCommands)
}

func TestLoadForTargetFromMergesLocalConfigOverProvidedSnapshot(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	target := t.TempDir()
	localPath := filepath.Join(target, ".kwt.toml")
	targetWorktrees := filepath.Join(target, "target-worktrees")
	local := []byte("[worktree]\nbasedir = '" + targetWorktrees + "'\n[fleet]\ntoken_env = 'ATTACKER_TOKEN'\n[[repository_settings]]\nrepository = '" + target + "'\nsetup_commands = ['echo trusted']\n")
	require.NoError(t, os.WriteFile(localPath, local, 0o600))
	absPath, err := normalizeConfigPath(localPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(local)))
	provided := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "refreshed-worktrees"), AutoMkdir: true},
		Fleet:    models.FleetConfig{TokenEnv: "REFRESHED_FLEET_TOKEN"},
		Naming: models.NamingConfig{
			Template:      "{{.Branch}}",
			SanitizeChars: map[string]string{"/": "-"},
		},
	}

	cfg, err := LoadForTargetFrom(provided, target, false)

	require.NoError(t, err)
	assert.Equal(t, targetWorktrees, cfg.Worktree.BaseDir)
	assert.True(t, cfg.Worktree.AutoMkdir)
	assert.Equal(t, "REFRESHED_FLEET_TOKEN", cfg.Fleet.TokenEnv)
	assert.Equal(t, "{{.Branch}}", cfg.Naming.Template)
	require.Len(t, cfg.RepositorySettings, 1)
	assert.Equal(t, []string{"echo trusted"}, cfg.RepositorySettings[0].SetupCommands)
}

func TestLoadForTargetResolvesRelativePathsAgainstSelectedRepository(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	target := t.TempDir()
	resolvedTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	localPath := filepath.Join(target, ".kwt.toml")
	local := []byte("[worktree]\nbasedir = 'target-worktrees'\n[[repository_settings]]\nrepository = '.'\nbasedir = 'repo-worktrees'\nsetup_commands = ['echo trusted']\n")
	require.NoError(t, os.WriteFile(localPath, local, 0o600))
	absPath, err := normalizeConfigPath(localPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(local)))
	caller := t.TempDir()
	changeDir(t, caller)

	cfg, err := LoadForTarget(target, false)

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(resolvedTarget, "target-worktrees"), cfg.Worktree.BaseDir)
	require.Len(t, cfg.RepositorySettings, 1)
	assert.Equal(t, resolvedTarget, cfg.RepositorySettings[0].Repository)
	assert.Equal(t, filepath.Join(resolvedTarget, "repo-worktrees"), cfg.RepositorySettings[0].BaseDir)
}

func TestLoadForTargetRejectsEmptyRepositoryLocalBaseDir(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())
	viper.Set("worktree.basedir", t.TempDir())

	target := t.TempDir()
	localPath := filepath.Join(target, ".kwt.toml")
	local := []byte("[worktree]\nbasedir = ''\n")
	require.NoError(t, os.WriteFile(localPath, local, 0o600))
	absPath, err := normalizeConfigPath(localPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(local)))

	_, err = LoadForTarget(target, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree base directory must not be empty")
}

func TestLoadForTargetPreservesRepositoryGlobSelectors(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	target := filepath.Join(t.TempDir(), "acme", "widget")
	require.NoError(t, os.MkdirAll(target, 0o755))
	localPath := filepath.Join(target, ".kwt.toml")
	local := []byte("[[repository_settings]]\nrepository = '**/acme/widget'\nsetup_commands = ['echo trusted']\n")
	require.NoError(t, os.WriteFile(localPath, local, 0o600))
	absPath, err := normalizeConfigPath(localPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(local)))

	cfg, err := LoadForTarget(target, false)

	require.NoError(t, err)
	require.Len(t, cfg.RepositorySettings, 1)
	assert.Equal(t, "**/acme/widget", cfg.RepositorySettings[0].Repository)
	assert.True(t, utils.MatchPath(cfg.RepositorySettings[0].Repository, utils.CanonicalPath(target)))
	assert.Equal(t, []string{"echo trusted"}, cfg.RepositorySettings[0].SetupCommands)
}

func TestLoadForTargetRejectsEnvironmentReferencesInRepositoryLocalPaths(t *testing.T) {
	secret := "credential-must-not-appear-in-a-workspace-path"
	for _, tc := range []struct {
		name  string
		local string
	}{
		{name: "worktree base directory", local: "[worktree]\nbasedir = '$KWT_GITHUB_TOKEN'\n"},
		{name: "naming template", local: "[naming]\ntemplate = '$KWT_GITHUB_TOKEN/{{.Branch}}'\n"},
		{name: "naming replacement", local: "[naming.sanitize_chars]\n'/' = '$KWT_GITHUB_TOKEN'\n"},
		{name: "repository selector", local: "[[repository_settings]]\nrepository = '$KWT_GITHUB_TOKEN'\nsetup_commands = ['echo trusted']\n"},
		{name: "repository base directory", local: "[[repository_settings]]\nrepository = '.'\nbasedir = '${KWT_GITHUB_TOKEN}'\nsetup_commands = ['echo trusted']\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kwtHome := t.TempDir()
			t.Setenv("KWT_HOME", kwtHome)
			t.Setenv("KWT_GITHUB_TOKEN", secret)
			viper.Reset()
			t.Cleanup(viper.Reset)
			require.NoError(t, Init())

			target := t.TempDir()
			localPath := filepath.Join(target, ".kwt.toml")
			local := []byte(tc.local)
			require.NoError(t, os.WriteFile(localPath, local, 0o600))
			absPath, err := normalizeConfigPath(localPath)
			require.NoError(t, err)
			store := &TrustStore{path: defaultTrustStorePath()}
			require.NoError(t, store.Add(absPath, computeSHA256(local)))

			_, err = LoadForTarget(target, false)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "environment variable")
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestLoadForTargetMarksRepositoryLocalNaming(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	target := t.TempDir()
	localPath := filepath.Join(target, ".kwt.toml")
	local := []byte(`[naming]
template = '{{printf "%c%s" 36 "KWT_GITHUB_TOKEN"}}/{{.Branch}}'
`)
	require.NoError(t, os.WriteFile(localPath, local, 0o600))
	absPath, err := normalizeConfigPath(localPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(local)))

	cfg, err := LoadForTarget(target, false)

	require.NoError(t, err)
	assert.True(t, cfg.Naming.TemplateRepositoryLocal)
	assert.False(t, cfg.Naming.SanitizeCharsRepositoryLocal)
}

func TestLoadForTargetMergesEquivalentGlobalAndLocalRepositoryPaths(t *testing.T) {
	kwtHome := t.TempDir()
	home := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())

	target := filepath.Join(home, "src", "widget")
	require.NoError(t, os.MkdirAll(target, 0o755))
	viper.Set("repository_settings", []models.RepositorySetting{{
		Repository: "~/src/widget", SetupCommands: []string{"echo global"},
	}})
	localPath := filepath.Join(target, ".kwt.toml")
	local := []byte("[[repository_settings]]\nrepository = '.'\nsetup_commands = ['echo local']\n")
	require.NoError(t, os.WriteFile(localPath, local, 0o600))
	absPath, err := normalizeConfigPath(localPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(absPath, computeSHA256(local)))

	cfg, err := LoadForTarget(target, false)

	require.NoError(t, err)
	require.Len(t, cfg.RepositorySettings, 1)
	assert.Equal(t, utils.CanonicalPath(target), cfg.RepositorySettings[0].Repository)
	assert.Equal(t, []string{"echo local"}, cfg.RepositorySettings[0].SetupCommands)
}

func TestLoadForTargetSkipsUntrustedConfigurationNoninteractively(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())
	globalBase := filepath.Join(t.TempDir(), "global-worktrees")
	viper.Set("worktree.basedir", globalBase)

	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, ".kwt.toml"), []byte(
		"[worktree]\nbasedir = '/attacker/worktrees'\n[[repository_settings]]\nrepository = '"+target+"'\nsetup_commands = ['echo untrusted']\n"), 0o600))

	cfg, err := LoadForTarget(target, false)

	require.NoError(t, err)
	assert.Equal(t, globalBase, cfg.Worktree.BaseDir)
	assert.Empty(t, cfg.RepositorySettings)
}

func TestLoadForTargetRejectsSymlinkToTrustedConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, Init())
	globalBase := filepath.Join(t.TempDir(), "global-worktrees")
	viper.Set("worktree.basedir", globalBase)

	trustedRepo := t.TempDir()
	trustedPath := filepath.Join(trustedRepo, ".kwt.toml")
	trustedConfig := []byte("[worktree]\nbasedir = '/attacker/worktrees'\n[[repository_settings]]\nrepository = '.'\nsetup_commands = ['echo trusted-elsewhere']\n")
	require.NoError(t, os.WriteFile(trustedPath, trustedConfig, 0o600))
	canonicalTrustedPath, err := normalizeConfigPath(trustedPath)
	require.NoError(t, err)
	store := &TrustStore{path: defaultTrustStorePath()}
	require.NoError(t, store.Add(canonicalTrustedPath, computeSHA256(trustedConfig)))

	target := t.TempDir()
	require.NoError(t, os.Symlink(trustedPath, filepath.Join(target, ".kwt.toml")))

	cfg, err := LoadForTarget(target, false)

	require.NoError(t, err)
	assert.Equal(t, globalBase, cfg.Worktree.BaseDir)
	assert.Empty(t, cfg.RepositorySettings)
}

func TestLoadExpandsWorkspacePaths(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() { viper.Reset() })

	home := t.TempDir()
	// Resolve symlinks for consistent paths on macOS where /var -> /private/var
	home, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "notes")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	viper.SetConfigType("toml")
	viper.Set("workspaces", []map[string]any{{"name": "notes", "path": "~/notes"}})

	cfg, err := Load()

	require.NoError(t, err)
	require.Len(t, cfg.Workspaces, 1)
	assert.Equal(t, "notes", cfg.Workspaces[0].Name)
	assert.Equal(t, dir, cfg.Workspaces[0].Path)
}

func TestLoadDaemonDefaults(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	applyGlobalDefaults(viper.GetViper())

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 2*time.Hour, cfg.Daemon.IdleTimeout)
	assert.Equal(t, "newer", cfg.Daemon.AutoRestart)
	assert.Equal(t, 5*time.Minute, cfg.Daemon.ReplacementGrace)
	assert.Equal(t, time.Hour, cfg.SSH.IdleTimeout)
}

func TestLoadSSHIdleTimeout(t *testing.T) {
	for name, value := range map[string]time.Duration{
		"immediate teardown": 0,
		"extended lifetime":  3 * time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			applyGlobalDefaults(viper.GetViper())
			viper.Set("ssh.idle_timeout", value)

			cfg, err := Load()

			require.NoError(t, err)
			assert.Equal(t, value, cfg.SSH.IdleTimeout)
		})
	}
}

func TestLoadRejectsInvalidDaemonConfig(t *testing.T) {
	for name, body := range map[string]string{
		"negative idle":     "[daemon]\nidle_timeout = \"-1s\"",
		"unknown policy":    "[daemon]\nauto_restart = \"sometimes\"",
		"nonpositive grace": "[daemon]\nreplacement_grace = \"0s\"",
		"negative SSH idle": "[ssh]\nidle_timeout = \"-1s\"",
	} {
		t.Run(name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			viper.SetConfigType("toml")
			applyGlobalDefaults(viper.GetViper())
			require.NoError(t, viper.ReadConfig(strings.NewReader(body)))
			_, err := Load()
			require.Error(t, err)
		})
	}
}

func TestCanonicalHomeResolvesRelativeAndSymlinkedKwtHome(t *testing.T) {
	realHome := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "kwt-home")
	require.NoError(t, os.Symlink(realHome, link))
	changeDir(t, parent)
	t.Setenv("KWT_HOME", "kwt-home")

	home, err := CanonicalHome()
	require.NoError(t, err)
	canonicalRealHome, err := filepath.EvalSymlinks(realHome)
	require.NoError(t, err)
	assert.Equal(t, canonicalRealHome, home)
}
