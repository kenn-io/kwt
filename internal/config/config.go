// Package config provides configuration management for the kwt application.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	worktreetemplate "go.kenn.io/kwt/internal/template"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

const (
	configName      = "config"
	configType      = "toml"
	localConfigName = ".kwt"

	cwdLocalNamingTemplateMarker = "kwt.internal.cwd_local_naming_template"
	cwdLocalNamingSanitizeMarker = "kwt.internal.cwd_local_naming_sanitize"

	// DefaultNamingTemplate is the starter naming template used for new global configs.
	DefaultNamingTemplate = "{{.FullPath}}/{{.Branch}}"

	legacyDefaultNamingTemplate = "{{.Host}}/{{.Owner}}/{{.Repository}}/{{.Branch}}"
)

func defaultAgents() map[string]string {
	return map[string]string{
		"codex":   "codex",
		"claude":  "claude",
		"roborev": "roborev tui",
	}
}

func applyGlobalDefaults(target *viper.Viper) {
	target.SetDefault("daemon.idle_timeout", 2*time.Hour)
	target.SetDefault("daemon.auto_restart", "newer")
	target.SetDefault("daemon.replacement_grace", 5*time.Minute)
	target.SetDefault("cd.launch_shell", true)
	target.SetDefault("worktree.basedir", "~/.kwt/worktrees")
	target.SetDefault("worktree.auto_mkdir", true)
	target.SetDefault("finder.preview", true)
	target.SetDefault("ui.icons", true)
	target.SetDefault("ui.tilde_home", true)
	target.SetDefault("naming.template", DefaultNamingTemplate)
	target.SetDefault("naming.sanitize_chars", map[string]string{
		"/": "-",
		":": "-",
	})
}

// CanonicalHome returns the absolute, symlink-resolved kwt home. Init must
// create the directory before callers canonicalize it.
func CanonicalHome() (string, error) {
	abs, err := filepath.Abs(getConfigDir())
	if err != nil {
		return "", fmt.Errorf("resolve kwt home: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize kwt home: %w", err)
	}
	return filepath.Clean(resolved), nil
}

// getConfigDir returns the configuration directory path.
func getConfigDir() string {
	if kwtHome := os.Getenv("KWT_HOME"); kwtHome != "" {
		if expanded, err := utils.ExpandPath(kwtHome); err == nil {
			return expanded
		}
		return kwtHome
	}

	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback to current directory if home is not available
		return filepath.Join(".", ".config", "kwt")
	}
	return filepath.Join(home, ".config", "kwt")
}

// getLocalConfigPath returns the path to the local config file if it exists.
// Returns empty string if no local config is found.
func getLocalConfigPath() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	localConfigPath := filepath.Join(cwd, localConfigName+"."+configType)
	if _, err := os.Stat(localConfigPath); os.IsNotExist(err) {
		return ""
	}

	return localConfigPath
}

// mergeLocalConfig reads the local `.kwt.toml` from CWD and merges it into global viper.
// Local config is treated as untrusted: it is only merged after the trust store confirms
// (absolute path, sha256) match, or the user explicitly grants trust via the prompter.
//
//   - Non-regular files (directory / FIFO / socket / device) are skipped with a stderr warning.
//   - If interactive is false (non-TTY), unknown files are skipped with a stderr warning.
//   - On user rejection, the file is skipped (command continues, global config only).
//   - Trust store write failures are non-fatal (merge proceeds with a stderr warning).
//   - fleet.* and daemon.* keys are always ignored because they control
//     machine-level credentials, endpoints, and process authority.
//
// For repository_settings, merging is done by the `repository` field as the key.
func mergeLocalConfig(store *TrustStore, prompter trustPrompter, interactive bool) error {
	viper.Set(cwdLocalNamingTemplateMarker, false)
	viper.Set(cwdLocalNamingSanitizeMarker, false)
	rawPath := getLocalConfigPath()
	if rawPath == "" {
		return nil
	}

	absPath, err := normalizeConfigPath(rawPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kwt: skipping local config: %v\n", err)
		return nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("read local config %s: %w", absPath, err)
	}
	sum := computeSHA256(data)

	if store == nil {
		store, err = LoadTrustStore(defaultTrustStorePath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "kwt: failed to load trust store (continuing empty): %v\n", err)
			store = &TrustStore{path: defaultTrustStorePath()}
		}
	}

	if !store.IsTrusted(absPath, sum) {
		if !interactive {
			fmt.Fprintf(os.Stderr, "kwt: skipping untrusted local config %s (non-interactive session)\n", absPath)
			return nil
		}
		granted, err := prompter.PromptTrust(absPath, data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kwt: trust prompt failed, skipping local config: %v\n", err)
			return nil
		}
		if !granted {
			fmt.Fprintf(os.Stderr, "kwt: local config %s not trusted, skipping\n", absPath)
			return nil
		}
		if err := store.Add(absPath, sum); err != nil {
			fmt.Fprintf(os.Stderr, "kwt: failed to persist trust decision (continuing): %v\n", err)
		}
	}

	localViper := viper.New()
	localViper.SetConfigType(configType)
	if err := localViper.ReadConfig(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("parse local config %s: %w", absPath, err)
	}
	if err := resolveTargetLocalPaths(
		localViper,
		filepath.Dir(absPath),
	); err != nil {
		return fmt.Errorf("resolve local config paths %s: %w", absPath, err)
	}
	viper.Set(
		cwdLocalNamingTemplateMarker,
		localViper.IsSet("naming.template"),
	)
	viper.Set(
		cwdLocalNamingSanitizeMarker,
		localViper.IsSet("naming.sanitize_chars"),
	)

	for _, key := range localViper.AllKeys() {
		switch {
		case key == "repository_settings":
			mergeRepositorySettings(localViper)
		case key == "projects" || key == "workspaces" || strings.HasPrefix(key, "workspaces."):
			continue
		case key == "fleet" || strings.HasPrefix(key, "fleet."):
			// Sync settings decide where bearer tokens are read from and
			// which hub they are sent to; accepting them from a repo-local
			// file would let a repository exfiltrate arbitrary secrets.
			fmt.Fprintf(os.Stderr, "kwt: ignoring %q in %s: sync settings are global-only\n", key, absPath)
		case key == "daemon" || strings.HasPrefix(key, "daemon."):
			fmt.Fprintf(os.Stderr, "kwt: ignoring %q in %s: daemon settings are global-only\n", key, absPath)
		default:
			viper.Set(key, localViper.Get(key))
		}
	}

	return nil
}

// mergeRepositorySettings merges repository_settings from local config into global config.
// The "repository" field is used as the key for merging:
// - Same repository: local overrides global
// - Different repository: both are kept
func mergeRepositorySettings(localViper *viper.Viper) {
	mergeRepositorySettingsInto(viper.GetViper(), localViper)
}

func mergeRepositorySettingsInto(targetViper, localViper *viper.Viper) {
	var globalSettings, localSettings []models.RepositorySetting

	if err := targetViper.UnmarshalKey("repository_settings", &globalSettings); err != nil {
		globalSettings = nil
	}

	if err := localViper.UnmarshalKey("repository_settings", &localSettings); err != nil {
		return
	}

	localMap := make(map[string]models.RepositorySetting, len(localSettings))
	for _, ls := range localSettings {
		localMap[repositorySettingMergeKey(ls.Repository)] = ls
	}

	merged := make([]models.RepositorySetting, 0, len(globalSettings)+len(localSettings))
	overridden := make(map[string]bool, len(localSettings))

	for _, gs := range globalSettings {
		key := repositorySettingMergeKey(gs.Repository)
		if ls, exists := localMap[key]; exists {
			if !overridden[key] {
				merged = append(merged, ls)
				overridden[key] = true
			}
		} else {
			merged = append(merged, gs)
		}
	}

	for _, ls := range localSettings {
		key := repositorySettingMergeKey(ls.Repository)
		if !overridden[key] {
			merged = append(merged, ls)
			overridden[key] = true
		}
	}

	targetViper.Set("repository_settings", merged)
}

func repositorySettingMergeKey(repository string) string {
	repository = strings.TrimSpace(repository)
	if repository == "" || strings.ContainsAny(repository, "*?[") {
		return repository
	}
	expanded, err := utils.ExpandPath(repository)
	if err != nil {
		return filepath.Clean(repository)
	}
	return utils.CanonicalPath(expanded)
}

// Init initializes the configuration system, creating default config if needed.
func Init() error {
	configDir := getConfigDir()
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	configPath := filepath.Join(configDir, configName+"."+configType)

	viper.SetConfigName(configName)
	viper.SetConfigType(configType)
	viper.AddConfigPath(configDir)

	applyGlobalDefaults(viper.GetViper())

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			if _, err := (globalConfigStore{path: configPath}).ensure(defaultConfigTOML()); err != nil {
				return fmt.Errorf("failed to create config file: %w", err)
			}
			if err := viper.ReadInConfig(); err != nil {
				return fmt.Errorf("failed to read created config: %w", err)
			}
		} else {
			return fmt.Errorf("failed to read config: %w", err)
		}
	}

	migrated, err := backfillWorkspaceConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to migrate workspace config: %w", err)
	}
	if migrated {
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("failed to read migrated config: %w", err)
		}
	}

	return nil
}

func backfillWorkspaceConfig(configPath string) (bool, error) {
	return (globalConfigStore{path: configPath}).mutate(
		func(globalViper *viper.Viper) (bool, error) {
			migrated := false
			if projects, ok := globalViper.Get("projects").(string); ok && projects == "[]" {
				globalViper.Set("projects", []map[string]any{})
				migrated = true
			}
			agents := globalViper.GetStringMapString("agents")
			if agents == nil {
				agents = make(map[string]string)
			}
			for name, command := range defaultAgents() {
				if strings.TrimSpace(agents[name]) == "" {
					agents[name] = command
					migrated = true
				}
			}
			if migrated {
				globalViper.Set("agents", agents)
			}

			if strings.TrimSpace(globalViper.GetString("naming.template")) == legacyDefaultNamingTemplate {
				globalViper.Set("naming.template", DefaultNamingTemplate)
				migrated = true
			}

			if !globalViper.IsSet("layouts.auto_launch_on_add") {
				globalViper.Set("layouts.auto_launch_on_add", true)
				migrated = true
			}
			return migrated, nil
		},
	)
}

func defaultConfigTOML() string {
	return fmt.Sprintf(`[daemon]
idle_timeout = "2h"
auto_restart = "newer"
replacement_grace = "5m"

[worktree]
basedir = "~/.kwt/worktrees"
auto_mkdir = true

[cd]
launch_shell = true

[finder]
preview = true

[ui]
icons = true
tilde_home = true

[naming]
template = "%s"

[naming.sanitize_chars]
"/" = "-"
":" = "-"

[agents]
codex = "codex"
claude = "claude"
roborev = "roborev tui"

[layouts]
# Unset (or "none") launches a blank single-pane session.
# Set to a preset name below to launch that layout by default.
# default = "quad"
auto_launch_on_add = true

[[layouts.presets]]
name = "quad"
arrange = "even-horizontal"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]

[[layouts.presets]]
name = "grid"
arrange = "tiled"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]

[[layouts.presets]]
name = "focus"
arrange = "main-vertical"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]

[[layouts.presets]]
name = "stack"
arrange = "even-vertical"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]
`, DefaultNamingTemplate)
}

// MergeCwdLocal merges the trust-gated cwd `.kwt.toml` into the global config.
// It is called explicitly by commands that operate on the current repository
// (via the root PersistentPreRunE); global/cross-project commands such as
// `open` skip it so a caller's cwd config never leaks into another worktree.
func MergeCwdLocal() error {
	if err := mergeLocalConfig(nil, newStdioPrompter(), isStdinInteractive()); err != nil {
		return fmt.Errorf("failed to merge local config: %w", err)
	}
	return nil
}

// LoadRepoLayoutDefault reads <repoRoot>/.kwt.toml (trust-gated, in isolation)
// and returns its layouts.default. Returns "" when the file is absent,
// untrusted in a non-interactive session, declined, or has no default. It does
// not touch the global viper singleton, so it is safe for cross-project use by
// `open`.
func LoadRepoLayoutDefault(repoRoot string, interactive bool) (string, error) {
	path := filepath.Join(repoRoot, localConfigName+"."+configType)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	absPath, err := normalizeConfigPath(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kwt: skipping target config: %v\n", err)
		return "", nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read target config %s: %w", absPath, err)
	}
	sum := computeSHA256(data)

	store, err := LoadTrustStore(defaultTrustStorePath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "kwt: failed to load trust store (continuing empty): %v\n", err)
		store = &TrustStore{path: defaultTrustStorePath()}
	}
	if !store.IsTrusted(absPath, sum) {
		if !interactive {
			fmt.Fprintf(os.Stderr, "kwt: skipping untrusted target config %s (non-interactive)\n", absPath)
			return "", nil
		}
		granted, err := newStdioPrompter().PromptTrust(absPath, data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kwt: trust prompt failed, skipping target config: %v\n", err)
			return "", nil
		}
		if !granted {
			return "", nil
		}
		if err := store.Add(absPath, sum); err != nil {
			fmt.Fprintf(os.Stderr, "kwt: failed to persist trust decision (continuing): %v\n", err)
		}
	}

	lv := viper.New()
	lv.SetConfigType(configType)
	if err := lv.ReadConfig(bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("parse target config %s: %w", absPath, err)
	}
	return lv.GetString("layouts.default"), nil
}

// LoadForTarget returns global configuration merged with the selected
// repository's trust-gated local configuration. It never consults the
// caller's working directory and does not prompt when interactive is false.
func LoadForTarget(repoRoot string, interactive bool) (*models.Config, error) {
	return loadForTarget(viper.AllSettings(), repoRoot, interactive)
}

// LoadForTargetFrom returns the provided effective configuration merged with
// the selected repository's trust-gated local configuration. Long-lived
// clients use it after refreshing global configuration outside the process-
// global Viper instance.
func LoadForTargetFrom(
	base *models.Config,
	repoRoot string,
	interactive bool,
) (*models.Config, error) {
	if base == nil {
		return nil, fmt.Errorf("base configuration is required")
	}
	settings := make(map[string]any)
	if err := mapstructure.Decode(base, &settings); err != nil {
		return nil, fmt.Errorf("encode base config: %w", err)
	}
	return loadForTarget(settings, repoRoot, interactive)
}

func loadForTarget(
	settings map[string]any,
	repoRoot string,
	interactive bool,
) (*models.Config, error) {
	target := viper.New()
	if err := target.MergeConfigMap(settings); err != nil {
		return nil, fmt.Errorf("copy global config: %w", err)
	}
	repositoryLocalTemplate := false
	repositoryLocalSanitizeChars := false

	path := filepath.Join(repoRoot, localConfigName+"."+configType)
	if _, err := os.Lstat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat target config %s: %w", path, err)
		}
	} else {
		absPath, normalizeErr := normalizeTargetConfigPath(path)
		if normalizeErr != nil {
			fmt.Fprintf(os.Stderr, "kwt: skipping target config: %v\n", normalizeErr)
		} else {
			data, readErr := os.ReadFile(absPath)
			if readErr != nil {
				return nil, fmt.Errorf("read target config %s: %w", absPath, readErr)
			}
			store, storeErr := LoadTrustStore(defaultTrustStorePath())
			if storeErr != nil {
				fmt.Fprintf(os.Stderr, "kwt: failed to load trust store (continuing empty): %v\n", storeErr)
				store = &TrustStore{path: defaultTrustStorePath()}
			}
			trusted := store.IsTrusted(absPath, computeSHA256(data))
			if !trusted && interactive {
				trusted, storeErr = newStdioPrompter().PromptTrust(absPath, data)
				if storeErr != nil {
					fmt.Fprintf(os.Stderr, "kwt: trust prompt failed, skipping target config: %v\n", storeErr)
					trusted = false
				} else if trusted {
					if storeErr = store.Add(absPath, computeSHA256(data)); storeErr != nil {
						fmt.Fprintf(os.Stderr, "kwt: failed to persist trust decision (continuing): %v\n", storeErr)
					}
				}
			}
			if !trusted {
				fmt.Fprintf(os.Stderr, "kwt: skipping untrusted target config %s (non-interactive)\n", absPath)
			} else {
				local := viper.New()
				local.SetConfigType(configType)
				if parseErr := local.ReadConfig(bytes.NewReader(data)); parseErr != nil {
					return nil, fmt.Errorf("parse target config %s: %w", absPath, parseErr)
				}
				if pathErr := resolveTargetLocalPaths(local, repoRoot); pathErr != nil {
					return nil, fmt.Errorf("resolve target config paths %s: %w", absPath, pathErr)
				}
				repositoryLocalTemplate = local.IsSet("naming.template")
				repositoryLocalSanitizeChars = local.IsSet(
					"naming.sanitize_chars",
				)
				for _, key := range local.AllKeys() {
					switch {
					case key == "repository_settings":
						mergeRepositorySettingsInto(target, local)
					case key == "projects" || key == "workspaces" || strings.HasPrefix(key, "workspaces."),
						key == "fleet" || strings.HasPrefix(key, "fleet."),
						key == "daemon" || strings.HasPrefix(key, "daemon."):
						continue
					default:
						target.Set(key, local.Get(key))
					}
				}
			}
		}
	}

	var cfg models.Config
	if err := target.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal target config: %w", err)
	}
	if err := expandConfigPaths(&cfg); err != nil {
		return nil, err
	}
	cfg.Naming.TemplateRepositoryLocal = repositoryLocalTemplate
	cfg.Naming.SanitizeCharsRepositoryLocal =
		repositoryLocalSanitizeChars
	return &cfg, nil
}

// normalizeTargetConfigPath returns a canonical lexical path for a target
// repository's config while rejecting a final-component symlink. Keeping the
// lexical filename, rather than resolving it to another repository's config,
// prevents one repository from reusing another repository's trust entry.
func normalizeTargetConfigPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("abs path %s: %w", path, err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve parent symlinks %s: %w", abs, err)
	}
	lexicalPath := filepath.Join(parent, filepath.Base(abs))
	info, err := os.Lstat(lexicalPath)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", lexicalPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s is a symlink", errUnsafeRepositoryConfig, lexicalPath)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf(
			"%w: %s is not a regular file (mode %s)",
			errUnsafeRepositoryConfig,
			lexicalPath,
			info.Mode(),
		)
	}
	return lexicalPath, nil
}

func resolveTargetLocalPaths(local *viper.Viper, repoRoot string) error {
	repoRoot = utils.CanonicalPath(repoRoot)
	if local.IsSet("naming.template") {
		hasEnvironmentReference, err :=
			worktreetemplate.LiteralTextHasEnvironmentReference(
				local.GetString("naming.template"),
			)
		if err != nil {
			return err
		}
		if hasEnvironmentReference {
			return fmt.Errorf(
				"environment variable references are not allowed in repository-local naming templates",
			)
		}
	}
	for _, replacement := range local.GetStringMapString(
		"naming.sanitize_chars",
	) {
		if targetPathHasEnvironmentReference(replacement) {
			return fmt.Errorf(
				"environment variable references are not allowed in repository-local naming replacements",
			)
		}
	}
	if local.IsSet("worktree.basedir") {
		baseDir := local.GetString("worktree.basedir")
		if strings.TrimSpace(baseDir) == "" {
			return fmt.Errorf("repository-local worktree base directory must not be empty")
		}
		resolved, err := resolveTargetRelativePath(repoRoot, baseDir)
		if err != nil {
			return fmt.Errorf("resolve target worktree base directory: %w", err)
		}
		local.Set("worktree.basedir", resolved)
	}

	var settings []models.RepositorySetting
	if err := local.UnmarshalKey("repository_settings", &settings); err != nil {
		return err
	}
	for i := range settings {
		resolvedRepository, err := resolveTargetRepositorySelector(repoRoot, settings[i].Repository)
		if err != nil {
			return fmt.Errorf("resolve target repository setting %d repository: %w", i, err)
		}
		resolvedBaseDir, err := resolveTargetRelativePath(repoRoot, settings[i].BaseDir)
		if err != nil {
			return fmt.Errorf("resolve target repository setting %d base directory: %w", i, err)
		}
		settings[i].Repository = resolvedRepository
		settings[i].BaseDir = resolvedBaseDir
	}
	if len(settings) > 0 {
		local.Set("repository_settings", settings)
	}
	return nil
}

func resolveTargetRepositorySelector(repoRoot, value string) (string, error) {
	value = strings.TrimSpace(value)
	if targetPathHasEnvironmentReference(value) {
		return "", fmt.Errorf("environment variable references are not allowed in repository-local paths")
	}
	if strings.ContainsAny(value, "*?[") {
		return value, nil
	}
	return resolveTargetRelativePath(repoRoot, value)
}

func resolveTargetRelativePath(repoRoot, value string) (string, error) {
	value = strings.TrimSpace(value)
	if targetPathHasEnvironmentReference(value) {
		return "", fmt.Errorf("environment variable references are not allowed in repository-local paths")
	}
	if value == "" || value == "~" || strings.HasPrefix(value, "~/") {
		return value, nil
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	return filepath.Join(repoRoot, value), nil
}

func targetPathHasEnvironmentReference(value string) bool {
	found := false
	_ = os.Expand(value, func(string) string {
		found = true
		return ""
	})
	return found
}

// StdinInteractive reports whether stdin is a terminal (exported for callers
// that gate interactive trust prompts).
func StdinInteractive() bool { return isStdinInteractive() }

// Load loads and returns the current configuration.
func Load() (*models.Config, error) {
	applyGlobalDefaults(viper.GetViper())
	var cfg models.Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	if err := validateDaemonConfig(cfg.Daemon); err != nil {
		return nil, err
	}

	if err := expandConfigPaths(&cfg); err != nil {
		return nil, err
	}
	cfg.Naming.TemplateRepositoryLocal = viper.GetBool(
		cwdLocalNamingTemplateMarker,
	)
	cfg.Naming.SanitizeCharsRepositoryLocal = viper.GetBool(
		cwdLocalNamingSanitizeMarker,
	)
	return &cfg, nil
}

func validateDaemonConfig(cfg models.DaemonConfig) error {
	if cfg.IdleTimeout < 0 {
		return fmt.Errorf("daemon idle_timeout must not be negative")
	}
	if cfg.ReplacementGrace <= 0 {
		return fmt.Errorf("daemon replacement_grace must be positive")
	}
	switch cfg.AutoRestart {
	case "newer", "never":
		return nil
	default:
		return fmt.Errorf("daemon auto_restart must be newer or never")
	}
}

// ProjectRegistration keeps the raw project value persisted in global TOML
// alongside the independently expanded value used for filesystem inspection.
type ProjectRegistration struct {
	Persisted models.Project
	Effective models.Project
	raw       map[string]any
}

// SamePersistedEntry compares the complete raw project values used as CAS
// tokens. Manually constructed registrations fall back to the known model.
func (p ProjectRegistration) SamePersistedEntry(other ProjectRegistration) bool {
	if p.raw == nil && other.raw == nil {
		return p.Persisted == other.Persisted
	}
	return sameProjectRegistrationRaw(p.raw, other.raw)
}

// GlobalSnapshot is a fresh global-only configuration view. Projects are
// paired by their stable order in the single TOML snapshot.
type GlobalSnapshot struct {
	Config   *models.Config
	Projects []ProjectRegistration
}

// LoadGlobalSnapshot re-reads global configuration without merging a
// repository-local .kwt.toml file.
func LoadGlobalSnapshot() (*GlobalSnapshot, error) {
	return LoadGlobalSnapshotAt(getConfigDir())
}

// LoadGlobalSnapshotAt re-reads global configuration from an explicit kwt
// home. Daemon requests use it to avoid process-global environment state.
func LoadGlobalSnapshotAt(home string) (*GlobalSnapshot, error) {
	return LoadGlobalSnapshotAtWithExpansion(home, utils.ExpandPath)
}

// LoadGlobalSnapshotAtWithExpansion re-reads global configuration while using
// the invoking client's path semantics instead of process-global daemon state.
func LoadGlobalSnapshotAtWithExpansion(
	home string,
	expandPath func(string) (string, error),
) (*GlobalSnapshot, error) {
	configPath := filepath.Join(home, configName+"."+configType)
	globalViper, err := readGlobalViper(configPath)
	if err != nil {
		return nil, err
	}
	applyGlobalDefaults(globalViper)

	var persisted models.Config
	if err := globalViper.Unmarshal(&persisted); err != nil {
		return nil, fmt.Errorf("failed to unmarshal global config: %w", err)
	}
	if err := validateDaemonConfig(persisted.Daemon); err != nil {
		return nil, err
	}
	effective := persisted
	effective.Projects = slices.Clone(persisted.Projects)
	effective.Workspaces = slices.Clone(persisted.Workspaces)
	effective.RepositorySettings = slices.Clone(persisted.RepositorySettings)
	if err := expandConfigPathsWith(&effective, expandPath); err != nil {
		return nil, err
	}

	projects := make([]ProjectRegistration, len(persisted.Projects))
	rawProjects, err := rawProjectEntries(globalViper)
	if err != nil {
		return nil, err
	}
	if len(rawProjects) != len(projects) {
		return nil, fmt.Errorf(
			"global project representations disagree: %d typed, %d raw",
			len(projects), len(rawProjects),
		)
	}
	for index := range persisted.Projects {
		projects[index] = ProjectRegistration{
			Persisted: persisted.Projects[index],
			Effective: effective.Projects[index],
			raw:       cloneStringMap(rawProjects[index]),
		}
	}
	return &GlobalSnapshot{Config: &effective, Projects: projects}, nil
}

// CompareAndSwapProject replaces or removes exactly one raw persisted project
// entry. Expanded path equality is intentionally not used as a concurrency
// token because it would collapse distinct user edits and symlink spellings.
func CompareAndSwapProject(
	expected ProjectRegistration,
	replacement *models.Project,
) (bool, error) {
	return compareAndSwapProjectAt(getConfigDir(), expected, replacement, true)
}

// CompareAndSwapProjectAt performs the raw-entry project CAS in an explicit
// kwt home. Daemon operations must not depend on process-global KWT_HOME.
func CompareAndSwapProjectAt(
	home string,
	expected ProjectRegistration,
	replacement *models.Project,
) (bool, error) {
	return compareAndSwapProjectAt(home, expected, replacement, false)
}

func compareAndSwapProjectAt(
	home string,
	expected ProjectRegistration,
	replacement *models.Project,
	updateProcessConfig bool,
) (bool, error) {
	var copiedReplacement *models.Project
	if replacement != nil {
		copied := *replacement
		copiedReplacement = &copied
	}
	var projects []map[string]any
	configPath := filepath.Join(home, configName+"."+configType)
	changed, err := (globalConfigStore{path: configPath}).mutate(
		func(globalViper *viper.Viper) (bool, error) {
			var err error
			projects, err = rawProjectEntries(globalViper)
			if err != nil {
				return false, err
			}
			var persisted []models.Project
			if err := globalViper.UnmarshalKey("projects", &persisted); err != nil {
				return false, fmt.Errorf("failed to read projects: %w", err)
			}
			if len(persisted) != len(projects) {
				return false, fmt.Errorf(
					"global project representations disagree: %d typed, %d raw",
					len(persisted),
					len(projects),
				)
			}
			match := -1
			matches := 0
			for index := range projects {
				if sameProjectRegistrationRaw(projects[index], expected.raw) {
					match = index
					matches++
				}
			}
			if matches != 1 {
				return false, nil
			}
			if copiedReplacement == nil {
				projects = append(projects[:match], projects[match+1:]...)
			} else {
				for index := range persisted {
					if index != match && sameProjectPath(
						persisted[index].Path,
						copiedReplacement.Path,
					) {
						return false, nil
					}
				}
				updated := cloneStringMap(projects[match])
				updated["repository"] = copiedReplacement.Repository
				updated["name"] = copiedReplacement.Name
				updated["path"] = copiedReplacement.Path
				updated["last_touched"] = copiedReplacement.LastTouched
				projects[match] = updated
			}
			globalViper.Set("projects", projects)
			return true, nil
		},
	)
	if err != nil {
		return false, err
	}
	if changed && updateProcessConfig {
		viper.Set("projects", projects)
	}
	return changed, nil
}

func rawProjectEntries(source *viper.Viper) ([]map[string]any, error) {
	var projects []map[string]any
	if err := source.UnmarshalKey("projects", &projects); err != nil {
		return nil, fmt.Errorf("failed to read raw projects: %w", err)
	}
	return projects, nil
}

func cloneStringMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// expandConfigPaths expands all path fields in the configuration.
func expandConfigPaths(cfg *models.Config) error {
	return expandConfigPathsWith(cfg, utils.ExpandPath)
}

func expandConfigPathsWith(
	cfg *models.Config,
	expandPath func(string) (string, error),
) error {
	if expandPath == nil {
		expandPath = utils.ExpandPath
	}
	expandedPath, err := expandPath(cfg.Worktree.BaseDir)
	if err != nil {
		return fmt.Errorf("failed to expand worktree base dir: %w", err)
	}
	cfg.Worktree.BaseDir = expandedPath

	if cfg.Fleet.TokenFile != "" {
		expandedPath, err = expandPath(cfg.Fleet.TokenFile)
		if err != nil {
			return fmt.Errorf("failed to expand fleet token file: %w", err)
		}
		cfg.Fleet.TokenFile = expandedPath
	}
	if cfg.Fleet.Hub.StorePath != "" {
		expandedPath, err = expandPath(cfg.Fleet.Hub.StorePath)
		if err != nil {
			return fmt.Errorf("failed to expand fleet hub store path: %w", err)
		}
		cfg.Fleet.Hub.StorePath = expandedPath
	}

	for i := range cfg.RepositorySettings {
		repo := cfg.RepositorySettings[i].Repository
		// Skip path expansion for glob patterns — ExpandPath would prepend
		// the CWD to relative globs like "**/owner/repo", breaking matching.
		if !strings.ContainsAny(repo, "*?[") {
			expandedPath, err = expandPath(repo)
			if err != nil {
				return fmt.Errorf("failed to expand repository setting path: %w", err)
			}
			// Resolve symlinks for consistent path comparison with git-derived paths
			if resolved, err := filepath.EvalSymlinks(expandedPath); err == nil {
				expandedPath = resolved
			}
			cfg.RepositorySettings[i].Repository = expandedPath
		}

		// BaseDir is a destination path (not compared against git-derived paths),
		// so symlink resolution is not needed here.
		if baseDir := cfg.RepositorySettings[i].BaseDir; baseDir != "" {
			expanded, err := expandPath(baseDir)
			if err != nil {
				return fmt.Errorf("failed to expand repository basedir: %w", err)
			}
			cfg.RepositorySettings[i].BaseDir = expanded
		}
	}
	for i := range cfg.Projects {
		path := cfg.Projects[i].Path
		if path == "" {
			continue
		}
		expandedPath, err = expandPath(path)
		if err != nil {
			return fmt.Errorf("failed to expand project path: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(expandedPath); err == nil {
			expandedPath = resolved
		}
		cfg.Projects[i].Path = expandedPath
	}
	for i := range cfg.Workspaces {
		path := cfg.Workspaces[i].Path
		if path == "" {
			continue
		}
		expandedPath, err = expandPath(path)
		if err != nil {
			return fmt.Errorf("failed to expand workspace path: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(expandedPath); err == nil {
			expandedPath = resolved
		}
		cfg.Workspaces[i].Path = expandedPath
	}
	return nil
}

// RegisterProject records a repository in the global project registry.
func RegisterProject(project models.Project) error {
	if strings.TrimSpace(project.Path) == "" {
		return fmt.Errorf("project path required")
	}

	path, err := normalizeProjectPath(project.Path)
	if err != nil {
		return err
	}
	project.Path = path
	if project.Name == "" {
		project.Name = filepath.Base(path)
	}
	if project.Repository == "" {
		project.Repository = path
	}
	project.LastTouched = time.Now().UTC().Format(time.RFC3339)

	var projects []map[string]any
	configPath := filepath.Join(getConfigDir(), configName+"."+configType)
	if _, err := (globalConfigStore{path: configPath}).mutate(
		func(globalViper *viper.Viper) (bool, error) {
			var persisted []models.Project
			if err := globalViper.UnmarshalKey("projects", &persisted); err != nil {
				return false, fmt.Errorf("failed to read projects: %w", err)
			}
			var err error
			projects, err = rawProjectEntries(globalViper)
			if err != nil {
				return false, err
			}
			if len(projects) != len(persisted) {
				return false, fmt.Errorf(
					"project representations disagree: %d typed, %d raw",
					len(persisted), len(projects),
				)
			}
			updated := false
			for i := range persisted {
				if sameProject(persisted[i], project) {
					projects[i] = updateRawProject(projects[i], project)
					updated = true
					break
				}
			}
			if !updated {
				projects = append(projects, updateRawProject(nil, project))
			}
			globalViper.Set("projects", projects)
			return true, nil
		},
	); err != nil {
		return err
	}

	viper.Set("projects", projects)
	return nil
}

func updateRawProject(raw map[string]any, project models.Project) map[string]any {
	updated := cloneStringMap(raw)
	updated["repository"] = project.Repository
	updated["name"] = project.Name
	updated["path"] = project.Path
	updated["last_touched"] = project.LastTouched
	return updated
}

func normalizeProjectPath(path string) (string, error) {
	expanded, err := utils.ExpandPath(path)
	if err != nil {
		return "", fmt.Errorf("failed to expand project path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
		expanded = resolved
	}
	return expanded, nil
}

func sameProject(existing, next models.Project) bool {
	if sameProjectPath(existing.Path, next.Path) {
		return true
	}
	if existing.Repository != "" && next.Repository != "" {
		existingIdentity, existingOK := repositoryurl.CanonicalRepositoryIdentity(
			existing.Repository,
		)
		nextIdentity, nextOK := repositoryurl.CanonicalRepositoryIdentity(
			next.Repository,
		)
		if existingOK && nextOK {
			return repositoryurl.FoldRepositoryIdentity(existingIdentity) ==
				repositoryurl.FoldRepositoryIdentity(nextIdentity)
		}
		return existing.Repository == next.Repository
	}
	return existing.Path == next.Path
}

func sameProjectPath(existingPath, nextPath string) bool {
	if existingPath == "" || nextPath == "" {
		return false
	}
	existingNormalized, err := normalizeProjectPath(existingPath)
	if err != nil {
		existingNormalized = filepath.Clean(existingPath)
	}
	nextNormalized, err := normalizeProjectPath(nextPath)
	if err != nil {
		nextNormalized = filepath.Clean(nextPath)
	}
	return utils.PathKey(existingNormalized) == utils.PathKey(nextNormalized)
}

// SetGlobal sets a configuration value and writes to the global config file only.
// This uses a separate viper instance to avoid writing merged local settings.
func SetGlobal(key string, value any) error {
	configPath := filepath.Join(getConfigDir(), configName+"."+configType)
	if _, err := (globalConfigStore{path: configPath}).mutate(
		func(globalViper *viper.Viper) (bool, error) {
			globalViper.Set(key, value)
			return true, nil
		},
	); err != nil {
		return err
	}

	// Update main viper instance as well
	viper.Set(key, value)
	return nil
}

// SetLocal sets a configuration value and writes to the local config file (.kwt.toml).
func SetLocal(key string, value any) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	localConfigPath := filepath.Join(cwd, localConfigName+"."+configType)

	localViper := viper.New()
	localViper.SetConfigFile(localConfigPath)
	localViper.SetConfigType(configType)

	_ = localViper.ReadInConfig()
	localViper.Set(key, value)

	if err := localViper.WriteConfigAs(localConfigPath); err != nil {
		return fmt.Errorf("failed to write local config: %w", err)
	}

	// Update main viper instance as well
	viper.Set(key, value)
	return nil
}

// Set sets a configuration value (defaults to global).
func Set(key string, value any) error {
	return SetGlobal(key, value)
}

// GetValue retrieves a configuration value by key.
func GetValue(key string) any {
	return viper.Get(key)
}

// AllSettings returns all configuration settings.
func AllSettings() map[string]any {
	return viper.AllSettings()
}

// Get returns the current loaded configuration, loading it if necessary.
func Get() *models.Config {
	cfg, err := Load()
	if err != nil {
		// Initialize with viper defaults if config cannot be loaded
		var defaultCfg models.Config
		if err := viper.Unmarshal(&defaultCfg); err != nil {
			return &models.Config{}
		}

		// Apply path expansions to defaults, ignoring errors
		_ = expandConfigPaths(&defaultCfg)
		return &defaultCfg
	}
	return cfg
}
