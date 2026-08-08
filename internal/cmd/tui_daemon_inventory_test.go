package cmd

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/pkg/models"
	publicworktree "go.kenn.io/kwt/worktree"
)

func TestTUIBackendDaemonInventoryUsesCacheThenCurrent(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
	}, "/launch")
	backend.listSessions = func() ([]string, error) { return nil, nil }
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{}, nil
	}
	backend.registerProject = nil
	backend.registerWorkspace = nil
	var requests []publicworktree.Request
	backend.queryInventory = func(
		_ context.Context,
		request publicworktree.Request,
		_ bool,
		_ io.Writer,
	) (publicworktree.Result, error) {
		requests = append(requests, request)
		path := "/cached"
		if request.RequireCurrent {
			path = "/fresh"
		}
		return publicworktree.Result{Snapshot: publicworktree.Snapshot{Entries: []publicworktree.Entry{{
			Path: path, Branch: "main", Repository: publicworktree.Repository{FullPath: "github.com/acme/repo", Name: "repo"},
		}}}}, nil
	}

	fast, _, err := backend.ListFast(context.Background())
	require.NoError(t, err)
	current, _, err := backend.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/cached", fast[0].Entry.Path)
	assert.Equal(t, "/fresh", current[0].Entry.Path)
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
	backend.registerProject = func(project models.Project) error {
		registered = append(registered, project)
		return nil
	}
	backend.queryInventory = func(
		_ context.Context,
		request publicworktree.Request,
		_ bool,
		_ io.Writer,
	) (publicworktree.Result, error) {
		unrelated := publicworktree.Entry{
			Path: "/other", IsMain: true,
			Repository: publicworktree.Repository{
				URL: "https://github.com/acme/other.git", FullPath: "github.com/acme/other", Name: "other",
			},
		}
		launch := publicworktree.Entry{
			Path: "/launch", IsMain: true,
			Repository: publicworktree.Repository{
				URL: "https://github.com/acme/launch.git", FullPath: "github.com/acme/launch", Name: "launch",
			},
		}
		freshness := publicworktree.Stale
		if request.RequireCurrent {
			freshness = publicworktree.Fresh
		}
		return publicworktree.Result{
			Freshness: freshness,
			Snapshot: publicworktree.Snapshot{
				Entries:       []publicworktree.Entry{unrelated, launch},
				LaunchEntries: []publicworktree.Entry{launch},
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
