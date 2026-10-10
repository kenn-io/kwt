package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	configpkg "go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
	shared "go.kenn.io/kwt/worktree"
)

// mockGit is a mock implementation of git operations for testing
type mockGit struct {
	worktrees         []models.Worktree
	repoName          string
	repoPath          string
	bareContainerPath string
	repoURL           string
	repoURLError      error
	addError          error
	listError         error
	recentCommits     []models.CommitInfo
	mainRepoPathError error
	trackingSource    string
	existingSource    string
	protectedNames    []string
}

type mockRemoteSourceState struct {
	entries              map[string]*registry.WorktreeEntry
	creationActive       bool
	creationReleaseError error
}

type materializedAddError struct {
	error
}

func (m *mockRemoteSourceState) CompareAndSwap(
	path string,
	expected *registry.WorktreeEntry,
	replacement *registry.WorktreeEntry,
) (bool, error) {
	entry, ok := m.entries[path]
	if expected == nil {
		if ok {
			return false, nil
		}
	} else if !ok || !reflect.DeepEqual(entry, expected) {
		return false, nil
	}
	if m.entries == nil {
		m.entries = make(map[string]*registry.WorktreeEntry)
	}
	delete(m.entries, path)
	if replacement != nil {
		copied := *replacement
		m.entries[replacement.Path] = &copied
	}
	return true, nil
}

func (m *mockRemoteSourceState) Get(
	path string,
) (*registry.WorktreeEntry, bool) {
	entry, ok := m.entries[path]
	if !ok {
		return nil, false
	}
	copied := *entry
	return &copied, true
}

func (m *mockRemoteSourceState) AcquireCreation(
	_ string,
) (func() error, bool, error) {
	if m.creationActive {
		return nil, false, nil
	}
	m.creationActive = true
	return func() error {
		m.creationActive = false
		return m.creationReleaseError
	}, true, nil
}

func (m *mockRemoteSourceState) CompleteCreation(
	path string,
	token string,
	generation string,
) (bool, error) {
	entry, ok := m.entries[path]
	if !ok || entry.CreationToken != token {
		return false, nil
	}
	entry.CreationToken = ""
	entry.Generation = generation
	return true, nil
}

func (m *mockRemoteSourceState) ReclaimCreation(
	path string,
	token string,
	replacement *registry.WorktreeEntry,
) (bool, error) {
	entry, ok := m.entries[path]
	if !ok || entry.CreationToken != token {
		return false, nil
	}
	copied := *replacement
	m.entries[path] = &copied
	return true, nil
}

func (m *mockRemoteSourceState) AbortCreation(
	path string,
	token string,
	previous *registry.WorktreeEntry,
) (bool, error) {
	entry, ok := m.entries[path]
	if !ok || entry.CreationToken != token {
		return false, nil
	}
	delete(m.entries, path)
	if previous != nil {
		copied := *previous
		copied.UnreviewedRemoteSource =
			previous.UnreviewedRemoteSource &&
				entry.UnreviewedRemoteSource
		m.entries[path] = &copied
	}
	return true, nil
}

func (m *mockGit) ListWorktrees() ([]models.Worktree, error) {
	if m.listError != nil {
		return nil, m.listError
	}
	return m.worktrees, nil
}

func (m *mockGit) ReadWorktreeGeneration(path string) (string, error) {
	for _, worktree := range m.worktrees {
		if utils.PathKey(worktree.Path) != utils.PathKey(path) {
			continue
		}
		if worktree.Generation == "" {
			return "", errors.New("worktree generation is not initialized")
		}
		return worktree.Generation, nil
	}
	return "", errors.New("worktree not found")
}

// newMockManager isolates application naming and provenance policy. Real Git
// creation and rollback are covered by shared_creation_test.go and CLI fixtures.
func newMockManager(g *mockGit, cfg *models.Config) *Manager {
	m := &Manager{git: g, config: cfg, openRemoteSourceState: func() (remoteSourceState, error) { return registry.New() }}
	m.listGit = func(context.Context) ([]models.Worktree, error) { return g.ListWorktrees() }
	m.readIdentity = func(_ context.Context, path string) (string, error) { return g.ReadWorktreeGeneration(path) }
	m.createGit = func(_ context.Context, opts CreateOptions) (shared.CreateResult, error) {
		result := shared.CreateResult{}
		if g.addError != nil {
			var materialized materializedAddError
			if errors.As(g.addError, &materialized) {
				result.Path = opts.Path
			}
			return result, g.addError
		}
		const generation = "0123456789abcdef0123456789abcdef"
		if opts.Source != "" {
			g.trackingSource = opts.Source
		} else if !opts.NewBranch {
			g.existingSource = opts.Branch
		}
		if opts.Source != "" || !opts.NewBranch {
			g.protectedNames = credentials.ProtectedNames(cfg)
		}
		for i := range g.worktrees {
			if utils.PathKey(g.worktrees[i].Path) == utils.PathKey(opts.Path) {
				g.worktrees = append(g.worktrees[:i], g.worktrees[i+1:]...)
				break
			}
		}
		g.worktrees = append(g.worktrees, models.Worktree{Path: opts.Path, Branch: opts.Branch, Generation: generation})
		return shared.CreateResult{Path: opts.Path, Branch: opts.Branch, IdentityValue: generation}, nil
	}
	return m
}

func (m *mockGit) GetRepositoryName() (string, error) {
	if m.repoName == "" {
		return "test-repo", nil
	}
	return m.repoName, nil
}

func (m *mockGit) GetRecentCommits(path string, limit int) ([]models.CommitInfo, error) {
	return m.recentCommits, nil
}

func (m *mockGit) GetRepositoryURL() (string, error) {
	if m.repoURLError != nil {
		return "", m.repoURLError
	}
	if m.repoURL != "" {
		return m.repoURL, nil
	}
	return "https://github.com/test-user/test-repo.git", nil
}

func (m *mockGit) GetMainRepositoryPath() (string, error) {
	if m.mainRepoPathError != nil {
		return "", m.mainRepoPathError
	}
	if m.repoPath == "" {
		return "/mock/repo/path", nil
	}
	return m.repoPath, nil
}

func (m *mockGit) GetBareContainerPath() (string, error) {
	return m.bareContainerPath, nil
}

func TestManagerAdd(t *testing.T) {
	tests := []struct {
		name         string
		branch       string
		customPath   string
		createBranch bool
		config       *models.Config
		wantErr      bool
		errContains  string
	}{
		{
			name:   "WithGeneratedPath",
			branch: "feature/test",
			config: &models.Config{
				Worktree: models.WorktreeConfig{
					BaseDir:   t.TempDir(),
					AutoMkdir: true,
				},
			},
			wantErr: false,
		},
		{
			name:       "WithCustomPath",
			branch:     "feature/test",
			customPath: filepath.Join(t.TempDir(), "custom-worktree"),
			config: &models.Config{
				Worktree: models.WorktreeConfig{
					BaseDir:   t.TempDir(),
					AutoMkdir: true,
				},
			},
			wantErr: false,
		},
		{
			name:         "CreateNewBranch",
			branch:       "feature/new",
			createBranch: true,
			config: &models.Config{
				Worktree: models.WorktreeConfig{
					BaseDir:   t.TempDir(),
					AutoMkdir: true,
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KWT_HOME", t.TempDir())
			mockG := &mockGit{}
			m := newMockManager(mockG, tt.config)
			state := &mockRemoteSourceState{}
			m.openRemoteSourceState = func() (remoteSourceState, error) {
				return state, nil
			}

			_, err := m.Create(t.Context(), CreateOptions{Branch: tt.branch, Path: tt.customPath, NewBranch: tt.createBranch})
			if (err != nil) != tt.wantErr {
				t.Errorf("Add() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("Add() error = %v, want error containing %s", err, tt.errContains)
			}

			if !tt.wantErr {
				// Verify worktree was added
				if len(mockG.worktrees) != 1 {
					t.Errorf("Expected 1 worktree, got %d", len(mockG.worktrees))
				}
				reg, err := registry.New()
				require.NoError(t, err)
				assert.Empty(t, reg.List())
			}
		})
	}
}

func TestManagerAddTrackingUsesRemoteSource(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := t.TempDir()
	worktreePath := filepath.Join(baseDir, "remote-worktree")
	require.NoError(t, os.MkdirAll(worktreePath, 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoDir, "copy-me"),
		[]byte("untrusted"),
		0644,
	))
	mockG := &mockGit{repoPath: repoDir}
	state := &mockRemoteSourceState{}
	manager := newMockManager(mockG, &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   baseDir,
			AutoMkdir: true,
		},
		RepositorySettings: []models.RepositorySetting{{
			Repository:    repoDir,
			CopyFiles:     []string{"copy-me"},
			SetupCommands: []string{"touch setup-ran"},
		}},
		Fleet: models.FleetConfig{TokenEnv: "Custom_Fleet_Token"},
	})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	created, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/remote", Source: "origin/feature/remote", Path: worktreePath})
	path := created.Path

	require.NoError(t, err)
	assert.Equal(t, worktreePath, path)
	require.Len(t, mockG.worktrees, 1)
	assert.Equal(t, "feature/remote", mockG.worktrees[0].Branch)
	assert.Equal(t, "origin/feature/remote", mockG.trackingSource)
	assert.ElementsMatch(
		t,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN", "Custom_Fleet_Token"},
		mockG.protectedNames,
	)
	require.Contains(t, state.entries, worktreePath)
	assert.True(t, state.entries[worktreePath].UnreviewedRemoteSource)
	assert.Equal(
		t,
		"0123456789abcdef0123456789abcdef",
		state.entries[worktreePath].Generation,
	)
	assert.NoFileExists(t, filepath.Join(worktreePath, "copy-me"))
	assert.NoFileExists(t, filepath.Join(worktreePath, "setup-ran"))
}

func TestManagerAddExistingMarksSourceUnreviewedAndSkipsSetup(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := t.TempDir()
	worktreePath := filepath.Join(baseDir, "local-worktree")
	require.NoError(t, os.MkdirAll(worktreePath, 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoDir, "copy-me"),
		[]byte("trusted config, untrusted branch"),
		0644,
	))
	mockG := &mockGit{repoPath: repoDir}
	state := &mockRemoteSourceState{}
	manager := newMockManager(mockG, &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   baseDir,
			AutoMkdir: true,
		},
		RepositorySettings: []models.RepositorySetting{{
			Repository: repoDir,
			CopyFiles:  []string{"copy-me"},
		}},
		Fleet: models.FleetConfig{TokenEnv: "Custom_Fleet_Token"},
	})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	created, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/local", Path: worktreePath, NewBranch: false})
	path := created.Path

	require.NoError(t, err)
	assert.Equal(t, worktreePath, path)
	assert.Equal(t, "feature/local", mockG.existingSource)
	assert.ElementsMatch(
		t,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN", "Custom_Fleet_Token"},
		mockG.protectedNames,
	)
	require.Contains(t, state.entries, worktreePath)
	assert.True(t, state.entries[worktreePath].UnreviewedRemoteSource)
	assert.Equal(
		t,
		"0123456789abcdef0123456789abcdef",
		state.entries[worktreePath].Generation,
	)
	assert.NoFileExists(t, filepath.Join(worktreePath, "copy-me"))
}

func TestManagerAddTrackingRestoresExistingMarkerAfterGitFailure(t *testing.T) {
	worktreePath := t.TempDir()
	future := time.Now().Add(time.Hour)
	generation := "fedcba9876543210fedcba9876543210"
	state := &mockRemoteSourceState{entries: map[string]*registry.WorktreeEntry{
		worktreePath: {
			Path:                   worktreePath,
			Branch:                 "feature/existing",
			Repository:             "github.com/acme/widget",
			ExpiresAt:              &future,
			Generation:             generation,
			UnreviewedRemoteSource: false,
		},
	}}
	manager := newMockManager(&mockGit{
		addError: errors.New("already checked out"),
		worktrees: []models.Worktree{{
			Path:       worktreePath,
			Branch:     "feature/existing",
			Generation: generation,
		}},
	}, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	_, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/existing", Source: "refs/remotes/origin/feature/existing", Path: worktreePath})

	require.Error(t, err)
	entry, ok := state.entries[worktreePath]
	require.True(t, ok, "failed repeated add must retain the safety marker")
	assert.False(t, entry.UnreviewedRemoteSource)
	assert.Equal(t, "github.com/acme/widget", entry.Repository)
	assert.Equal(t, &future, entry.ExpiresAt)
	assert.Equal(t, generation, entry.Generation)
}

func TestManagerAddTrackingReplacesStaleMetadataAfterSuccess(t *testing.T) {
	worktreePath := t.TempDir()
	expired := time.Now().Add(-time.Hour)
	state := &mockRemoteSourceState{entries: map[string]*registry.WorktreeEntry{
		worktreePath: {
			Path:                   worktreePath,
			Branch:                 "feature/stale",
			Repository:             "github.com/acme/old",
			ExpiresAt:              &expired,
			UnreviewedRemoteSource: true,
		},
	}}
	manager := newMockManager(&mockGit{}, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	_, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/reused", Source: "refs/remotes/origin/feature/reused", Path: worktreePath})

	require.NoError(t, err)
	entry, ok := state.entries[worktreePath]
	require.True(t, ok)
	assert.Equal(t, "feature/reused", entry.Branch)
	assert.Empty(t, entry.Repository)
	assert.Nil(t, entry.ExpiresAt)
	assert.True(t, entry.UnreviewedRemoteSource)
}

func TestManagerAddTrackingRetainsMarkerWhenFailedCheckoutPathExists(t *testing.T) {
	worktreePath := t.TempDir()
	state := &mockRemoteSourceState{}
	manager := newMockManager(&mockGit{addError: materializedAddError{
		error: errors.New("checkout failed"),
	}}, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	_, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/partial", Source: "refs/remotes/origin/feature/partial", Path: worktreePath})

	require.Error(t, err)
	entry, ok := state.entries[worktreePath]
	require.True(t, ok, "a possibly materialized checkout must remain isolated")
	assert.True(t, entry.UnreviewedRemoteSource)
	assert.Empty(t, entry.CreationToken)
}

func TestManagerAddTrackingReportsOperationAndCreationReleaseErrors(t *testing.T) {
	worktreePath := filepath.Join(t.TempDir(), "failed-worktree")
	operationErr := errors.New("checkout failed")
	releaseErr := errors.New("creation lock release failed")
	state := &mockRemoteSourceState{creationReleaseError: releaseErr}
	manager := newMockManager(&mockGit{addError: operationErr}, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	_, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/failure", Source: "refs/remotes/origin/feature/failure", Path: worktreePath})

	require.ErrorIs(t, err, operationErr)
	require.ErrorIs(t, err, releaseErr)
}

func TestManagerAddTrackingRejectsActiveCreationMarker(t *testing.T) {
	worktreePath := filepath.Join(t.TempDir(), "creating-worktree")
	state := &mockRemoteSourceState{entries: map[string]*registry.WorktreeEntry{
		worktreePath: {
			Path:          worktreePath,
			Branch:        "feature/creating",
			CreationToken: "active-creation",
		},
	}, creationActive: true}
	mockG := &mockGit{}
	manager := newMockManager(mockG, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	_, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/competing", Source: "refs/remotes/origin/feature/competing", Path: worktreePath})

	require.ErrorContains(t, err, "worktree creation in progress")
	assert.Empty(t, mockG.worktrees)
	entry, ok := state.entries[worktreePath]
	require.True(t, ok)
	assert.Equal(t, "active-creation", entry.CreationToken)
}

func TestManagerAddTrackingRecoversAbandonedCreationMarker(t *testing.T) {
	worktreePath := filepath.Join(t.TempDir(), "abandoned-worktree")
	state := &mockRemoteSourceState{entries: map[string]*registry.WorktreeEntry{
		worktreePath: {
			Path:          worktreePath,
			Branch:        "feature/abandoned",
			CreationToken: "abandoned-creation",
		},
	}}
	mockG := &mockGit{}
	manager := newMockManager(mockG, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	created, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/recovered", Source: "refs/remotes/origin/feature/recovered", Path: worktreePath})
	path := created.Path

	require.NoError(t, err)
	assert.Equal(t, worktreePath, path)
	assert.Len(t, mockG.worktrees, 1)
	entry, ok := state.entries[worktreePath]
	require.True(t, ok)
	assert.Equal(t, "feature/recovered", entry.Branch)
	assert.Empty(t, entry.CreationToken)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", entry.Generation)
	assert.True(t, entry.UnreviewedRemoteSource)
}

func TestManagerAddTrackingFinalizesAbandonedCompletedCheckout(t *testing.T) {
	worktreePath := filepath.Join(t.TempDir(), "completed-worktree")
	generation := "fedcba9876543210fedcba9876543210"
	state := &mockRemoteSourceState{entries: map[string]*registry.WorktreeEntry{
		worktreePath: {
			Path:                   worktreePath,
			Branch:                 "feature/completed",
			CreationToken:          "abandoned-creation",
			UnreviewedRemoteSource: true,
		},
	}}
	mockG := &mockGit{worktrees: []models.Worktree{{
		Path:       worktreePath,
		Branch:     "feature/completed",
		Generation: generation,
	}}}
	manager := newMockManager(mockG, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	_, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/replacement", Source: "refs/remotes/origin/feature/replacement", Path: worktreePath})

	require.ErrorContains(t, err, "recovered completed remote-source worktree")
	assert.Len(t, mockG.worktrees, 1, "recovery must not replace the checkout")
	entry, ok := state.entries[worktreePath]
	require.True(t, ok)
	assert.Equal(t, "feature/completed", entry.Branch)
	assert.Empty(t, entry.CreationToken)
	assert.Equal(t, generation, entry.Generation)
	assert.True(t, entry.UnreviewedRemoteSource)
}

func TestManagerAddTrackingPreservesAbandonedGenerationlessCheckout(
	t *testing.T,
) {
	worktreePath := filepath.Join(t.TempDir(), "incomplete-worktree")
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))
	state := &mockRemoteSourceState{entries: map[string]*registry.WorktreeEntry{
		worktreePath: {
			Path:                   worktreePath,
			Branch:                 "feature/incomplete",
			CreationToken:          "abandoned-creation",
			UnreviewedRemoteSource: true,
		},
	}}
	mockG := &mockGit{
		addError: errors.New("already registered without a generation"),
		worktrees: []models.Worktree{{
			Path:   worktreePath,
			Branch: "feature/incomplete",
		}},
	}
	manager := newMockManager(mockG, &models.Config{})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return state, nil
	}

	_, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/incomplete", Source: "refs/remotes/origin/feature/incomplete", Path: worktreePath})

	require.ErrorContains(t, err, "already registered without a generation")
	assert.Empty(t, mockG.trackingSource)
	entry, ok := state.entries[worktreePath]
	require.True(t, ok)
	assert.Empty(t, entry.CreationToken)
	assert.Empty(t, entry.Generation)
	assert.Equal(t, "feature/incomplete", entry.Branch)
	assert.True(t, entry.UnreviewedRemoteSource)
}

func TestManagerAddTrackingDoesNotExpandRemoteBranchEnvironmentReferences(
	t *testing.T,
) {
	trustedBase := t.TempDir()
	t.Setenv("KWT_TEST_WORKTREE_BASE", trustedBase)
	t.Setenv("KWT_GITHUB_TOKEN", "credential-must-not-appear-in-path")
	mockG := &mockGit{repoURL: "https://github.com/acme/widget.git"}
	manager := newMockManager(mockG, &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir: "$KWT_TEST_WORKTREE_BASE",
		},
		Naming: models.NamingConfig{Template: "{{.Branch}}"},
	})
	manager.openRemoteSourceState = func() (remoteSourceState, error) {
		return &mockRemoteSourceState{}, nil
	}

	created, err := manager.Create(t.Context(), CreateOptions{Branch: "$KWT_GITHUB_TOKEN", Source: "refs/remotes/origin/$KWT_GITHUB_TOKEN", Path: ""})
	path := created.Path

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(trustedBase, "$KWT_GITHUB_TOKEN"), path)
	assert.NotContains(t, path, "credential-must-not-appear-in-path")
}

func TestManagerList(t *testing.T) {
	expectedWorktrees := []models.Worktree{
		{Path: "/path/1", Branch: "main", IsMain: true},
		{Path: "/path/2", Branch: "feature"},
	}

	mockG := &mockGit{
		worktrees: expectedWorktrees,
	}

	m := newMockManager(mockG, &models.Config{})

	worktrees, err := m.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(worktrees) != len(expectedWorktrees) {
		t.Errorf("List() returned %d worktrees, want %d", len(worktrees), len(expectedWorktrees))
	}
}

func TestManagerGetWorktreePath(t *testing.T) {
	mockG := &mockGit{
		worktrees: []models.Worktree{
			{Path: "/path/to/feature-test", Branch: "feature/test"},
			{Path: "/path/to/main", Branch: "main"},
			{Path: "/path/to/bugfix", Branch: "bugfix/issue-123"},
		},
	}

	m := newMockManager(mockG, &models.Config{})

	tests := []struct {
		name     string
		pattern  string
		wantPath string
		wantErr  bool
	}{
		{
			name:     "MatchBranch",
			pattern:  "feature",
			wantPath: "/path/to/feature-test",
		},
		{
			name:     "MatchPath",
			pattern:  "bugfix",
			wantPath: "/path/to/bugfix",
		},
		{
			name:    "NoMatch",
			pattern: "nonexistent",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := m.GetWorktreePath(tt.pattern)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetWorktreePath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && path != tt.wantPath {
				t.Errorf("GetWorktreePath() = %s, want %s", path, tt.wantPath)
			}
		})
	}
}

func TestManagerGetMatchingWorktrees(t *testing.T) {
	mockG := &mockGit{
		worktrees: []models.Worktree{
			{Path: "/path/to/feature-test", Branch: "feature/test"},
			{Path: "/path/to/main", Branch: "main"},
			{Path: "/path/to/bugfix", Branch: "bugfix/issue-123"},
			{Path: "/path/to/feature-auth", Branch: "feature/auth"},
			{Path: "/path/to/feature-api", Branch: "feature/api"},
		},
	}

	m := newMockManager(mockG, &models.Config{})

	tests := []struct {
		name         string
		pattern      string
		wantCount    int
		wantBranches []string
	}{
		{
			name:         "MatchMultiple",
			pattern:      "feature",
			wantCount:    3,
			wantBranches: []string{"feature/test", "feature/auth", "feature/api"},
		},
		{
			name:         "MatchSingle",
			pattern:      "main",
			wantCount:    1,
			wantBranches: []string{"main"},
		},
		{
			name:         "MatchPath",
			pattern:      "bugfix",
			wantCount:    1,
			wantBranches: []string{"bugfix/issue-123"},
		},
		{
			name:         "NoMatch",
			pattern:      "nonexistent",
			wantCount:    0,
			wantBranches: []string{},
		},
		{
			name:         "CaseInsensitive",
			pattern:      "FEATURE",
			wantCount:    3,
			wantBranches: []string{"feature/test", "feature/auth", "feature/api"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches, err := m.GetMatchingWorktrees(tt.pattern)
			if err != nil {
				t.Errorf("GetMatchingWorktrees() unexpected error = %v", err)
				return
			}

			if len(matches) != tt.wantCount {
				t.Errorf("GetMatchingWorktrees() returned %d matches, want %d", len(matches), tt.wantCount)
			}

			// Check that all expected branches are found
			foundBranches := make(map[string]bool)
			for _, wt := range matches {
				foundBranches[wt.Branch] = true
			}

			for _, expectedBranch := range tt.wantBranches {
				if !foundBranches[expectedBranch] {
					t.Errorf("Expected branch %s not found in matches", expectedBranch)
				}
			}
		})
	}
}

func TestManagerGetMatchingWorktreesResolvesEquivalentPaths(t *testing.T) {
	target := t.TempDir()
	alias := filepath.Join(t.TempDir(), "worktree-alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	m := newMockManager(&mockGit{
		worktrees: []models.Worktree{{
			Path:   target,
			Branch: "feature/equivalent-path",
		}},
	}, &models.Config{})

	matches, err := m.GetMatchingWorktrees(alias)

	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "feature/equivalent-path", matches[0].Branch)
}

func TestManagerGetMatchingWorktreesPrefersExactPath(t *testing.T) {
	exactPath := filepath.Join(t.TempDir(), "task")
	m := newMockManager(&mockGit{
		worktrees: []models.Worktree{
			{Path: exactPath, Branch: "feature/task"},
			{Path: exactPath + "-old", Branch: "feature/task-old"},
		},
	}, &models.Config{})

	matches, err := m.GetMatchingWorktrees(exactPath)

	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "feature/task", matches[0].Branch)
}

func TestManagerGetMatchingWorktreesPrefersCaseFoldedWindowsExactPath(
	t *testing.T,
) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path comparison")
	}
	m := newMockManager(&mockGit{
		worktrees: []models.Worktree{
			{Path: `C:\work\foo`, Branch: "feature/foo"},
			{Path: `C:\work\foo-old`, Branch: "feature/foo-old"},
		},
	}, &models.Config{})

	matches, err := m.GetMatchingWorktrees(`c:\WORK\foo`)

	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, `C:\work\foo`, matches[0].Path)
}

func TestManagerGetMatchingWorktreesPrefersAbsolutePathOverBranchSubstring(
	t *testing.T,
) {
	exactPath := filepath.Join(t.TempDir(), "task")
	unrelatedPath := filepath.Join(t.TempDir(), "unrelated")
	m := newMockManager(&mockGit{
		worktrees: []models.Worktree{
			{Path: exactPath, Branch: "feature/task"},
			{
				Path:   unrelatedPath,
				Branch: "topic" + filepath.ToSlash(exactPath),
			},
		},
	}, &models.Config{})

	matches, err := m.GetMatchingWorktrees(exactPath)

	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, exactPath, matches[0].Path)
}

func TestManagerGetMatchingWorktreesPrefersBranchOverRelativeSymlink(t *testing.T) {
	currentPath := t.TempDir()
	mainPath := t.TempDir()
	t.Chdir(currentPath)
	if err := os.Symlink(".", "main"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	m := newMockManager(&mockGit{
		worktrees: []models.Worktree{
			{Path: currentPath, Branch: "feature/current"},
			{Path: mainPath, Branch: "main"},
		},
	}, &models.Config{})

	matches, err := m.GetMatchingWorktrees("main")

	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, mainPath, matches[0].Path)
}

func TestManagerValidateWorktreePath(t *testing.T) {
	tests := []struct {
		name      string
		setupPath func() string
		wantErr   bool
		errMsg    string
	}{
		{
			name: "NonExistentPath",
			setupPath: func() string {
				return filepath.Join(t.TempDir(), "nonexistent")
			},
			wantErr: false,
		},
		{
			name: "EmptyDirectory",
			setupPath: func() string {
				dir := filepath.Join(t.TempDir(), "empty")
				_ = os.MkdirAll(dir, 0755)
				return dir
			},
			wantErr: false,
		},
		{
			name: "NonEmptyDirectory",
			setupPath: func() string {
				dir := filepath.Join(t.TempDir(), "nonempty")
				_ = os.MkdirAll(dir, 0755)
				_ = os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0644)
				return dir
			},
			wantErr: true,
			errMsg:  "directory is not empty",
		},
		{
			name: "ExistingFile",
			setupPath: func() string {
				dir := t.TempDir()
				file := filepath.Join(dir, "file")
				_ = os.WriteFile(file, []byte("content"), 0644)
				return file
			},
			wantErr: true,
			errMsg:  "is not a directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockManager(nil, &models.Config{})
			path := tt.setupPath()

			err := m.ValidateWorktreePath(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateWorktreePath() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateWorktreePath() error = %v, want error containing %s", err, tt.errMsg)
			}
		})
	}
}

func TestGenerateWorktreePath(t *testing.T) {
	defaultBaseDir := filepath.Join(t.TempDir(), "base")
	perRepoBaseDir := filepath.Join(t.TempDir(), "per-repo-base")
	tests := []struct {
		name               string
		branch             string
		repoName           string
		wantSuffix         string
		repoPath           string
		repositorySettings []models.RepositorySetting
		mainRepoPathError  error
		wantErr            bool
		wantBaseDir        string
	}{
		{
			name:       "BasicTemplate",
			branch:     "feature/test",
			repoName:   "myrepo",
			wantSuffix: "github.com/test-user/test-repo/feature-test",
		},
		{
			name:       "BranchOnly",
			branch:     "main",
			repoName:   "myrepo",
			wantSuffix: "github.com/test-user/test-repo/main",
		},
		{
			name:       "ComplexSanitization",
			branch:     "feature/test:new",
			repoName:   "myrepo",
			wantSuffix: "github.com/test-user/test-repo/feature-test-new",
		},
		{
			name:     "PerRepoBaseDir",
			branch:   "feature/test",
			repoName: "myrepo",
			repoPath: "/mock/repo/path",
			repositorySettings: []models.RepositorySetting{
				{Repository: "/mock/repo/path", BaseDir: perRepoBaseDir},
			},
			wantSuffix:  "github.com/test-user/test-repo/feature-test",
			wantBaseDir: perRepoBaseDir,
		},
		{
			name:     "PerRepoBaseDirEmpty",
			branch:   "feature/test",
			repoName: "myrepo",
			repoPath: "/mock/repo/path",
			repositorySettings: []models.RepositorySetting{
				{Repository: "/mock/repo/path", BaseDir: ""},
			},
			wantSuffix: "github.com/test-user/test-repo/feature-test",
		},
		{
			name:              "GetMainRepoPathError",
			branch:            "feature/test",
			repoName:          "myrepo",
			mainRepoPathError: errors.New("git error"),
			repositorySettings: []models.RepositorySetting{
				{Repository: "/mock/repo/path", BaseDir: perRepoBaseDir},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockG := &mockGit{
				repoName:          tt.repoName,
				repoPath:          tt.repoPath,
				mainRepoPathError: tt.mainRepoPathError,
			}

			config := &models.Config{
				Worktree: models.WorktreeConfig{
					BaseDir: defaultBaseDir,
				},
				RepositorySettings: tt.repositorySettings,
			}

			m := newMockManager(mockG, config)

			path, err := m.generateWorktreePath(tt.branch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("generateWorktreePath() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("generateWorktreePath() error = %v", err)
			}

			baseDir := defaultBaseDir
			if tt.wantBaseDir != "" {
				baseDir = tt.wantBaseDir
			}
			expectedPath := filepath.Join(baseDir, tt.wantSuffix)
			if path != expectedPath {
				t.Errorf("generateWorktreePath() = %s, want %s", path, expectedPath)
			}
		})
	}
}

func TestGenerateWorktreePathDefaultTemplatePreservesNestedRemoteNamespace(t *testing.T) {
	baseDir := t.TempDir()
	branch := "feature/read-api"

	makePath := func(repoURL string) string {
		t.Helper()
		m := newMockManager(&mockGit{repoURL: repoURL}, &models.Config{
			Worktree: models.WorktreeConfig{BaseDir: baseDir},
			Naming: models.NamingConfig{
				Template: configpkg.DefaultNamingTemplate,
				SanitizeChars: map[string]string{
					"/": "-",
					":": "-",
				},
			},
		})
		path, err := m.generateWorktreePath(branch)
		if err != nil {
			t.Fatalf("generateWorktreePath(%q): %v", repoURL, err)
		}
		return path
	}

	left := makePath("https://gitlab.com/org/team-a/service.git")
	right := makePath("https://gitlab.com/org/team-b/service.git")

	assertPath := func(got string, wantSuffix string) {
		t.Helper()
		want := filepath.Join(baseDir, wantSuffix)
		if got != want {
			t.Fatalf("generateWorktreePath() = %s, want %s", got, want)
		}
	}
	assertPath(left, "gitlab.com/org/team-a/service/feature-read-api")
	assertPath(right, "gitlab.com/org/team-b/service/feature-read-api")
	if left == right {
		t.Fatalf("nested remotes generated colliding paths: %s", left)
	}
}

func TestGenerateWorktreePathEncodesTmuxFormatCharacters(t *testing.T) {
	baseDir := t.TempDir()
	manager := newMockManager(&mockGit{
		repoURL: "https://github.com/acme/widget.git",
	}, &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir},
		Naming: models.NamingConfig{
			Template:      "{{.Branch}}",
			SanitizeChars: map[string]string{"/": "-"},
		},
	})

	path, err := manager.generateWorktreePath(
		"feature/%23/#(touch$IFS.pwn)",
	)

	require.NoError(t, err)
	assert.Equal(
		t,
		filepath.Join(baseDir, "feature-%2523-%23(touch$IFS.pwn)"),
		path,
	)
	assert.NotContains(t, path, "#")
}

// TestRepositoryInfoFromGitCanonicalResolver pins the single canonical
// resolver every identity-reporting surface routes through: an origin URL wins,
// a no-remote repository falls back to the "local/..." identity, and a
// non-git directory (no URL and no main-repo path) yields an error.
func TestRepositoryInfoFromGitCanonicalResolver(t *testing.T) {
	repoPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval repo path: %v", err)
	}
	localInfo, err := RepositoryInfoFromLocalPath(repoPath)
	if err != nil {
		t.Fatalf("building expected local identity: %v", err)
	}

	tests := []struct {
		name     string
		git      *mockGit
		wantFull string
		wantErr  bool
	}{
		{
			name:     "normal remote",
			git:      &mockGit{repoURL: "https://github.com/example/demo.git"},
			wantFull: "github.com/example/demo",
		},
		{
			name:     "no remote falls back to local identity",
			git:      &mockGit{repoURLError: errors.New("no origin"), repoPath: repoPath},
			wantFull: localInfo.FullPath,
		},
		{
			// git accepts a relative filesystem path with no leading "./" as a
			// remote; it must fall back to the local identity, not launder into
			// a shareable-looking "cache/team/repo" slug (the remote-derived
			// canonical bar every other provenance-gated surface applies).
			name:     "relative dotless remote falls back to local identity",
			git:      &mockGit{repoURL: "cache/team/repo.git", repoPath: repoPath},
			wantFull: localInfo.FullPath,
		},
		{
			// A dotted directory name does not make a relative path a remote:
			// git only reads a remote as non-path when it has a URL scheme or
			// an scp-style colon before the first slash (git-clone(1)).
			name:     "relative dotted remote falls back to local identity",
			git:      &mockGit{repoURL: "cache.example/team/repo.git", repoPath: repoPath},
			wantFull: localInfo.FullPath,
		},
		{
			// git-clone(1): scp-like syntax "is only recognized if there are
			// no slashes before the first colon", so this is a local path.
			name:     "colon after slash falls back to local identity",
			git:      &mockGit{repoURL: "team/na:me/repo", repoPath: repoPath},
			wantFull: localInfo.FullPath,
		},
		{
			name:     "scp alias remote resolves canonically",
			git:      &mockGit{repoURL: "workgit:org/repo.git"},
			wantFull: "workgit/org/repo",
		},
		{
			name:    "non-git dir yields an error",
			git:     &mockGit{repoURLError: errors.New("no origin"), mainRepoPathError: errors.New("not a git dir")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := RepositoryInfoFromGit(tt.git)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("RepositoryInfoFromGit() = %+v, want error", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("RepositoryInfoFromGit() error = %v", err)
			}
			if info.FullPath != tt.wantFull {
				t.Errorf("FullPath = %q, want %q", info.FullPath, tt.wantFull)
			}
		})
	}
}

// TestRepositoryInfoWithProjects pins the single registered-identity
// precedence policy: when the repository's main path matches a registered
// project whose configured Repository is canonical, the registered identity
// wins over the origin-derived one (a registered upstream identity beats a
// fork origin); otherwise the canonical resolver's origin-then-local
// precedence applies unchanged.
func TestRepositoryInfoWithProjects(t *testing.T) {
	repoPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval repo path: %v", err)
	}
	localInfo, err := RepositoryInfoFromLocalPath(repoPath)
	if err != nil {
		t.Fatalf("building expected local identity: %v", err)
	}
	forkGit := &mockGit{repoURL: "https://github.com/fork/demo.git", repoPath: repoPath}

	tests := []struct {
		name     string
		git      *mockGit
		projects []models.Project
		wantFull string
		wantErr  bool
	}{
		{
			name: "registered fork: registered identity wins over origin",
			git:  forkGit,
			projects: []models.Project{
				{Repository: "github.com/upstream/demo", Path: repoPath},
			},
			wantFull: "github.com/upstream/demo",
		},
		{
			name:     "unregistered repo: origin-derived",
			git:      forkGit,
			projects: nil,
			wantFull: "github.com/fork/demo",
		},
		{
			name: "registered project at a different path: origin-derived",
			git:  forkGit,
			projects: []models.Project{
				{Repository: "github.com/upstream/demo", Path: filepath.Join(repoPath, "elsewhere")},
			},
			wantFull: "github.com/fork/demo",
		},
		{
			name: "registered non-canonical identity: origin-derived",
			git:  forkGit,
			projects: []models.Project{
				{Repository: "local/home/user/demo", Path: repoPath},
			},
			wantFull: "github.com/fork/demo",
		},
		{
			name: "configured identity stays authoritative over a barred relative remote",
			git:  &mockGit{repoURL: "cache/team/repo.git", repoPath: repoPath},
			projects: []models.Project{
				{Repository: "cache/team/repo", Path: repoPath},
			},
			wantFull: "cache/team/repo",
		},
		{
			name: "configured port-authority identity stays authoritative",
			git:  &mockGit{repoURL: "localhost:8080/user/repo.git", repoPath: repoPath},
			projects: []models.Project{
				{Repository: "localhost:8080/user/repo", Path: repoPath},
			},
			wantFull: "localhost:8080/user/repo",
		},
		{
			name:     "no remote, unregistered: local fallback",
			git:      &mockGit{repoURLError: errors.New("no origin"), repoPath: repoPath},
			projects: nil,
			wantFull: localInfo.FullPath,
		},
		{
			name: "no remote, registered canonical: registered identity wins",
			git:  &mockGit{repoURLError: errors.New("no origin"), repoPath: repoPath},
			projects: []models.Project{
				{Repository: "github.com/upstream/demo", Path: repoPath},
			},
			wantFull: "github.com/upstream/demo",
		},
		{
			name:    "non-git dir yields an error",
			git:     &mockGit{repoURLError: errors.New("no origin"), mainRepoPathError: errors.New("not a git dir")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := RepositoryInfoWithProjects(tt.git, tt.projects)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("RepositoryInfoWithProjects() = %+v, want error", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("RepositoryInfoWithProjects() error = %v", err)
			}
			if info.FullPath != tt.wantFull {
				t.Errorf("FullPath = %q, want %q", info.FullPath, tt.wantFull)
			}
		})
	}
}

func TestRepositoryInfoFromExactLocalPathPreservesTrailingWhitespace(t *testing.T) {
	info, err := RepositoryInfoFromExactLocalPath("/repo ")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(info.FullPath, "/repo "), info.FullPath)
}

func TestManagerAddGeneratesPathForLocalOnlyRepository(t *testing.T) {
	baseDir := t.TempDir()
	repoPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to eval repo path: %v", err)
	}
	mockG := &mockGit{
		repoPath:      repoPath,
		repoURLError:  errors.New("no origin"),
		repoName:      filepath.Base(repoPath),
		worktrees:     nil,
		recentCommits: nil,
	}
	m := newMockManager(mockG, &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   baseDir,
			AutoMkdir: true,
		},
		Naming: models.NamingConfig{
			Template: configpkg.DefaultNamingTemplate,
			SanitizeChars: map[string]string{
				"/": "-",
				":": "-",
			},
		},
	})

	created, err := m.Create(t.Context(), CreateOptions{Branch: "feature/local", Path: "", NewBranch: true})
	path := created.Path

	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	want := filepath.Join(baseDir, localRepositoryFullPath(repoPath), "feature-local")
	if path != want {
		t.Fatalf("Add() path = %s, want %s", path, want)
	}
}

func TestLocalRepositoryFullPathUsesPathSafeRelativeIdentity(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		want        string
		skipWindows bool
	}{
		{
			name: "unix absolute path",
			path: "/var/repos/service",
			want: "local/var/repos/service",
		},
		{
			name: "windows drive path",
			path: `C:\Users\me\repo`,
			want: "local/C/Users/me/repo",
		},
		{
			name: "windows unc path",
			path: `\\server\share\repo`,
			want: "local/server/share/repo",
		},
		{
			name:        "unix literal backslash path",
			path:        `/tmp/foo\bar/repo`,
			want:        "local/tmp/foo%5Cbar/repo",
			skipWindows: true,
		},
		{
			name: "unix slash path distinct from literal backslash",
			path: "/tmp/foo/bar/repo",
			want: "local/tmp/foo/bar/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipWindows && runtime.GOOS == "windows" {
				t.Skip("backslash is a path separator on Windows")
			}
			got := localRepositoryFullPath(tt.path)

			if got != tt.want {
				t.Fatalf("localRepositoryFullPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestGenerateWorktreePathRejectsPathOutsideBaseDir(t *testing.T) {
	baseDir := t.TempDir()
	tests := []struct {
		name     string
		template string
		repoURL  string
	}{
		{
			name:     "template escapes base",
			template: "../{{.Branch}}",
			repoURL:  "https://github.com/test-user/test-repo.git",
		},
		{
			name:     "full path template escapes after cleaning",
			template: "{{.FullPath}}/../../../../{{.Branch}}",
			repoURL:  "https://github.com/test-user/test-repo.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockManager(&mockGit{repoURL: tt.repoURL}, &models.Config{
				Worktree: models.WorktreeConfig{BaseDir: baseDir},
				Naming: models.NamingConfig{
					Template: tt.template,
					SanitizeChars: map[string]string{
						"/": "-",
						":": "-",
					},
				},
			})

			path, err := m.generateWorktreePath("feature/read-api")

			if err == nil {
				t.Fatalf("generateWorktreePath() expected containment error, got path %s", path)
			}
			if !strings.Contains(err.Error(), "outside worktree base") {
				t.Fatalf("generateWorktreePath() error = %v, want outside worktree base", err)
			}
		})
	}
}

func TestPreparePathDoesNotExpandRepositoryLocalTemplateOutput(t *testing.T) {
	t.Setenv("KWT_GITHUB_TOKEN", "credential-must-not-appear-in-path")
	baseDir := t.TempDir()
	manager := newMockManager(
		&mockGit{repoURL: "https://github.com/acme/widget.git"},
		&models.Config{
			Worktree: models.WorktreeConfig{BaseDir: baseDir},
			Naming: models.NamingConfig{
				Template:                `{{printf "%c%s" 36 "KWT_GITHUB_TOKEN"}}/{{.Branch}}`,
				TemplateRepositoryLocal: true,
			},
		},
	)

	path, err := manager.PreparePath("", "feature/widgets")

	require.NoError(t, err)
	assert.NotContains(t, path, "credential-must-not-appear-in-path")
	assert.Contains(t, path, "$KWT_GITHUB_TOKEN")
}

func TestPreparePathUsesBareContainerSiblingLayout(t *testing.T) {
	containerPath := filepath.Join(t.TempDir(), "widget")
	manager := newMockManager(
		&mockGit{
			repoURL:           "https://github.com/acme/widget.git",
			bareContainerPath: containerPath,
		},
		&models.Config{
			Worktree: models.WorktreeConfig{
				BaseDir: filepath.Join(t.TempDir(), "global-worktrees"),
			},
			Naming: models.NamingConfig{
				Template: "{{.FullPath}}/{{.Branch}}",
				SanitizeChars: map[string]string{
					"/": "-",
					":": "-",
				},
			},
		},
	)

	path, err := manager.PreparePath("", "feature/widgets")

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(containerPath, "feature-widgets"), path)
}

func TestAddRejectsTmuxFormatPathBeforeMutation(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "uncreated")
	worktreePath := filepath.Join(parent, "feature#widgets")
	repositoryGit := &mockGit{}
	manager := newMockManager(repositoryGit, &models.Config{
		Worktree: models.WorktreeConfig{AutoMkdir: true},
	})

	created, err := manager.Create(t.Context(), CreateOptions{Branch: "feature/widgets", Path: worktreePath, NewBranch: true})
	path := created.Path

	require.Error(t, err)
	assert.Empty(t, path)
	assert.Contains(t, err.Error(), "tmux format syntax")
	assert.Empty(t, repositoryGit.worktrees)
	assert.NoDirExists(t, parent)
}

func TestPreparePathExpandsOnlyLiteralTextInGlobalTemplate(t *testing.T) {
	t.Setenv("KWT_WORKTREE_GROUP", "trusted-group")
	baseDir := t.TempDir()
	manager := newMockManager(
		&mockGit{repoURL: "https://github.com/acme/widget.git"},
		&models.Config{
			Worktree: models.WorktreeConfig{BaseDir: baseDir},
			Naming: models.NamingConfig{
				Template: `$KWT_WORKTREE_GROUP/{{$branch := .Branch}}{{$branch}}`,
				SanitizeChars: map[string]string{
					"/": "-",
				},
			},
		},
	)

	path, err := manager.PreparePath("", "feature/widgets")

	require.NoError(t, err)
	assert.Equal(
		t,
		filepath.Join(baseDir, "trusted-group", "feature-widgets"),
		path,
	)
}

func TestPreparePathExpandsGlobalSanitizationReplacements(t *testing.T) {
	t.Setenv("KWT_BRANCH_SEPARATOR", "__")
	baseDir := t.TempDir()
	manager := newMockManager(
		&mockGit{repoURL: "https://github.com/acme/widget.git"},
		&models.Config{
			Worktree: models.WorktreeConfig{BaseDir: baseDir},
			Naming: models.NamingConfig{
				Template: "{{.Branch}}",
				SanitizeChars: map[string]string{
					"/": "$KWT_BRANCH_SEPARATOR",
				},
			},
		},
	)

	path, err := manager.PreparePath("", "feature/widgets")

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(baseDir, "feature__widgets"), path)
}

func TestPreparePathExpandsNamingComponentsByProvenance(t *testing.T) {
	t.Setenv("KWT_WORKTREE_GROUP", "trusted-group")
	t.Setenv("KWT_BRANCH_SEPARATOR", "__")
	baseDir := t.TempDir()
	manager := newMockManager(
		&mockGit{repoURL: "https://github.com/acme/widget.git"},
		&models.Config{
			Worktree: models.WorktreeConfig{BaseDir: baseDir},
			Naming: models.NamingConfig{
				Template:                     "$KWT_WORKTREE_GROUP/{{.Branch}}",
				TemplateRepositoryLocal:      false,
				SanitizeCharsRepositoryLocal: true,
				SanitizeChars: map[string]string{
					"/": "$KWT_BRANCH_SEPARATOR",
				},
			},
		},
	)

	path, err := manager.PreparePath("", "feature/widgets")

	require.NoError(t, err)
	assert.Equal(
		t,
		filepath.Join(
			baseDir,
			"trusted-group",
			"feature$KWT_BRANCH_SEPARATORwidgets",
		),
		path,
	)
}

func TestGenerateWorktreePathRejectsSymlinkEscapeFromBaseDir(t *testing.T) {
	tempDir := t.TempDir()
	baseDir := filepath.Join(tempDir, "base")
	outsideDir := filepath.Join(tempDir, "outside")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		t.Fatalf("failed to create base dir: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(baseDir, "escape")); err != nil {
		t.Skipf("symbolic links are not supported or allowed on this filesystem: %v", err)
	}
	m := newMockManager(&mockGit{repoURL: "https://github.com/test-user/test-repo.git"}, &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir},
		Naming: models.NamingConfig{
			Template: "escape/{{.Branch}}",
			SanitizeChars: map[string]string{
				"/": "-",
				":": "-",
			},
		},
	})

	path, err := m.generateWorktreePath("feature/read-api")

	if err == nil {
		t.Fatalf("generateWorktreePath() expected containment error, got path %s", path)
	}
	if !strings.Contains(err.Error(), "outside worktree base") {
		t.Fatalf("generateWorktreePath() error = %v, want outside worktree base", err)
	}
}

func TestManagerAdd_ConfigurableSetupIntegration(t *testing.T) {
	repoDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to eval symlinks: %v", err)
	}
	worktreeDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to eval symlinks: %v", err)
	}

	srcFile := filepath.Join(repoDir, "copyme.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write src file: %v", err)
	}

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   worktreeDir,
			AutoMkdir: true,
		},
		RepositorySettings: []models.RepositorySetting{
			{
				Repository:    repoDir,
				CopyFiles:     []string{"copyme.txt"},
				SetupCommands: []string{"echo test"},
			},
		},
	}

	mockG := &mockGit{repoPath: repoDir}
	m := newMockManager(mockG, cfg)

	_, err = m.Create(t.Context(), CreateOptions{Branch: "feature/test", Path: filepath.Join(worktreeDir, "wt1"), NewBranch: true})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	copied := filepath.Join(worktreeDir, "wt1", "copyme.txt")
	if _, err := os.Stat(copied); err != nil {
		t.Errorf("expected file to be copied: %v", err)
	}
}

func TestManagerAdd_SetupFromWorktreeContext(t *testing.T) {
	repoDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to eval symlinks: %v", err)
	}
	worktreeDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to eval symlinks: %v", err)
	}

	srcFile := filepath.Join(repoDir, "copyme.txt")
	if err := os.WriteFile(srcFile, []byte("from worktree"), 0644); err != nil {
		t.Fatalf("failed to write src file: %v", err)
	}

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   worktreeDir,
			AutoMkdir: true,
		},
		RepositorySettings: []models.RepositorySetting{
			{
				Repository: repoDir,
				CopyFiles:  []string{"copyme.txt"},
			},
		},
	}

	// repoPath is repoDir but cwd is different — simulates running from worktree
	mockG := &mockGit{repoPath: repoDir}
	m := newMockManager(mockG, cfg)

	_, err = m.Create(t.Context(), CreateOptions{Branch: "feature/wt-test", Path: filepath.Join(worktreeDir, "wt1"), NewBranch: true})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	copied := filepath.Join(worktreeDir, "wt1", "copyme.txt")
	if _, err := os.Stat(copied); err != nil {
		t.Errorf("expected file to be copied from worktree context: %v", err)
	}
}
