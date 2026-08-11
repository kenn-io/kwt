package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func TestProjectFenceWaitHonorsCancellation(t *testing.T) {
	home := t.TempDir()
	release, err := acquireProjectFence(context.Background(), home, "github.com/acme/widget")
	require.NoError(t, err)
	defer func() { require.NoError(t, release()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err = acquireProjectFence(ctx, home, "github.com/acme/widget")

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestProjectFenceRepositoryCaseVariantsContend(t *testing.T) {
	home := t.TempDir()
	release, err := acquireProjectFence(
		context.Background(), home, "github.com/Acme/Widget",
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, release()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err = acquireProjectFence(ctx, home, "github.com/acme/widget")

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestLocalProjectIdentityPreservesTrailingWhitespace(t *testing.T) {
	identity := "local/" + filepath.ToSlash(
		filepath.Join(t.TempDir(), "repo "),
	)

	validated, err := validateStableProjectIdentity(identity)

	require.NoError(t, err)
	assert.Equal(t, identity, validated)
	assert.False(t, EqualProjectIdentity(identity, strings.TrimSuffix(identity, " ")))
	_, err = validateStableProjectIdentity("github.com/acme/widget ")
	assert.Error(t, err)
}

func TestProjectClaimRejectsRegistrationRemovedWhileWaiting(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = '"+path+"'\n",
	), 0o600))
	expansion := testExpansion(t)
	claim, err := ObserveProjectClaim(context.Background(), home, path, expansion)
	require.NoError(t, err)
	require.NotNil(t, claim)
	release, err := acquireProjectFence(context.Background(), home, claim.Identity)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		acquired, acquireErr := AcquireProjectClaim(context.Background(), home, claim)
		if acquired != nil {
			_ = acquired()
		}
		done <- acquireErr
	}()
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(home, expansion.expandPath)
	require.NoError(t, err)
	changed, err := config.CompareAndSwapProjectAt(home, snapshot.Projects[0], nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, release())

	err = <-done
	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestObserveProjectClaimAllowsUnregisteredRepository(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), nil, 0o600))

	claim, err := ObserveProjectClaim(context.Background(), home, t.TempDir(), testExpansion(t))

	require.NoError(t, err)
	assert.Nil(t, claim)
}

func TestRequiredProjectClaimRejectsUnregisteredRepository(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), nil, 0o600))

	release, err := AcquireRequiredProjectClaim(
		context.Background(), home, nil,
	)

	assert.Nil(t, release)
	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestProjectRegistrationTransitionWaitsForExistingIdentity(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = 'github.com/acme/old'\nname = 'repo'\npath = '"+path+"'\n",
	), 0o600))
	expansion := testExpansion(t)
	claim, err := ObserveProjectClaim(context.Background(), home, path, expansion)
	require.NoError(t, err)
	releaseOperation, err := AcquireRequiredProjectClaim(context.Background(), home, claim)
	require.NoError(t, err)

	mutationStarted := make(chan struct{})
	done := make(chan error, 1)
	replacement := models.Project{
		Repository: "github.com/acme/new", Name: "repo", Path: path,
	}
	go func() {
		done <- TransitionProjectRegistration(
			context.Background(), home, expansion, replacement,
			func() error {
				close(mutationStarted)
				snapshot, loadErr := config.LoadGlobalSnapshotAtWithExpansion(
					home, expansion.expandPath,
				)
				if loadErr != nil {
					return loadErr
				}
				changed, swapErr := config.CompareAndSwapProjectAt(
					home, snapshot.Projects[0], &replacement,
				)
				if swapErr == nil && !changed {
					return errors.New("registration was not replaced")
				}
				return swapErr
			},
		)
	}()
	select {
	case <-mutationStarted:
		t.Fatal("registration changed while the old identity was in use")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, releaseOperation())
	require.NoError(t, <-done)
	select {
	case <-mutationStarted:
	default:
		t.Fatal("registration mutation did not run")
	}
}

func TestProjectRegistrationTransitionReacquiresChangedIdentitySet(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = 'github.com/acme/a'\nname = 'repo'\npath = '"+path+"'\n",
	), 0o600))
	expansion := testExpansion(t)
	releaseA, err := acquireProjectFence(context.Background(), home, "github.com/acme/a")
	require.NoError(t, err)

	mutationStarted := make(chan struct{})
	done := make(chan error, 1)
	replacementB := models.Project{
		Repository: "github.com/acme/b", Name: "repo", Path: path,
	}
	go func() {
		done <- TransitionProjectRegistration(
			context.Background(), home, expansion, replacementB,
			func() error {
				close(mutationStarted)
				snapshot, loadErr := config.LoadGlobalSnapshotAtWithExpansion(
					home, expansion.expandPath,
				)
				if loadErr != nil {
					return loadErr
				}
				changed, swapErr := config.CompareAndSwapProjectAt(
					home, snapshot.Projects[0], &replacementB,
				)
				if swapErr == nil && !changed {
					return errors.New("registration was not replaced")
				}
				return swapErr
			},
		)
	}()

	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		release, lockErr := acquireProjectTransitionFence(ctx, home)
		if lockErr != nil {
			return errors.Is(lockErr, context.DeadlineExceeded)
		}
		require.NoError(t, release())
		return false
	}, time.Second, 5*time.Millisecond)
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(home, expansion.expandPath)
	require.NoError(t, err)
	replacementC := models.Project{
		Repository: "github.com/acme/c", Name: "repo", Path: path,
	}
	changed, err := config.CompareAndSwapProjectAt(home, snapshot.Projects[0], &replacementC)
	require.NoError(t, err)
	require.True(t, changed)
	releaseC, err := acquireProjectFence(context.Background(), home, "github.com/acme/c")
	require.NoError(t, err)
	require.NoError(t, releaseA())
	select {
	case <-mutationStarted:
		t.Fatal("registration mutated without owning the replacement identity")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, releaseC())
	require.NoError(t, <-done)
}

func TestProjectRegistrationTransitionDeduplicatesRepositoryCaseVariants(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = 'github.com/Acme/Widget'\nname = 'repo'\npath = '"+path+"'\n",
	), 0o600))

	identities, err := projectRegistrationTransitionIdentities(
		context.Background(), home, testExpansion(t), models.Project{
			Repository: "github.com/acme/widget", Name: "repo", Path: path,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"github.com/acme/widget"}, identities)
}
