// Package kwt provides embeddable worktree inventory and lifecycle services.
package kwt

import (
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/status"
)

type (
	View                         = lifecycle.View
	Freshness                    = lifecycle.Freshness
	UntrustedConfigPolicy        = lifecycle.UntrustedConfigPolicy
	Request                      = lifecycle.Request
	ExpansionContext             = lifecycle.ExpansionContext
	Repository                   = lifecycle.Repository
	Entry                        = lifecycle.Entry
	Note                         = lifecycle.Note
	Diagnostic                   = lifecycle.Diagnostic
	Project                      = lifecycle.InventoryProject
	Snapshot                     = lifecycle.Snapshot
	Result                       = lifecycle.Result
	ConfigApproval               = lifecycle.ConfigApproval
	Inventory                    = lifecycle.Inventory
	Source                       = lifecycle.Source
	Cache                        = lifecycle.Cache
	FileCache                    = lifecycle.FileCache
	SourceOptions                = lifecycle.SourceOptions
	InventoryServiceOptions      = lifecycle.InventoryServiceOptions
	InventoryService             = lifecycle.InventoryService
	RemovalRequest               = lifecycle.RemovalRequest
	RemovalSessionCondition      = lifecycle.RemovalSessionCondition
	RemovalResult                = lifecycle.RemovalResult
	Remover                      = lifecycle.Remover
	RemovalServiceOptions        = lifecycle.RemovalServiceOptions
	ProjectRemovalRequest        = lifecycle.ProjectRemovalRequest
	ProjectRemovalResult         = lifecycle.ProjectRemovalResult
	ProjectRemover               = lifecycle.ProjectRemover
	ProjectRemovalServiceOptions = lifecycle.ProjectRemovalServiceOptions
	FileState                    = status.FileState
	FileChange                   = status.FileChange
	ChangeState                  = status.ChangeState
	ChangeSummary                = status.ChangeSummary
	ChangeSet                    = status.ChangeSet
	InspectionRequest            = status.InspectionRequest
	WorktreeIdentity             = status.WorktreeIdentity
	InspectionResult             = status.InspectionResult
	Inspector                    = status.Inspector
	InspectionServiceOptions     = status.InspectionServiceOptions
)

const (
	ViewProjects   = lifecycle.ViewProjects
	ViewRepository = lifecycle.ViewRepository
	ViewGlobal     = lifecycle.ViewGlobal
	ViewDashboard  = lifecycle.ViewDashboard

	DefaultRefreshTimeout = lifecycle.DefaultRefreshTimeout

	Fresh      = lifecycle.Fresh
	Refreshing = lifecycle.Refreshing
	Stale      = lifecycle.Stale

	RequireConfigInteraction = lifecycle.RequireConfigInteraction
	IgnoreUntrustedConfig    = lifecycle.IgnoreUntrustedConfig

	FileStateModified   = status.FileStateModified
	FileStateAdded      = status.FileStateAdded
	FileStateDeleted    = status.FileStateDeleted
	FileStateRenamed    = status.FileStateRenamed
	FileStateCopied     = status.FileStateCopied
	FileStateConflicted = status.FileStateConflicted
	FileStateUntracked  = status.FileStateUntracked

	ChangeStateClean      = status.ChangeStateClean
	ChangeStateModified   = status.ChangeStateModified
	ChangeStateStaged     = status.ChangeStateStaged
	ChangeStateConflicted = status.ChangeStateConflicted
)

func CaptureExpansionContext() (ExpansionContext, error) {
	return lifecycle.CaptureExpansionContext()
}

func NewFileCache(home string) (*FileCache, *Diagnostic, error) {
	return lifecycle.NewFileCache(home)
}

func NewSource(options SourceOptions) Source {
	return lifecycle.NewSource(options)
}

func NewInventoryService(options InventoryServiceOptions) *InventoryService {
	return lifecycle.NewInventoryService(options)
}

func NewRemovalService(options RemovalServiceOptions) Remover {
	return lifecycle.NewRemovalService(options)
}

func NewProjectRemovalService(options ProjectRemovalServiceOptions) ProjectRemover {
	return lifecycle.NewProjectRemovalService(options)
}

func NewInspectionService(options InspectionServiceOptions) Inspector {
	return status.NewInspectionService(options)
}
