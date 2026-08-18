package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	internalworktree "go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/service"
)

type recordingRemovalSessionGuard struct {
	condition  RemovalSessionCondition
	err        error
	quiesced   bool
	terminated bool
	resumed    bool
	path       string
	pathLive   bool
	onQuiesce  func() error
}

type signalingRemovalSessionGuard struct {
	called chan struct{}
	err    error
}

func (g *signalingRemovalSessionGuard) Quiesce(
	_ context.Context,
	_ RemovalSessionCondition,
) (tmux.RemovalSessionLease, error) {
	close(g.called)
	return nil, g.err
}

func (g *recordingRemovalSessionGuard) Quiesce(
	_ context.Context,
	condition RemovalSessionCondition,
) (tmux.RemovalSessionLease, error) {
	g.quiesced = true
	g.condition = condition
	if g.path != "" {
		_, err := os.Stat(g.path)
		g.pathLive = err == nil
	}
	if g.err != nil {
		return nil, g.err
	}
	if g.onQuiesce != nil {
		if err := g.onQuiesce(); err != nil {
			return nil, err
		}
	}
	return recordingRemovalSessionLease{guard: g}, nil
}

type recordingRemovalSessionLease struct{ guard *recordingRemovalSessionGuard }

func (l recordingRemovalSessionLease) Terminate(context.Context) error {
	l.guard.terminated = true
	return nil
}

func (l recordingRemovalSessionLease) Resume() error {
	l.guard.resumed = true
	return nil
}

func TestRemovalServiceRemovesWorktreeAndRegistryRecord(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "remove-me")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	reg, err := registry.NewAt(home)
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "example/widget", Branch: "remove-me", Path: worktreePath,
		Generation: generation,
	}))

	result, err := NewRemovalService(RemovalServiceOptions{Home: home}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath: repositoryPath,
			Path:           worktreePath, ExpectedGeneration: generation,
			DeleteBranch: true,
		},
	)

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
	assert.True(t, result.BranchDeleted)
	assert.True(t, result.RegistryUnregistered)
	assert.Equal(t, "remove-me", result.Branch)
	assert.NoDirExists(t, worktreePath)
	reloaded, err := registry.NewAt(home)
	require.NoError(t, err)
	_, registered := reloaded.Get(worktreePath)
	assert.False(t, registered)
}

func TestRemovalServiceRejectsChangedGeneration(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "keep-me")

	result, err := NewRemovalService(RemovalServiceOptions{Home: t.TempDir()}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath:     repositoryPath,
			Path:               worktreePath,
			ExpectedGeneration: "0123456789abcdef0123456789abcdef",
		},
	)

	require.Error(t, err)
	assert.False(t, result.WorktreeRemoved)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceTerminatesConfirmedSessionBeforeRemovingWorktree(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "guarded-remove")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{path: worktreePath}
	condition := RemovalSessionCondition{
		SessionName: removalSessionName(t, worktreePath, "guarded-remove"),
		ServerPID:   "321",
		SessionID:   "$7",
		CreatedAt:   "1720000000",
	}
	expansion := testExpansion(t)

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: expansion,
		Session:   &condition,
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	assert.True(t, guard.terminated)
	assert.True(t, guard.pathLive, "session guard must run before checkout removal")
	expectedCondition := condition
	expectedCondition.SocketDirectory = removalSocketDirectory(expansion)
	expectedCondition.WorkspacePath = worktreePath
	expectedCondition.WorkspaceGeneration = generation
	expectedCondition.ProtectedNames = credentials.ProtectedNames(nil)
	assert.Equal(t, expectedCondition, guard.condition)
	assert.True(t, result.WorktreeRemoved)
	assert.NoDirExists(t, worktreePath)
}

func TestRemovalServiceReloadsConfiguredProtectedNamesPerRequest(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "configured-token")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	guard := &recordingRemovalSessionGuard{}
	service := NewRemovalService(RemovalServiceOptions{Home: home, SessionGuard: guard})
	originalNewGit := newRemovalInventoryGit
	t.Cleanup(func() { newRemovalInventoryGit = originalNewGit })
	var gitProtectedNames [][]string
	newRemovalInventoryGit = func(
		ctx context.Context,
		path string,
		protectedNames []string,
	) *git.Git {
		gitProtectedNames = append(
			gitProtectedNames,
			append([]string(nil), protectedNames...),
		)
		return git.NewForInventory(ctx, path, protectedNames)
	}
	contents := fmt.Sprintf(
		"[fleet]\ntoken_env = %q\n[[projects]]\nrepository = %q\nname = \"repository\"\npath = %q\n",
		"CUSTOM_FLEET_TOKEN",
		repositoryPath,
		repositoryPath,
	)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600))

	result, err := service.Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "configured-token"),
			Absent:      true,
		},
	})

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
	assert.Contains(t, guard.condition.ProtectedNames, "CUSTOM_FLEET_TOKEN")
	require.NotEmpty(t, gitProtectedNames)
	for _, protectedNames := range gitProtectedNames {
		assert.Contains(t, protectedNames, "CUSTOM_FLEET_TOKEN")
	}
}

func TestGuardedRemovalSupportsUnregisteredRepository(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "unregistered-guarded")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), nil, 0o600))
	guard := &recordingRemovalSessionGuard{}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "unregistered-guarded"), Absent: true,
		},
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	assert.True(t, guard.terminated)
	assert.True(t, result.WorktreeRemoved)
}

func TestRemovalServicePreservesWorktreeWhenSessionConditionChanges(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "guarded-conflict")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{err: &RemovalSessionConditionError{
		Reason: "tmux session identity changed",
	}}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "guarded-conflict"), Absent: true,
		},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceRejectsStaleSessionNameAfterBranchSwitch(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "branch-a")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	staleSessionName := removalSessionName(t, worktreePath, "branch-a")
	runRemovalGit(t, worktreePath, "switch", "-c", "branch-b")
	currentGeneration, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	require.Equal(t, generation, currentGeneration, "an in-place branch switch keeps the worktree generation")
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: staleSessionName,
			Absent:      true,
		},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, guard.quiesced, "stale session authority must fail before tmux inspection")
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceRejectsOldBranchSessionForCurrentWorktree(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "old-branch")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	oldSessionName := removalSessionName(t, worktreePath, "old-branch")
	runRemovalGit(t, worktreePath, "switch", "-c", "new-branch")
	currentSessionName := removalSessionName(t, worktreePath, "new-branch")
	require.NotEqual(t, oldSessionName, currentSessionName)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{}
	guard.onQuiesce = func() error {
		if guard.condition.WorkspacePath == worktreePath &&
			guard.condition.SessionName != oldSessionName {
			return &RemovalSessionConditionError{
				Reason: "worktree tmux session remains live",
			}
		}
		return nil
	}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: currentSessionName,
			Absent:      true,
		},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.True(t, guard.quiesced)
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceRejectsStaleSessionSocket(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "socket-changed")
	runRemovalGit(t, repositoryPath, "remote", "add", "origin", "https://github.com/acme/widget.git")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	sessionName := removalSessionName(t, worktreePath, "socket-changed")
	require.NoError(t, pullrequest.NewFileStore(
		filepath.Join(home, "pull-requests.json"),
	).Update(context.Background(), func(records map[string]pullrequest.Provenance) error {
		records["github:acme/widget#1"] = pullrequest.Provenance{
			Repository: "github.com/acme/widget",
			Project: pullrequest.Project{
				Identity: "github.com/acme/widget", Path: repositoryPath,
			},
			Workspace: pullrequest.Workspace{
				Repository:  "github.com/acme/widget",
				Branch:      "socket-changed",
				Path:        worktreePath,
				Generation:  generation,
				SessionName: sessionName,
			},
		}
		return nil
	}))
	guard := &recordingRemovalSessionGuard{}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: sessionName,
			SocketName:  "stale-protected-socket",
			Absent:      true,
		},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, guard.quiesced)
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceAcceptsDirectSessionOnCanonicalSocketEndpoint(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "custom-endpoint")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{}
	condition := RemovalSessionCondition{
		SessionName:     removalSessionName(t, worktreePath, "custom-endpoint"),
		SocketName:      tmux.KWTServerSocketName,
		SocketDirectory: "/srv/tmux",
		Absent:          true,
	}
	expansion := testExpansion(t)
	expansion.Environment[normalizedEnvironmentName("TMUX_TMPDIR")] = "/srv/tmux"

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          expansion,
		Session:            &condition,
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	expectedCondition := condition
	expectedCondition.WorkspacePath = worktreePath
	expectedCondition.WorkspaceGeneration = generation
	expectedCondition.ProtectedNames = credentials.ProtectedNames(nil)
	assert.Equal(t, expectedCondition, guard.condition)
	assert.False(t, guard.condition.ProtectedSocketTopology)
	assert.True(t, result.WorktreeRemoved)
}

func TestRemovalServiceRejectsDirectSessionSocketDirectoryOutsideRequest(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "wrong-socket-directory")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{}
	expansion := testExpansion(t)
	expansion.Environment[normalizedEnvironmentName("TMUX_TMPDIR")] = "/srv/expected-tmux"

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          expansion,
		Session: &RemovalSessionCondition{
			SessionName:     removalSessionName(t, worktreePath, "wrong-socket-directory"),
			SocketName:      tmux.KWTServerSocketName,
			SocketDirectory: "/srv/unrelated-tmux",
			Absent:          true,
		},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, guard.quiesced)
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceUsesRequestSocketDirectoryWhenConditionOmitsIt(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "implicit-socket-directory")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{}
	expansion := testExpansion(t)
	expansion.Environment[normalizedEnvironmentName("TMUX_TMPDIR")] = "/srv/request-tmux"

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          expansion,
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "implicit-socket-directory"),
			SocketName:  tmux.KWTServerSocketName,
			Absent:      true,
		},
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	assert.Equal(t, "/srv/request-tmux", guard.condition.SocketDirectory)
	assert.True(t, result.WorktreeRemoved)
}

func TestRemovalServiceRejectsDirectSessionOnArbitraryNamedSocket(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "arbitrary-endpoint")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "arbitrary-endpoint"),
			SocketName:  "team-server",
			Absent:      true,
		},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, guard.quiesced)
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceCarriesDurableGenerationAfterWorktreeMove(t *testing.T) {
	repositoryPath, originalPath := removalRepository(t, "moved-worktree")
	generation, err := git.New(repositoryPath).WorktreeGeneration(originalPath)
	require.NoError(t, err)
	movedPath := originalPath + "-new-location"
	runRemovalGit(t, repositoryPath, "worktree", "move", originalPath, movedPath)
	movedGeneration, err := git.New(repositoryPath).WorktreeGeneration(movedPath)
	require.NoError(t, err)
	require.Equal(t, generation, movedGeneration)

	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{}
	condition := RemovalSessionCondition{
		SessionName: removalSessionName(t, movedPath, "moved-worktree"),
		Absent:      true,
	}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               movedPath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session:            &condition,
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	assert.Equal(t, movedPath, guard.condition.WorkspacePath)
	assert.Equal(t, generation, guard.condition.WorkspaceGeneration)
	assert.True(t, result.WorktreeRemoved)
	assert.NoDirExists(t, movedPath)
}

func TestRemovalServiceAcceptsCurrentProtectedSessionEndpoint(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "protected-session")
	runRemovalGit(t, repositoryPath, "remote", "add", "origin", "https://github.com/acme/widget.git")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	sessionName := removalSessionName(t, worktreePath, "protected-session")
	socketName := tmux.ProtectedWorkspaceSocketName(sessionName, worktreePath)
	record := pullrequest.Provenance{
		Repository: "github.com/acme/widget",
		Project: pullrequest.Project{
			Identity: "github.com/acme/widget", Path: repositoryPath,
		},
		Workspace: pullrequest.Workspace{
			Repository:  "github.com/acme/widget",
			Branch:      "protected-session",
			Path:        worktreePath,
			Generation:  generation,
			SessionName: sessionName,
		},
	}
	require.NoError(t, pullrequest.NewFileStore(
		filepath.Join(home, "pull-requests.json"),
	).Update(context.Background(), func(records map[string]pullrequest.Provenance) error {
		records["github:acme/widget#1"] = record
		return nil
	}))
	guard := &recordingRemovalSessionGuard{}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               worktreePath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: sessionName,
			SocketName:  socketName,
			Absent:      true,
		},
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	assert.Equal(t, socketName, guard.condition.SocketName)
	assert.True(t, guard.condition.ProtectedSocketTopology)
	assert.True(t, result.WorktreeRemoved)
}

func TestRemovalServiceFindsProtectedProvenanceAfterWorktreeMove(t *testing.T) {
	repositoryPath, originalPath := removalRepository(t, "moved-protected")
	runRemovalGit(t, repositoryPath, "remote", "add", "origin", "https://github.com/acme/widget.git")
	generation, err := git.New(repositoryPath).WorktreeGeneration(originalPath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	sessionName := removalSessionName(t, originalPath, "moved-protected")
	socketName := tmux.ProtectedWorkspaceSocketName(sessionName, originalPath)
	record := pullrequest.Provenance{
		Repository: "github.com/acme/widget",
		Project: pullrequest.Project{
			Identity: "github.com/acme/widget", Path: repositoryPath,
		},
		Workspace: pullrequest.Workspace{
			Repository:  "github.com/acme/widget",
			Branch:      "moved-protected",
			Path:        originalPath,
			Generation:  generation,
			SessionName: sessionName,
		},
	}
	require.NoError(t, pullrequest.NewFileStore(
		filepath.Join(home, "pull-requests.json"),
	).Update(context.Background(), func(records map[string]pullrequest.Provenance) error {
		records["github:acme/widget#2"] = record
		return nil
	}))
	movedPath := originalPath + "-new-location"
	runRemovalGit(t, repositoryPath, "worktree", "move", originalPath, movedPath)
	guard := &recordingRemovalSessionGuard{}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath:     repositoryPath,
		Path:               movedPath,
		ExpectedGeneration: generation,
		Expansion:          testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: sessionName,
			SocketName:  socketName,
			Absent:      true,
		},
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	assert.Equal(t, socketName, guard.condition.SocketName)
	assert.True(t, guard.condition.ProtectedSocketTopology)
	assert.True(t, result.WorktreeRemoved)
}

func TestRemovalServicePreservesConfirmedSessionWhenDirtyWorktreeCannotBeRemoved(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "guarded-dirty")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreePath, "untracked.txt"),
		[]byte("keep me\n"),
		0o644,
	))
	guard := &recordingRemovalSessionGuard{}
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "guarded-dirty"), Absent: true,
		},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, result.WorktreeRemoved)
	assert.False(t, guard.quiesced, "dirty worktree must fail before quiescing its session")
	assert.DirExists(t, worktreePath)
}

func TestRemovalServicePreservesNativeDirtyErrorWithoutSessionGuard(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "direct-dirty")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreePath, "untracked.txt"),
		[]byte("keep me\n"),
		0o644,
	))

	result, err := NewRemovalService(RemovalServiceOptions{Home: t.TempDir()}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath: repositoryPath,
			Path:           worktreePath, ExpectedGeneration: generation,
		},
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.RemovalFailed))
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceUsesClientExpansionForProjectFence(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "expanded-project")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	projectRoot := filepath.Dir(repositoryPath)
	configuredPath := filepath.Join("$PROJECT_ROOT", filepath.Base(repositoryPath))
	contents := fmt.Sprintf(
		"[[projects]]\nrepository = %q\nname = \"repository\"\npath = %q\n",
		repositoryPath,
		configuredPath,
	)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600))
	t.Setenv("PROJECT_ROOT", t.TempDir())
	expansion := testExpansion(t)
	expansion.Environment[normalizedEnvironmentName("PROJECT_ROOT")] = projectRoot
	guard := &recordingRemovalSessionGuard{}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: expansion,
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "expanded-project"), Absent: true,
		},
	})

	require.NoError(t, err)
	assert.True(t, guard.quiesced)
	assert.True(t, guard.terminated)
	assert.True(t, result.WorktreeRemoved)
}

func TestRemovalServiceWaitsForProjectSessionStartupFence(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "guarded-race")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	expansion, err := CaptureExpansionContext()
	require.NoError(t, err)
	claim, err := ObserveProjectClaim(
		context.Background(), home, repositoryPath, expansion,
	)
	require.NoError(t, err)
	require.NotNil(t, claim)
	identity := claim.Identity
	releaseFence, err := acquireProjectFence(context.Background(), home, identity)
	require.NoError(t, err)
	guard := &signalingRemovalSessionGuard{
		called: make(chan struct{}),
		err: &RemovalSessionConditionError{
			Reason: "tmux session started after confirmation",
		},
	}
	result := make(chan error, 1)
	go func() {
		_, removeErr := NewRemovalService(RemovalServiceOptions{
			Home: home, SessionGuard: guard,
		}).Remove(context.Background(), RemovalRequest{
			RepositoryPath: repositoryPath,
			Path:           worktreePath, ExpectedGeneration: generation,
			Expansion: testExpansion(t),
			Session: &RemovalSessionCondition{
				SessionName: removalSessionName(t, worktreePath, "guarded-race"), Absent: true,
			},
		})
		result <- removeErr
	}()

	select {
	case <-guard.called:
		t.Fatal("removal inspected the session while startup held the project fence")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, releaseFence())

	err = <-result
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.DirExists(t, worktreePath)
}

func TestRemovalServicePreservesConfirmedSessionForInitializedSubmodule(t *testing.T) {
	submodulePath := filepath.Join(t.TempDir(), "submodule")
	require.NoError(t, os.MkdirAll(submodulePath, 0o755))
	runRemovalGit(t, submodulePath, "init", "-b", "main")
	runRemovalGit(t, submodulePath, "config", "user.email", "test@example.com")
	runRemovalGit(t, submodulePath, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(
		filepath.Join(submodulePath, "README.md"), []byte("submodule\n"), 0o644,
	))
	runRemovalGit(t, submodulePath, "add", "README.md")
	runRemovalGit(t, submodulePath, "commit", "-m", "initial")

	repositoryPath := filepath.Join(t.TempDir(), "repository")
	require.NoError(t, os.MkdirAll(repositoryPath, 0o755))
	runRemovalGit(t, repositoryPath, "init", "-b", "main")
	runRemovalGit(t, repositoryPath, "config", "user.email", "test@example.com")
	runRemovalGit(t, repositoryPath, "config", "user.name", "Test User")
	runRemovalGit(
		t, repositoryPath, "-c", "protocol.file.allow=always",
		"submodule", "add", submodulePath, "dependency",
	)
	runRemovalGit(t, repositoryPath, "commit", "-m", "add submodule")
	runRemovalGit(t, repositoryPath, "branch", "guarded-submodule")
	worktreePath := filepath.Join(t.TempDir(), "guarded-submodule")
	runRemovalGit(
		t, repositoryPath, "worktree", "add", worktreePath, "guarded-submodule",
	)
	runRemovalGit(
		t, worktreePath, "-c", "protocol.file.allow=always",
		"submodule", "update", "--init",
	)
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	guard := &recordingRemovalSessionGuard{}
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "guarded-submodule"), Absent: true,
		},
	})

	require.Error(t, err)
	assert.False(t, result.WorktreeRemoved)
	assert.False(t, guard.quiesced, "initialized submodule must fail before quiescing its session")
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceResumesSessionWhenCheckoutChangesDuringQuiesce(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "guarded-quiesce-race")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	registerRemovalRepository(t, home, repositoryPath)
	guard := &recordingRemovalSessionGuard{onQuiesce: func() error {
		return os.WriteFile(filepath.Join(worktreePath, "late-change.txt"), []byte("keep me\n"), 0o644)
	}}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: testExpansion(t),
		Session: &RemovalSessionCondition{
			SessionName: removalSessionName(t, worktreePath, "guarded-quiesce-race"),
			ServerPID:   "123", SessionID: "$4", CreatedAt: "1720000000",
		},
	})

	require.Error(t, err)
	assert.False(t, result.WorktreeRemoved)
	assert.True(t, guard.quiesced)
	assert.True(t, guard.resumed)
	assert.False(t, guard.terminated)
	assert.DirExists(t, worktreePath)
}

func TestRemovalServiceRejectsActiveCreation(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "creating")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	home := t.TempDir()
	reg, err := registry.NewAt(home)
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "example/widget", Branch: "creating", Path: worktreePath,
		Generation: generation, CreationToken: "creating",
	}))
	release, acquired, err := reg.AcquireCreation(worktreePath)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() { require.NoError(t, release()) })

	result, err := NewRemovalService(RemovalServiceOptions{Home: home}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath: repositoryPath,
			Path:           worktreePath, ExpectedGeneration: generation,
		},
	)

	require.Error(t, err)
	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.Conflict, typed.Code)
	assert.True(t, typed.Retryable)
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
	reloaded, reloadErr := registry.NewAt(home)
	require.NoError(t, reloadErr)
	_, registered := reloaded.Get(worktreePath)
	assert.True(t, registered)
}

func TestRemovalServiceIgnoresDaemonRepositoryRoutingEnvironment(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "routed-remove")
	generation, err := git.New(repositoryPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	otherRepository, _ := removalRepository(t, "other-worktree")
	t.Setenv("GIT_DIR", filepath.Join(otherRepository, ".git"))
	t.Setenv("GIT_WORK_TREE", otherRepository)

	result, err := NewRemovalService(RemovalServiceOptions{Home: t.TempDir()}).Remove(
		context.Background(),
		RemovalRequest{
			RepositoryPath:     repositoryPath,
			Path:               worktreePath,
			ExpectedGeneration: generation,
		},
	)

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
	assert.NoDirExists(t, worktreePath)
}

func TestClassifyRemovalErrorHidesUnexpectedCredentialBearingCause(t *testing.T) {
	const secret = "removal-password"
	cause := errors.New("fetch ssh://user:" + secret + "@example.invalid/repository")

	err := classifyRemovalError(cause, RemovalResult{
		Path: "/worktrees/topic", WorktreeRemoved: true,
	})

	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.Internal, typed.Code)
	assert.Equal(t, "internal failure", typed.Message)
	assert.NotContains(t, typed.Message, secret)
	assert.Equal(t, "/worktrees/topic", typed.Details["path"])
	assert.Equal(t, true, typed.Details["worktree_removed"])
	assert.ErrorIs(t, err, cause)
}

func removalRepository(t *testing.T, branch string) (string, string) {
	t.Helper()
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	require.NoError(t, os.MkdirAll(repositoryPath, 0o755))
	runRemovalGit(t, repositoryPath, "init", "-b", "main")
	runRemovalGit(t, repositoryPath, "config", "user.email", "test@example.com")
	runRemovalGit(t, repositoryPath, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("test\n"), 0o644))
	runRemovalGit(t, repositoryPath, "add", "README.md")
	runRemovalGit(t, repositoryPath, "commit", "-m", "initial")
	runRemovalGit(t, repositoryPath, "branch", branch)
	worktreePath := filepath.Join(t.TempDir(), branch)
	runRemovalGit(t, repositoryPath, "worktree", "add", worktreePath, branch)
	return repositoryPath, worktreePath
}

func runRemovalGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func registerRemovalRepository(t *testing.T, home, repositoryPath string) {
	t.Helper()
	contents := fmt.Sprintf(
		"[[projects]]\nrepository = %q\nname = \"repository\"\npath = %q\n",
		repositoryPath,
		repositoryPath,
	)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600))
}

func removalSessionName(t *testing.T, worktreePath, branch string) string {
	t.Helper()
	info, err := internalworktree.RepositoryInfoWithProjects(
		git.NewForInventory(context.Background(), worktreePath, nil),
		nil,
	)
	require.NoError(t, err)
	return tmux.WorkspaceSessionName(info, branch, worktreePath)
}
