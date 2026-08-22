package cmd

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/tmux"
	dashboard "go.kenn.io/kwt/internal/tui"
	"go.kenn.io/kwt/pkg/models"
)

func TestTUIBackendInventoryModesSeparateCurrencyAndStatus(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "/launch")
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	var requests []kwt.Request
	var statusCalls int
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, []string, error) {
		statusCalls++
		return map[string]*models.WorktreeStatus{}, nil, nil
	}
	backend.queryInventory = func(_ context.Context, request kwt.Request, _ bool, _ io.Writer) (kwt.Result, error) {
		requests = append(requests, request)
		return kwt.Result{Freshness: kwt.Fresh, Snapshot: kwt.Snapshot{
			Config:  &models.Config{},
			Entries: []kwt.Entry{{Path: "/work", IsMain: true, Repository: kwt.Repository{FullPath: "github.com/acme/widget"}}},
		}}, nil
	}

	_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCachedDashboard})
	_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard})
	_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard, CollectStatuses: true})

	require.Len(t, requests, 3)
	assert.False(t, requests[0].RequireCurrent)
	assert.True(t, requests[1].RequireCurrent)
	assert.True(t, requests[2].RequireCurrent)
	assert.Equal(t, 1, statusCalls)
}

func TestCurrentDashboardWithoutStatusStillAppliesConfigAndLaunchRegistration(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "/launch")
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	var registered []string
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project.Path)
		return nil
	}
	backend.registerWorkspace = nil
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{Freshness: kwt.Fresh, Snapshot: kwt.Snapshot{
			Config:        &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/fresh-base"}},
			Entries:       []kwt.Entry{{Path: "/launch", IsMain: true, Repository: kwt.Repository{URL: "https://github.com/acme/widget.git", FullPath: "github.com/acme/widget"}}},
			LaunchEntries: []kwt.Entry{{Path: "/launch", IsMain: true, Repository: kwt.Repository{URL: "https://github.com/acme/widget.git", FullPath: "github.com/acme/widget"}}},
		}}, nil
	}
	_, err := backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard})
	require.NoError(t, err)
	assert.Equal(t, "/fresh-base", backend.cfg.Worktree.BaseDir)
	assert.Equal(t, []string{"/launch"}, registered)
}

func TestInventoryQueryDoesNotHoldBackendConfigurationLock(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "/launch")
	entered := make(chan struct{})
	release := make(chan struct{})
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		close(entered)
		<-release
		return kwt.Result{}, nil
	}
	done := make(chan struct{})
	go func() {
		_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard})
		close(done)
	}()
	<-entered

	layouts := make(chan []string, 1)
	go func() { layouts <- backend.LayoutNames() }()
	select {
	case <-layouts:
	case <-time.After(time.Second):
		t.Fatal("inventory query held the backend configuration lock")
	}
	close(release)
	<-done
}

func TestRepositoryInventoryRejectsGlobalFallback(t *testing.T) {
	workingDirectory := t.TempDir()
	backend := newTUIBackendWithLaunchDir(&models.Config{}, workingDirectory)
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{Freshness: kwt.Fresh, Snapshot: kwt.Snapshot{Entries: []kwt.Entry{
			{Path: workingDirectory, IsMain: true, Repository: kwt.Repository{FullPath: "github.com/acme/expected"}},
			{Path: "/other", IsMain: true, Repository: kwt.Repository{FullPath: "github.com/acme/other"}},
		}}}, nil
	}
	_, err := backend.LoadInventory(context.Background(), dashboard.InventoryRequest{
		Scope: dashboard.InventoryCurrentRepository, WorkingDirectory: workingDirectory,
		ProjectIdentity: "github.com/acme/expected", CollectStatuses: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned unrelated repository")
}

func TestRepositoryInventoryRejectsEmptyResult(t *testing.T) {
	workingDirectory := t.TempDir()
	backend := newTUIBackendWithLaunchDir(&models.Config{}, workingDirectory)
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{Freshness: kwt.Fresh}, nil
	}
	_, err := backend.LoadInventory(context.Background(), dashboard.InventoryRequest{
		Scope: dashboard.InventoryCurrentRepository, WorkingDirectory: workingDirectory,
		ProjectIdentity: "github.com/acme/expected",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned no worktrees")
}

func TestRepositoryInventoryRejectsDeletedWorkingDirectoryBeforeQuery(t *testing.T) {
	workingDirectory := filepath.Join(t.TempDir(), "gone")
	backend := newTUIBackendWithLaunchDir(&models.Config{}, workingDirectory)
	queried := false
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		queried = true
		return kwt.Result{}, nil
	}
	_, err := backend.LoadInventory(context.Background(), dashboard.InventoryRequest{
		Scope: dashboard.InventoryCurrentRepository, WorkingDirectory: workingDirectory,
		ProjectIdentity: "github.com/acme/expected",
	})
	require.Error(t, err)
	assert.False(t, queried)
}

func TestTUIBackendApplyInventoryConfigRewiresCleanupResolver(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing"))
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.liveEndpoints = func(
		context.Context,
		tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		return nil, errors.New("stale cleanup resolver")
	}

	err := backend.applyInventoryConfig(&models.Config{})
	require.NoError(t, err)
	endpoints, err := backend.liveEndpoints(
		context.Background(),
		tmux.WorkspaceEndpointRequest{SessionName: "workspace", WorkspacePath: "/work"},
	)

	require.NoError(t, err)
	assert.Empty(t, endpoints)
}

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
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{}, nil, nil
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
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	var statusBaseDirectory string
	backend.collectStatuses = func(_ context.Context, baseDirectory string, _ []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, []string, error) {
		statusBaseDirectory = baseDirectory
		return map[string]*models.WorktreeStatus{}, nil, nil
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
				Config: effective,
				Projects: []kwt.Project{{
					Repository:  project.Repository,
					Name:        project.Name,
					Path:        project.Path,
					LastTouched: project.LastTouched,
				}},
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
	assert.True(t, requests[0].IncludeProtectedSockets)
	assert.True(t, requests[1].IncludeProtectedSockets)
}

func TestTUIBackendDaemonInventoryRetainsProtectedEndpoint(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.resolveSessions = func(
		_ context.Context,
		requests []tmux.WorkspaceEndpointRequest,
	) ([]tmux.WorkspaceSession, error) {
		assert.Empty(t, requests, "protected workspaces must bypass shared-server resolution")
		return nil, nil
	}
	var request kwt.Request
	backend.queryInventory = func(
		_ context.Context,
		got kwt.Request,
		_ bool,
		_ io.Writer,
	) (kwt.Result, error) {
		request = got
		return kwt.Result{Snapshot: kwt.Snapshot{Entries: []kwt.Entry{{
			Path:           "/work/protected",
			Branch:         "feature/protected",
			Repository:     kwt.Repository{FullPath: "github.com/acme/widget", Name: "widget"},
			SessionName:    "kwt-wt-widget-feature-protected-01234567",
			TmuxSocketName: "kwt-pr-0123456789abcdef",
			TmuxAttachMode: models.TmuxAttachProtected,
		}}}}, nil
	}

	rows, _, err := backend.ListFast(context.Background())

	require.NoError(t, err)
	assert.True(t, request.IncludeProtectedSockets)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Entry)
	assert.True(t, rows[0].Entry.Protected)
	assert.Equal(t, "kwt-pr-0123456789abcdef", rows[0].TmuxEndpoint.SocketName)
	assert.Equal(t, "kwt-wt-widget-feature-protected-01234567", rows[0].TmuxEndpoint.SessionName)
}

func TestTUIBackendDaemonInventoryReusesPublishedDirectSession(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.resolveSessions = func(
		_ context.Context,
		requests []tmux.WorkspaceEndpointRequest,
	) ([]tmux.WorkspaceSession, error) {
		assert.Empty(t, requests, "daemon worktrees must not be resolved twice")
		return nil, nil
	}
	backend.queryInventory = func(
		context.Context,
		kwt.Request,
		bool,
		io.Writer,
	) (kwt.Result, error) {
		return kwt.Result{Snapshot: kwt.Snapshot{Entries: []kwt.Entry{{
			Path:           "/work/adopted",
			Branch:         "feature/adopted",
			Repository:     kwt.Repository{FullPath: "github.com/acme/widget", Name: "widget"},
			SessionName:    "kwt-wt-widget-feature-adopted-01234567",
			SessionLive:    true,
			TmuxAttachMode: models.TmuxAttachDirect,
		}}}}, nil
	}

	rows, _, err := backend.ListFast(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].SessionLive)
	assert.Empty(t, rows[0].TmuxEndpoint.SocketName)
	assert.Equal(t, "kwt-wt-widget-feature-adopted-01234567", rows[0].SessionName)
}

func TestTUIBackendRegistersOnlyCurrentLaunchInventory(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
	}, "/launch")
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{}, nil, nil
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
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{}, nil, nil
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
