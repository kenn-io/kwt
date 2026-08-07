package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, "invalid_request", problem.Code)
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
	var problem Problem
	require.NoError(t, json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(t, "daemon_draining", problem.Code)
	assert.True(t, problem.Retryable)
	require.NotNil(t, problem.DrainDeadline)
	assert.Equal(t, deadline, *problem.DrainDeadline)
}
