package tui

import (
	"context"
	"os/exec"
	"time"

	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

type HandoffKind int

const (
	HandoffNone HandoffKind = iota
	HandoffAttach
	HandoffShell
)

type Handoff struct {
	Kind         HandoffKind
	Row          Row
	LayoutName   string
	ExistingOnly bool
}

type Row struct {
	Entry        *discovery.GlobalWorktreeEntry
	Status       *models.WorktreeStatus
	Fleet        *FleetInfo
	Workspace    *WorkspaceInfo
	SessionName  string
	SessionLive  bool
	TmuxEndpoint tmux.SessionEndpoint
	Creating     bool
	Removing     bool
}

type InventoryScope int

const (
	InventoryCachedDashboard InventoryScope = iota
	InventoryCurrentRepository
	InventoryCurrentDashboard
)

type InventoryRequest struct {
	Scope            InventoryScope
	WorkingDirectory string
	ProjectIdentity  string
	CollectStatuses  bool
}

type InventoryResult struct {
	Rows       []Row
	Warnings   []string
	ObservedAt time.Time
	Current    bool
}

// WorkspaceInfo is the TUI-facing view of one registered directory workspace.
type WorkspaceInfo struct {
	Name string
	Path string
}

// FleetInfo is the TUI-facing summary of one multi-machine sync row.
type FleetInfo struct {
	ProjectIdentity  string
	ProjectName      string
	Kind             string
	Ref              string
	Branch           string
	Local            bool
	Hosts            []string
	Sync             string
	Dirty            string
	Freshness        string
	AllPrimary       bool
	MaterializeHost  string
	RemotePath       string
	RemoteHead       string
	RemoteUpstream   string
	RemoteAhead      int
	CanMaterialize   bool
	MaterializeLabel string
}

type Backend interface {
	LoadInventory(context.Context, InventoryRequest) (InventoryResult, error)
	// ListFast returns enough local metadata to paint dashboard rows without
	// waiting for repository status collection.
	ListFast(ctx context.Context) ([]Row, []string, error)
	// List returns local dashboard rows plus non-fatal warnings that should
	// be surfaced to the user. Fleet hub work belongs in MergeFleet.
	List(ctx context.Context) ([]Row, []string, error)
	// MergeFleet overlays multi-machine sync state onto rows and returns the
	// merged rows plus hub warnings. When sync is disabled it returns rows
	// unchanged.
	MergeFleet(ctx context.Context, rows []Row) ([]Row, []string)
	// PreviewWorktree resolves where CreateWorktree will place the worktree so
	// the dashboard can show it before Git and setup finish. The returned path
	// is a display value only: passing it back as a destination would re-run
	// path resolution over an already-resolved path.
	PreviewWorktree(row Row, branch string) (Row, error)
	ListBranches(ctx context.Context, row Row) ([]models.Branch, error)
	CreateWorktree(ctx context.Context, row Row, branch, source string) (string, error)
	MaterializeWorktree(ctx context.Context, row Row) (string, error)
	RemoveWorktree(ctx context.Context, row Row, force bool) error
	UnregisterWorkspace(row Row) error
	KillSession(row Row) error
	OpenInTmux(ctx context.Context, row Row, layoutName string) (*exec.Cmd, error)
	OpenExistingInTmux(ctx context.Context, row Row) (*exec.Cmd, error)
	LayoutNames() []string
	InsideTmux() bool
}
