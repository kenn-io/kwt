package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type fakeDaemonSSHLease struct {
	mu         sync.Mutex
	mode       kwt.SSHLeaseMode
	touches    int
	releases   int
	touchErr   error
	releaseErr error
}

type blockingDaemonSSHLease struct {
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (*blockingDaemonSSHLease) Mode() kwt.SSHLeaseMode { return kwt.SSHLeaseModeMultiplexed }
func (*blockingDaemonSSHLease) Generation() uint64     { return 18 }
func (*blockingDaemonSSHLease) Arguments(context.Context) ([]string, error) {
	return []string{"-S", "/private/control"}, nil
}
func (*blockingDaemonSSHLease) Touch() error { return nil }
func (l *blockingDaemonSSHLease) Release(ctx context.Context) error {
	l.once.Do(func() { close(l.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.unblock:
		return nil
	}
}

func (l *fakeDaemonSSHLease) Mode() kwt.SSHLeaseMode {
	if l.mode == "" {
		return kwt.SSHLeaseModeMultiplexed
	}
	return l.mode
}
func (*fakeDaemonSSHLease) Generation() uint64 { return 17 }
func (*fakeDaemonSSHLease) Arguments(context.Context) ([]string, error) {
	return []string{"-S", "/private/control"}, nil
}
func (l *fakeDaemonSSHLease) Touch() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touches++
	return l.touchErr
}
func (l *fakeDaemonSSHLease) Release(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releases++
	return l.releaseErr
}

type fakeDaemonSSHLifecycle struct {
	lease    *fakeDaemonSSHLease
	mu       sync.Mutex
	requests []kwt.SSHLeaseRequest
}

type promptingDaemonSSHLifecycle struct {
	lease   kwt.SSHLease
	answers []string
	done    chan error
}

type hostDaemonSSHLifecycle struct {
	lease  *fakeDaemonSSHLease
	closed chan struct{}
}

func (s *hostDaemonSSHLifecycle) Resolve(
	_ context.Context,
	request kwt.SSHResolveRequest,
) (kwt.SSHRouteSnapshot, error) {
	return kwt.SSHRouteSnapshot{
		LogicalTarget: request.Target, RouteIdentity: "route-one",
		ProjectionPolicy: kwt.SSHProjectionPolicyV1,
	}, nil
}

func (s *hostDaemonSSHLifecycle) Acquire(
	context.Context,
	kwt.SSHLeaseRequest,
) (kwt.SSHLease, error) {
	return s.lease, nil
}

func (s *hostDaemonSSHLifecycle) Close(ctx context.Context) error {
	if err := s.lease.Release(ctx); err != nil {
		return err
	}
	close(s.closed)
	return nil
}

func (s *promptingDaemonSSHLifecycle) Acquire(
	ctx context.Context,
	request kwt.SSHLeaseRequest,
) (kwt.SSHLease, error) {
	for _, message := range []string{"Password:", "Verification code:"} {
		answer, err := request.Prompt(ctx, service.OperationPrompt{
			Kind: "authentication", Message: message, Sensitive: true,
		})
		if err != nil {
			if s.done != nil {
				s.done <- err
			}
			return nil, err
		}
		s.answers = append(s.answers, answer)
	}
	return s.lease, nil
}

func (s *fakeDaemonSSHLifecycle) Acquire(
	_ context.Context,
	request kwt.SSHLeaseRequest,
) (kwt.SSHLease, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	return s.lease, nil
}

func TestServerAcquiresTouchesAndReleasesSSHLease(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		IDSource: func() (string, error) { return "operation-1", nil },
	})
	gate := NewGate(now)
	lease := &fakeDaemonSSHLease{}
	provider := &testStatusProvider{status: Status{State: StateReady}}
	handler := NewServer(ServerOptions{
		Token:        "secret",
		ExpectedHost: "127.0.0.1:43210",
		Status:       provider,
		Now:          func() time.Time { return now },
		Operations:   hub,
		Gate:         gate,
		SSHLifecycle: &fakeDaemonSSHLifecycle{lease: lease},
	})
	requestBody, err := json.Marshal(SSHLeaseOperationRequest{
		OperationID: "request-1",
		Lease: kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget:    kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity:    "route-one",
			ProjectionPolicy: kwt.SSHProjectionPolicyV1,
			Targets: []kwt.SSHResolvedTarget{{
				LogicalTarget:   kwt.SSHTarget{Hostname: "build.example.test"},
				EffectiveTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			}},
		}, WorkingDirectory: "/workspace", Environment: []string{"PATH=/usr/bin"}},
	})
	require.NoError(t, err)
	response := serveSSHRequest(
		handler, http.MethodPost, "/api/v1/ssh/leases", requestBody,
	)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	var accepted SSHLeaseOperation
	require.NoError(t, json.NewDecoder(response.Body).Decode(&accepted))
	assert.Equal(t, "request-1", accepted.OperationID)

	subscription, err := hub.Subscribe(accepted.OperationID, 0)
	require.NoError(t, err)
	defer subscription.Close()
	var result SSHLeaseResult
	for retained := range subscription.Events() {
		var event service.OperationEvent
		require.NoError(t, json.Unmarshal([]byte(retained.encoded), &event))
		if event.Result != nil {
			require.NoError(t, json.Unmarshal(event.Result, &result))
			break
		}
	}
	assert.NotEmpty(t, result.LeaseID)
	assert.Equal(t, uint64(17), result.Generation)
	assert.Equal(t, []string{"-S", "/private/control"}, result.Arguments)
	assert.Equal(t, 1, gate.Snapshot().ActiveLeases)
	deadline := now.Add(time.Minute)
	provider.status = Status{State: StateDraining, DrainDeadline: &deadline}
	response = serveSSHRequest(
		handler, http.MethodPost, "/api/v1/ssh/leases", requestBody,
	)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())

	response = serveSSHRequest(
		handler, http.MethodPost, "/api/v1/ssh/leases/"+result.LeaseID+"/touch", nil,
	)
	assert.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	response = serveSSHRequest(
		handler, http.MethodDelete, "/api/v1/ssh/leases/"+result.LeaseID, nil,
	)
	assert.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	assert.Equal(t, 0, gate.Snapshot().ActiveLeases)
	lease.mu.Lock()
	assert.Equal(t, 1, lease.touches)
	assert.Equal(t, 1, lease.releases)
	lease.mu.Unlock()
}

func TestServerRejectsMasterlessSSHLease(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	gate := NewGate(time.Now())
	lease := &fakeDaemonSSHLease{mode: kwt.SSHLeaseModeMasterless}
	handler := NewServer(ServerOptions{
		Token: "secret", ExpectedHost: "127.0.0.1:43210",
		Status:     &testStatusProvider{status: Status{State: StateReady}},
		Operations: hub, Gate: gate,
		SSHLifecycle: &fakeDaemonSSHLifecycle{lease: lease},
	})
	requestBody, err := json.Marshal(SSHLeaseOperationRequest{
		OperationID: "masterless-operation",
		Lease: kwt.SSHLeaseRequest{
			Snapshot: kwt.SSHRouteSnapshot{
				LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
				RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
			},
			WorkingDirectory: "/workspace", Environment: []string{"PATH=/usr/bin"},
		},
	})
	require.NoError(t, err)
	response := serveSSHRequest(handler, http.MethodPost, sshLeaseRoute, requestBody)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	subscription, err := hub.Subscribe("masterless-operation", 0)
	require.NoError(t, err)
	defer subscription.Close()
	var failure *service.Descriptor
	for retained := range subscription.Events() {
		var event service.OperationEvent
		require.NoError(t, json.Unmarshal([]byte(retained.encoded), &event))
		if event.Kind == service.OperationEventComplete {
			failure = event.Failure
			break
		}
	}
	require.NotNil(t, failure)
	assert.Equal(t, service.SSHRouteUnreviewable, failure.Code)
	assert.False(t, failure.Retryable)
	assert.Equal(t, 0, gate.Snapshot().ActiveLeases)
	lease.mu.Lock()
	assert.Equal(t, 1, lease.releases)
	lease.mu.Unlock()
}

func TestClientFollowsSSHLeaseOperationAndReleasesLease(t *testing.T) {
	lease := &fakeDaemonSSHLease{}
	lifecycle := &fakeDaemonSSHLifecycle{lease: lease}
	client, closeServer := newSSHLifecycleTestClient(t, lifecycle)
	defer closeServer()
	var events []service.OperationEventKind
	result, err := client.AcquireSSH(
		context.Background(),
		kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}},
		OperationCallbacks{Event: func(event service.OperationEvent) error {
			events = append(events, event.Kind)
			return nil
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, uint64(17), result.Generation)
	assert.Equal(t, []string{"-S", "/private/control"}, result.Arguments)
	assert.Contains(t, events, service.OperationEventProgress)
	assert.Contains(t, events, service.OperationEventComplete)
	holdContext, cancelHold := context.WithCancel(context.Background())
	hold, err := client.HoldSSHLease(holdContext, result.LeaseID)
	require.NoError(t, err)
	cancelHold()
	require.NoError(t, hold.Close())
	require.NoError(t, client.TouchSSHLease(context.Background(), result.LeaseID))
	require.NoError(t, client.ReleaseSSHLease(context.Background(), result.LeaseID))

	lifecycle.mu.Lock()
	require.Len(t, lifecycle.requests, 1)
	assert.True(t, filepath.IsAbs(lifecycle.requests[0].WorkingDirectory))
	assert.NotNil(t, lifecycle.requests[0].Environment)
	lifecycle.mu.Unlock()
}

func TestSSHLeaseHoldSuppressesExpiryUntilOwnerDisconnects(t *testing.T) {
	lease := &fakeDaemonSSHLease{}
	registry := newSSHLeaseRegistry(nil, time.Now, 20*time.Millisecond)
	leaseID, err := registry.add(lease)
	require.NoError(t, err)
	releaseHold, err := registry.hold(leaseID)
	require.NoError(t, err)

	time.Sleep(60 * time.Millisecond)
	require.NoError(t, registry.touch(leaseID))
	releaseHold()
	require.Eventually(t, func() bool {
		_, entryErr := registry.entry(leaseID)
		return service.IsCode(entryErr, service.NotFound)
	}, time.Second, 10*time.Millisecond)

	lease.mu.Lock()
	assert.Equal(t, 1, lease.releases)
	lease.mu.Unlock()
}

func TestClientPrefersTerminalSSHFailureAfterLostStartResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == sshLeaseRoute:
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack SSH lease response: %v", err)
				return
			}
			_ = connection.Close()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			operationID := strings.TrimSuffix(
				strings.TrimPrefix(r.URL.Path, operationRoutePrefix),
				"/events",
			)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(service.OperationEvent{
				OperationID: operationID,
				Sequence:    1,
				Kind:        service.OperationEventComplete,
				Failure: &service.Descriptor{
					Code:      service.SSHPromptTimedOut,
					Message:   "SSH prompt timed out",
					Retryable: false,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	_, err := client.AcquireSSH(
		context.Background(),
		kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}},
		OperationCallbacks{},
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHPromptTimedOut), err)
	assert.False(t, service.IsCode(err, service.DaemonTransportFailed), err)
}

func TestClientReturnsSSHLeaseResultWithTerminalCallbackFailure(t *testing.T) {
	lease := &fakeDaemonSSHLease{}
	client, closeServer := newSSHLifecycleTestClient(
		t, &fakeDaemonSSHLifecycle{lease: lease},
	)
	defer closeServer()
	writeErr := errors.New("output closed")
	result, err := client.AcquireSSH(
		context.Background(),
		kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}},
		OperationCallbacks{Event: func(event service.OperationEvent) error {
			if event.Kind == service.OperationEventComplete {
				return writeErr
			}
			return nil
		}},
	)
	assert.ErrorIs(t, err, writeErr)
	assert.NotEmpty(t, result.LeaseID)
	require.NoError(t, client.ReleaseSSHLease(context.Background(), result.LeaseID))
}

func TestClientRoundTripsSSHLeaseControlErrors(t *testing.T) {
	lease := &fakeDaemonSSHLease{}
	client, closeServer := newSSHLifecycleTestClient(
		t, &fakeDaemonSSHLifecycle{lease: lease},
	)
	defer closeServer()
	result, err := client.AcquireSSH(
		context.Background(),
		kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}},
		OperationCallbacks{},
	)
	require.NoError(t, err)

	lease.mu.Lock()
	lease.touchErr = service.NewError(
		service.SSHConnectionChanged, "SSH connection changed", false, nil, nil,
	)
	lease.mu.Unlock()
	err = client.TouchSSHLease(context.Background(), result.LeaseID)
	assert.True(t, service.IsCode(err, service.SSHConnectionChanged), err)

	lease.mu.Lock()
	lease.touchErr = nil
	lease.releaseErr = context.DeadlineExceeded
	lease.mu.Unlock()
	err = client.ReleaseSSHLease(context.Background(), result.LeaseID)
	assert.True(t, service.IsCode(err, service.SSHCleanupFailed), err)
	assert.True(t, service.AsError(err).Retryable)

	lease.mu.Lock()
	lease.releaseErr = nil
	lease.mu.Unlock()
	require.NoError(t, client.ReleaseSSHLease(context.Background(), result.LeaseID))
}

func TestClientCarriesMultipleBoundSSHPromptRounds(t *testing.T) {
	lease := &fakeDaemonSSHLease{}
	lifecycle := &promptingDaemonSSHLifecycle{lease: lease}
	client, closeServer := newSSHLifecycleTestClient(t, lifecycle)
	defer closeServer()
	responses := []string{"secret", "123456"}
	result, err := client.AcquireSSH(
		context.Background(),
		kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}},
		OperationCallbacks{Prompt: func(
			_ context.Context,
			_ service.OperationPrompt,
		) (string, error) {
			response := responses[0]
			responses = responses[1:]
			return response, nil
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"secret", "123456"}, lifecycle.answers)
	require.NoError(t, client.ReleaseSSHLease(context.Background(), result.LeaseID))
}

func TestClientCancelsSSHOperationWithoutPromptHandler(t *testing.T) {
	lifecycle := &promptingDaemonSSHLifecycle{
		lease: &fakeDaemonSSHLease{}, done: make(chan error, 1),
	}
	client, closeServer := newSSHLifecycleTestClient(t, lifecycle)
	defer closeServer()

	_, err := client.AcquireSSH(
		context.Background(),
		kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}},
		OperationCallbacks{},
	)
	assert.True(t, service.IsCode(err, service.SSHInteractionRequired), err)
	select {
	case operationErr := <-lifecycle.done:
		assert.ErrorIs(t, operationErr, context.Canceled)
	case <-time.After(100 * time.Millisecond):
		require.Fail(t, "SSH operation was not canceled")
	}
}

func TestServerExpiresSSHLeaseWhenClientStopsTouching(t *testing.T) {
	now := time.Now()
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	gate := NewGate(now)
	lease := &fakeDaemonSSHLease{}
	handler := NewServer(ServerOptions{
		Token: "secret", ExpectedHost: "127.0.0.1:43210",
		Status: &testStatusProvider{status: Status{State: StateReady}},
		Now:    time.Now, Operations: hub, Gate: gate,
		SSHLifecycle:    &fakeDaemonSSHLifecycle{lease: lease},
		SSHLeaseTimeout: 20 * time.Millisecond,
	})
	requestBody, err := json.Marshal(SSHLeaseOperationRequest{
		OperationID: "expiry-operation",
		Lease: kwt.SSHLeaseRequest{
			Snapshot: kwt.SSHRouteSnapshot{
				LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
				RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
			},
			WorkingDirectory: "/workspace", Environment: []string{"PATH=/usr/bin"},
		},
	})
	require.NoError(t, err)
	response := serveSSHRequest(handler, http.MethodPost, sshLeaseRoute, requestBody)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	subscription, err := hub.Subscribe("expiry-operation", 0)
	require.NoError(t, err)
	for retained := range subscription.Events() {
		if retained.kind == service.OperationEventComplete {
			break
		}
	}
	subscription.Close()
	require.Eventually(t, func() bool {
		lease.mu.Lock()
		defer lease.mu.Unlock()
		return lease.releases == 1 && gate.Snapshot().ActiveLeases == 0
	}, time.Second, 5*time.Millisecond)
}

func TestSSHLeaseRegistryCloseInvalidatesEveryDaemonLease(t *testing.T) {
	gate := NewGate(time.Now())
	registry := newSSHLeaseRegistry(gate, time.Now, time.Hour)
	first := &fakeDaemonSSHLease{}
	second := &fakeDaemonSSHLease{}
	_, err := registry.add(first)
	require.NoError(t, err)
	_, err = registry.add(second)
	require.NoError(t, err)
	assert.Equal(t, 2, gate.Snapshot().ActiveLeases)

	require.NoError(t, registry.close(context.Background()))
	assert.Equal(t, 0, gate.Snapshot().ActiveLeases)
}

func TestSSHLeaseRegistryCloseDoesNotWaitForBlockedLeaseRelease(t *testing.T) {
	gate := NewGate(time.Now())
	registry := newSSHLeaseRegistry(gate, time.Now, time.Hour)
	lease := &blockingDaemonSSHLease{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	defer close(lease.unblock)
	leaseID, err := registry.add(lease)
	require.NoError(t, err)
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- registry.release(context.Background(), leaseID) }()
	<-lease.started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- registry.close(ctx) }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(100 * time.Millisecond):
		require.Fail(t, "registry close waited for blocked SSH release")
	}
	assert.Equal(t, 0, gate.Snapshot().ActiveLeases)
}

func TestSSHLeaseReleaseDeadlineLeavesCleanupRetryable(t *testing.T) {
	gate := NewGate(time.Now())
	registry := newSSHLeaseRegistry(gate, time.Now, time.Hour)
	registry.cleanupTimeout = 20 * time.Millisecond
	lease := &blockingDaemonSSHLease{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	leaseID, err := registry.add(lease)
	require.NoError(t, err)
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- registry.release(context.Background(), leaseID) }()
	<-lease.started

	select {
	case err = <-releaseDone:
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.True(t, service.IsCode(err, service.SSHCleanupFailed), err)
		assert.True(t, service.AsError(err).Retryable)
	case <-time.After(100 * time.Millisecond):
		close(lease.unblock)
		<-releaseDone
		require.FailNow(t, "SSH lease release ignored its context deadline")
	}
	assert.Equal(t, 1, gate.Snapshot().ActiveLeases)
	close(lease.unblock)
	require.NoError(t, registry.release(context.Background(), leaseID))
	assert.Equal(t, 0, gate.Snapshot().ActiveLeases)
}

func TestServeDrainsActiveSSHLeaseThenClosesLifecycleOwner(t *testing.T) {
	home := t.TempDir()
	owner := &hostDaemonSSHLifecycle{
		lease: &fakeDaemonSSHLease{}, closed: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{
			Home: home, Build: Build{Version: "v1.0.0", Revision: "abc"},
			Config: models.DaemonConfig{
				IdleTimeout: time.Hour, AutoRestart: "newer",
				ReplacementGrace: 30 * time.Millisecond,
			},
			Foreground: true, Now: time.Now,
			SSHResolver: owner, SSHLifecycle: owner,
		})
	}()
	observation := waitForRuntime(t, home)
	assert.Contains(t, observation.Status.Capabilities, CapabilitySSHLifecycle)
	assert.Contains(t, observation.Status.Capabilities, CapabilitySSHLeaseHold)
	result, err := observation.Client.AcquireSSH(
		context.Background(),
		kwt.SSHLeaseRequest{Snapshot: kwt.SSHRouteSnapshot{
			LogicalTarget: kwt.SSHTarget{Hostname: "build.example.test"},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}},
		OperationCallbacks{},
	)
	require.NoError(t, err)
	assert.NotEmpty(t, result.LeaseID)
	status, err := observation.Client.Shutdown(context.Background(), "replacement")
	require.NoError(t, err)
	assert.Equal(t, StateDraining, status.Status.State)
	assert.Equal(t, 1, status.Status.ActiveLeases)
	require.NoError(t, <-done)
	select {
	case <-owner.closed:
	default:
		require.Fail(t, "SSH lifecycle owner was not closed")
	}
	assert.Empty(t, runtimeFiles(t, home))
}

func TestServeClosesDistinctSSHResolverAndLifecycleOwners(t *testing.T) {
	home := t.TempDir()
	resolver := &hostDaemonSSHLifecycle{
		lease: &fakeDaemonSSHLease{}, closed: make(chan struct{}),
	}
	lifecycle := &hostDaemonSSHLifecycle{
		lease: &fakeDaemonSSHLease{}, closed: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{
			Home: home, Build: Build{Version: "v1.0.0", Revision: "abc"},
			Config: models.DaemonConfig{
				IdleTimeout: time.Hour, AutoRestart: "newer",
				ReplacementGrace: 30 * time.Millisecond,
			},
			Foreground: true, Now: time.Now,
			SSHResolver: resolver, SSHLifecycle: lifecycle,
		})
	}()
	observation := waitForRuntime(t, home)
	_, err := observation.Client.Shutdown(context.Background(), "replacement")
	require.NoError(t, err)
	require.NoError(t, <-done)
	select {
	case <-resolver.closed:
	default:
		require.Fail(t, "SSH resolver owner was not closed")
	}
	select {
	case <-lifecycle.closed:
	default:
		require.Fail(t, "SSH lifecycle owner was not closed")
	}
}

func newSSHLifecycleTestClient(
	t *testing.T,
	lifecycle SSHLifecycle,
) (*Client, func()) {
	t.Helper()
	provider := &testStatusProvider{status: Status{State: StateReady}}
	gate := NewGate(time.Now())
	hubContext, cancelHub := context.WithCancel(context.Background())
	hub := NewOperationHub(hubContext, OperationHubOptions{
		Reserve: func() (func(), error) {
			return gate.Reserve(ReservationWork, time.Now())
		},
	})
	var server *httptest.Server
	handler := NewServer(ServerOptions{
		Token: "secret", Status: provider, Operations: hub,
		Gate: gate, SSHLifecycle: lifecycle, Now: time.Now,
	})
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		request.Host = server.Listener.Addr().String()
		handler.ServeHTTP(w, request)
	}))
	handler = NewServer(ServerOptions{
		Token: "secret", ExpectedHost: server.Listener.Addr().String(), Status: provider,
		Operations: hub, Gate: gate, SSHLifecycle: lifecycle, Now: time.Now,
	})
	ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: server.Listener.Addr().String()}
	return newClient(ep, "secret", server.Client()), func() {
		cancelHub()
		server.Close()
	}
}

func serveSSHRequest(
	handler http.Handler,
	method string,
	path string,
	body []byte,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1:43210"+path, bytes.NewReader(body))
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
