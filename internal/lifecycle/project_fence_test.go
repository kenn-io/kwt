package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
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
