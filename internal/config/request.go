package config

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
	"go.kenn.io/kwt/pkg/models"
)

type UntrustedPolicy string

const (
	RequireInteraction UntrustedPolicy = "require_interaction"
	IgnoreUntrusted    UntrustedPolicy = "ignore"
)

var ErrConfigChanged = errors.New("repository config changed")

var errUnsafeRepositoryConfig = errors.New("unsafe repository config")

type ResolveRequest struct {
	Home             string
	WorkingDirectory string
	UntrustedPolicy  UntrustedPolicy
	ExpandPath       func(string) (string, error)
}

type ConfigNote struct {
	Code string `json:"code"`
	Path string `json:"path"`
}

type ResolveResult struct {
	Config *models.Config
	Notes  []ConfigNote
}

type TrustRequiredError struct {
	Path      string
	Digest    string
	Size      int
	Preview   string
	Truncated bool
}

func (e *TrustRequiredError) Error() string {
	return fmt.Sprintf("repository config %s requires trust", e.Path)
}

type Approval struct {
	Home   string
	Path   string
	Digest string
}

// ResolveWorkingDirectory returns an isolated configuration snapshot for one
// request. It never mutates the process-global Viper instance.
func ResolveWorkingDirectory(request ResolveRequest) (*ResolveResult, error) {
	home, err := canonicalRequestDir(request.Home, "kwt home")
	if err != nil {
		return nil, err
	}
	workingDirectory, err := canonicalRequestDir(request.WorkingDirectory, "working directory")
	if err != nil {
		return nil, err
	}

	target, err := readGlobalViper(filepath.Join(home, configName+"."+configType))
	if err != nil {
		return nil, err
	}
	applyGlobalDefaults(target)

	path := filepath.Join(workingDirectory, localConfigName+"."+configType)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			config, loadErr := configFromViper(target, false, false, request.ExpandPath)
			return &ResolveResult{Config: config}, loadErr
		}
		return nil, fmt.Errorf("stat repository config %s: %w", path, err)
	}

	normalizedPath, err := normalizeTargetConfigPath(path)
	if err != nil {
		if errors.Is(err, errUnsafeRepositoryConfig) {
			config, loadErr := configFromViper(target, false, false, request.ExpandPath)
			return &ResolveResult{
				Config: config,
				Notes:  []ConfigNote{{Code: "unsafe_config_skipped", Path: path}},
			}, loadErr
		}
		return nil, err
	}
	path = normalizedPath
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read repository config %s: %w", path, err)
	}
	digest := computeSHA256(data)
	trustStorePath := filepath.Join(home, trustStoreFilename)
	store, err := LoadTrustStore(trustStorePath)
	if err != nil {
		if request.UntrustedPolicy == IgnoreUntrusted {
			config, loadErr := configFromViper(target, false, false, request.ExpandPath)
			return &ResolveResult{
				Config: config,
				Notes: []ConfigNote{
					{Code: "trust_store_unavailable", Path: trustStorePath},
					{Code: "untrusted_config_skipped", Path: path},
				},
			}, loadErr
		}
		return nil, err
	}
	if !store.IsTrusted(path, digest) {
		if request.UntrustedPolicy == IgnoreUntrusted {
			config, loadErr := configFromViper(target, false, false, request.ExpandPath)
			return &ResolveResult{
				Config: config,
				Notes:  []ConfigNote{{Code: "untrusted_config_skipped", Path: path}},
			}, loadErr
		}
		preview := data
		truncated := len(preview) > promptPreviewLimit
		if truncated {
			preview = preview[:promptPreviewLimit]
		}
		return nil, &TrustRequiredError{
			Path:      path,
			Digest:    digest,
			Size:      len(data),
			Preview:   sanitizeForTerminal(string(preview)),
			Truncated: truncated,
		}
	}

	local := viper.New()
	local.SetConfigType(configType)
	if err := local.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("parse repository config %s: %w", path, err)
	}
	if err := resolveTargetLocalPaths(local, workingDirectory); err != nil {
		return nil, fmt.Errorf("resolve repository config paths %s: %w", path, err)
	}
	localTemplate := local.IsSet("naming.template")
	localSanitize := local.IsSet("naming.sanitize_chars")
	mergeRequestLocal(target, local)
	config, err := configFromViper(target, localTemplate, localSanitize, request.ExpandPath)
	return &ResolveResult{Config: config}, err
}

// ApproveWorkingDirectory persists trust only after reopening the file and
// confirming that its content still matches the prompted digest.
func ApproveWorkingDirectory(approval Approval) error {
	home, err := canonicalRequestDir(approval.Home, "kwt home")
	if err != nil {
		return err
	}
	path, err := normalizeTargetConfigPath(approval.Path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read repository config %s: %w", path, err)
	}
	actual := computeSHA256(data)
	if len(actual) != len(approval.Digest) || subtle.ConstantTimeCompare([]byte(actual), []byte(approval.Digest)) != 1 {
		return ErrConfigChanged
	}
	store, err := LoadTrustStore(filepath.Join(home, trustStoreFilename))
	if err != nil {
		return err
	}
	return store.Add(path, actual)
}

func canonicalRequestDir(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s must not be empty", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", label, err)
	}
	return filepath.Clean(resolved), nil
}

func mergeRequestLocal(target, local *viper.Viper) {
	for _, key := range local.AllKeys() {
		switch {
		case key == "repository_settings":
			mergeRepositorySettingsInto(target, local)
		case key == "projects" || key == "workspaces" || strings.HasPrefix(key, "workspaces."),
			key == "fleet" || strings.HasPrefix(key, "fleet."),
			key == "daemon" || strings.HasPrefix(key, "daemon."),
			key == "ssh" || strings.HasPrefix(key, "ssh."):
			continue
		default:
			target.Set(key, local.Get(key))
		}
	}
}

func configFromViper(
	target *viper.Viper,
	localTemplate, localSanitize bool,
	expandPath func(string) (string, error),
) (*models.Config, error) {
	var config models.Config
	if err := target.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("unmarshal request config: %w", err)
	}
	if err := validateDaemonConfig(config.Daemon); err != nil {
		return nil, err
	}
	if err := validateSSHConfig(config.SSH); err != nil {
		return nil, err
	}
	if err := expandConfigPathsWith(&config, expandPath); err != nil {
		return nil, err
	}
	config.Naming.TemplateRepositoryLocal = localTemplate
	config.Naming.SanitizeCharsRepositoryLocal = localSanitize
	return &config, nil
}
