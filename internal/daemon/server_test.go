package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

type testStatusProvider struct{ status Status }

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
