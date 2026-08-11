package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/pkg/models"
)

func TestFixerRepairsBeforePruningAndRegistryCleanup(t *testing.T) {
	generation := "0123456789abcdef0123456789abcdef"
	entry := &registry.WorktreeEntry{
		Path: "/worktrees/missing", Repository: "github.com/acme/widget",
		Generation: generation,
	}
	var calls []string
	store := &fakeRegistryMutator{
		unregister: func(path, gotGeneration string) (bool, error) {
			calls = append(calls, "unregister")
			assert.Equal(t, entry.Path, path)
			assert.Equal(t, generation, gotGeneration)
			return true, nil
		},
	}
	fixer := &Fixer{
		Registry:        store,
		RegistryEntries: []*registry.WorktreeEntry{entry},
		MaintainRepository: func(root string, request gitadapter.WorktreeMaintenanceRequest) ([]gitadapter.WorktreeInspection, error) {
			calls = append(calls, "maintain")
			assert.Equal(t, "/repos/widget", root)
			assert.True(t, request.RepairBacklinks)
			assert.True(t, request.PruneMissing)
			require.Len(t, request.Expected, 2)
			return nil, nil
		},
		PathExists: func(string) (bool, error) { return false, nil },
	}
	report := Report{Repositories: []RepositoryReport{{
		Root: "/repos/widget",
		Worktrees: []gitadapter.WorktreeInspection{
			{Path: "/worktrees/broken", GitDir: "/repos/widget/.git/worktrees/broken", DotGitTarget: "/old/widget/.git/worktrees/broken", Exists: true},
			{Path: "/worktrees/missing", GitDir: "/repos/widget/.git/worktrees/missing", Generation: generation, Exists: false},
		},
		Findings: []Finding{
			{Code: BrokenWorktreeBacklink, Path: "/worktrees/broken", Fixable: true},
			{Code: MissingWorktreeDirectory, Path: "/worktrees/missing", Fixable: true},
			{Code: StaleRegistryEntry, Path: "/worktrees/missing", Fixable: true},
		},
	}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Equal(t, []string{"maintain", "unregister"}, calls)
}

func TestFixerSkipsRepositoryPruneWhenAnyMissingRecordIsNotFixable(t *testing.T) {
	var mutationCalls int
	fixer := &Fixer{
		MaintainRepository: func(string, gitadapter.WorktreeMaintenanceRequest) ([]gitadapter.WorktreeInspection, error) {
			mutationCalls++
			return nil, nil
		},
	}
	report := Report{Repositories: []RepositoryReport{{
		Root: "/repos/widget",
		Worktrees: []gitadapter.WorktreeInspection{
			{Path: "/worktrees/fixable", Exists: false, Prunable: true},
			{Path: "/worktrees/ambiguous", Exists: false, Prunable: true},
		},
		Findings: []Finding{
			{Code: MissingWorktreeDirectory, Path: "/worktrees/fixable", Fixable: true},
			{Code: AmbiguousWorktreeBacklink, Path: "/worktrees/ambiguous", Fixable: false},
			{Code: MissingWorktreeDirectory, Path: "/worktrees/ambiguous", Fixable: false},
		},
	}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Zero(t, mutationCalls)
}

func TestFixerSkipsBacklinkRepairButContinuesOtherSafeCleanup(t *testing.T) {
	generation := "0123456789abcdef0123456789abcdef"
	entry := &registry.WorktreeEntry{
		Path: "/worktrees/stale", Generation: generation,
	}
	var calls []string
	fixer := &Fixer{
		Registry:        &fakeRegistryMutator{unregister: func(string, string) (bool, error) { calls = append(calls, "unregister"); return true, nil }},
		RegistryEntries: []*registry.WorktreeEntry{entry},
		MaintainRepository: func(_ string, request gitadapter.WorktreeMaintenanceRequest) ([]gitadapter.WorktreeInspection, error) {
			if request.RepairBacklinks {
				return nil, errors.New("unexpected partial backlink repair")
			}
			calls = append(calls, "prune")
			require.True(t, request.PruneMissing)
			require.Len(t, request.Expected, 1)
			assert.Equal(t, "/worktrees/missing", request.Expected[0].Path)
			return nil, nil
		},
		PathExists: func(string) (bool, error) { return false, nil },
	}
	report := Report{Repositories: []RepositoryReport{{
		Root: "/repos/widget",
		Worktrees: []gitadapter.WorktreeInspection{
			{Path: "/worktrees/fixable-backlink", Exists: true},
			{Path: "/worktrees/ambiguous-backlink", Exists: true},
			{Path: "/worktrees/missing", Generation: generation, Exists: false},
		},
		Findings: []Finding{
			{Code: BrokenWorktreeBacklink, Path: "/worktrees/fixable-backlink", Fixable: true},
			{Code: AmbiguousWorktreeBacklink, Path: "/worktrees/ambiguous-backlink", Fixable: false},
			{Code: MissingWorktreeDirectory, Path: "/worktrees/missing", Fixable: true},
			{Code: StaleRegistryEntry, Path: entry.Path, Fixable: true},
		},
	}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Equal(t, []string{"prune", "unregister"}, calls)
}

func TestFixerDeletesGenerationlessStaleRegistryEntryByFullEntryCAS(t *testing.T) {
	observed := &registry.WorktreeEntry{
		Path: "/worktrees/legacy", Repository: "github.com/acme/widget", Branch: "legacy",
	}
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{{
		Code: StaleRegistryEntry, Path: observed.Path, Fixable: true,
	}}}}}

	t.Run("unchanged entry", func(t *testing.T) {
		var removed bool
		fixer := &Fixer{
			Registry: &fakeRegistryMutator{compareAndSwap: func(path string, expected, replacement *registry.WorktreeEntry) (bool, error) {
				assert.Equal(t, observed.Path, path)
				assert.Equal(t, observed, expected)
				assert.Nil(t, replacement)
				removed = true
				return true, nil
			}},
			RegistryEntries: []*registry.WorktreeEntry{observed},
			PathExists:      func(string) (bool, error) { return false, nil },
		}

		err := fixer.Fix(context.Background(), report)

		require.NoError(t, err)
		assert.True(t, removed)
	})

	t.Run("concurrent replacement", func(t *testing.T) {
		replacement := *observed
		replacement.Branch = "replacement"
		current := &replacement
		fixer := &Fixer{
			Registry: &fakeRegistryMutator{compareAndSwap: func(_ string, expected, _ *registry.WorktreeEntry) (bool, error) {
				assert.Equal(t, observed, expected)
				return false, nil
			}},
			RegistryEntries: []*registry.WorktreeEntry{observed},
			PathExists:      func(string) (bool, error) { return false, nil },
		}

		err := fixer.Fix(context.Background(), report)

		require.NoError(t, err)
		assert.Equal(t, "replacement", current.Branch)
	})

	t.Run("active creation", func(t *testing.T) {
		creating := *observed
		creating.CreationToken = "creation-owner"
		var compared bool
		fixer := &Fixer{
			Registry: &fakeRegistryMutator{
				acquireCreation: func(string) (func() error, bool, error) {
					return nil, false, nil
				},
				compareAndSwap: func(string, *registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error) {
					compared = true
					return true, nil
				},
			},
			RegistryEntries: []*registry.WorktreeEntry{&creating},
			PathExists:      func(string) (bool, error) { return false, nil },
		}

		err := fixer.Fix(context.Background(), report)

		require.NoError(t, err)
		assert.False(t, compared)
	})
}

func TestFixerCleansAbandonedCreationUnderPathLock(t *testing.T) {
	entry := &registry.WorktreeEntry{
		Path: "/worktrees/abandoned", Branch: "topic", CreationToken: "abandoned",
	}
	var calls []string
	store := &fakeRegistryMutator{
		acquireCreation: func(path string) (func() error, bool, error) {
			assert.Equal(t, entry.Path, path)
			calls = append(calls, "lock")
			return func() error { calls = append(calls, "unlock"); return nil }, true, nil
		},
		compareAndSwap: func(path string, expected, replacement *registry.WorktreeEntry) (bool, error) {
			calls = append(calls, "remove")
			assert.Equal(t, entry.Path, path)
			assert.Equal(t, entry, expected)
			assert.Nil(t, replacement)
			return true, nil
		},
	}
	fixer := &Fixer{
		Registry: store, RegistryEntries: []*registry.WorktreeEntry{entry},
		PathExists: func(string) (bool, error) { return false, nil },
	}
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{{
		Code: StaleRegistryEntry, Path: entry.Path, Fixable: true,
	}}}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Equal(t, []string{"lock", "remove", "unlock"}, calls)
}

func TestFixerPreservesNonemptyRegistryGenerationMismatch(t *testing.T) {
	gitGeneration := "0123456789abcdef0123456789abcdef"
	observed := &registry.WorktreeEntry{
		Path:       "/worktrees/topic",
		Repository: "github.com/acme/widget",
		Branch:     "topic",
		Generation: "fedcba9876543210fedcba9876543210",
	}
	report := Report{Repositories: []RepositoryReport{{
		Worktrees: []gitadapter.WorktreeInspection{{
			Path:             observed.Path,
			Exists:           true,
			Generation:       gitGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		}},
		Findings: []Finding{{
			Code: RegistryGenerationMismatch, Path: observed.Path, Fixable: true,
		}},
	}}}
	var swapped bool
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{compareAndSwap: func(string, *registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error) {
			swapped = true
			return true, nil
		}},
		RegistryEntries: []*registry.WorktreeEntry{observed},
		PathExists:      func(string) (bool, error) { return true, nil },
		WithWorktreeGeneration: func(_ string, _ string, operation func() error) error {
			return operation()
		},
	}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.False(t, swapped)
}

func TestFixerCollapsesOnlyCompleteEquivalentRegistryAliasGroup(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	worktreePath := t.TempDir()
	aliasPath := filepath.Dir(worktreePath) + string(os.PathSeparator) + "." +
		string(os.PathSeparator) + filepath.Base(worktreePath)
	first := &registry.WorktreeEntry{
		Path: worktreePath, Repository: "github.com/acme/widget",
		Branch: "feature/topic", Generation: generation,
	}
	second := *first
	second.Path = aliasPath
	stale := &registry.WorktreeEntry{
		Path: filepath.Join(t.TempDir(), "stale"), Generation: generation,
	}
	aliasCalls := 0
	staleCalls := 0
	mutator := &fakeRegistryMutator{
		compareAliases: func(
			expected []*registry.WorktreeEntry,
			retained *registry.WorktreeEntry,
		) (bool, error) {
			aliasCalls++
			assert.Equal(t, []*registry.WorktreeEntry{first, &second}, expected)
			assert.Equal(t, first, retained)
			return true, nil
		},
		unregister: func(path, gotGeneration string) (bool, error) {
			staleCalls++
			assert.Equal(t, stale.Path, path)
			assert.Equal(t, stale.Generation, gotGeneration)
			return true, nil
		},
	}
	condition := &RegistryAliasRepairCondition{
		Expected: []*registry.WorktreeEntry{first, &second},
		Retained: first,
	}
	fixer := &Fixer{
		Registry:        mutator,
		RegistryEntries: []*registry.WorktreeEntry{first, &second, stale},
		PathExists: func(path string) (bool, error) {
			return path != stale.Path, nil
		},
	}
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{
		{
			Code: DuplicateRegistryEntry, Path: first.Path, Fixable: true,
			RegistryAliasRepair: condition,
		},
		{Code: StaleRegistryEntry, Path: stale.Path, Fixable: true},
		{
			Code: DuplicateRegistryEntry, Path: "/manual", Fixable: false,
			RegistryAliasRepair: condition,
		},
	}}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Equal(t, 1, aliasCalls)
	assert.Equal(t, 1, staleCalls)
}

func TestFixerRemovesCompleteMissingRegistryAliasGroup(t *testing.T) {
	realParent := t.TempDir()
	worktreePath := filepath.Join(realParent, "missing-worktree")
	aliasPath := realParent + string(os.PathSeparator) + "." +
		string(os.PathSeparator) + "missing-worktree"
	first := &registry.WorktreeEntry{
		Path: worktreePath, Repository: "github.com/acme/widget",
		Branch: "feature/topic",
	}
	second := *first
	second.Path = aliasPath
	aliasCalls := 0
	singleEntryCalls := 0
	mutator := &fakeRegistryMutator{
		compareAliases: func(
			expected []*registry.WorktreeEntry,
			retained *registry.WorktreeEntry,
		) (bool, error) {
			aliasCalls++
			assert.Equal(t, []*registry.WorktreeEntry{first, &second}, expected)
			assert.Nil(t, retained)
			return true, nil
		},
		compareAndSwap: func(
			string,
			*registry.WorktreeEntry,
			*registry.WorktreeEntry,
		) (bool, error) {
			singleEntryCalls++
			return true, nil
		},
		unregister: func(string, string) (bool, error) {
			singleEntryCalls++
			return true, nil
		},
	}
	condition := &RegistryAliasRepairCondition{
		Expected: []*registry.WorktreeEntry{first, &second},
	}
	fixer := &Fixer{
		Registry:        mutator,
		RegistryEntries: []*registry.WorktreeEntry{first, &second},
	}
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{{
		Code: DuplicateRegistryEntry, Path: first.Path, Fixable: true,
		RegistryAliasRepair: condition,
	}}}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Equal(t, 1, aliasCalls)
	assert.Zero(t, singleEntryCalls)
}

func TestFixerPreservesMissingRegistryAliasGroupWhenPathReappears(t *testing.T) {
	first := &registry.WorktreeEntry{
		Path: "/worktrees/reused", Repository: "github.com/acme/widget",
		Branch: "feature/topic",
	}
	second := *first
	second.Path = "/worktrees/./reused"
	aliasCalls := 0
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{compareAliases: func(
			[]*registry.WorktreeEntry,
			*registry.WorktreeEntry,
		) (bool, error) {
			aliasCalls++
			return true, nil
		}},
		RegistryEntries: []*registry.WorktreeEntry{first, &second},
		PathExists: func(string) (bool, error) {
			return true, nil
		},
	}
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{{
		Code: DuplicateRegistryEntry, Path: first.Path, Fixable: true,
		RegistryAliasRepair: &RegistryAliasRepairCondition{
			Expected: []*registry.WorktreeEntry{first, &second},
		},
	}}}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Zero(t, aliasCalls)
}

func TestFixerAdoptsGenerationlessRegistryEntryWithoutExpiration(t *testing.T) {
	gitGeneration := "0123456789abcdef0123456789abcdef"
	expiresAt := time.Now().Add(-time.Hour)
	observed := &registry.WorktreeEntry{
		Path:       "/worktrees/legacy",
		Repository: "github.com/acme/widget",
		Branch:     "legacy",
		Generation: "",
		ExpiresAt:  &expiresAt,
	}
	report := Report{Repositories: []RepositoryReport{{
		Worktrees: []gitadapter.WorktreeInspection{{
			Path:             observed.Path,
			Exists:           true,
			Generation:       gitGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		}},
		Findings: []Finding{{
			Code: RegistryGenerationMismatch, Path: observed.Path, Fixable: true,
		}},
	}}}
	var swapped bool
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{compareAndSwap: func(path string, expected, replacement *registry.WorktreeEntry) (bool, error) {
			assert.Equal(t, observed.Path, path)
			assert.Equal(t, observed, expected)
			require.NotNil(t, replacement)
			assert.Equal(t, gitGeneration, replacement.Generation)
			assert.Nil(t, replacement.ExpiresAt)
			assert.Equal(t, observed.Branch, replacement.Branch)
			swapped = true
			return true, nil
		}},
		RegistryEntries: []*registry.WorktreeEntry{observed},
		PathExists:      func(string) (bool, error) { return true, nil },
		WithWorktreeGeneration: func(_ string, _ string, operation func() error) error {
			return operation()
		},
	}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.True(t, swapped)
}

func TestFixerDoesNotAdoptGenerationAfterWorktreeReplacement(t *testing.T) {
	inspectedGeneration := "0123456789abcdef0123456789abcdef"
	entry := &registry.WorktreeEntry{Path: "/worktrees/replaced"}
	report := Report{Repositories: []RepositoryReport{{
		Worktrees: []gitadapter.WorktreeInspection{{
			Path: entry.Path, Exists: true, Generation: inspectedGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		}},
		Findings: []Finding{{
			Code: RegistryGenerationMismatch, Path: entry.Path, Fixable: true,
		}},
	}}}
	var swapped bool
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{compareAndSwap: func(string, *registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error) {
			swapped = true
			return true, nil
		}},
		RegistryEntries: []*registry.WorktreeEntry{entry},
		PathExists:      func(string) (bool, error) { return true, nil },
		WithWorktreeGeneration: func(path, expected string, operation func() error) error {
			assert.Equal(t, entry.Path, path)
			assert.Equal(t, inspectedGeneration, expected)
			return &gitadapter.ConditionError{Reason: gitadapter.ReasonGenerationChanged, Path: path}
		},
	}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.False(t, swapped)
}

func TestFixerIgnoresGenerationChangeFromRealGitAdapter(t *testing.T) {
	const (
		inspectedGeneration   = "0123456789abcdef0123456789abcdef"
		replacementGeneration = "fedcba9876543210fedcba9876543210"
	)
	repositoryPath := newMaintenanceTestRepository(t)
	worktreePath := filepath.Join(t.TempDir(), "replacement")
	g := gitadapter.New(repositoryPath)
	_, err := g.RunCommand("branch", "replacement")
	require.NoError(t, err)
	_, err = g.RunCommand("worktree", "add", worktreePath, "replacement")
	require.NoError(t, err)
	gitDir, err := gitadapter.ReadWorktreeBacklink(worktreePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(gitDir, "kwt-generation"),
		[]byte(replacementGeneration+"\n"),
		0o600,
	))
	entry := &registry.WorktreeEntry{Path: worktreePath}
	report := Report{Repositories: []RepositoryReport{{
		Worktrees: []gitadapter.WorktreeInspection{{
			Path: worktreePath, Exists: true, Generation: inspectedGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		}},
		Findings: []Finding{{
			Code: RegistryGenerationMismatch, Path: worktreePath, Fixable: true,
		}},
	}}}
	var swapped bool
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{compareAndSwap: func(
			string,
			*registry.WorktreeEntry,
			*registry.WorktreeEntry,
		) (bool, error) {
			swapped = true
			return true, nil
		}},
		RegistryEntries: []*registry.WorktreeEntry{entry},
		PathExists:      func(string) (bool, error) { return true, nil },
	}

	err = fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.False(t, swapped)
}

func TestFixerAdoptsGenerationThroughSymlinkedRegistryPath(t *testing.T) {
	gitGeneration := "0123456789abcdef0123456789abcdef"
	realParent := t.TempDir()
	realPath := filepath.Join(realParent, "workspace")
	require.NoError(t, os.Mkdir(realPath, 0o755))
	aliasParent := filepath.Join(t.TempDir(), "worktrees-link")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	entry := &registry.WorktreeEntry{
		Path: filepath.Join(aliasParent, "workspace"), Branch: "topic",
	}
	report := Report{Repositories: []RepositoryReport{{
		Worktrees: []gitadapter.WorktreeInspection{{
			Path:             realPath,
			Exists:           true,
			Generation:       gitGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		}},
		Findings: []Finding{{
			Code: RegistryGenerationMismatch, Path: entry.Path, Fixable: true,
		}},
	}}}
	var swapped bool
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{compareAndSwap: func(path string, expected, replacement *registry.WorktreeEntry) (bool, error) {
			assert.Equal(t, entry.Path, path)
			assert.Equal(t, entry, expected)
			require.NotNil(t, replacement)
			assert.Equal(t, gitGeneration, replacement.Generation)
			swapped = true
			return true, nil
		}},
		RegistryEntries: []*registry.WorktreeEntry{entry},
		PathExists:      func(string) (bool, error) { return true, nil },
		WithWorktreeGeneration: func(_ string, _ string, operation func() error) error {
			return operation()
		},
	}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.True(t, swapped)
}

func TestFixerPreservesAmbiguousFinding(t *testing.T) {
	var mutationCalls int
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{},
		MaintainRepository: func(string, gitadapter.WorktreeMaintenanceRequest) ([]gitadapter.WorktreeInspection, error) {
			mutationCalls++
			return nil, nil
		},
		PathExists: func(string) (bool, error) {
			mutationCalls++
			return false, nil
		},
	}
	report := Report{Repositories: []RepositoryReport{{
		Root: "/repos/widget",
		Findings: []Finding{{
			Code: AmbiguousWorktreeBacklink, Path: "/worktrees/topic", Fixable: false,
		}},
	}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Zero(t, mutationCalls)
}

func TestFixerRepairsProjectAfterRegistryCleanup(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	expected := models.Project{
		Repository: "https://github.com/acme/widget.git", Name: "Widget",
		Path: "~/old/widget", LastTouched: "2026-08-01T12:00:00Z",
	}
	entry := &registry.WorktreeEntry{Path: "/worktrees/stale", Generation: generation}
	condition := &ProjectRepairCondition{
		Action: RelocateProject, Expected: config.ProjectRegistration{Persisted: expected},
		TargetRoot: "/repos/widget", TargetCommonDir: "/repos/widget/.git",
		TargetRepository: "github.com/acme/widget",
	}
	var calls []string
	fixer := &Fixer{
		Registry: &fakeRegistryMutator{unregister: func(string, string) (bool, error) {
			calls = append(calls, "registry")
			return true, nil
		}},
		RegistryEntries: []*registry.WorktreeEntry{entry},
		Projects: &fakeProjectMutator{relocate: func(_ context.Context, got config.ProjectRegistration, replacement models.Project) (bool, error) {
			calls = append(calls, "project")
			assert.Equal(t, expected, got.Persisted)
			assert.Equal(t, models.Project{
				Repository: "github.com/acme/widget", Name: "Widget",
				Path: "/repos/widget", LastTouched: "2026-08-01T12:00:00Z",
			}, replacement)
			return true, nil
		}},
		InspectRepository: func(path string) (RepositorySnapshot, error) {
			calls = append(calls, "inspect")
			assert.Equal(t, condition.TargetRoot, path)
			return RepositorySnapshot{
				Root: condition.TargetRoot, CommonDir: condition.TargetCommonDir,
				RepositoryIdentity: condition.TargetRepository,
			}, nil
		},
		PathExists: func(path string) (bool, error) {
			return path != entry.Path && path != "/old/widget", nil
		},
	}
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{
		{Code: StaleRegistryEntry, Path: entry.Path, Fixable: true},
		{Code: ProjectPathMoved, Path: "/old/widget", Fixable: true, ProjectRepair: condition},
	}}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Equal(t, []string{"registry", "inspect", "project"}, calls)
}

func TestFixerRevalidatesProjectRepair(t *testing.T) {
	base := ProjectRepairCondition{
		Action: RelocateProject,
		Expected: config.ProjectRegistration{Persisted: models.Project{
			Repository: "github.com/acme/widget", Path: "/old/widget",
		}},
		TargetRoot: "/repos/widget", TargetCommonDir: "/repos/widget/.git",
		TargetRepository: "github.com/acme/widget",
	}
	tests := []struct {
		name       string
		oldExists  bool
		statErr    error
		snapshot   RepositorySnapshot
		inspectErr error
		wantErr    string
	}{
		{name: "old path recreated", oldExists: true},
		{name: "target root changed", snapshot: RepositorySnapshot{Root: "/repos/replacement", CommonDir: base.TargetCommonDir, RepositoryIdentity: base.TargetRepository}},
		{name: "target common directory changed", snapshot: RepositorySnapshot{Root: base.TargetRoot, CommonDir: "/repos/other/.git", RepositoryIdentity: base.TargetRepository}},
		{name: "target identity changed", snapshot: RepositorySnapshot{Root: base.TargetRoot, CommonDir: base.TargetCommonDir, RepositoryIdentity: "github.com/other/widget"}},
		{name: "old path access failure", statErr: errors.New("permission denied"), wantErr: "recheck configured project path"},
		{name: "target inspection failure", inspectErr: errors.New("repository unavailable"), wantErr: "recheck project relocation target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			condition := base
			var mutations int
			fixer := &Fixer{
				Projects: &fakeProjectMutator{relocate: func(context.Context, config.ProjectRegistration, models.Project) (bool, error) {
					mutations++
					return true, nil
				}},
				PathExists: func(string) (bool, error) { return tt.oldExists, tt.statErr },
				InspectRepository: func(string) (RepositorySnapshot, error) {
					return tt.snapshot, tt.inspectErr
				},
			}
			report := Report{Repositories: []RepositoryReport{{Findings: []Finding{{
				Code: ProjectPathMoved, Path: "/old/widget", Fixable: true,
				ProjectRepair: &condition,
			}}}}}

			err := fixer.Fix(context.Background(), report)

			assert.Zero(t, mutations)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestFixerRemovesOnlyFixableStaleProjectByCAS(t *testing.T) {
	expected := models.Project{Repository: "github.com/acme/widget", Path: "~/old/widget"}
	var calls int
	fixer := &Fixer{
		Projects: &fakeProjectMutator{remove: func(_ context.Context, got config.ProjectRegistration) (bool, error) {
			calls++
			assert.Equal(t, expected, got.Persisted)
			return false, nil
		}},
		PathExists: func(string) (bool, error) { return false, nil },
	}
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{
		{Code: StaleProjectRegistration, Path: "/old/widget", Fixable: true, ProjectRepair: &ProjectRepairCondition{Action: RemoveProject, Expected: config.ProjectRegistration{Persisted: expected}}},
		{Code: AmbiguousProjectRelocation, Path: "/other/widget", Fixable: false},
	}}}}

	err := fixer.Fix(context.Background(), report)

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

type fakeRegistryMutator struct {
	unregister      func(string, string) (bool, error)
	compareAndSwap  func(string, *registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error)
	compareAliases  func([]*registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error)
	acquireCreation func(string) (func() error, bool, error)
}

type fakeProjectMutator struct {
	remove   func(context.Context, config.ProjectRegistration) (bool, error)
	relocate func(context.Context, config.ProjectRegistration, models.Project) (bool, error)
}

func (f *fakeProjectMutator) RemoveProject(
	ctx context.Context,
	expected config.ProjectRegistration,
) (bool, error) {
	if f.remove == nil {
		return false, nil
	}
	return f.remove(ctx, expected)
}

func (f *fakeProjectMutator) RelocateProject(
	ctx context.Context,
	expected config.ProjectRegistration,
	replacement models.Project,
) (bool, error) {
	if f.relocate == nil {
		return false, nil
	}
	return f.relocate(ctx, expected, replacement)
}

func (f *fakeRegistryMutator) UnregisterIfGeneration(path, generation string) (bool, error) {
	if f.unregister == nil {
		return false, nil
	}
	return f.unregister(path, generation)
}

func (f *fakeRegistryMutator) CompareAndSwap(
	path string,
	expected *registry.WorktreeEntry,
	replacement *registry.WorktreeEntry,
) (bool, error) {
	if f.compareAndSwap == nil {
		return false, nil
	}
	return f.compareAndSwap(path, expected, replacement)
}

func (f *fakeRegistryMutator) CompareAndSwapAliases(
	expected []*registry.WorktreeEntry,
	retained *registry.WorktreeEntry,
) (bool, error) {
	if f.compareAliases == nil {
		return false, nil
	}
	return f.compareAliases(expected, retained)
}

func (f *fakeRegistryMutator) AcquireCreation(path string) (func() error, bool, error) {
	if f.acquireCreation == nil {
		return func() error { return nil }, true, nil
	}
	return f.acquireCreation(path)
}
