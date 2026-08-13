package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

type fakeSSHResolver struct {
	mu       sync.Mutex
	result   kwt.SSHRouteSnapshot
	err      error
	requests []kwt.SSHResolveRequest
	started  chan struct{}
	release  chan struct{}
}

func (r *fakeSSHResolver) Resolve(
	ctx context.Context,
	request kwt.SSHResolveRequest,
) (kwt.SSHRouteSnapshot, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	if r.release != nil {
		select {
		case <-ctx.Done():
			return kwt.SSHRouteSnapshot{}, ctx.Err()
		case <-r.release:
		}
	}
	return r.result, r.err
}

func TestSSHResolveRouteRoundTripsSnapshotAndReusesService(t *testing.T) {
	resolver := &fakeSSHResolver{result: kwt.SSHRouteSnapshot{
		LogicalTarget:    kwt.SSHTarget{User: "deploy", Hostname: "build.example.test"},
		RouteIdentity:    strings.Repeat("a", 64),
		ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		ObservedAt:       time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC),
		Targets: []kwt.SSHResolvedTarget{{
			LogicalTarget:   kwt.SSHTarget{Hostname: "build.example.test"},
			EffectiveTarget: kwt.SSHTarget{User: "deploy", Hostname: "build.internal", Port: 22},
			DisplayTarget:   "deploy@build.internal:22",
			Projection: kwt.SSHExecutionProjection{
				Arguments: []string{"-F", "/dev/null", "-o", "User=deploy"},
			},
		}},
	}}
	client, closeServer := newSSHResolutionTestClient(t, resolver, NewGate(time.Now()))
	defer closeServer()

	request := kwt.SSHResolveRequest{Target: kwt.SSHTarget{
		User: "deploy", Hostname: "build.example.test",
	}}
	for range 2 {
		result, err := client.ResolveSSH(context.Background(), request)
		require.NoError(t, err)
		assert.Equal(t, resolver.result, result)
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	require.Len(t, resolver.requests, 2)
	for _, captured := range resolver.requests {
		assert.Equal(t, request.Target, captured.Target)
		assert.True(t, filepath.IsAbs(captured.WorkingDirectory))
		assert.NotNil(t, captured.Environment)
	}
}

func TestSSHResolveRouteAcceptsSnapshotAboveControlResponseLimit(t *testing.T) {
	argument := strings.Repeat("x", int(controlResponseLimit)+1)
	resolver := &fakeSSHResolver{result: kwt.SSHRouteSnapshot{
		RouteIdentity:    strings.Repeat("a", 64),
		ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		Targets: []kwt.SSHResolvedTarget{{
			Projection: kwt.SSHExecutionProjection{Arguments: []string{argument}},
		}},
	}}
	client, closeServer := newSSHResolutionTestClient(t, resolver, NewGate(time.Now()))
	defer closeServer()

	result, err := client.ResolveSSH(context.Background(), kwt.SSHResolveRequest{
		Target: kwt.SSHTarget{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	require.Len(t, result.Targets, 1)
	assert.Equal(t, argument, result.Targets[0].Projection.Arguments[0])
}

func TestSSHResolveRouteRejectsSnapshotAboveEndpointLimit(t *testing.T) {
	resolver := &fakeSSHResolver{result: kwt.SSHRouteSnapshot{
		RouteIdentity:    strings.Repeat("a", 64),
		ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		Targets: []kwt.SSHResolvedTarget{{
			Projection: kwt.SSHExecutionProjection{
				Arguments: []string{strings.Repeat("x", int(sshSnapshotLimit)+1)},
			},
		}},
	}}
	client, closeServer := newSSHResolutionTestClient(t, resolver, NewGate(time.Now()))
	defer closeServer()

	_, err := client.ResolveSSH(context.Background(), kwt.SSHResolveRequest{
		Target: kwt.SSHTarget{Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHResolutionFailed), err)
}

func TestSSHResolveRouteUsesFreshSanitizedInvocationEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[fleet]\ntoken_env = \"GHOSTHUB_AUTH\"\n"),
		0o600,
	))
	t.Setenv("GHOSTHUB_AUTH", "fleet-secret")
	t.Setenv("KWT_GITHUB_TOKEN", "github-secret")
	t.Setenv("KWT_SSH_INVOCATION_CONTEXT", "first")

	resolver := &fakeSSHResolver{}
	client, closeServer := newSSHResolutionTestClient(t, resolver, NewGate(time.Now()))
	defer closeServer()
	request := kwt.SSHResolveRequest{Target: kwt.SSHTarget{Hostname: "build.example.test"}}
	_, err := client.ResolveSSH(context.Background(), request)
	require.NoError(t, err)
	t.Setenv("KWT_SSH_INVOCATION_CONTEXT", "second")
	_, err = client.ResolveSSH(context.Background(), request)
	require.NoError(t, err)

	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	require.Len(t, resolver.requests, 2)
	assert.Contains(t, resolver.requests[0].Environment, "KWT_SSH_INVOCATION_CONTEXT=first")
	assert.Contains(t, resolver.requests[1].Environment, "KWT_SSH_INVOCATION_CONTEXT=second")
	for _, captured := range resolver.requests {
		assert.NotContains(t, captured.Environment, "GHOSTHUB_AUTH=fleet-secret")
		assert.NotContains(t, captured.Environment, "KWT_GITHUB_TOKEN=github-secret")
	}
}

func TestSSHResolveRoutePreservesStableFailures(t *testing.T) {
	private := errors.New("ssh://operator:secret@example.test")
	resolver := &fakeSSHResolver{err: service.NewError(
		service.SSHRouteUnreviewable,
		"SSH route cannot be reviewed safely",
		false,
		nil,
		private,
	)}
	client, closeServer := newSSHResolutionTestClient(t, resolver, NewGate(time.Now()))
	defer closeServer()

	_, err := client.ResolveSSH(context.Background(), kwt.SSHResolveRequest{
		Target: kwt.SSHTarget{Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHRouteUnreviewable))
	assert.NotContains(t, err.Error(), "secret")
}

func TestSSHResolveRouteReservesDaemonWorkAndHonorsCancellation(t *testing.T) {
	gate := NewGate(time.Now())
	resolver := &fakeSSHResolver{started: make(chan struct{}, 1), release: make(chan struct{})}
	client, closeServer := newSSHResolutionTestClient(t, resolver, gate)
	defer closeServer()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.ResolveSSH(ctx, kwt.SSHResolveRequest{
			Target: kwt.SSHTarget{Hostname: "build.example.test"},
		})
		done <- err
	}()
	<-resolver.started
	assert.Equal(t, 1, gate.Snapshot().ActiveWork)
	cancel()
	err := <-done
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	require.Eventually(t, func() bool {
		return gate.Snapshot().ActiveWork == 0
	}, time.Second, 10*time.Millisecond)
}

func TestSSHResolveCapabilityAndSchemaAreAdvertised(t *testing.T) {
	assert.Equal(t, "1.8.0", APISchemaVersion)
	record, _, err := NewRuntimeRecord(
		t.TempDir(),
		Build{Version: "development"},
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
	)
	require.NoError(t, err)
	capabilities := strings.Split(record.Metadata[metadataCapabilities], ",")
	assert.Contains(t, capabilities, CapabilitySSHResolve)
}

func TestSSHResolveResponseDoesNotPublishCanonicalOptions(t *testing.T) {
	resolver := &fakeSSHResolver{result: kwt.SSHRouteSnapshot{
		RouteIdentity: strings.Repeat("b", 64), ProjectionPolicy: kwt.SSHProjectionPolicyV1,
	}}
	provider := &testStatusProvider{status: Status{State: StateReady}}
	handler := NewServer(ServerOptions{
		Token: "secret", ExpectedHost: "127.0.0.1:43210", Status: provider,
		Shutdown: func(context.Context, ShutdownRequest) (Status, error) {
			return provider.status, nil
		},
		SSHResolver: resolver,
	})
	body, err := json.Marshal(kwt.SSHResolveRequest{
		Target:           kwt.SSHTarget{Hostname: "build.example.test"},
		WorkingDirectory: t.TempDir(),
		Environment:      []string{},
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43210/api/v1/ssh/resolve", strings.NewReader(string(body)))
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "canonical_options")
}

func TestSSHResolveRouteRejectsMissingInvocationEnvironment(t *testing.T) {
	for _, test := range []struct {
		name    string
		request kwt.SSHResolveRequest
	}{
		{
			name: "environment",
			request: kwt.SSHResolveRequest{
				Target: kwt.SSHTarget{Hostname: "build.example.test"}, WorkingDirectory: t.TempDir(),
			},
		},
		{
			name: "working directory",
			request: kwt.SSHResolveRequest{
				Target: kwt.SSHTarget{Hostname: "build.example.test"}, Environment: []string{},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeSSHResolver{}
			provider := &testStatusProvider{status: Status{State: StateReady}}
			handler := NewServer(ServerOptions{
				Token: "secret", ExpectedHost: "127.0.0.1:43210", Status: provider,
				SSHResolver: resolver,
			})
			body, err := json.Marshal(test.request)
			require.NoError(t, err)
			request := httptest.NewRequest(
				http.MethodPost,
				"http://127.0.0.1:43210/api/v1/ssh/resolve",
				strings.NewReader(string(body)),
			)
			request.Host = "127.0.0.1:43210"
			request.Header.Set("Authorization", "Bearer secret")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			assert.Equal(t, http.StatusBadRequest, response.Code)
			resolver.mu.Lock()
			defer resolver.mu.Unlock()
			assert.Empty(t, resolver.requests)
		})
	}
}

func newSSHResolutionTestClient(
	t *testing.T,
	resolver SSHResolver,
	gate *Gate,
) (*Client, func()) {
	t.Helper()
	provider := &testStatusProvider{status: Status{State: StateReady}}
	var server *httptest.Server
	handler := NewServer(ServerOptions{
		Token: "secret", Status: provider, SSHResolver: resolver, Gate: gate,
		Shutdown: func(context.Context, ShutdownRequest) (Status, error) {
			return provider.status, nil
		},
		Now: func() time.Time { return time.Now() },
	})
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		request.Host = server.Listener.Addr().String()
		handler.ServeHTTP(w, request)
	}))
	provider.status.Endpoint = server.Listener.Addr().String()
	// The test wrapper normalizes Host to this exact value before the secure
	// handler evaluates it.
	handler = NewServer(ServerOptions{
		Token: "secret", ExpectedHost: server.Listener.Addr().String(), Status: provider,
		SSHResolver: resolver, Gate: gate,
		Shutdown: func(context.Context, ShutdownRequest) (Status, error) {
			return provider.status, nil
		},
		Now: time.Now,
	})
	ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: server.Listener.Addr().String()}
	return newClient(ep, "secret", server.Client()), server.Close
}
