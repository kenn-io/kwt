package cmd

import (
	"go.kenn.io/kwt/internal/registry"
)

var newRemoteSourceRegistry = registry.New

var acknowledgeRemoteSourcePath = func(path string) error {
	reg, err := newRemoteSourceRegistry()
	if err != nil {
		// Opening a workspace is the explicit review action. Registry
		// bookkeeping must not prevent that action for unrelated worktrees.
		return nil
	}
	if !reg.IsUnreviewedRemoteSource(path) {
		return nil
	}
	// A failed write may leave the marker for a later retry, but the explicit
	// open remains authoritative and should continue.
	_ = reg.AcknowledgeRemoteSource(path)
	return nil
}
