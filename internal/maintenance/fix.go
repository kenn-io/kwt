package maintenance

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kwt/internal/config"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/pkg/models"
)

// RegistryMutator is the narrow registry surface needed after Git maintenance
// releases its repository lock.
type RegistryMutator interface {
	UnregisterIfGeneration(string, string) (bool, error)
	CompareAndSwap(string, *registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error)
	CompareAndSwapAliases([]*registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error)
	AcquireCreation(string) (func() error, bool, error)
}

// ProjectMutator is the narrow configuration surface used after Git and
// registry maintenance have released their locks.
type ProjectMutator interface {
	RemoveProject(context.Context, config.ProjectRegistration) (bool, error)
	RelocateProject(context.Context, config.ProjectRegistration, models.Project) (bool, error)
}

// Fixer applies only findings whose inspection established unique ownership.
type Fixer struct {
	Registry               RegistryMutator
	RegistryEntries        []*registry.WorktreeEntry
	MaintainRepository     func(string, gitadapter.WorktreeMaintenanceRequest) ([]gitadapter.WorktreeInspection, error)
	PathExists             func(string) (bool, error)
	Projects               ProjectMutator
	InspectRepository      func(string) (RepositorySnapshot, error)
	WithWorktreeGeneration func(string, string, func() error) error
}

// Fix repairs backlinks before native metadata pruning per repository, then
// conditionally removes unchanged stale registry entries after Git unlocks.
func (f *Fixer) Fix(ctx context.Context, report Report) error {
	f.setDefaults()
	entries := make(map[string]*registry.WorktreeEntry, len(f.RegistryEntries))
	for _, entry := range f.RegistryEntries {
		if entry == nil {
			continue
		}
		entries[pathKey(entry.Path)] = entry
	}
	for _, repositoryReport := range report.Repositories {
		if err := ctx.Err(); err != nil {
			return err
		}
		blocked := make(map[string]bool)
		structural := make(map[string]FindingCode)
		var repairBacklinks bool
		var repairBacklinksBlocked bool
		var pruneMissing bool
		var pruneMissingBlocked bool
		for _, finding := range repositoryReport.Findings {
			key := pathKey(finding.Path)
			switch finding.Code {
			case AmbiguousWorktreeBacklink:
				blocked[key] = true
				repairBacklinksBlocked = true
			case RepositoryIdentityMismatch:
				blocked[key] = true
			case BrokenWorktreeBacklink:
				if finding.Fixable {
					repairBacklinks = true
					structural[key] = finding.Code
				} else {
					repairBacklinksBlocked = true
				}
			case MissingWorktreeDirectory:
				if finding.Fixable {
					pruneMissing = true
					structural[key] = finding.Code
				} else {
					pruneMissingBlocked = true
				}
			}
		}
		if repairBacklinksBlocked {
			repairBacklinks = false
			for path, code := range structural {
				if code == BrokenWorktreeBacklink {
					delete(structural, path)
				}
			}
		}
		if pruneMissingBlocked {
			pruneMissing = false
			for path, code := range structural {
				if code == MissingWorktreeDirectory {
					delete(structural, path)
				}
			}
		}
		if repairBacklinks || pruneMissing {
			request := gitadapter.WorktreeMaintenanceRequest{
				RepairBacklinks: repairBacklinks,
				PruneMissing:    pruneMissing,
			}
			for _, inspection := range repositoryReport.Worktrees {
				if _, ok := structural[pathKey(inspection.Path)]; !ok {
					continue
				}
				request.Expected = append(request.Expected, gitadapter.WorktreeStructuralCondition{
					Path:         inspection.Path,
					GitDir:       inspection.GitDir,
					DotGitTarget: inspection.DotGitTarget,
					Generation:   inspection.Generation,
					Exists:       inspection.Exists,
				})
			}
			if _, err := f.MaintainRepository(repositoryReport.Root, request); err != nil {
				return fmt.Errorf("maintain worktrees for %s: %w", repositoryReport.Root, err)
			}
		}

		for _, finding := range repositoryReport.Findings {
			condition := finding.RegistryAliasRepair
			if finding.Code != DuplicateRegistryEntry || !finding.Fixable ||
				condition == nil || f.Registry == nil {
				continue
			}
			if condition.Retained == nil {
				confirmedAbsent := true
				for _, entry := range condition.Expected {
					if entry == nil {
						confirmedAbsent = false
						break
					}
					exists, err := f.PathExists(entry.Path)
					if err != nil {
						return fmt.Errorf(
							"recheck stale registry alias path %s: %w",
							entry.Path,
							err,
						)
					}
					if exists {
						confirmedAbsent = false
						break
					}
				}
				if !confirmedAbsent {
					continue
				}
			}
			if _, err := f.Registry.CompareAndSwapAliases(
				condition.Expected,
				condition.Retained,
			); err != nil {
				return fmt.Errorf(
					"collapse duplicate registry aliases for %s: %w",
					finding.Path,
					err,
				)
			}
		}

		inspections := make(map[string]gitadapter.WorktreeInspection, len(repositoryReport.Worktrees))
		for _, inspection := range repositoryReport.Worktrees {
			inspections[pathKey(inspection.Path)] = inspection
		}
		for _, finding := range repositoryReport.Findings {
			key := pathKey(finding.Path)
			if finding.Code != RegistryGenerationMismatch || !finding.Fixable || blocked[key] {
				continue
			}
			entry := entries[key]
			inspection, ok := inspections[key]
			if entry == nil || entry.Generation != "" || !ok || f.Registry == nil ||
				inspection.GenerationStatus != gitadapter.GenerationValid {
				continue
			}
			if err := f.withInactiveCreation(entry, func() error {
				err := f.WithWorktreeGeneration(entry.Path, inspection.Generation, func() error {
					exists, err := f.PathExists(entry.Path)
					if err != nil {
						return fmt.Errorf("recheck registry generation path %s: %w", entry.Path, err)
					}
					if !exists {
						return nil
					}
					replacement := *entry
					replacement.Generation = inspection.Generation
					replacement.CreationToken = ""
					replacement.ExpiresAt = nil
					if _, err := f.Registry.CompareAndSwap(entry.Path, entry, &replacement); err != nil {
						return fmt.Errorf("reconcile registry generation for %s: %w", entry.Path, err)
					}
					return nil
				})
				var conditionErr *gitadapter.ConditionError
				if errors.As(err, &conditionErr) && conditionErr.Reason == gitadapter.ReasonGenerationChanged {
					return nil
				}
				return err
			}); err != nil {
				return err
			}
		}

		for _, finding := range repositoryReport.Findings {
			if finding.Code != StaleRegistryEntry || !finding.Fixable ||
				blocked[pathKey(finding.Path)] {
				continue
			}
			entry := entries[pathKey(finding.Path)]
			if entry == nil || f.Registry == nil {
				continue
			}
			if err := f.withInactiveCreation(entry, func() error {
				exists, err := f.PathExists(entry.Path)
				if err != nil {
					return fmt.Errorf("recheck stale registry path %s: %w", entry.Path, err)
				}
				if exists {
					return nil
				}
				if entry.CreationToken == "" && gitadapter.ValidateWorktreeGeneration(entry.Generation) == nil {
					if _, err := f.Registry.UnregisterIfGeneration(entry.Path, entry.Generation); err != nil {
						return fmt.Errorf("unregister stale worktree %s: %w", entry.Path, err)
					}
					return nil
				}
				if _, err := f.Registry.CompareAndSwap(entry.Path, entry, nil); err != nil {
					return fmt.Errorf("remove stale legacy registry entry %s: %w", entry.Path, err)
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	if f.Projects == nil {
		return nil
	}
	for _, repositoryReport := range report.Repositories {
		for _, finding := range repositoryReport.Findings {
			if err := ctx.Err(); err != nil {
				return err
			}
			condition := finding.ProjectRepair
			if !finding.Fixable || condition == nil {
				continue
			}
			exists, err := f.PathExists(finding.Path)
			if err != nil {
				return fmt.Errorf("recheck configured project path %s: %w", finding.Path, err)
			}
			if exists {
				continue
			}

			switch condition.Action {
			case RemoveProject:
				if _, err := f.Projects.RemoveProject(ctx, condition.Expected); err != nil {
					return fmt.Errorf("remove stale project registration %s: %w", finding.Path, err)
				}
			case RelocateProject:
				target, err := f.InspectRepository(condition.TargetRoot)
				if err != nil {
					return fmt.Errorf("recheck project relocation target %s: %w", condition.TargetRoot, err)
				}
				targetIdentity, identityOK := repairableProjectIdentity(target.RepositoryIdentity)
				if pathKey(target.Root) != pathKey(condition.TargetRoot) ||
					pathKey(target.CommonDir) != pathKey(condition.TargetCommonDir) ||
					!identityOK || !repositoryIdentityMatchesAny(targetIdentity, condition.TargetRepository) {
					continue
				}
				replacement := condition.Expected.Persisted
				replacement.Path = condition.TargetRoot
				replacement.Repository = condition.TargetRepository
				if _, err := f.Projects.RelocateProject(ctx, condition.Expected, replacement); err != nil {
					return fmt.Errorf("relocate project registration %s: %w", finding.Path, err)
				}
			}
		}
	}
	return nil
}

func (f *Fixer) withInactiveCreation(
	entry *registry.WorktreeEntry,
	change func() error,
) error {
	if entry.CreationToken == "" {
		return change()
	}
	release, acquired, err := f.Registry.AcquireCreation(entry.Path)
	if err != nil {
		return fmt.Errorf("inspect registry creation ownership for %s: %w", entry.Path, err)
	}
	if !acquired {
		return nil
	}
	changeErr := change()
	releaseErr := release()
	if changeErr != nil {
		return changeErr
	}
	if releaseErr != nil {
		return fmt.Errorf("release registry creation ownership for %s: %w", entry.Path, releaseErr)
	}
	return nil
}

func (f *Fixer) setDefaults() {
	if f.MaintainRepository == nil {
		f.MaintainRepository = func(
			root string,
			request gitadapter.WorktreeMaintenanceRequest,
		) ([]gitadapter.WorktreeInspection, error) {
			return gitadapter.New(root).MaintainWorktrees(request)
		}
	}
	if f.PathExists == nil {
		f.PathExists = pathExists
	}
	if f.InspectRepository == nil {
		inspector := &Inspector{Config: &models.Config{}}
		inspector.setDefaults()
		f.InspectRepository = inspector.inspectRepository
	}
	if f.WithWorktreeGeneration == nil {
		f.WithWorktreeGeneration = func(path, expected string, operation func() error) error {
			return gitadapter.New(path).WithWorktreeGeneration(path, expected, operation)
		}
	}
}
