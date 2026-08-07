package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/pkg/models"
)

const projectInspectionGeneration = "0123456789abcdef0123456789abcdef"

func TestInspectorClassifiesMissingProjectRegistrations(t *testing.T) {
	tests := []struct {
		name           string
		registration   config.ProjectRegistration
		targets        map[string]RepositorySnapshot
		globalPaths    []string
		pathExists     map[string]bool
		pathErrors     map[string]error
		inspectErrors  map[string]error
		wantCode       FindingCode
		wantFixable    bool
		wantEvidence   map[string]string
		wantCandidates []string
	}{
		{
			name:         "unique visible repository relocates",
			registration: projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget"),
			targets: map[string]RepositorySnapshot{
				"/worktrees/widget-topic": projectTargetSnapshot("/repos/widget", "/worktrees/widget-topic"),
			},
			globalPaths: []string{"/worktrees/widget-topic"},
			pathExists:  map[string]bool{"/old/widget": false},
			wantCode:    ProjectPathMoved,
			wantFixable: true,
			wantEvidence: map[string]string{
				"old_path": "/old/widget", "new_path": "/repos/widget",
			},
		},
		{
			name:         "no visible repository removes stale registration",
			registration: projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget"),
			pathExists:   map[string]bool{"/old/widget": false},
			wantCode:     StaleProjectRegistration,
			wantFixable:  true,
		},
		{
			name:         "distinct live clones are ambiguous",
			registration: projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget"),
			targets: map[string]RepositorySnapshot{
				"/worktrees/one": projectTargetSnapshot("/repos/widget-one", "/worktrees/one"),
				"/worktrees/two": projectTargetSnapshot("/repos/widget-two", "/worktrees/two"),
			},
			globalPaths:    []string{"/worktrees/two", "/worktrees/one"},
			pathExists:     map[string]bool{"/old/widget": false},
			wantCode:       AmbiguousProjectRelocation,
			wantCandidates: []string{"/repos/widget-one", "/repos/widget-two"},
		},
		{
			name:         "duplicate roots for one common directory stay unique",
			registration: projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget"),
			targets: map[string]RepositorySnapshot{
				"/worktrees/one": projectTargetSnapshot("/repos/widget", "/worktrees/one"),
				"/worktrees/two": projectTargetSnapshot("/repos/widget", "/worktrees/two"),
			},
			globalPaths: []string{"/worktrees/two", "/worktrees/one"},
			pathExists:  map[string]bool{"/old/widget": false},
			wantCode:    ProjectPathMoved,
			wantFixable: true,
		},
		{
			name:         "permission error remains manual",
			registration: projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget"),
			pathErrors:   map[string]error{"/old/widget": errors.New("permission denied")},
			wantCode:     ProjectUnreachable,
		},
		{
			name:         "existing non repository remains manual",
			registration: projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget"),
			pathExists:   map[string]bool{"/old/widget": true},
			inspectErrors: map[string]error{
				"/old/widget": errors.New("not a git repository"),
			},
			wantCode: ProjectUnreachable,
		},
		{
			name:         "absolute path repository identity remains manual",
			registration: projectRegistration("/old/widget", "~/old/widget", "/old/widget"),
			pathExists:   map[string]bool{"/old/widget": false},
			wantCode:     ProjectUnreachable,
		},
		{
			name:         "local fallback identity remains manual",
			registration: projectRegistration("local/old/widget", "~/old/widget", "/old/widget"),
			pathExists:   map[string]bool{"/old/widget": false},
			wantCode:     ProjectUnreachable,
		},
		{
			name:         "file scheme identity remains manual",
			registration: projectRegistration("file:acme/widget", "~/old/widget", "/old/widget"),
			pathExists:   map[string]bool{"/old/widget": false},
			wantCode:     ProjectUnreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector := projectRepairInspector(
				[]config.ProjectRegistration{tt.registration}, nil,
				tt.targets, tt.globalPaths, tt.pathExists, tt.pathErrors, tt.inspectErrors,
			)

			report, err := inspector.Inspect(context.Background())

			require.NoError(t, err)
			finding, repository := requireFindingCode(t, report, tt.wantCode)
			assert.Equal(t, tt.wantFixable, finding.Fixable)
			assert.Equal(t, tt.registration.Effective.Path, repository.Root)
			for key, want := range tt.wantEvidence {
				assert.Equal(t, want, finding.Evidence[key])
			}
			if len(tt.wantCandidates) > 0 {
				assert.Equal(t, tt.wantCandidates, splitEvidencePaths(finding.Evidence["candidate_paths"]))
			}
			if tt.wantFixable {
				require.NotNil(t, finding.ProjectRepair)
				assert.Equal(t, tt.registration, finding.ProjectRepair.Expected)
			} else {
				assert.Nil(t, finding.ProjectRepair)
			}
		})
	}
}

func TestInspectorUsesLiveRegistryPathAsProjectRelocationTarget(t *testing.T) {
	registration := projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget")
	entry := &registry.WorktreeEntry{
		Path: "/repos/widget", Repository: "github.com/acme/widget",
		Generation: projectInspectionGeneration,
	}
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{registration}, []*registry.WorktreeEntry{entry},
		map[string]RepositorySnapshot{
			entry.Path: projectTargetSnapshot("/repos/widget", entry.Path),
		},
		nil,
		map[string]bool{registration.Effective.Path: false, entry.Path: true},
		nil, nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	finding, _ := requireFindingCode(t, report, ProjectPathMoved)
	assert.Equal(t, "/repos/widget", finding.Evidence["new_path"])
}

func TestInspectorKeepsMissingProjectManualWhenGlobalInventoryIsIncomplete(
	t *testing.T,
) {
	registration := projectRegistration(
		"github.com/acme/widget", "~/old/widget", "/old/widget",
	)
	unreachablePath := "/worktrees/unreachable"
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{registration}, nil, nil,
		[]string{unreachablePath},
		map[string]bool{registration.Effective.Path: false, unreachablePath: true},
		nil,
		map[string]error{unreachablePath: errors.New("not a git repository")},
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	finding := requireProjectFindingAtPath(t, report, registration.Effective.Path)
	assert.Equal(t, ProjectUnreachable, finding.Code)
	assert.False(t, finding.Fixable)
	assert.Nil(t, finding.ProjectRepair)
	assert.Contains(t, finding.Message, "inventory is incomplete")
}

func TestInspectorKeepsMissingProjectManualWhenRegistryInventoryIsIncomplete(
	t *testing.T,
) {
	registration := projectRegistration(
		"github.com/acme/widget", "~/old/widget", "/old/widget",
	)
	entry := &registry.WorktreeEntry{
		Path: "/worktrees/unreachable", Repository: "github.com/acme/widget",
		Generation: projectInspectionGeneration,
	}
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{registration},
		[]*registry.WorktreeEntry{entry},
		nil, nil,
		map[string]bool{registration.Effective.Path: false, entry.Path: true},
		nil,
		map[string]error{entry.Path: errors.New("not a git repository")},
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	finding := requireProjectFindingAtPath(t, report, registration.Effective.Path)
	assert.Equal(t, ProjectUnreachable, finding.Code)
	assert.False(t, finding.Fixable)
	assert.Nil(t, finding.ProjectRepair)
	assert.Contains(t, finding.Message, "inventory is incomplete")
}

func TestInspectorRemovesMissingDuplicateOfRegisteredLiveProject(t *testing.T) {
	stale := projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget")
	live := projectRegistration("github.com/acme/widget", "/repos/widget", "/repos/widget")
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{stale, live}, nil,
		map[string]RepositorySnapshot{
			live.Effective.Path: projectTargetSnapshot(live.Effective.Path, live.Effective.Path),
		},
		nil,
		map[string]bool{stale.Effective.Path: false, live.Effective.Path: true},
		nil, nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	finding, _ := requireFindingCode(t, report, StaleProjectRegistration)
	assert.Equal(t, stale.Effective.Path, finding.Path)
	require.NotNil(t, finding.ProjectRepair)
	assert.Equal(t, RemoveProject, finding.ProjectRepair.Action)
	assert.Equal(t, live.Effective.Path, finding.Evidence["registered_path"])
	assert.NotContains(t, findingCodes(report), ProjectPathMoved)

	projects := []models.Project{stale.Persisted, live.Persisted}
	fixer := &Fixer{
		Projects: &fakeProjectMutator{compareAndSwap: func(
			expected config.ProjectRegistration,
			replacement *models.Project,
		) (bool, error) {
			require.Nil(t, replacement)
			for index := range projects {
				if projects[index] == expected.Persisted {
					projects = append(projects[:index], projects[index+1:]...)
					return true, nil
				}
			}
			return false, nil
		}},
		PathExists: func(path string) (bool, error) {
			return path == live.Effective.Path, nil
		},
	}

	require.NoError(t, fixer.Fix(context.Background(), report))
	require.Len(t, projects, 1)
	assert.Equal(t, live.Persisted, projects[0])
}

func TestInspectorReportsMultipleRegistrationsClaimingOneRelocationTargetAsManual(t *testing.T) {
	first := projectRegistration(
		"github.com/acme/widget",
		"~/old/widget-one",
		"/old/widget-one",
	)
	first.Persisted.Name = "widget-one"
	first.Effective.Name = "widget-one"
	second := projectRegistration(
		"https://github.com/acme/widget.git",
		"~/old/widget-two",
		"/old/widget-two",
	)
	second.Persisted.Name = "widget-two"
	second.Effective.Name = "widget-two"
	targetPath := "/repos/widget"
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{first, second}, nil,
		map[string]RepositorySnapshot{
			targetPath: projectTargetSnapshot(targetPath, targetPath),
		},
		[]string{targetPath},
		map[string]bool{
			first.Effective.Path:  false,
			second.Effective.Path: false,
			targetPath:            true,
		},
		nil, nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	var ambiguous []Finding
	for _, repository := range report.Repositories {
		for _, finding := range repository.Findings {
			if finding.Code == AmbiguousProjectRelocation {
				ambiguous = append(ambiguous, finding)
			}
			assert.NotEqual(t, ProjectPathMoved, finding.Code)
		}
	}
	require.Len(t, ambiguous, 2)
	for _, finding := range ambiguous {
		assert.False(t, finding.Fixable)
		assert.Nil(t, finding.ProjectRepair)
		assert.Equal(t, targetPath, finding.Evidence["candidate_paths"])
	}
}

func TestInspectorReportsDuplicateMissingProjectRegistrationAsManual(t *testing.T) {
	registration := projectRegistration("github.com/acme/widget", "~/old/widget", "/old/widget")
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{registration, registration}, nil, nil, nil,
		map[string]bool{registration.Effective.Path: false}, nil, nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	finding, _ := requireFindingCode(t, report, ProjectUnreachable)
	assert.False(t, finding.Fixable)
	assert.Contains(t, finding.Message, "duplicate")
	assert.NotContains(t, findingCodes(report), StaleProjectRegistration)
}

func TestInspectorDoesNotArbitrarilyJoinRegistryEntryToOneOfMultipleClones(t *testing.T) {
	entryPath := "/worktrees/missing"
	registrations := []config.ProjectRegistration{
		projectRegistration("github.com/acme/widget", "/repos/one", "/repos/one"),
		projectRegistration("github.com/acme/widget", "/repos/two", "/repos/two"),
	}
	inspector := projectRepairInspector(
		registrations,
		[]*registry.WorktreeEntry{{
			Path: entryPath, Repository: "github.com/acme/widget",
			Generation: projectInspectionGeneration,
		}},
		map[string]RepositorySnapshot{
			"/repos/one": projectTargetSnapshot("/repos/one", "/repos/one"),
			"/repos/two": projectTargetSnapshot("/repos/two", "/repos/two"),
		},
		nil,
		map[string]bool{entryPath: false},
		nil, nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	finding, owner := requireFindingCode(t, report, StaleRegistryEntry)
	assert.Equal(t, entryPath, finding.Path)
	assert.Equal(t, entryPath, owner.Root)
	assert.Empty(t, owner.ProjectNames)
}

func TestInspectorDoesNotUseRegistrySubdirectoryForProjectRelocation(t *testing.T) {
	registration := projectRegistration(
		"github.com/acme/widget", "~/old/widget", "/old/widget",
	)
	repositoryRoot := "/repos/widget"
	registryPath := "/repos/widget/nested"
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{registration},
		[]*registry.WorktreeEntry{{
			Path: registryPath, Repository: "github.com/acme/widget",
			Generation: projectInspectionGeneration,
		}},
		map[string]RepositorySnapshot{
			registryPath: projectTargetSnapshot(repositoryRoot, repositoryRoot),
		},
		nil,
		map[string]bool{registration.Effective.Path: false, registryPath: true},
		nil,
		nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	assert.Contains(t, findingCodes(report), UnverifiedRegistryEntry)
	assert.NotContains(t, findingCodes(report), ProjectPathMoved)
	assert.NotContains(t, findingCodes(report), StaleProjectRegistration)
}

func TestInspectorDoesNotInventoryRegistryEntryOwnedByCreation(t *testing.T) {
	entry := &registry.WorktreeEntry{
		Path: "/repos/creating", Repository: "github.com/acme/widget",
		CreationToken: "creator",
	}
	inspector := projectRepairInspector(
		nil, []*registry.WorktreeEntry{entry}, nil, nil,
		map[string]bool{entry.Path: true}, nil, nil,
	)
	inspector.InspectRepository = func(path string) (RepositorySnapshot, error) {
		return RepositorySnapshot{}, fmt.Errorf("active creation path inspected: %s", path)
	}
	inspector.CreationActive = func(string) (bool, error) { return true, nil }

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	assert.True(t, report.Summary.Healthy)
}

func TestInspectorDefersMissingProjectRepairDuringActiveCreation(t *testing.T) {
	registration := projectRegistration(
		"github.com/acme/widget", "~/old/widget", "/old/widget",
	)
	entry := &registry.WorktreeEntry{
		Path: "/repos/creating", Repository: "github.com/acme/widget",
		CreationToken: "creator",
	}
	inspector := projectRepairInspector(
		[]config.ProjectRegistration{registration},
		[]*registry.WorktreeEntry{entry},
		nil,
		nil,
		map[string]bool{registration.Effective.Path: false, entry.Path: true},
		nil,
		nil,
	)
	inspector.InspectRepository = func(path string) (RepositorySnapshot, error) {
		return RepositorySnapshot{}, fmt.Errorf("active creation path inspected: %s", path)
	}
	inspector.CreationActive = func(string) (bool, error) { return true, nil }

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	assert.Contains(t, findingCodes(report), ProjectUnreachable)
	assert.NotContains(t, findingCodes(report), ProjectPathMoved)
	assert.NotContains(t, findingCodes(report), StaleProjectRegistration)
}

func TestInspectorIncludesAbandonedCreationRegistryEntries(t *testing.T) {
	t.Run("missing path", func(t *testing.T) {
		entry := &registry.WorktreeEntry{
			Path: "/worktrees/abandoned", Repository: "github.com/acme/widget",
			CreationToken: "abandoned",
		}
		inspector := projectRepairInspector(
			nil, []*registry.WorktreeEntry{entry}, nil, nil,
			map[string]bool{entry.Path: false}, nil, nil,
		)
		inspector.CreationActive = func(string) (bool, error) { return false, nil }

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		finding, _ := requireFindingCode(t, report, StaleRegistryEntry)
		assert.True(t, finding.Fixable)
	})

	t.Run("materialized path", func(t *testing.T) {
		entry := &registry.WorktreeEntry{
			Path: "/repos/widget", Repository: "github.com/acme/widget",
			CreationToken: "abandoned",
		}
		inspector := projectRepairInspector(
			nil, []*registry.WorktreeEntry{entry},
			map[string]RepositorySnapshot{entry.Path: projectTargetSnapshot(entry.Path, entry.Path)},
			nil, map[string]bool{entry.Path: true}, nil, nil,
		)
		inspector.CreationActive = func(string) (bool, error) { return false, nil }

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		finding, _ := requireFindingCode(t, report, RegistryGenerationMismatch)
		assert.True(t, finding.Fixable)
	})
}

func TestProjectRepairConditionIsExcludedFromJSON(t *testing.T) {
	rawMarker := "raw-project-marker"
	report := Report{Repositories: []RepositoryReport{{Findings: []Finding{{
		Code: ProjectPathMoved, Path: "/old/widget", Fixable: true,
		ProjectRepair: &ProjectRepairCondition{
			Action:   RelocateProject,
			Expected: config.ProjectRegistration{Persisted: models.Project{Name: rawMarker}},
		},
	}}}}}

	encoded, err := json.Marshal(report)

	require.NoError(t, err)
	assert.NotContains(t, string(encoded), rawMarker)
	assert.NotContains(t, string(encoded), "project_repair")
}

func projectRepairInspector(
	registrations []config.ProjectRegistration,
	entries []*registry.WorktreeEntry,
	snapshots map[string]RepositorySnapshot,
	globalPaths []string,
	exists map[string]bool,
	pathErrors map[string]error,
	inspectErrors map[string]error,
) *Inspector {
	effectiveProjects := make([]models.Project, len(registrations))
	for index := range registrations {
		effectiveProjects[index] = registrations[index].Effective
	}
	return &Inspector{
		Config: &models.Config{
			Projects: effectiveProjects,
			Worktree: models.WorktreeConfig{BaseDir: "/worktrees"},
		},
		ProjectRegistrations: registrations,
		RegistryEntries:      entries,
		InspectRepository: func(path string) (RepositorySnapshot, error) {
			if err := inspectErrors[path]; err != nil {
				return RepositorySnapshot{}, err
			}
			if snapshot, ok := snapshots[path]; ok {
				return snapshot, nil
			}
			return RepositorySnapshot{}, errors.New("repository snapshot missing")
		},
		FindGlobalPaths: func(string) ([]string, error) {
			return append([]string(nil), globalPaths...), nil
		},
		ReadDotGitTarget: func(path string) (string, error) {
			return filepath.Join(path, ".git"), nil
		},
		PathExists: func(path string) (bool, error) {
			if err := pathErrors[path]; err != nil {
				return false, err
			}
			value, ok := exists[path]
			if !ok {
				return true, nil
			}
			return value, nil
		},
	}
}

func projectRegistration(repository, persistedPath, effectivePath string) config.ProjectRegistration {
	return config.ProjectRegistration{
		Persisted: models.Project{
			Repository: repository, Name: "widget", Path: persistedPath, LastTouched: "before",
		},
		Effective: models.Project{
			Repository: repository, Name: "widget", Path: effectivePath, LastTouched: "before",
		},
	}
}

func projectTargetSnapshot(root, inspectionPath string) RepositorySnapshot {
	return RepositorySnapshot{
		Root: root, CommonDir: filepath.Join(root, ".git"),
		RepositoryIdentity: "github.com/acme/widget",
		Worktrees: []gitadapter.WorktreeInspection{{
			Path: inspectionPath, Exists: true, IsMain: inspectionPath == root,
			Generation:       projectInspectionGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		}},
	}
}

func requireFindingCode(t *testing.T, report Report, code FindingCode) (*Finding, *RepositoryReport) {
	t.Helper()
	for repositoryIndex := range report.Repositories {
		for findingIndex := range report.Repositories[repositoryIndex].Findings {
			finding := &report.Repositories[repositoryIndex].Findings[findingIndex]
			if finding.Code == code {
				return finding, &report.Repositories[repositoryIndex]
			}
		}
	}
	t.Fatalf("finding %s not found in %+v", code, report.Repositories)
	return nil, nil
}

func requireProjectFindingAtPath(t *testing.T, report Report, path string) *Finding {
	t.Helper()
	for repositoryIndex := range report.Repositories {
		for findingIndex := range report.Repositories[repositoryIndex].Findings {
			finding := &report.Repositories[repositoryIndex].Findings[findingIndex]
			if pathKey(finding.Path) == pathKey(path) {
				return finding
			}
		}
	}
	t.Fatalf("finding for %s not found in %+v", path, report.Repositories)
	return nil
}

func splitEvidencePaths(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, "\n")
}
