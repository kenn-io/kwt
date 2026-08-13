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
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/service"
)

type recordingRemovalSessionGuard struct {
	condition RemovalSessionCondition
	err       error
	called    bool
	path      string
	pathLive  bool
}

type signalingRemovalSessionGuard struct {
	called chan struct{}
	err    error
}

func (g *signalingRemovalSessionGuard) ValidateAndTerminate(
	_ context.Context,
	_ RemovalSessionCondition,
) error {
	close(g.called)
	return g.err
}

func (g *recordingRemovalSessionGuard) ValidateAndTerminate(
	_ context.Context,
	condition RemovalSessionCondition,
) error {
	g.called = true
	g.condition = condition
	if g.path != "" {
		_, err := os.Stat(g.path)
		g.pathLive = err == nil
	}
	return g.err
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
		SessionName: "kwt-workspace-guarded",
		ServerPID:   "321",
		SessionID:   "$7",
		CreatedAt:   "1720000000",
	}

	result, err := NewRemovalService(RemovalServiceOptions{
		Home: home, SessionGuard: guard,
	}).Remove(context.Background(), RemovalRequest{
		RepositoryPath: repositoryPath,
		Path:           worktreePath, ExpectedGeneration: generation,
		Expansion: testExpansion(t),
		Session:   &condition,
	})

	require.NoError(t, err)
	assert.True(t, guard.called)
	assert.True(t, guard.pathLive, "session guard must run before checkout removal")
	assert.Equal(t, condition, guard.condition)
	assert.True(t, result.WorktreeRemoved)
	assert.NoDirExists(t, worktreePath)
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
		Session:   &RemovalSessionCondition{SessionName: "kwt-workspace-guarded", Absent: true},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, result.WorktreeRemoved)
	assert.DirExists(t, worktreePath)
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
		Session:   &RemovalSessionCondition{SessionName: "kwt-workspace-dirty", Absent: true},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Conflict))
	assert.False(t, result.WorktreeRemoved)
	assert.False(t, guard.called, "dirty worktree must fail before terminating its session")
	assert.DirExists(t, worktreePath)
}

func TestRemovalServicePreservesNativeDirtyErrorWithoutSessionGuard(t *testing.T) {
	repositoryPath, worktreePath := removalRepository(t, "ordinary-dirty")
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
		Session:   &RemovalSessionCondition{SessionName: "expanded", Absent: true},
	})

	require.NoError(t, err)
	assert.True(t, guard.called)
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
				SessionName: "kwt-workspace-race", Absent: true,
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
			SessionName: "kwt-workspace-submodule", Absent: true,
		},
	})

	require.Error(t, err)
	assert.False(t, result.WorktreeRemoved)
	assert.False(t, guard.called, "initialized submodule must fail before terminating its session")
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
