package cmd

import (
	"fmt"

	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/utils"
)

func unreviewedRemoteSourcePaths() (map[string]struct{}, error) {
	reg, err := registry.New()
	if err != nil {
		return nil, fmt.Errorf("open worktree registry: %w", err)
	}
	paths := reg.UnreviewedRemoteSourcePaths()
	result := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		result[utils.CanonicalPath(path)] = struct{}{}
	}
	return result, nil
}

func pathSetContains(paths map[string]struct{}, path string) bool {
	_, ok := paths[utils.CanonicalPath(path)]
	return ok
}

func isUnreviewedRemoteSourcePath(path string) (bool, error) {
	reg, err := registry.New()
	if err != nil {
		return false, fmt.Errorf("open worktree registry: %w", err)
	}
	paths := reg.UnreviewedRemoteSourcePaths()
	live := make(map[string]struct{}, len(paths))
	for _, registeredPath := range paths {
		live[utils.CanonicalPath(registeredPath)] = struct{}{}
	}
	return pathSetContains(live, path), nil
}

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
