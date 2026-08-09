// Package kwt provides embeddable worktree inventory and lifecycle services.
package kwt

import (
	"context"

	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/pkg/models"
)

type (
	View                    = lifecycle.View
	Freshness               = lifecycle.Freshness
	UntrustedConfigPolicy   = lifecycle.UntrustedConfigPolicy
	Request                 = lifecycle.Request
	ExpansionContext        = lifecycle.ExpansionContext
	Repository              = lifecycle.Repository
	Entry                   = lifecycle.Entry
	Note                    = lifecycle.Note
	Diagnostic              = lifecycle.Diagnostic
	Snapshot                = lifecycle.Snapshot
	Result                  = lifecycle.Result
	ConfigApproval          = lifecycle.ConfigApproval
	Inventory               = lifecycle.Inventory
	Source                  = lifecycle.Source
	Cache                   = lifecycle.Cache
	FileCache               = lifecycle.FileCache
	SourceOptions           = lifecycle.SourceOptions
	InventoryServiceOptions = lifecycle.InventoryServiceOptions
	InventoryService        = lifecycle.InventoryService
	RemovalRequest          = lifecycle.RemovalRequest
	RemovalResult           = lifecycle.RemovalResult
	Remover                 = lifecycle.Remover
	RemovalServiceOptions   = lifecycle.RemovalServiceOptions
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

func CanonicalProjects(
	ctx context.Context,
	projects []models.Project,
	protectedNames ...string,
) ([]models.Project, error) {
	return lifecycle.CanonicalProjects(ctx, projects, protectedNames...)
}
