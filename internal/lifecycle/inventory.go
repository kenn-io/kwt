// Package kwt provides embeddable worktree inventory and lifecycle services.
package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.kenn.io/kwt/pkg/models"
)

type View string

const (
	ViewProjects   View = "projects"
	ViewRepository View = "repository"
	ViewGlobal     View = "global"
	ViewDashboard  View = "dashboard"
)

type Freshness string

// DefaultRefreshTimeout bounds one service-owned inventory refresh. Transports
// must allow additional time for request delivery and response encoding.
const DefaultRefreshTimeout = 30 * time.Second

const (
	Fresh      Freshness = "fresh"
	Refreshing Freshness = "refreshing"
	Stale      Freshness = "stale"
)

type UntrustedConfigPolicy string

const (
	RequireConfigInteraction UntrustedConfigPolicy = "require_interaction"
	IgnoreUntrustedConfig    UntrustedConfigPolicy = "ignore"
)

type Request struct {
	View                    View                  `json:"view"`
	WorkingDirectory        string                `json:"working_directory,omitempty"`
	LaunchDirectory         string                `json:"launch_directory,omitempty"`
	Expansion               ExpansionContext      `json:"expansion"`
	ForceGlobal             bool                  `json:"force_global,omitempty"`
	RequireCurrent          bool                  `json:"require_current,omitempty"`
	IncludeProtectedSockets bool                  `json:"include_protected_sockets,omitempty"`
	UntrustedConfig         UntrustedConfigPolicy `json:"untrusted_config"`
}

// ExpansionContext carries the invoking client's path-expansion inputs across
// the daemon boundary. The authenticated daemon uses it only for this request.
type ExpansionContext struct {
	WorkingDirectory string            `json:"working_directory"`
	HomeDirectory    string            `json:"home_directory"`
	Environment      map[string]string `json:"environment,omitempty"`
}

func CaptureExpansionContext() (ExpansionContext, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return ExpansionContext{}, fmt.Errorf("get path-expansion working directory: %w", err)
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return ExpansionContext{}, fmt.Errorf("resolve path-expansion working directory: %w", err)
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return ExpansionContext{}, fmt.Errorf("get path-expansion home directory: %w", err)
	}
	environment := make(map[string]string)
	for _, assignment := range os.Environ() {
		name, value, ok := strings.Cut(assignment, "=")
		if !ok || !validEnvironmentName(name) {
			continue
		}
		environment[normalizedEnvironmentName(name)] = value
	}
	return ExpansionContext{
		WorkingDirectory: workingDirectory,
		HomeDirectory:    homeDirectory,
		Environment:      environment,
	}, nil
}

func (c ExpansionContext) validate() error {
	if !filepath.IsAbs(c.WorkingDirectory) {
		return fmt.Errorf("path-expansion working directory must be absolute")
	}
	if !filepath.IsAbs(c.HomeDirectory) {
		return fmt.Errorf("path-expansion home directory must be absolute")
	}
	for name := range c.Environment {
		if !validEnvironmentName(name) {
			return fmt.Errorf("invalid path-expansion environment name %q", name)
		}
	}
	return nil
}

func (c ExpansionContext) expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	path = os.Expand(path, func(name string) string {
		return c.Environment[normalizedEnvironmentName(name)]
	})
	if path == "~" {
		path = c.HomeDirectory
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(c.HomeDirectory, path[2:])
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.WorkingDirectory, path)
	}
	return path, nil
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func normalizedEnvironmentName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func (r Request) validate() error {
	if r.UntrustedConfig != RequireConfigInteraction && r.UntrustedConfig != IgnoreUntrustedConfig {
		return fmt.Errorf("unknown untrusted config policy %q", r.UntrustedConfig)
	}
	switch r.View {
	case ViewProjects, ViewGlobal:
		if r.WorkingDirectory != "" || r.LaunchDirectory != "" || r.ForceGlobal {
			return fmt.Errorf("%s inventory does not accept location metadata", r.View)
		}
	case ViewRepository:
		if !filepath.IsAbs(r.WorkingDirectory) {
			return fmt.Errorf("repository working directory must be absolute")
		}
		if r.LaunchDirectory != "" {
			return fmt.Errorf("repository inventory does not accept a launch directory")
		}
	case ViewDashboard:
		if r.WorkingDirectory != "" || r.ForceGlobal {
			return fmt.Errorf("dashboard inventory does not accept repository location metadata")
		}
		if r.LaunchDirectory != "" && !filepath.IsAbs(r.LaunchDirectory) {
			return fmt.Errorf("dashboard launch directory must be absolute")
		}
	default:
		return fmt.Errorf("unknown inventory view %q", r.View)
	}
	return r.Expansion.validate()
}

type Repository struct {
	URL      string `json:"url,omitempty"`
	Host     string `json:"host,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Name     string `json:"name,omitempty"`
	FullPath string `json:"full_path,omitempty"`
}

type Entry struct {
	Path           string     `json:"path"`
	Branch         string     `json:"branch"`
	CommitHash     string     `json:"commit_hash"`
	IsMain         bool       `json:"is_main"`
	CreatedAt      time.Time  `json:"created_at"`
	Generation     string     `json:"generation"`
	Repository     Repository `json:"repository"`
	SessionName    string     `json:"session_name"`
	TmuxSocketName string     `json:"tmux_socket_name,omitempty"`
}

type Note struct {
	Code string `json:"code"`
	Path string `json:"path"`
}

type Diagnostic struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// InventoryProject is the published project-registration contract. It lives
// outside pkg/models so the observation-only concurrency token cannot be
// serialized back into the user's configuration.
type InventoryProject struct {
	Repository              string `json:"repository"`
	Name                    string `json:"name"`
	Path                    string `json:"path"`
	LastTouched             string `json:"last_touched"`
	RegistrationFingerprint string `json:"registration_fingerprint"`
}

type Snapshot struct {
	Config        *models.Config     `json:"config"`
	Projects      []InventoryProject `json:"projects"`
	Entries       []Entry            `json:"entries"`
	LaunchEntries []Entry            `json:"launch_entries,omitempty"`
	Workspaces    []models.Workspace `json:"workspaces"`
}

type Result struct {
	Snapshot     Snapshot    `json:"snapshot"`
	Freshness    Freshness   `json:"freshness"`
	ObservedAt   time.Time   `json:"observed_at"`
	RefreshError *Diagnostic `json:"refresh_error,omitempty"`
	Notes        []Note      `json:"notes,omitempty"`
}

type ConfigApproval struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type Inventory interface {
	Query(context.Context, Request) (Result, error)
	ApproveConfig(context.Context, ConfigApproval) error
}

type Source interface {
	Load(context.Context, Request) (Result, error)
	ApproveConfig(context.Context, ConfigApproval) error
}
