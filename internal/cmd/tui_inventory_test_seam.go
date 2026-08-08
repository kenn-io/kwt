package cmd

import (
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/pkg/models"
)

// discoverLegacyTUIWorktrees supports the direct-discovery unit-test seam in
// tuiBackend. Production TUI construction always replaces that seam with the
// daemon inventory bridge.
func discoverLegacyTUIWorktrees(
	baseDirectory string,
	projects []models.Project,
) ([]*discovery.GlobalWorktreeEntry, error) {
	return discovery.DiscoverGlobalWorktrees(baseDirectory, projects)
}
