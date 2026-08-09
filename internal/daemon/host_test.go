package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
	publicworktree "go.kenn.io/kwt/worktree"
)

func TestHTTPServerBoundsUnauthenticatedRequests(t *testing.T) {
	server := newHTTPServer(http.NotFoundHandler())

	assert.Positive(t, server.ReadHeaderTimeout)
	assert.Positive(t, server.ReadTimeout)
	assert.Positive(t, server.IdleTimeout)
	assert.Positive(t, server.MaxHeaderBytes)
	assert.LessOrEqual(t, server.MaxHeaderBytes, 64<<10)
}

func TestHTTPServerClosesUnauthenticatedStalledBody(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	handler := NewServer(ServerOptions{
		Token:        "secret",
		ExpectedHost: listener.Addr().String(),
	})
	server := newHTTPServer(handler)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(
		conn,
		"POST /api/v1/daemon/shutdown HTTP/1.1\r\nHost: %s\r\nContent-Length: 1024\r\n\r\n",
		listener.Addr().String(),
	)
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(httpReadTimeout+time.Second)))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	buffer := make([]byte, 1)
	_, err = conn.Read(buffer)
	assert.ErrorIs(t, err, io.EOF)
}

func TestShutdownHTTPServerHonorsAbsoluteDrainDeadline(t *testing.T) {
	for _, test := range []struct {
		name       string
		drain      DrainResult
		deadline   time.Duration
		maximumRun time.Duration
	}{
		{
			name:       "released reservations use remaining deadline",
			drain:      DrainReleased,
			deadline:   50 * time.Millisecond,
			maximumRun: time.Second,
		},
		{
			name:       "expired drain forces immediate close",
			drain:      DrainDeadline,
			deadline:   time.Second,
			maximumRun: 500 * time.Millisecond,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			started := make(chan struct{})
			canceled := make(chan struct{})
			server := newHTTPServer(http.HandlerFunc(func(
				_ http.ResponseWriter,
				request *http.Request,
			) {
				close(started)
				<-request.Context().Done()
				close(canceled)
			}))
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			client := &http.Client{Transport: &http.Transport{Proxy: nil}}
			requestDone := make(chan error, 1)
			go func() {
				_, err := client.Get("http://" + listener.Addr().String())
				requestDone <- err
			}()
			<-started

			before := time.Now()
			_ = shutdownHTTPServer(server, before.Add(test.deadline), test.drain)
			assert.Less(t, time.Since(before), test.maximumRun)
			select {
			case <-canceled:
			case <-time.After(time.Second):
				require.FailNow(t, "active handler was not canceled")
			}
			<-requestDone
			<-serveDone
		})
	}
}

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

func TestServeContinuesWithoutDisposableInventoryCache(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "cache"), []byte("blocked"), 0o600))
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
		})
	}()
	select {
	case err := <-done:
		require.FailNow(t, "daemon stopped for a disposable cache failure", err)
	case <-time.After(100 * time.Millisecond):
	}

	observation := waitForRuntime(t, home)
	result, err := observation.Client.Inventory(context.Background(), publicworktree.Request{
		View: publicworktree.ViewProjects, UntrustedConfig: publicworktree.IgnoreUntrustedConfig,
	})
	require.NoError(t, err)
	assert.Equal(t, publicworktree.Fresh, result.Freshness)
	result, err = observation.Client.Inventory(context.Background(), publicworktree.Request{
		View: publicworktree.ViewDashboard, UntrustedConfig: publicworktree.IgnoreUntrustedConfig,
	})
	require.NoError(t, err)
	assert.Equal(t, publicworktree.Fresh, result.Freshness)
	status, err := observation.Client.Status(context.Background())
	require.NoError(t, err)
	require.NotNil(t, status.LastError)
	assert.Contains(t, status.LastError.Message, "inventory cache")

	_, err = observation.Client.Shutdown(context.Background(), "stop")
	require.NoError(t, err)
	require.NoError(t, <-done)
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
