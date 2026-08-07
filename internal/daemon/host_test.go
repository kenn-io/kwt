package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func waitForRuntime(t *testing.T, home string) Observation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		observation, err := Inspect(context.Background(), RuntimeStore(home), home)
		if err == nil && (observation.State == RuntimeReady ||
			observation.State == RuntimeDraining) {
			return observation
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.FailNow(t, "daemon did not publish a ready runtime")
	return Observation{}
}

func runtimeFiles(t *testing.T, home string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, "runtime", "kwt.*.json"))
	require.NoError(t, err)
	return paths
}

func TestServePublishesReadyRuntimeAndRemovesItOnShutdown(t *testing.T) {
	home := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{
			Home:  home,
			Build: Build{Version: "v1.0.0", Revision: "abc"},
			Config: models.DaemonConfig{
				IdleTimeout:      time.Hour,
				AutoRestart:      "newer",
				ReplacementGrace: time.Second,
			},
			Foreground: true,
			Now:        time.Now,
		})
	}()

	observation := waitForRuntime(t, home)
	assert.Equal(t, RuntimeReady, observation.State)
	assert.Equal(t, home, observation.Status.Home)
	assert.Contains(t, observation.Status.Endpoint, "127.0.0.1:")

	_, err := observation.Client.Shutdown(context.Background(), "stop")
	require.NoError(t, err)
	require.NoError(t, <-done)
	assert.Empty(t, runtimeFiles(t, home))
	cancel()
}

func TestServeRefusesASecondWritableOwner(t *testing.T) {
	home := t.TempDir()
	store := RuntimeStore(home)
	release, err := store.AcquireOwnerLock(context.Background())
	require.NoError(t, err)
	defer release()

	err = Serve(context.Background(), ServeOptions{
		Home:       home,
		Build:      Build{Version: "v1"},
		Config:     models.DaemonConfig{ReplacementGrace: time.Second},
		Foreground: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner")
}

func TestBackgroundServeStopsAfterIdleButForegroundServeDoesNot(t *testing.T) {
	backgroundHome := t.TempDir()
	backgroundDone := make(chan error, 1)
	go func() {
		backgroundDone <- Serve(context.Background(), ServeOptions{
			Home:  backgroundHome,
			Build: Build{Version: "v1"},
			Config: models.DaemonConfig{
				IdleTimeout:      30 * time.Millisecond,
				AutoRestart:      "newer",
				ReplacementGrace: time.Second,
			},
			IdleCheckInterval: 5 * time.Millisecond,
		})
	}()
	select {
	case err := <-backgroundDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "background daemon did not stop after idle timeout")
	}

	foregroundHome := t.TempDir()
	foregroundDone := make(chan error, 1)
	go func() {
		foregroundDone <- Serve(context.Background(), ServeOptions{
			Home:  foregroundHome,
			Build: Build{Version: "v1"},
			Config: models.DaemonConfig{
				IdleTimeout:      30 * time.Millisecond,
				AutoRestart:      "newer",
				ReplacementGrace: time.Second,
			},
			Foreground:        true,
			IdleCheckInterval: 5 * time.Millisecond,
		})
	}()
	observation := waitForRuntime(t, foregroundHome)
	select {
	case err := <-foregroundDone:
		require.FailNow(t, "foreground daemon exited for idle", err)
	case <-time.After(60 * time.Millisecond):
	}
	_, err := observation.Client.Shutdown(context.Background(), "stop")
	require.NoError(t, err)
	require.NoError(t, <-foregroundDone)
}
