package worktree

import (
	"context"
	"fmt"
	"path/filepath"
)

// StructuralCondition captures the reviewed state before repository-wide repair.
type StructuralCondition struct {
	Path, GitDir, DotGitTarget, Generation string
	Exists                                 bool
}
type MaintenanceRequest struct {
	Expected      []StructuralCondition
	Repair, Prune bool
}

func (r *Repository) Maintain(ctx context.Context, req MaintenanceRequest) (inventory Inventory, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { inventory, err = s.Maintain(ctx, req); return err })
	return inventory, err
}

// Maintain repairs backlinks before pruning, and refuses to affect registrations
// that were not present in the caller's reviewed snapshot.
func (s *Scope) Maintain(ctx context.Context, req MaintenanceRequest) (Inventory, error) {
	if err := s.check(ctx); err != nil {
		return Inventory{}, err
	}
	if err := s.rejectCreation(); err != nil {
		return Inventory{}, err
	}
	current, err := s.Inspect(ctx)
	if err != nil {
		return current, err
	}
	if err := validateStructuralConditions(current.Entries, req.Expected); err != nil {
		return current, err
	}
	if req.Repair {
		if err := validateRepairScope(current.Entries, req.Expected); err != nil {
			return current, err
		}
		if _, err := s.repo.run(ctx, s.repo.path, "worktree", "repair"); err != nil {
			return current, fmt.Errorf("repair worktree backlinks: %w", err)
		}
		current, err = s.Inspect(ctx)
		if err != nil {
			return current, err
		}
	}
	if req.Prune {
		if err := validatePruneScope(current.Entries, req.Expected); err != nil {
			return current, err
		}
		if _, err := s.repo.run(ctx, s.repo.path, "worktree", "prune", "--expire", "now"); err != nil {
			return current, fmt.Errorf("prune missing worktrees: %w", err)
		}
	}
	return s.Inspect(ctx)
}

func validateRepairScope(
	current []Entry,
	expected []StructuralCondition,
) error {
	expectedRepair := make(map[string]bool, len(expected))
	for _, condition := range expected {
		if condition.Exists &&
			comparableWorktreePath(condition.DotGitTarget) !=
				comparableWorktreePath(condition.GitDir) {
			expectedRepair[comparableWorktreePath(condition.Path)] = true
		}
	}
	for _, inspection := range current {
		if inspection.Exists &&
			comparableWorktreePath(inspection.DotGitTarget) !=
				comparableWorktreePath(inspection.GitDir) &&
			!expectedRepair[comparableWorktreePath(inspection.Path)] {
			return fmt.Errorf(
				"unexpected repairable worktree %s prevents repository-wide repair",
				inspection.Path,
			)
		}
	}
	return nil
}

func validatePruneScope(
	current []Entry,
	expected []StructuralCondition,
) error {
	expectedMissing := make(map[string]StructuralCondition, len(expected))
	for _, condition := range expected {
		if !condition.Exists {
			expectedMissing[comparableWorktreePath(condition.Path)] = condition
		}
	}
	for _, inspection := range current {
		if !inspection.Prunable {
			continue
		}
		condition, expected := expectedMissing[comparableWorktreePath(inspection.Path)]
		if !expected {
			return fmt.Errorf(
				"unexpected prunable worktree %s prevents repository-wide metadata pruning",
				inspection.Path,
			)
		}
		if pathKey(inspection.GitDir) != pathKey(condition.GitDir) ||
			filepath.Clean(inspection.DotGitTarget) != filepath.Clean(condition.DotGitTarget) ||
			inspection.Generation != condition.Generation || inspection.Exists {
			return fmt.Errorf(
				"worktree structural state changed for %s before repository-wide metadata pruning",
				inspection.Path,
			)
		}
	}
	return nil
}

func validateStructuralConditions(
	current []Entry,
	expected []StructuralCondition,
) error {
	byPath := make(map[string]Entry, len(current))
	for _, inspection := range current {
		byPath[comparableWorktreePath(inspection.Path)] = inspection
	}
	for _, condition := range expected {
		inspection, ok := byPath[comparableWorktreePath(condition.Path)]
		if !ok ||
			pathKey(inspection.GitDir) != pathKey(condition.GitDir) ||
			filepath.Clean(inspection.DotGitTarget) != filepath.Clean(condition.DotGitTarget) ||
			inspection.Generation != condition.Generation ||
			inspection.Exists != condition.Exists {
			return fmt.Errorf(
				"worktree structural state changed for %s",
				condition.Path,
			)
		}
	}
	return nil
}
