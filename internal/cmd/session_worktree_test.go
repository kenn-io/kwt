package cmd

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	kwttmux "go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/tmuxrestore"
	"go.kenn.io/kwt/pkg/models"
	shared "go.kenn.io/kwt/worktree"
)

// savedWorktree is a real checkout with a kwt generation, as restore finds
// it after a reboot.
type savedWorktree struct {
	path       string
	generation string
	session    string
	runner     *recordingOpenWorkspaceRunner
}

func newSavedWorktree(t *testing.T) savedWorktree {
	t.Helper()
	t.Setenv("KWT_HOME", t.TempDir())
	repo := t.TempDir()
	require.NoError(t, exec.CommandContext(t.Context(), "git", "init", "-b", "main", repo).Run())
	generation, err := openSharedWorktrees(t, git.New(repo)).EnsureIdentity(t.Context(), repo,
		shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	session, _, err := lifecycle.ResolveCurrentWorktreeSessionIdentity(t.Context(), repo, nil, nil)
	require.NoError(t, err)

	w := savedWorktree{path: repo, generation: generation, session: session, runner: &recordingOpenWorkspaceRunner{}}
	old := newSavedWorktreeSessions
	t.Cleanup(func() { newSavedWorktreeSessions = old })
	newSavedWorktreeSessions = func([]string, kwttmux.SkeletonRestorer) openWorkspaceRunner { return w.runner }
	return w
}

func (w savedWorktree) entry(session, path, generation string) tmuxrestore.Entry {
	var entry tmuxrestore.Entry
	entry.Session.Name, entry.Session.Path, entry.Session.WorkspaceGeneration = session, path, generation
	return entry
}

func TestSavedWorktreeSessionOpensThroughKwt(t *testing.T) {
	w := newSavedWorktree(t)

	err := openSavedWorktreeSession(nil, nil)(t.Context(), w.entry(w.session, w.path, w.generation), nil)

	require.NoError(t, err)
	assert.True(t, w.runner.ensured)
	assert.Equal(t, w.session, w.runner.sessionName)
	assert.Equal(t, w.path, w.runner.workingDirectory)
	assert.Equal(t, w.generation, w.runner.generation)
}

func TestSavedWorktreeSessionThatNoLongerMatchesIsLeftAlone(t *testing.T) {
	for name, entry := range map[string]func(savedWorktree) tmuxrestore.Entry{
		"another checkout at the path": func(w savedWorktree) tmuxrestore.Entry {
			return w.entry(w.session, w.path, "fedcba9876543210fedcba9876543210")
		},
		"kwt names the session differently": func(w savedWorktree) tmuxrestore.Entry {
			return w.entry("an-older-name", w.path, w.generation)
		},
		"the worktree is gone": func(w savedWorktree) tmuxrestore.Entry {
			return w.entry(w.session, filepath.Join(w.path, "removed"), w.generation)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newSavedWorktree(t)

			err := openSavedWorktreeSession(nil, nil)(t.Context(), entry(w), nil)

			require.ErrorIs(t, err, tmuxrestore.ErrWorktreeChanged)
			assert.False(t, w.runner.ensured, "no session is created in its place")
		})
	}
}

func TestSavedDirectoryWorkspaceSessionOpensThroughKwt(t *testing.T) {
	w := newSavedWorktree(t)
	dir := t.TempDir()
	workspaces := []models.Workspace{{Name: "notes", Path: dir}}
	session := kwttmux.DirWorkspaceSessionName("notes", dir)

	err := openSavedWorktreeSession(nil, workspaces)(t.Context(), w.entry(session, dir, ""), nil)

	require.NoError(t, err)
	assert.True(t, w.runner.ensured)
	assert.Equal(t, session, w.runner.sessionName)
	assert.Equal(t, dir, w.runner.workingDirectory)
	assert.Empty(t, w.runner.generation)
}

func TestSavedDirectoryWorkspaceSessionThatIsNoLongerRegisteredIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	session := kwttmux.DirWorkspaceSessionName("notes", dir)
	for name, workspaces := range map[string][]models.Workspace{
		"unregistered": nil,
		"renamed":      {{Name: "journal", Path: dir}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newSavedWorktree(t)

			err := openSavedWorktreeSession(nil, workspaces)(t.Context(), w.entry(session, dir, ""), nil)

			require.ErrorIs(t, err, tmuxrestore.ErrWorktreeChanged)
			assert.False(t, w.runner.ensured, "no session is created in its place")
		})
	}
}
