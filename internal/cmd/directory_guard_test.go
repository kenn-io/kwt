package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/tmux"
	dashboard "go.kenn.io/kwt/internal/tui"
	"go.kenn.io/kwt/pkg/models"
)

func TestCoreDirectoryEstablishmentHoldsRepositoryGuard(t *testing.T) {
	isolateCommandTestHome(t)
	for _, checkout := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary directory", true: "checkout root"}[checkout], func(t *testing.T) {
			path := t.TempDir()
			if checkout {
				path = newTUITestRepo(t)
			}
			cfg := &models.Config{}
			cfg.Workspace.Backend = "herdr-core"
			backend := newTUIBackendWithLaunchDir(cfg, "")
			called := false
			backend.ensureWorkspace = func(_ context.Context, session, directory string, _ models.Layout) (tmux.SessionEndpoint, error) {
				called = true
				require.Equal(t, path, directory)
				require.Equal(t, tmux.DirWorkspaceSessionName("notes", path), session)
				if checkout {
					lock := flock.New(filepath.Join(path, ".git", "kwt-worktree.lock"))
					locked, err := lock.TryLock()
					require.NoError(t, err)
					if locked {
						require.NoError(t, lock.Unlock())
					}
					require.False(t, locked, "Core establishment must hold the repository mutation lock")
				}
				return tmux.SessionEndpoint{Runtime: "herdr-core", SessionName: session}, nil
			}
			_, err := backend.establishWorkspaceEndpoint(t.Context(), dashboard.Row{Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: path}}, tmux.BlankLayoutName, false)
			require.NoError(t, err)
			require.True(t, called)
		})
	}
}

func TestCoreDirectoryEstablishmentRejectsChangedCheckout(t *testing.T) {
	isolateCommandTestHome(t)
	path := newTUITestRepo(t)
	cfg := &models.Config{}
	cfg.Workspace.Backend = "herdr-core"
	backend := newTUIBackendWithLaunchDir(cfg, "")
	called := false
	backend.ensureWorkspace = func(context.Context, string, string, models.Layout) (tmux.SessionEndpoint, error) {
		called = true
		return tmux.SessionEndpoint{}, nil
	}
	originalHook := beforeProjectGuardAcquire
	t.Cleanup(func() { beforeProjectGuardAcquire = originalHook })
	beforeProjectGuardAcquire = func() {
		require.NoError(t, os.WriteFile(filepath.Join(path, ".git", "kwt-generation"), []byte("ffffffffffffffffffffffffffffffff\n"), 0600))
	}
	_, err := backend.establishWorkspaceEndpoint(t.Context(), dashboard.Row{Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: path}}, tmux.BlankLayoutName, false)
	require.Error(t, err)
	require.False(t, called)
}

func TestCoreDirectoryOpenWaitsForRepositoryMutation(t *testing.T) {
	isolateCommandTestHome(t)
	path := newTUITestRepo(t)
	cfg := &models.Config{}
	cfg.Workspace.Backend = "herdr-core"
	cfg.Workspace.CoreSocket = filepath.Join(t.TempDir(), "missing.sock")
	originalLayout := openLayout
	openLayout = tmux.BlankLayoutName
	t.Cleanup(func() { openLayout = originalLayout })
	lock := flock.New(filepath.Join(path, ".git", "kwt-worktree.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := openSelectedDirectoryWorkspace(ctx, &CommandContext{Config: cfg}, models.Workspace{Name: "notes", Path: path}, nil, true, false)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
