// Package worktree provides embeddable worktree inventory contracts.
package worktree

import (
	"context"
	"fmt"
	"path/filepath"
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
	ForceGlobal             bool                  `json:"force_global,omitempty"`
	RequireCurrent          bool                  `json:"require_current,omitempty"`
	IncludeProtectedSockets bool                  `json:"include_protected_sockets,omitempty"`
	UntrustedConfig         UntrustedConfigPolicy `json:"untrusted_config"`
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
	return nil
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

type Snapshot struct {
	Projects      []models.Project   `json:"projects"`
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
