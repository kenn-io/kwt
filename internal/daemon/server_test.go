package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

type testStatusProvider struct{ status Status }

type stalledOperationResponseWriter struct {
	header      http.Header
	deadlineSet chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (w *stalledOperationResponseWriter) Header() http.Header { return w.header }
func (w *stalledOperationResponseWriter) WriteHeader(int)     {}
func (w *stalledOperationResponseWriter) Flush()              {}
func (w *stalledOperationResponseWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		w.once.Do(func() { close(w.deadlineSet) })
	}
	return nil
}
func (w *stalledOperationResponseWriter) Write(value []byte) (int, error) {
	select {
	case <-w.deadlineSet:
		return 0, context.DeadlineExceeded
	case <-w.release:
		return len(value), nil
	}
}

func (p *testStatusProvider) Status(time.Time) Status { return p.status }

func newTestHandler(
	t *testing.T,
	token string,
	host string,
	provider *testStatusProvider,
) http.Handler {
	t.Helper()
	return NewServer(ServerOptions{
		Token:        token,
		ExpectedHost: host,
		Status:       provider,
		Shutdown: func(_ context.Context, _ ShutdownRequest) (Status, error) {
			return provider.status, nil
		},
		Now:          func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) },
		MaxBodyBytes: 32,
	})
}

func TestServerRequiresExactHostAndBearerForStatus(t *testing.T) {
	provider := &testStatusProvider{status: Status{Service: ServiceName, State: StateReady}}
	handler := newTestHandler(t, "secret", "127.0.0.1:43210", provider)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43210/api/v1/status", nil)
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code)

	request = httptest.NewRequest(http.MethodGet, "http://evil.invalid/api/v1/status", nil)
	request.Host = "evil.invalid"
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusBadRequest, response.Code)

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43210/api/v1/status", nil)
	request.Host = "127.0.0.1:43210"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestServerRejectsBrowserOriginAndOversizedBodies(t *testing.T) {
	provider := &testStatusProvider{status: Status{Service: ServiceName, State: StateReady}}
	handler := newTestHandler(t, "secret", "127.0.0.1:43210", provider)

	request := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:43210/api/v1/status",
		nil,
	)
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Origin", "https://example.invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.Empty(t, response.Header().Get("Access-Control-Allow-Origin"))

	request = httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:43210/api/v1/daemon/shutdown",
		strings.NewReader(`{"reason":"stop","padding":"012345678901234567890123456789"}`),
	)
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	var problem Problem
	require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(t, service.InvalidRequest, problem.Code)
}

func TestServerDefaultBodyLimitAccommodatesClientExpansionContext(t *testing.T) {
	provider := &testStatusProvider{status: Status{State: StateReady}}
	handler := NewServer(ServerOptions{
		Token:        "secret",
		ExpectedHost: "127.0.0.1:43210",
		Status:       provider,
		Inventory:    &fakeInventory{},
		Gate:         NewGate(time.Now()),
	})
	body, err := json.Marshal(kwt.Request{
		View: kwt.ViewProjects,
		Expansion: kwt.ExpansionContext{
			WorkingDirectory: "/workspace",
			HomeDirectory:    "/home/user",
			Environment:      map[string]string{"LARGE_PATH_CONTEXT": strings.Repeat("x", 100<<10)},
		},
		UntrustedConfig: kwt.IgnoreUntrustedConfig,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:43210/api/v1/inventory",
		strings.NewReader(string(body)),
	)
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestServerPublishesOpenAPIWithoutAuthentication(t *testing.T) {
	provider := &testStatusProvider{status: Status{Service: ServiceName, State: StateReady}}
	handler := newTestHandler(t, "secret", "127.0.0.1:43210", provider)
	request := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:43210/openapi.json",
		nil,
	)
	request.Host = "127.0.0.1:43210"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&document))
	assert.Equal(t, "daemon-status", document.Paths["/api/v1/status"]["get"].OperationID)
	assert.Equal(t,
		"daemon-shutdown",
		document.Paths["/api/v1/daemon/shutdown"]["post"].OperationID,
	)
}

func TestDrainingServerReturnsRetryableProblemWithDeadline(t *testing.T) {
	deadline := time.Date(2026, 8, 7, 12, 5, 0, 0, time.UTC)
	provider := &testStatusProvider{status: Status{
		Service:       ServiceName,
		State:         StateDraining,
		DrainDeadline: &deadline,
	}}
	handler := newTestHandler(t, "secret", "127.0.0.1:43210", provider)
	request := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:43210/api/v1/future-operation",
		nil,
	)
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	var problem struct {
		service.Descriptor
		DrainDeadline *time.Time `json:"drain_deadline"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(t, service.DaemonDraining, problem.Code)
	assert.True(t, problem.Retryable)
	assert.Equal(t, deadline.Format(time.RFC3339), problem.Details["drain_deadline"])
	require.NotNil(t, problem.DrainDeadline)
	assert.Equal(t, deadline, *problem.DrainDeadline)
}

func TestProblemRoundTripsServiceDescriptor(t *testing.T) {
	descriptor := service.Descriptor{
		Code: service.DaemonDraining, Message: "the kwt daemon is draining", Retryable: true,
		Details: map[string]any{"drain_deadline": "2026-08-10T01:02:03Z"},
	}
	problem := problemFromError(service.NewDescriptorError(descriptor, errors.New("private cause")))
	encoded, err := json.Marshal(problem)
	require.NoError(t, err)

	decoded := decodeProblem(problem.Status, bytes.NewReader(encoded))
	typed := service.AsError(decoded)
	assert.Equal(t, descriptor.Code, typed.Code)
	assert.Equal(t, descriptor.Message, typed.Message)
	assert.Equal(t, descriptor.Retryable, typed.Retryable)
	assert.Equal(t, descriptor.Details, typed.Details)
	assert.NotContains(t, string(encoded), "private cause")
}

func TestSSHProblemCodesRoundTrip(t *testing.T) {
	codes := []service.Code{
		service.SSHInvalidTarget,
		service.SSHResolutionFailed,
		service.SSHRouteUnreviewable,
		service.SSHConfigurationChanged,
		service.SSHUnsupportedVersion,
		service.SSHInteractionRequired,
		service.SSHPromptRejected,
		service.SSHPromptTimedOut,
		service.SSHConnectionFailed,
		service.SSHConnectionChanged,
		service.SSHControlPathOccupied,
		service.SSHCleanupFailed,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			descriptor := service.Descriptor{Code: code, Message: "SSH lifecycle failure"}
			problem := problemFromError(service.NewDescriptorError(descriptor, nil))
			encoded, err := json.Marshal(problem)
			require.NoError(t, err)
			decoded := service.AsError(decodeProblem(problem.Status, bytes.NewReader(encoded)))
			assert.Equal(t, code, decoded.Code)
			assert.Equal(t, descriptor.Message, decoded.Message)
		})
	}
}

func TestProblemMakesUnknownOutcomeNonRetryable(t *testing.T) {
	problem := problemFromError(service.NewError(
		service.OperationOutcomeUnknown,
		"operation outcome is unknown",
		true,
		nil,
		nil,
	))
	encoded, err := json.Marshal(problem)
	require.NoError(t, err)

	decoded := service.AsError(decodeProblem(problem.Status, bytes.NewReader(encoded)))
	assert.Equal(t, service.OperationOutcomeUnknown, decoded.Code)
	assert.False(t, decoded.Retryable)
}

func TestOperationEventRouteReplaysThenStreamsLiveEvents(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	replayed := make(chan struct{})
	release := make(chan struct{})
	operation, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(_ context.Context, operation *Operation) (json.RawMessage, error) {
			if err := operation.Progress("replayed"); err != nil {
				return nil, err
			}
			close(replayed)
			<-release
			if err := operation.Progress("live"); err != nil {
				return nil, err
			}
			return json.RawMessage(`{"status":"ready"}`), nil
		},
	})
	require.NoError(t, err)
	<-replayed

	server := newOperationHTTPTestServer(t, hub)
	defer server.Close()
	request, err := http.NewRequest(
		http.MethodGet,
		server.URL+"/api/v1/operations/"+operation.ID()+"/events?after_sequence=0",
		nil,
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer secret")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "application/x-ndjson", response.Header.Get("Content-Type"))

	decoder := json.NewDecoder(response.Body)
	var event service.OperationEvent
	require.NoError(t, decoder.Decode(&event))
	assert.Equal(t, "replayed", event.Message)
	close(release)
	require.NoError(t, decoder.Decode(&event))
	assert.Equal(t, "live", event.Message)
	require.NoError(t, decoder.Decode(&event))
	assert.Equal(t, service.OperationEventComplete, event.Kind)
}

func TestOperationResponseRouteBindsCurrentPrompt(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	operation, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(ctx context.Context, operation *Operation) (json.RawMessage, error) {
			value, err := operation.Prompt(ctx, service.OperationPrompt{
				Kind: "password", Message: "Password:", Sensitive: true,
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]string{"value": value})
		},
	})
	require.NoError(t, err)
	subscription, err := hub.Subscribe(operation.ID(), 0)
	require.NoError(t, err)
	defer subscription.Close()
	prompt := receiveOperationEvent(t, subscription.Events())
	require.NotNil(t, prompt.Prompt)

	server := newOperationHTTPTestServer(t, hub)
	defer server.Close()
	body, err := json.Marshal(service.OperationResponse{
		PromptID: prompt.Prompt.ID,
		Value:    "fleet secret",
	})
	require.NoError(t, err)
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/api/v1/operations/"+operation.ID()+"/responses",
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusNoContent, response.StatusCode)

	completion := receiveOperationEvent(t, subscription.Events())
	require.Equal(t, service.OperationEventComplete, completion.Kind)
	assert.JSONEq(t, `{"value":"fleet secret"}`, string(completion.Result))
}

func TestOperationEventWriteDeadlineStopsStalledSubscriber(t *testing.T) {
	releaseWorker := make(chan struct{})
	replayReady := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		MaxSubscribersPerOperation: 1,
		MaxSubscribers:             1,
	})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(_ context.Context, operation *Operation) (json.RawMessage, error) {
			if err := operation.Progress("replay"); err != nil {
				return nil, err
			}
			close(replayReady)
			<-releaseWorker
			return json.RawMessage(`{}`), nil
		},
	})
	require.NoError(t, err)
	<-replayReady

	writer := &stalledOperationResponseWriter{
		header:      make(http.Header),
		deadlineSet: make(chan struct{}),
		release:     make(chan struct{}),
	}
	requestContext, cancelRequest := context.WithCancel(context.Background())
	request := httptest.NewRequest(
		http.MethodGet,
		"http://kwt.invalid/api/v1/operations/"+operation.ID()+"/events",
		nil,
	).WithContext(requestContext)
	done := make(chan struct{})
	go func() {
		serveOperationEvents(writer, request, hub, ServerOptions{}, operation.ID())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		cancelRequest()
		close(writer.release)
		close(releaseWorker)
		t.Fatal("stalled operation response survived its write deadline")
	}
	cancelRequest()
	close(writer.release)
	replacement, err := hub.Subscribe(operation.ID(), 0)
	require.NoError(t, err)
	replacement.Close()
	close(releaseWorker)
}

func TestOperationCancelRouteCancelsWorker(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	started := make(chan struct{})
	canceled := make(chan struct{})
	operation, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(ctx context.Context, _ *Operation) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
	})
	require.NoError(t, err)
	<-started

	server := newOperationHTTPTestServer(t, hub)
	defer server.Close()
	request, err := http.NewRequest(
		http.MethodDelete,
		server.URL+"/api/v1/operations/"+operation.ID(),
		nil,
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer secret")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("operation worker was not canceled")
	}
}

func TestOperationRoutesRequireAuthenticationAndStableNotFoundProblem(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	server := newOperationHTTPTestServer(t, hub)
	defer server.Close()

	request, err := http.NewRequest(
		http.MethodGet,
		server.URL+"/api/v1/operations/missing/events",
		nil,
	)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)

	request, err = http.NewRequest(
		http.MethodGet,
		server.URL+"/api/v1/operations/missing/events",
		nil,
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer secret")
	response, err = server.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	var problem Problem
	require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(t, service.NotFound, problem.Code)
}

func TestDrainingServerKeepsExistingOperationControlReachable(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	operation, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(_ context.Context, operation *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), operation.Progress("finishing")
		},
	})
	require.NoError(t, err)
	deadline := time.Now().Add(time.Minute)
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = NewServer(ServerOptions{
		Token:        "secret",
		ExpectedHost: server.Listener.Addr().String(),
		Status: &testStatusProvider{status: Status{
			Service: ServiceName, State: StateDraining, DrainDeadline: &deadline,
		}},
		Operations: hub,
	})
	server.Start()
	defer server.Close()
	request, err := http.NewRequest(
		http.MethodGet,
		server.URL+"/api/v1/operations/"+operation.ID()+"/events",
		nil,
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer secret")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func newOperationHTTPTestServer(
	t *testing.T,
	hub *OperationHub,
) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = NewServer(ServerOptions{
		Token:        "secret",
		ExpectedHost: server.Listener.Addr().String(),
		Status: &testStatusProvider{status: Status{
			Service: ServiceName, State: StateReady,
		}},
		Shutdown: func(context.Context, ShutdownRequest) (Status, error) {
			return Status{Service: ServiceName, State: StateReady}, nil
		},
		Operations: hub,
	})
	server.Start()
	return server
}
