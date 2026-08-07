package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/pkg/models"
)

func TestInspectorClassifiesLocalFindings(t *testing.T) {
	validGeneration := "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name       string
		projects   []models.Project
		registry   []*registry.WorktreeEntry
		snapshots  map[string]RepositorySnapshot
		inspectErr map[string]error
		global     []string
		targets    map[string]string
		exists     map[string]bool
		want       []FindingCode
	}{
		{
			name:     "moved main",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/moved/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/moved/widget": repositorySnapshot("/moved/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/topic", GitDir: "/moved/widget/.git/worktrees/topic",
					DotGitTarget: "/old/widget/.git/worktrees/topic", Exists: true,
					Generation: validGeneration, GenerationStatus: gitadapter.GenerationValid,
				}),
			},
			global:  []string{"/worktrees/topic"},
			targets: map[string]string{"/worktrees/topic": "/old/widget/.git/worktrees/topic"},
			exists:  map[string]bool{"/worktrees/topic": true},
			want:    []FindingCode{BrokenWorktreeBacklink},
		},
		{
			name:     "copied claimant",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/moved/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/moved/widget": repositorySnapshot("/moved/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/topic", GitDir: "/moved/widget/.git/worktrees/topic",
					DotGitTarget: "/old/widget/.git/worktrees/topic", Exists: true,
					Generation: validGeneration, GenerationStatus: gitadapter.GenerationValid,
				}),
			},
			global: []string{"/worktrees/topic", "/copies/topic"},
			targets: map[string]string{
				"/worktrees/topic": "/old/widget/.git/worktrees/topic",
				"/copies/topic":    "/old/widget/.git/worktrees/topic",
			},
			exists: map[string]bool{"/worktrees/topic": true, "/copies/topic": true},
			want: []FindingCode{
				AmbiguousWorktreeBacklink,
				ProjectUnreachable,
			},
		},
		{
			name:     "missing path",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/missing", GitDir: "/repos/widget/.git/worktrees/missing",
					Exists: false, Prunable: true, Generation: validGeneration,
					GenerationStatus: gitadapter.GenerationValid,
				}),
			},
			exists: map[string]bool{"/worktrees/missing": false},
			want:   []FindingCode{MissingWorktreeDirectory},
		},
		{
			name:     "legacy registry",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget"),
			},
			registry: []*registry.WorktreeEntry{{
				Path: "/worktrees/stale", Repository: "github.com/acme/widget", Generation: validGeneration,
			}},
			exists: map[string]bool{"/worktrees/stale": false},
			want:   []FindingCode{StaleRegistryEntry},
		},
		{
			name:     "active registry creation",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget"),
			},
			registry: []*registry.WorktreeEntry{{
				Path: "/worktrees/creating", Repository: "github.com/acme/widget",
				CreationToken: "creation-owner",
			}},
			exists: map[string]bool{"/worktrees/creating": false},
			want:   nil,
		},
		{
			name:     "live missing generation",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/legacy", GitDir: "/repos/widget/.git/worktrees/legacy",
					DotGitTarget: "/repos/widget/.git/worktrees/legacy", Exists: true,
					GenerationStatus: gitadapter.GenerationMissing,
				}),
			},
			global:  []string{"/worktrees/legacy"},
			targets: map[string]string{"/worktrees/legacy": "/repos/widget/.git/worktrees/legacy"},
			exists:  map[string]bool{"/worktrees/legacy": true},
			want:    []FindingCode{MissingGeneration},
		},
		{
			name:     "live registry missing generation",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/legacy", GitDir: "/repos/widget/.git/worktrees/legacy",
					DotGitTarget: "/repos/widget/.git/worktrees/legacy", Exists: true,
					Generation: validGeneration, GenerationStatus: gitadapter.GenerationValid,
				}),
			},
			registry: []*registry.WorktreeEntry{{
				Path: "/worktrees/legacy", Repository: "github.com/acme/widget",
			}},
			global:  []string{"/worktrees/legacy"},
			targets: map[string]string{"/worktrees/legacy": "/repos/widget/.git/worktrees/legacy"},
			exists:  map[string]bool{"/worktrees/legacy": true},
			want:    []FindingCode{RegistryGenerationMismatch},
		},
		{
			name:     "live registry generation differs from Git",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/topic", GitDir: "/repos/widget/.git/worktrees/topic",
					DotGitTarget: "/repos/widget/.git/worktrees/topic", Exists: true,
					Generation: validGeneration, GenerationStatus: gitadapter.GenerationValid,
				}),
			},
			registry: []*registry.WorktreeEntry{{
				Path: "/worktrees/topic", Repository: "github.com/acme/widget",
				Generation: "fedcba9876543210fedcba9876543210",
			}},
			global:  []string{"/worktrees/topic"},
			targets: map[string]string{"/worktrees/topic": "/repos/widget/.git/worktrees/topic"},
			exists:  map[string]bool{"/worktrees/topic": true},
			want:    []FindingCode{RegistryGenerationMismatch},
		},
		{
			name:     "wrong repository",
			projects: []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			snapshots: map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/topic", GitDir: "/repos/widget/.git/worktrees/topic",
					DotGitTarget: "/repos/widget/.git/worktrees/topic", Exists: true,
					Generation: validGeneration, GenerationStatus: gitadapter.GenerationValid,
				}),
			},
			registry: []*registry.WorktreeEntry{{
				Path: "/worktrees/topic", Repository: "github.com/other/widget", Generation: validGeneration,
			}},
			global:  []string{"/worktrees/topic"},
			targets: map[string]string{"/worktrees/topic": "/repos/widget/.git/worktrees/topic"},
			exists:  map[string]bool{"/worktrees/topic": true},
			want:    []FindingCode{RepositoryIdentityMismatch},
		},
		{
			name:       "unreachable project",
			projects:   []models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/missing/widget"}},
			inspectErr: map[string]error{"/missing/widget": errors.New("permission denied")},
			want:       []FindingCode{ProjectUnreachable},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector := fakeInspector(tt.projects, tt.registry, tt.snapshots, tt.inspectErr, tt.global, tt.targets, tt.exists)

			report, err := inspector.Inspect(context.Background())

			require.NoError(t, err)
			assert.Equal(t, tt.want, findingCodes(report))
		})
	}
}

func TestInspectorReportsGitDirInspectionAmbiguity(t *testing.T) {
	const path = "/worktrees/topic"
	const gitDirError = "multiple Git administrative directories claim the worktree"
	inspector := fakeInspector(
		[]models.Project{{
			Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget",
		}},
		nil,
		map[string]RepositorySnapshot{
			"/repos/widget": repositorySnapshot(
				"/repos/widget",
				gitadapter.WorktreeInspection{
					Path: path, Exists: true, GitDirError: gitDirError,
					GenerationStatus: gitadapter.GenerationMissing,
				},
			),
		},
		nil, nil, nil, map[string]bool{path: true},
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	require.Len(t, report.Repositories, 1)
	require.Len(t, report.Repositories[0].Findings, 1)
	finding := report.Repositories[0].Findings[0]
	assert.Equal(t, AmbiguousWorktreeBacklink, finding.Code)
	assert.False(t, finding.Fixable)
	assert.Equal(t, gitDirError, finding.Evidence["git_dir_error"])
}

func TestInspectorTreatsDanglingSymlinksAsUnreachable(t *testing.T) {
	t.Run("configured project", func(t *testing.T) {
		path := newDanglingInspectionSymlink(t)
		inspector := NewInspector(&models.Config{Projects: []models.Project{{
			Name: "widget", Repository: "github.com/acme/widget", Path: path,
		}}}, nil, nil)

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		assert.Contains(t, findingCodes(report), ProjectUnreachable)
		assert.NotContains(t, findingCodes(report), StaleProjectRegistration)
	})

	t.Run("registry path", func(t *testing.T) {
		path := newDanglingInspectionSymlink(t)
		inspector := NewInspector(
			&models.Config{},
			[]*registry.WorktreeEntry{{
				Path: path, Repository: "github.com/acme/widget",
				Generation: "0123456789abcdef0123456789abcdef",
			}},
			nil,
		)

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		assert.Contains(t, findingCodes(report), ProjectUnreachable)
		assert.NotContains(t, findingCodes(report), StaleRegistryEntry)
	})

	t.Run("Git recorded worktree path", func(t *testing.T) {
		repositoryRoot := t.TempDir()
		path := newDanglingInspectionSymlink(t)
		inspector := &Inspector{
			Config: &models.Config{Projects: []models.Project{{
				Name: "widget", Repository: "github.com/acme/widget",
				Path: repositoryRoot,
			}}},
			InspectRepository: func(string) (RepositorySnapshot, error) {
				return repositorySnapshot(repositoryRoot, gitadapter.WorktreeInspection{
					Path: path, GitDir: filepath.Join(repositoryRoot, ".git", "worktrees", "topic"),
					Exists: false, Prunable: true,
					Generation:       "0123456789abcdef0123456789abcdef",
					GenerationStatus: gitadapter.GenerationValid,
				}), nil
			},
		}

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		assert.Contains(t, findingCodes(report), ProjectUnreachable)
		assert.NotContains(t, findingCodes(report), MissingWorktreeDirectory)
	})
}

func newDanglingInspectionSymlink(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	path := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "missing-target"), path); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	return path
}

func TestInspectorOnlyAutoFixesGenerationlessRegistryMismatch(t *testing.T) {
	const gitGeneration = "0123456789abcdef0123456789abcdef"
	for _, tt := range []struct {
		name               string
		registryGeneration string
		wantFixable        bool
	}{
		{name: "generationless legacy record", wantFixable: true},
		{name: "nonempty replacement conflict", registryGeneration: "fedcba9876543210fedcba9876543210", wantFixable: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inspector := fakeInspector(
				[]models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
				[]*registry.WorktreeEntry{{
					Path: "/worktrees/topic", Repository: "github.com/acme/widget",
					Generation: tt.registryGeneration,
				}},
				map[string]RepositorySnapshot{
					"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
						Path: "/worktrees/topic", GitDir: "/repos/widget/.git/worktrees/topic",
						DotGitTarget: "/repos/widget/.git/worktrees/topic", Exists: true,
						Generation: gitGeneration, GenerationStatus: gitadapter.GenerationValid,
					}),
				},
				nil,
				[]string{"/worktrees/topic"},
				map[string]string{"/worktrees/topic": "/repos/widget/.git/worktrees/topic"},
				map[string]bool{"/worktrees/topic": true},
			)

			report, err := inspector.Inspect(context.Background())

			require.NoError(t, err)
			require.Len(t, report.Repositories, 1)
			require.Len(t, report.Repositories[0].Findings, 1)
			finding := report.Repositories[0].Findings[0]
			assert.Equal(t, RegistryGenerationMismatch, finding.Code)
			assert.Equal(t, tt.wantFixable, finding.Fixable)
			if tt.wantFixable {
				assert.Contains(t, finding.Remediation, "kwt doctor --fix")
			} else {
				assert.Contains(t, finding.Remediation, "replacement")
			}
		})
	}
}

func TestInspectorReportsExistingUnverifiedRegistryPath(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	repositoryRoot := "/repos/widget"
	registryPath := "/repos/widget/copied-metadata"
	snapshot := repositorySnapshot(repositoryRoot, gitadapter.WorktreeInspection{
		Path: repositoryRoot, IsMain: true, Exists: true,
		GitDir:       filepath.Join(repositoryRoot, ".git"),
		DotGitTarget: filepath.Join(repositoryRoot, ".git"),
		Generation:   generation, GenerationStatus: gitadapter.GenerationValid,
	})
	inspector := fakeInspector(
		[]models.Project{{
			Name: "widget", Repository: "github.com/acme/widget", Path: repositoryRoot,
		}},
		[]*registry.WorktreeEntry{{
			Path: registryPath, Repository: "github.com/acme/widget",
			Generation: generation,
		}},
		map[string]RepositorySnapshot{
			repositoryRoot: snapshot,
			registryPath:   snapshot,
		},
		nil, nil, nil,
		map[string]bool{repositoryRoot: true, registryPath: true},
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	assert.Contains(t, findingCodes(report), UnverifiedRegistryEntry)
	for _, repository := range report.Repositories {
		for _, finding := range repository.Findings {
			if finding.Code == UnverifiedRegistryEntry {
				assert.Equal(t, registryPath, finding.Path)
				assert.False(t, finding.Fixable)
				assert.Equal(t, generation, finding.Evidence["generation"])
			}
		}
	}
}

func TestInspectorClassifiesDuplicateRegistryAliases(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	worktreePath := t.TempDir()
	aliasPath := filepath.Dir(worktreePath) + string(os.PathSeparator) + "." +
		string(os.PathSeparator) + filepath.Base(worktreePath)
	registeredAt := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	expiresAt := registeredAt.Add(24 * time.Hour)
	base := registry.WorktreeEntry{
		Repository:   "github.com/acme/widget",
		Branch:       "feature/topic",
		Path:         worktreePath,
		Hash:         "abc123",
		RegisteredAt: registeredAt,
		ExpiresAt:    &expiresAt,
		Generation:   generation,
	}
	tests := []struct {
		name        string
		changeAlias func(*registry.WorktreeEntry)
		wantFinding bool
		wantFixable bool
	}{
		{name: "equivalent aliases", wantFinding: true, wantFixable: true},
		{
			name: "different expiration", wantFinding: true,
			changeAlias: func(entry *registry.WorktreeEntry) {
				other := expiresAt.Add(time.Hour)
				entry.ExpiresAt = &other
			},
		},
		{
			name: "different generation", wantFinding: true,
			changeAlias: func(entry *registry.WorktreeEntry) {
				entry.Generation = "fedcba9876543210fedcba9876543210"
			},
		},
		{
			name: "active creation owner defers group",
			changeAlias: func(entry *registry.WorktreeEntry) {
				entry.CreationToken = "active-owner"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := base
			second := base
			second.Path = aliasPath
			if tt.changeAlias != nil {
				tt.changeAlias(&second)
			}
			inspector := fakeInspector(
				nil,
				[]*registry.WorktreeEntry{&first, &second},
				map[string]RepositorySnapshot{
					worktreePath: repositorySnapshot(worktreePath, gitadapter.WorktreeInspection{
						Path: worktreePath, Exists: true, Generation: generation,
						GenerationStatus: gitadapter.GenerationValid,
					}),
				},
				nil, nil, nil,
				map[string]bool{worktreePath: true, aliasPath: true},
			)

			report, err := inspector.Inspect(context.Background())

			require.NoError(t, err)
			var duplicates []Finding
			for _, repositoryReport := range report.Repositories {
				for _, finding := range repositoryReport.Findings {
					if finding.Code == DuplicateRegistryEntry {
						duplicates = append(duplicates, finding)
					}
				}
			}
			if !tt.wantFinding {
				assert.Empty(t, duplicates)
				return
			}
			require.Len(t, duplicates, 1)
			finding := duplicates[0]
			assert.Equal(t, tt.wantFixable, finding.Fixable)
			assert.Contains(t, finding.Evidence["paths"], worktreePath)
			assert.Contains(t, finding.Evidence["paths"], aliasPath)
			if tt.wantFixable {
				require.NotNil(t, finding.RegistryAliasRepair)
				require.Len(t, finding.RegistryAliasRepair.Expected, 2)
				assert.Equal(t, worktreePath, finding.RegistryAliasRepair.Retained.Path)
			} else {
				assert.Nil(t, finding.RegistryAliasRepair)
			}
		})
	}
}

func TestInspectorClassifiesMissingDuplicateRegistryAliases(t *testing.T) {
	realParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "worktrees-link")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	worktreePath := filepath.Join(
		realParent,
		"missing-parent",
		"missing-worktree",
	)
	aliasPath := filepath.Join(
		aliasParent,
		"missing-parent",
		"missing-worktree",
	)
	registeredAt := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	base := registry.WorktreeEntry{
		Repository: "github.com/acme/widget", Branch: "feature/topic",
		Path: worktreePath, RegisteredAt: registeredAt,
	}
	tests := []struct {
		name        string
		changeAlias func(*registry.WorktreeEntry)
		wantFixable bool
	}{
		{name: "equivalent group", wantFixable: true},
		{
			name: "conflicting group",
			changeAlias: func(entry *registry.WorktreeEntry) {
				entry.Branch = "feature/other"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := base
			second := base
			second.Path = aliasPath
			if tt.changeAlias != nil {
				tt.changeAlias(&second)
			}
			inspector := fakeInspector(
				nil,
				[]*registry.WorktreeEntry{&first, &second},
				nil, nil, nil, nil,
				map[string]bool{worktreePath: false, aliasPath: false},
			)

			report, err := inspector.Inspect(context.Background())

			require.NoError(t, err)
			var duplicates []Finding
			var stale []Finding
			for _, repositoryReport := range report.Repositories {
				for _, finding := range repositoryReport.Findings {
					switch finding.Code {
					case DuplicateRegistryEntry:
						duplicates = append(duplicates, finding)
					case StaleRegistryEntry:
						stale = append(stale, finding)
					}
				}
			}
			require.Len(t, duplicates, 1)
			assert.Empty(t, stale)
			finding := duplicates[0]
			assert.Equal(t, tt.wantFixable, finding.Fixable)
			assert.Contains(t, finding.Evidence["paths"], worktreePath)
			assert.Contains(t, finding.Evidence["paths"], aliasPath)
			if tt.wantFixable {
				require.NotNil(t, finding.RegistryAliasRepair)
				require.Len(t, finding.RegistryAliasRepair.Expected, 2)
				assert.Nil(t, finding.RegistryAliasRepair.Retained)
			} else {
				assert.Nil(t, finding.RegistryAliasRepair)
			}
		})
	}
}

func TestInspectorCanonicalizesLegacyRegistryRepositoryURL(t *testing.T) {
	const (
		secret     = "credential-must-not-appear"
		generation = "0123456789abcdef0123456789abcdef"
	)
	inspector := fakeInspector(
		[]models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
		[]*registry.WorktreeEntry{{
			Path:       "/worktrees/topic",
			Repository: "https://user:" + secret + "@github.com/acme/widget.git",
			Generation: generation,
		}},
		map[string]RepositorySnapshot{
			"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
				Path: "/worktrees/topic", Exists: true, Generation: generation,
				GenerationStatus: gitadapter.GenerationValid,
			}),
		},
		nil, nil, nil,
		map[string]bool{"/worktrees/topic": true},
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	assert.Empty(t, findingCodes(report))
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), secret)
}

func TestInspectorAcceptsOriginBasedExpirationForConfiguredUpstream(t *testing.T) {
	const (
		upstream = "github.com/acme/widget"
		fork     = "github.com/octocat/widget"
	)
	repositoryPath := newMaintenanceTestRepository(t)
	g := gitadapter.New(repositoryPath)
	_, err := g.RunCommand("remote", "set-url", "origin", "https://github.com/octocat/widget.git")
	require.NoError(t, err)
	_, err = g.RunCommand("branch", "feature/expiring")
	require.NoError(t, err)
	worktreePath := filepath.Join(t.TempDir(), "expiring")
	_, err = g.RunCommand("worktree", "add", worktreePath, "feature/expiring")
	require.NoError(t, err)
	generation, err := g.WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	inspector := NewInspector(
		&models.Config{Projects: []models.Project{{
			Name: "widget", Path: repositoryPath, Repository: upstream,
		}}},
		[]*registry.WorktreeEntry{{
			Path: worktreePath, Repository: fork, Generation: generation,
		}},
		nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	assert.NotContains(t, findingCodes(report), RepositoryIdentityMismatch)
}

func TestInspectorRedactsUnparseableRegistryRepository(t *testing.T) {
	const (
		secret     = "credential-must-not-appear"
		generation = "0123456789abcdef0123456789abcdef"
	)
	rawRepository := "ext::command --token=" + secret

	t.Run("standalone registry report", func(t *testing.T) {
		inspector := fakeInspector(
			nil,
			[]*registry.WorktreeEntry{{Path: "/worktrees/stale", Repository: rawRepository}},
			nil, nil, nil, nil,
			map[string]bool{"/worktrees/stale": false},
		)

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		encoded, err := json.Marshal(report)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), secret)
		assert.NotContains(t, string(encoded), rawRepository)
	})

	t.Run("mismatch evidence", func(t *testing.T) {
		inspector := fakeInspector(
			[]models.Project{{Name: "widget", Repository: "github.com/acme/widget", Path: "/repos/widget"}},
			[]*registry.WorktreeEntry{{
				Path: "/worktrees/topic", Repository: rawRepository, Generation: generation,
			}},
			map[string]RepositorySnapshot{
				"/repos/widget": repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
					Path: "/worktrees/topic", Exists: true, Generation: generation,
					GenerationStatus: gitadapter.GenerationValid,
				}),
			},
			nil, nil, nil,
			map[string]bool{"/worktrees/topic": true},
		)

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		require.Len(t, report.Repositories, 1)
		require.Len(t, report.Repositories[0].Findings, 1)
		finding := report.Repositories[0].Findings[0]
		assert.Equal(t, RepositoryIdentityMismatch, finding.Code)
		assert.Equal(t, "[redacted]", finding.Evidence["registry_repository"])
		encoded, err := json.Marshal(report)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), secret)
	})
}

func TestInspectorSanitizesConfiguredRepositoryIdentity(t *testing.T) {
	const secret = "configured-credential-must-not-appear"

	t.Run("reachable credential URL is canonicalized", func(t *testing.T) {
		repositoryPath := newMaintenanceTestRepository(t)
		inspector := NewInspector(&models.Config{Projects: []models.Project{{
			Name: "widget", Path: repositoryPath,
			Repository: "https://user:" + secret + "@github.com/acme/widget.git",
		}}}, nil, nil)

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		require.Len(t, report.Repositories, 1)
		assert.Equal(t, "github.com/acme/widget", report.Repositories[0].RepositoryIdentity)
		assertReportOmits(t, report, secret)
	})

	t.Run("reachable malformed identity falls back to origin", func(t *testing.T) {
		repositoryPath := newMaintenanceTestRepository(t)
		inspector := NewInspector(&models.Config{Projects: []models.Project{{
			Name: "widget", Path: repositoryPath,
			Repository: "ext::command --token=" + secret,
		}}}, nil, nil)

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		require.Len(t, report.Repositories, 1)
		assert.Equal(t, "github.com/acme/widget", report.Repositories[0].RepositoryIdentity)
		assertReportOmits(t, report, secret)
	})

	t.Run("unreachable malformed identity is omitted", func(t *testing.T) {
		inspector := fakeInspector(
			[]models.Project{{
				Name: "widget", Path: "/missing/widget",
				Repository: "ext::command --token=" + secret,
			}},
			nil, nil,
			map[string]error{"/missing/widget": errors.New("permission denied")},
			nil, nil, nil,
		)

		report, err := inspector.Inspect(context.Background())

		require.NoError(t, err)
		require.Len(t, report.Repositories, 1)
		assert.Empty(t, report.Repositories[0].RepositoryIdentity)
		assertReportOmits(t, report, secret)
	})
}

func TestInspectorReportsConfiguredRepositoryIdentityClaims(t *testing.T) {
	const secret = "configured-claim-secret-must-not-appear"
	tests := []struct {
		name             string
		repository       string
		want             bool
		wantRepositories string
		wantInvalid      string
	}{
		{
			name:       "equivalent canonical identity",
			repository: "https://github.com/acme/widget.git",
		},
		{
			name:             "conflicting canonical identity",
			repository:       "github.com/other/widget",
			want:             true,
			wantRepositories: "github.com/acme/widget\ngithub.com/other/widget",
		},
		{
			name:       "invalid competing identity",
			repository: "ext::command --token=" + secret,
			want:       true, wantRepositories: "github.com/acme/widget",
			wantInvalid: "1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := repositorySnapshot(
				"/repos/widget",
				gitadapter.WorktreeInspection{
					Path: "/configured/widget-a", Exists: true,
					Generation:       projectInspectionGeneration,
					GenerationStatus: gitadapter.GenerationValid,
				},
				gitadapter.WorktreeInspection{
					Path: "/configured/widget-b", Exists: true,
					Generation:       projectInspectionGeneration,
					GenerationStatus: gitadapter.GenerationValid,
				},
			)
			inspector := fakeInspector(
				[]models.Project{
					{
						Name: "widget-a", Repository: "github.com/acme/widget",
						Path: "/configured/widget-a",
					},
					{
						Name: "widget-b", Repository: tt.repository,
						Path: "/configured/widget-b",
					},
				},
				nil,
				map[string]RepositorySnapshot{
					"/configured/widget-a": snapshot,
					"/configured/widget-b": snapshot,
				},
				nil, nil, nil, nil,
			)

			report, err := inspector.Inspect(context.Background())

			require.NoError(t, err)
			require.Len(t, report.Repositories, 1)
			if tt.want {
				require.Len(t, report.Repositories[0].Findings, 1)
				finding := report.Repositories[0].Findings[0]
				assert.Equal(t, RepositoryIdentityMismatch, finding.Code)
				assert.False(t, finding.Fixable)
				assert.Equal(
					t, tt.wantRepositories,
					finding.Evidence["configured_repositories"],
				)
				assert.Equal(t, tt.wantInvalid, finding.Evidence["invalid_claim_count"])
			} else {
				assert.Empty(t, report.Repositories[0].Findings)
			}
			assertReportOmits(t, report, secret)
		})
	}
}

func TestInspectRepositoryUsesMainRepositoryIdentityForLinkedRoot(t *testing.T) {
	repositoryPath := newMaintenanceTestRepository(t)
	g := gitadapter.New(repositoryPath)
	_, err := g.RunCommand("branch", "linked-origin")
	require.NoError(t, err)
	worktreePath := filepath.Join(t.TempDir(), "linked-origin")
	_, err = g.RunCommand("worktree", "add", worktreePath, "linked-origin")
	require.NoError(t, err)
	_, err = g.RunCommand("remote", "remove", "origin")
	require.NoError(t, err)
	linkedGitDir, err := gitadapter.New(worktreePath).RunCommand(
		"rev-parse", "--absolute-git-dir",
	)
	require.NoError(t, err)
	linkedConfig := filepath.Join(t.TempDir(), "linked.config")
	_, err = g.RunCommand(
		"config", "--file", linkedConfig,
		"remote.origin.url", "https://github.com/hubot/widget.git",
	)
	require.NoError(t, err)
	_, err = g.RunCommand(
		"config", "--add",
		"includeIf.gitdir:"+strings.TrimSpace(linkedGitDir)+".path", linkedConfig,
	)
	require.NoError(t, err)
	_, err = g.RunCommand(
		"remote", "add", "origin", "https://github.com/acme/widget.git",
	)
	require.NoError(t, err)

	inspector := NewInspector(&models.Config{}, nil, nil)
	snapshot, err := inspector.inspectRepository(worktreePath)

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", snapshot.RepositoryIdentity)
	assert.Equal(
		t,
		"github.com/hubot/widget",
		snapshot.LiveRepositoryIdentities[pathKey(worktreePath)],
	)
}

func TestInspectRepositorySupportsSeparateGitDirectory(t *testing.T) {
	mainPath, linkedPath := newMaintenanceSeparateGitRepository(t)
	inspector := NewInspector(&models.Config{Projects: []models.Project{{
		Name: "widget", Path: mainPath, Repository: "github.com/acme/widget",
	}}}, nil, nil)

	snapshot, err := inspector.inspectRepository(mainPath)

	require.NoError(t, err)
	assert.Equal(t, pathKey(mainPath), pathKey(snapshot.Root))
	require.Len(t, snapshot.Worktrees, 2)
	mainFound := false
	linkedFound := false
	for _, inspection := range snapshot.Worktrees {
		switch pathKey(inspection.Path) {
		case pathKey(mainPath):
			mainFound = inspection.IsMain
		case pathKey(linkedPath):
			linkedFound = !inspection.IsMain
		}
	}
	assert.True(t, mainFound)
	assert.True(t, linkedFound)
}

func TestInspectorDeduplicatesProjectsByCommonDirectory(t *testing.T) {
	snapshot := repositorySnapshot(
		"/repos/widget",
		gitadapter.WorktreeInspection{
			Path: "/aliases/zeta", Exists: true,
			Generation:       projectInspectionGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		},
		gitadapter.WorktreeInspection{
			Path: "/aliases/alpha", Exists: true,
			Generation:       projectInspectionGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		},
	)
	inspector := fakeInspector(
		[]models.Project{
			{Name: "zeta", Repository: "github.com/acme/widget", Path: "/aliases/zeta"},
			{Name: "alpha", Repository: "github.com/acme/widget", Path: "/aliases/alpha"},
		},
		nil,
		map[string]RepositorySnapshot{
			"/aliases/zeta":  snapshot,
			"/aliases/alpha": snapshot,
		},
		nil,
		nil,
		nil,
		nil,
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	require.Len(t, report.Repositories, 1)
	assert.Equal(t, []string{"alpha", "zeta"}, report.Repositories[0].ProjectNames)
}

func TestInspectorInventoriesRepositoryFromGlobalLinkedWorktree(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	linkedPath := "/worktrees/topic"
	inspector := fakeInspector(
		nil,
		nil,
		map[string]RepositorySnapshot{
			linkedPath: repositorySnapshot("/repos/widget", gitadapter.WorktreeInspection{
				Path: linkedPath, GitDir: "/repos/widget/.git/worktrees/topic",
				DotGitTarget: "/repos/widget/.git/worktrees/topic", Exists: true,
				Generation: generation, GenerationStatus: gitadapter.GenerationValid,
			}),
		},
		nil,
		[]string{linkedPath},
		map[string]string{linkedPath: "/repos/widget/.git/worktrees/topic"},
		map[string]bool{linkedPath: true},
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	require.Len(t, report.Repositories, 1)
	assert.Equal(t, "/repos/widget/.git", report.Repositories[0].CommonDir)
	assert.Empty(t, report.Repositories[0].Findings)
}

func TestInspectorReportsUninspectableGlobalLinkedWorktree(t *testing.T) {
	linkedPath := "/worktrees/topic"
	inspector := fakeInspector(
		nil,
		nil,
		nil,
		map[string]error{linkedPath: errors.New("linked repository unavailable")},
		[]string{linkedPath},
		map[string]string{linkedPath: "/repos/widget/.git/worktrees/topic"},
		map[string]bool{linkedPath: true},
	)

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	require.Len(t, report.Repositories, 1)
	assert.Equal(t, linkedPath, report.Repositories[0].Root)
	require.Len(t, report.Repositories[0].Findings, 1)
	assert.Equal(t, ProjectUnreachable, report.Repositories[0].Findings[0].Code)
	assert.Contains(t, report.Repositories[0].Findings[0].Message, "linked repository unavailable")
}

func TestInspectorReportsUnreadableGlobalDotGit(t *testing.T) {
	linkedPath := "/worktrees/topic"
	inspector := &Inspector{
		Config: &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/worktrees"}},
		FindGlobalPaths: func(string) ([]string, error) {
			return []string{linkedPath}, nil
		},
		ReadDotGitTarget: func(string) (string, error) {
			return "", errors.New("permission denied")
		},
		InspectRepository: func(string) (RepositorySnapshot, error) {
			t.Fatal("repository inspection must not run without a readable .git entry")
			return RepositorySnapshot{}, nil
		},
		PathExists: func(string) (bool, error) { return true, nil },
	}

	report, err := inspector.Inspect(context.Background())

	require.NoError(t, err)
	require.Len(t, report.Repositories, 1)
	require.Len(t, report.Repositories[0].Findings, 1)
	finding := report.Repositories[0].Findings[0]
	assert.Equal(t, ProjectUnreachable, finding.Code)
	assert.Contains(t, finding.Message, "permission denied")
}

func repositorySnapshot(root string, worktrees ...gitadapter.WorktreeInspection) RepositorySnapshot {
	hasMain := false
	for _, inspection := range worktrees {
		if pathKey(inspection.Path) == pathKey(root) {
			hasMain = true
			break
		}
	}
	if !hasMain {
		worktrees = append([]gitadapter.WorktreeInspection{{
			Path: root, IsMain: true, Exists: true,
			GitDir:           filepath.Join(root, ".git"),
			DotGitTarget:     filepath.Join(root, ".git"),
			Generation:       projectInspectionGeneration,
			GenerationStatus: gitadapter.GenerationValid,
		}}, worktrees...)
	}
	return RepositorySnapshot{
		Root:               root,
		CommonDir:          root + "/.git",
		RepositoryIdentity: "github.com/acme/widget",
		Worktrees:          worktrees,
	}
}

func newMaintenanceTestRepository(t *testing.T) string {
	t.Helper()
	repositoryPath := t.TempDir()
	g := gitadapter.New(repositoryPath)
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
		{"remote", "add", "origin", "https://github.com/acme/widget.git"},
	} {
		_, err := g.RunCommand(args...)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("test\n"), 0o644))
	_, err := g.RunCommand("add", "README.md")
	require.NoError(t, err)
	_, err = g.RunCommand("commit", "-m", "initial")
	require.NoError(t, err)
	return repositoryPath
}

func newMaintenanceSeparateGitRepository(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	mainPath := filepath.Join(base, "main-worktree")
	linkedPath := filepath.Join(base, "linked-worktree")
	separateGitDir := filepath.Join(base, "repository.git")
	_, err := gitadapter.New(base).RunCommand(
		"init", "-b", "main", "--separate-git-dir", separateGitDir, mainPath,
	)
	require.NoError(t, err)
	g := gitadapter.New(mainPath)
	for _, args := range [][]string{
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
		{"remote", "add", "origin", "https://github.com/acme/widget.git"},
	} {
		_, err := g.RunCommand(args...)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(mainPath, "README.md"),
		[]byte("test\n"),
		0o644,
	))
	_, err = g.RunCommand("add", "README.md")
	require.NoError(t, err)
	_, err = g.RunCommand("commit", "-m", "initial")
	require.NoError(t, err)
	_, err = g.RunCommand("branch", "topic")
	require.NoError(t, err)
	_, err = g.RunCommand("worktree", "add", linkedPath, "topic")
	require.NoError(t, err)
	return mainPath, linkedPath
}

func assertReportOmits(t *testing.T, report Report, value string) {
	t.Helper()
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), value)
}

func fakeInspector(
	projects []models.Project,
	entries []*registry.WorktreeEntry,
	snapshots map[string]RepositorySnapshot,
	inspectErr map[string]error,
	global []string,
	targets map[string]string,
	exists map[string]bool,
) *Inspector {
	return &Inspector{
		Config:          &models.Config{Projects: projects, Worktree: models.WorktreeConfig{BaseDir: "/worktrees"}},
		RegistryEntries: entries,
		InspectRepository: func(path string) (RepositorySnapshot, error) {
			if err := inspectErr[path]; err != nil {
				return RepositorySnapshot{}, err
			}
			if snapshot, ok := snapshots[path]; ok {
				return snapshot, nil
			}
			target := targets[path]
			for _, snapshot := range snapshots {
				for _, inspection := range snapshot.Worktrees {
					if pathKey(inspection.Path) == pathKey(path) ||
						(target != "" && (pathKey(inspection.GitDir) == pathKey(target) ||
							pathKey(inspection.DotGitTarget) == pathKey(target))) {
						return snapshot, nil
					}
				}
			}
			return RepositorySnapshot{}, nil
		},
		FindGlobalPaths: func(string) ([]string, error) {
			return append([]string(nil), global...), nil
		},
		ReadDotGitTarget: func(path string) (string, error) {
			return targets[path], nil
		},
		PathExists: func(path string) (bool, error) {
			value, ok := exists[path]
			if !ok {
				return true, nil
			}
			return value, nil
		},
	}
}

func findingCodes(report Report) []FindingCode {
	var codes []FindingCode
	for _, repository := range report.Repositories {
		for _, finding := range repository.Findings {
			codes = append(codes, finding.Code)
		}
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return codes
}
