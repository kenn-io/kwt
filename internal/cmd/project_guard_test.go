package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func TestGuardedProjectOperationRejectsRemovedRegistration(t *testing.T) {
	home := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = '"+projectPath+"'\n"),
		0o600,
	))
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	guard, err := observeGuardedProjectOperation(
		context.Background(), home, projectPath, expansion,
	)
	require.NoError(t, err)

	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	changed, err := config.CompareAndSwapProjectAt(
		home, snapshot.Projects[0], nil,
	)
	require.NoError(t, err)
	require.True(t, changed)
	called := false

	err = guard.run(context.Background(), func() error {
		called = true
		return nil
	})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, called)
}

func TestWorktreeSessionEstablishmentCancelsWhileWaitingForMutationLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	repoPath := newTUITestRepo(t)
	generation, err := git.New(repoPath).WorktreeGeneration(repoPath)
	require.NoError(t, err)
	lock := flock.New(filepath.Join(repoPath, ".git", "kwt-worktree.lock"))
	require.NoError(t, lock.Lock())
	defer func() { _ = lock.Unlock() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reachedGuard := make(chan struct{})
	originalHook := beforeProjectGuardAcquire
	beforeProjectGuardAcquire = func() { close(reachedGuard) }
	t.Cleanup(func() { beforeProjectGuardAcquire = originalHook })
	done := make(chan error, 1)
	called := false
	go func() {
		_, establishErr := runWorktreeSessionEstablishment(
			ctx,
			repoPath,
			generation,
			nil,
			func(string) error {
				called = true
				return nil
			},
		)
		done <- establishErr
	}()
	select {
	case <-reachedGuard:
	case <-time.After(time.Second):
		t.Fatal("session establishment did not reach the project guard")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.False(t, called)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("session establishment did not honor cancellation while waiting for the mutation lock")
	}
}

func TestRequiredProjectGuardRejectsUnexpectedRepository(t *testing.T) {
	home := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]]\nrepository = 'github.com/acme/replacement'\nname = 'widget'\npath = '"+projectPath+"'\n"),
		0o600,
	))
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)

	guard, err := observeRequiredGuardedProjectOperation(
		context.Background(), home, projectPath, expansion,
		"github.com/acme/original",
	)

	assert.Nil(t, guard)
	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestRequiredProjectGuardRejectsMatchingUnregisteredRepository(t *testing.T) {
	home := t.TempDir()
	repositoryPath := newTUITestRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), nil, 0o600))
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	claim, err := lifecycle.ObserveProjectClaim(
		context.Background(), home, repositoryPath, expansion,
	)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.False(t, claim.Registered)

	guard, err := observeRequiredGuardedProjectOperation(
		context.Background(), home, repositoryPath, expansion, claim.Identity,
	)

	assert.Nil(t, guard)
	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestRequiredProjectGuardAcceptsEquivalentRepositoryCase(t *testing.T) {
	home := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]]\nrepository = 'github.com/Acme/Widget'\nname = 'widget'\npath = '"+projectPath+"'\n"),
		0o600,
	))
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)

	guard, err := observeRequiredGuardedProjectOperation(
		context.Background(), home, projectPath, expansion,
		"github.com/acme/widget",
	)

	require.NoError(t, err)
	require.NotNil(t, guard)
	called := false
	require.NoError(t, guard.run(context.Background(), func() error {
		called = true
		return nil
	}))
	assert.True(t, called)
}

func TestRegisterProjectIdentityMatchesPublishedCanonicalIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	projectPath := newTUITestRepo(t)

	registered, err := registerProjectIdentityWithLifecycle(
		context.Background(),
		models.Project{Repository: "repo", Name: "repo", Path: projectPath},
	)
	require.NoError(t, err)
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	inventory, err := kwt.NewSource(kwt.SourceOptions{Home: home}).Load(
		context.Background(),
		kwt.Request{
			View: kwt.ViewProjects, Expansion: expansion,
			UntrustedConfig: kwt.IgnoreUntrustedConfig,
		},
	)
	require.NoError(t, err)
	require.Len(t, inventory.Snapshot.Projects, 1)

	assert.NotEqual(t, "repo", registered.Repository)
	assert.Equal(t, inventory.Snapshot.Projects[0].Repository, registered.Repository)
	assert.Equal(
		t,
		inventory.Snapshot.Projects[0].RegistrationFingerprint,
		registered.RegistrationFingerprint,
	)
}
