package cmd

import (
	"fmt"

	"go.kenn.io/kwt/internal/registry"
)

var acknowledgeRemoteSourcePath = func(path string) error {
	reg, err := registry.New()
	if err != nil {
		return fmt.Errorf("open worktree registry: %w", err)
	}
	if err := reg.AcknowledgeRemoteSource(path); err != nil {
		return fmt.Errorf("acknowledge remote-source worktree: %w", err)
	}
	return nil
}
