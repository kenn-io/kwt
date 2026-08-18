// Package models defines the core data structures used throughout the kwt application.
package models

import "time"

// TmuxAttachMode tells clients whether a selected tmux endpoint can be
// attached directly or requires KWT's protected-workspace attach flow. Every
// wire type that emits tmux_socket_name must emit tmux_attach_mode beside it;
// clients must never infer attachment policy from the socket name.
type TmuxAttachMode string

const (
	// TmuxAttachDirect selects direct tmux attachment on the returned socket.
	TmuxAttachDirect TmuxAttachMode = "direct"
	// TmuxAttachProtected requires KWT's protected pull-request attachment.
	TmuxAttachProtected TmuxAttachMode = "protected"
)

// Worktree represents a Git worktree with its associated metadata.
type Worktree struct {
	Path           string         `json:"path"`                       // Absolute path to the worktree directory
	Branch         string         `json:"branch"`                     // Branch name associated with this worktree
	CommitHash     string         `json:"commit_hash"`                // Current HEAD commit hash
	IsMain         bool           `json:"is_main"`                    // Whether this is the main worktree
	CreatedAt      time.Time      `json:"created_at"`                 // Worktree directory modification time
	Generation     string         `json:"generation"`                 // Durable identity for this worktree registration
	Repository     string         `json:"repository"`                 // Repository slug, e.g. github.com/owner/name
	SessionName    string         `json:"session_name"`               // Computed tmux workspace session name
	TmuxSocketName string         `json:"tmux_socket_name,omitempty"` // Selected endpoint; empty direct means default, empty protected means unresolved
	TmuxAttachMode TmuxAttachMode `json:"tmux_attach_mode"`           // Client attachment policy for the selected endpoint
	Prunable       bool           `json:"-"`                          // Git reports this worktree entry as stale/prunable
}

// Branch represents a Git branch with its metadata.
type Branch struct {
	Name       string     `json:"name"`        // Branch name
	Label      string     `json:"label"`       // Source-qualified display label
	Source     string     `json:"source"`      // Local branch or remote ref used to create a worktree
	IsCurrent  bool       `json:"is_current"`  // Whether this is the current branch
	IsRemote   bool       `json:"is_remote"`   // Whether this is a remote branch
	LastCommit CommitInfo `json:"last_commit"` // Information about the last commit
}

// CommitInfo contains information about a Git commit.
type CommitInfo struct {
	Hash    string    `json:"hash"`    // Commit hash
	Message string    `json:"message"` // Commit message
	Author  string    `json:"author"`  // Commit author
	Date    time.Time `json:"date"`    // Commit date
}

// CdConfig contains configuration for the cd command behavior.
type CdConfig struct {
	LaunchShell bool `mapstructure:"launch_shell"` // Whether to launch a new shell on cd
}

// DaemonConfig controls the local kwt service lifecycle.
type DaemonConfig struct {
	IdleTimeout      time.Duration `mapstructure:"idle_timeout" toml:"idle_timeout"`
	AutoRestart      string        `mapstructure:"auto_restart" toml:"auto_restart"`
	ReplacementGrace time.Duration `mapstructure:"replacement_grace" toml:"replacement_grace"`
}

// SSHConfig controls machine-level SSH connection lifecycle policy.
type SSHConfig struct {
	IdleTimeout time.Duration `mapstructure:"idle_timeout" toml:"idle_timeout"`
}

// Config represents the application configuration.
type Config struct {
	Daemon             DaemonConfig        `mapstructure:"daemon" toml:"daemon"`
	SSH                SSHConfig           `mapstructure:"ssh" toml:"ssh"`
	Worktree           WorktreeConfig      `mapstructure:"worktree"`                     // Worktree-related configuration
	Fleet              FleetConfig         `mapstructure:"fleet" toml:"fleet"`           // Multi-machine sync configuration
	Cd                 CdConfig            `mapstructure:"cd"`                           // Cd command configuration
	Finder             FinderConfig        `mapstructure:"finder"`                       // Fuzzy finder configuration
	UI                 UIConfig            `mapstructure:"ui"`                           // UI-related configuration
	Naming             NamingConfig        `mapstructure:"naming"`                       // Naming and template configuration
	Agents             map[string]string   `mapstructure:"agents"`                       // Named agent invocation commands for layouts
	Projects           []Project           `mapstructure:"projects"`                     // Known repositories for cross-project discovery
	Workspaces         []Workspace         `mapstructure:"workspaces" toml:"workspaces"` // Registered directory workspaces
	RepositorySettings []RepositorySetting `mapstructure:"repository_settings"`          // Per-repository setup/copy overrides
	Layouts            LayoutsConfig       `mapstructure:"layouts"`                      // Named tmux workspace layout library
}

// FleetConfig contains optional multi-machine sync configuration.
type FleetConfig struct {
	Enabled   bool           `mapstructure:"enabled" toml:"enabled"`       // Enable multi-machine sync behavior
	HostID    string         `mapstructure:"host_id" toml:"host_id"`       // Stable identity for this host
	HubURL    string         `mapstructure:"hub_url" toml:"hub_url"`       // Hub API base URL
	TokenFile string         `mapstructure:"token_file" toml:"token_file"` // Path to bearer token file
	TokenEnv  string         `mapstructure:"token_env" toml:"token_env"`   // Environment variable containing bearer token
	Hub       FleetHubConfig `mapstructure:"hub" toml:"hub"`               // Hub listener/store configuration
}

// FleetHubConfig contains configuration for running the fleet hub.
type FleetHubConfig struct {
	ListenAddr string `mapstructure:"listen_addr" toml:"listen_addr"` // Hub HTTP listen address
	StorePath  string `mapstructure:"store_path" toml:"store_path"`   // Hub state store path
}

// LayoutsConfig defines the named tmux workspace layout library.
type LayoutsConfig struct {
	Default         string   `mapstructure:"default" toml:"default"`                       // Preset launched by default
	AutoLaunchOnAdd bool     `mapstructure:"auto_launch_on_add" toml:"auto_launch_on_add"` // Launch a workspace on add
	Presets         []Layout `mapstructure:"presets" toml:"presets"`                       // Available named layouts
}

// Layout is one named tmux pane layout.
type Layout struct {
	Name    string   `mapstructure:"name" toml:"name"`       // Preset name, e.g. "quad"
	Arrange string   `mapstructure:"arrange" toml:"arrange"` // tmux select-layout preset
	Panes   []string `mapstructure:"panes" toml:"panes"`     // one command per pane; "" = login shell
}

// RepositorySetting defines per-repository setup commands and files to copy for worktree creation.
type RepositorySetting struct {
	Repository    string   `mapstructure:"repository"`     // Path or pattern for repository
	SetupCommands []string `mapstructure:"setup_commands"` // Commands to run in new worktree
	CopyFiles     []string `mapstructure:"copy_files"`     // Files/globs to copy into new worktree
	BaseDir       string   `mapstructure:"basedir"`        // Override global worktree.basedir for this repository
}

// Project records a repository kwt has touched so cross-project commands can
// discover its worktrees even when they live outside the global base directory.
type Project struct {
	Repository  string `mapstructure:"repository" toml:"repository" json:"repository"`       // Stable repository identifier, usually host/owner/name
	Name        string `mapstructure:"name" toml:"name" json:"name"`                         // Human-friendly display name
	Path        string `mapstructure:"path" toml:"path" json:"path"`                         // Main repository root path
	LastTouched string `mapstructure:"last_touched" toml:"last_touched" json:"last_touched"` // RFC3339 UTC timestamp
}

// Workspace records a plain directory kwt can open as a tmux workspace,
// independent of any git worktree.
type Workspace struct {
	Name string `mapstructure:"name" toml:"name"` // Unique display name
	Path string `mapstructure:"path" toml:"path"` // Directory root
}

// WorktreeConfig contains worktree-specific configuration options.
type WorktreeConfig struct {
	BaseDir   string `mapstructure:"basedir"`    // Base directory for creating worktrees
	AutoMkdir bool   `mapstructure:"auto_mkdir"` // Automatically create directories
}

// FinderConfig contains fuzzy finder configuration options.
type FinderConfig struct {
	Preview bool `mapstructure:"preview"` // Enable preview window
}

// UIConfig contains UI-related configuration options.
type UIConfig struct {
	Icons     bool `mapstructure:"icons"`      // Enable icon display
	TildeHome bool `mapstructure:"tilde_home"` // Display home directory as ~
}

// NamingConfig contains directory naming and template configuration options.
type NamingConfig struct {
	Template                     string            `mapstructure:"template"`            // Directory name template
	SanitizeChars                map[string]string `mapstructure:"sanitize_chars"`      // Character replacement for branch names
	TemplateRepositoryLocal      bool              `mapstructure:"-" toml:"-" json:"-"` // Template came from repository-local config
	SanitizeCharsRepositoryLocal bool              `mapstructure:"-" toml:"-" json:"-"` // Replacements came from repository-local config
}

// WorktreeStatus represents the current status of a worktree.
type WorktreeStatus struct {
	Path         string        `json:"path"`          // Absolute path to the worktree
	Branch       string        `json:"branch"`        // Branch name
	Repository   string        `json:"repository"`    // Repository identifier
	Status       WorktreeState `json:"status"`        // Current status (clean, modified, etc.)
	GitStatus    GitStatus     `json:"git_status"`    // Detailed git status
	LastActivity time.Time     `json:"last_activity"` // Last modification time
	IsCurrent    bool          `json:"is_current"`    // Whether this is the current worktree
}

// WorktreeState represents the overall state of a worktree.
type WorktreeState string

const (
	// WorktreeStatusClean indicates a worktree with no uncommitted changes.
	WorktreeStatusClean WorktreeState = "clean"
	// WorktreeStatusModified indicates a worktree with uncommitted modifications.
	WorktreeStatusModified WorktreeState = "modified"
	// WorktreeStatusStaged indicates a worktree with staged changes ready to commit.
	WorktreeStatusStaged WorktreeState = "staged"
	// WorktreeStatusConflict indicates a worktree with merge conflicts.
	WorktreeStatusConflict WorktreeState = "conflict"
	// WorktreeStatusStale indicates a worktree that is out of sync with the remote.
	WorktreeStatusStale WorktreeState = "stale"
	// WorktreeStatusUnknown indicates a worktree with an undetermined status.
	WorktreeStatusUnknown WorktreeState = "unknown"
)

// GitStatus contains detailed git status information.
type GitStatus struct {
	Modified  int `json:"modified"`  // Number of modified files
	Added     int `json:"added"`     // Number of added files
	Deleted   int `json:"deleted"`   // Number of deleted files
	Untracked int `json:"untracked"` // Number of untracked files
	Staged    int `json:"staged"`    // Number of staged files
	Ahead     int `json:"ahead"`     // Number of commits ahead of remote
	Behind    int `json:"behind"`    // Number of commits behind remote
	Conflicts int `json:"conflicts"` // Number of files with conflicts
}

// Worktree type constants for display purposes.
const (
	// WorktreeTypeMain represents the main worktree (repository root).
	WorktreeTypeMain = "main"
	// WorktreeTypeWorktree represents an additional worktree.
	WorktreeTypeWorktree = "worktree"
)
