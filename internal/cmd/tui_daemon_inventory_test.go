package cmd

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/pkg/models"
)

func TestTUIBackendMutationUsesLatestDaemonConfiguration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, config.Init())
	staleBase := filepath.Join(t.TempDir(), "stale")
	viper.Set("worktree.basedir", staleBase)
	viper.Set("worktree.auto_mkdir", true)
	viper.Set("naming.template", "{{.Branch}}")
	viper.Set("naming.sanitize_chars", map[string]string{"/": "-"})

	repository := newTUITestRepo(t)
	runTUITestGit(t, repository, "remote", "add", "origin", "https://github.com/acme/widget.git")
	currentBase := filepath.Join(t.TempDir(), "current")
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.listSessions = func() ([]string, error) { return nil, nil }
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{}, nil
	}
	backend.queryInventory = func(
		context.Context,
		kwt.Request,
		bool,
		io.Writer,
	) (kwt.Result, error) {
		return kwt.Result{
			Freshness: kwt.Fresh,
			Snapshot: kwt.Snapshot{
				Config: &models.Config{
					Worktree: models.WorktreeConfig{BaseDir: currentBase, AutoMkdir: true},
					Naming: models.NamingConfig{
						Template: "{{.Branch}}", SanitizeChars: map[string]string{"/": "-"},
					},
				},
				Entries: []kwt.Entry{{
					Path: repository, Branch: "main", IsMain: true,
					Repository: kwt.Repository{FullPath: "github.com/acme/widget", Name: "widget"},
				}},
			},
		}, nil
	}

	rows, _, err := backend.List(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	planned, err := backend.PreviewWorktree(rows[0], "feature/refreshed")

	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(planned.Entry.Path))
	assert.Equal(t, filepath.Join(currentBase, "feature-refreshed"), planned.Entry.Path)
}

func TestTUIBackendDaemonInventoryUsesCacheThenCurrent(t *testing.T) {
	cfg := &models.Config{
		Worktree:   models.WorktreeConfig{BaseDir: t.TempDir()},
		Projects:   []models.Project{{Repository: "github.com/acme/live", Name: "live", Path: "/live"}},
		Workspaces: []models.Workspace{{Name: "live", Path: "/live/workspace"}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "/launch")
	backend.listSessions = func() ([]string, error) { return nil, nil }
	var statusBaseDirectory string
	backend.collectStatuses = func(_ context.Context, baseDirectory string, _ []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, error) {
		statusBaseDirectory = baseDirectory
		return map[string]*models.WorktreeStatus{}, nil
	}
	backend.registerProject = nil
	backend.registerWorkspace = nil
	var requests []kwt.Request
	backend.queryInventory = func(
		_ context.Context,
		request kwt.Request,
		_ bool,
		_ io.Writer,
	) (kwt.Result, error) {
		requests = append(requests, request)
		path := "/cached"
		project := models.Project{Repository: "github.com/acme/cached", Name: "cached", Path: "/cached"}
		workspace := models.Workspace{Name: "cached", Path: "/cached/workspace"}
		freshness := kwt.Stale
		if request.RequireCurrent {
			path = "/fresh"
			project = models.Project{Repository: "github.com/acme/fresh", Name: "fresh", Path: "/fresh"}
			workspace = models.Workspace{Name: "fresh", Path: "/fresh/workspace"}
			freshness = kwt.Fresh
		}
		effective := &models.Config{
			Worktree:   models.WorktreeConfig{BaseDir: "/cached-base"},
			Projects:   []models.Project{project},
			Workspaces: []models.Workspace{workspace},
			Layouts:    models.LayoutsConfig{Default: "cached-layout"},
		}
		if request.RequireCurrent {
			effective.Worktree.BaseDir = "/fresh-base"
			effective.Layouts.Default = "fresh-layout"
		}
		return kwt.Result{
			Freshness: freshness,
			Snapshot: kwt.Snapshot{
				Config:   effective,
				Projects: []models.Project{project},
				Entries: []kwt.Entry{{
					Path: path, Branch: "main", Repository: kwt.Repository{FullPath: "github.com/acme/repo", Name: "repo"},
				}},
				Workspaces: []models.Workspace{workspace},
			},
		}, nil
	}

	fast, _, err := backend.ListFast(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/live", cfg.Projects[0].Path)
	assert.Equal(t, "/live/workspace", cfg.Workspaces[0].Path)
	assert.Equal(t, "/cached/workspace", fast[1].Workspace.Path)
	current, _, err := backend.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/cached", fast[0].Entry.Path)
	assert.Equal(t, "/fresh", current[0].Entry.Path)
	assert.Equal(t, "/fresh", cfg.Projects[0].Path)
	assert.Equal(t, "/fresh/workspace", cfg.Workspaces[0].Path)
	assert.Equal(t, "/fresh-base", cfg.Worktree.BaseDir)
	assert.Equal(t, "fresh-layout", cfg.Layouts.Default)
	assert.Equal(t, "/fresh-base", statusBaseDirectory)
	require.Len(t, requests, 2)
	assert.False(t, requests[0].RequireCurrent)
	assert.True(t, requests[1].RequireCurrent)
}

func TestTUIBackendRegistersOnlyCurrentLaunchInventory(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
	}, "/launch")
	backend.listSessions = func() ([]string, error) { return nil, nil }
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{}, nil
	}
	backend.registerWorkspace = nil
	var registered []models.Project
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project)
		return nil
	}
	backend.queryInventory = func(
		_ context.Context,
		request kwt.Request,
		_ bool,
		_ io.Writer,
	) (kwt.Result, error) {
		unrelated := kwt.Entry{
			Path: "/other", IsMain: true,
			Repository: kwt.Repository{
				URL: "https://github.com/acme/other.git", FullPath: "github.com/acme/other", Name: "other",
			},
		}
		launch := kwt.Entry{
			Path: "/launch", IsMain: true,
			Repository: kwt.Repository{
				URL: "https://github.com/acme/launch.git", FullPath: "github.com/acme/launch", Name: "launch",
			},
		}
		freshness := kwt.Stale
		if request.RequireCurrent {
			freshness = kwt.Fresh
		}
		return kwt.Result{
			Freshness: freshness,
			Snapshot: kwt.Snapshot{
				Config:        &models.Config{},
				Entries:       []kwt.Entry{unrelated, launch},
				LaunchEntries: []kwt.Entry{launch},
			},
		}, nil
	}

	_, _, err := backend.ListFast(context.Background())
	require.NoError(t, err)
	assert.Empty(t, registered, "stale inventory must not mutate launch registration")
	_, _, err = backend.List(context.Background())
	require.NoError(t, err)

	require.Len(t, registered, 1)
	assert.Equal(t, "github.com/acme/launch", registered[0].Repository)
	assert.Equal(t, "/launch", registered[0].Path)
}

func TestTUIBackendCurrentInventoryIncludesNewLaunchWorkspace(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
	}, "/launch")
	backend.listSessions = func() ([]string, error) { return nil, nil }
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{}, nil
	}
	backend.registerProject = nil
	backend.registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		workspace.Name = "launch"
		return workspace, nil
	}
	backend.queryInventory = func(
		context.Context,
		kwt.Request,
		bool,
		io.Writer,
	) (kwt.Result, error) {
		return kwt.Result{
			Freshness: kwt.Fresh,
			Snapshot:  kwt.Snapshot{Config: &models.Config{}},
		}, nil
	}

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Workspace)
	assert.Equal(t, "launch", rows[0].Workspace.Name)
	assert.Equal(t, "/launch", rows[0].Workspace.Path)
}
