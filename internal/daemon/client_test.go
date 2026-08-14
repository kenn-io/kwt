package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

func runtimeRecordForServer(
	t *testing.T,
	server *httptest.Server,
	_ string,
) kitdaemon.RuntimeRecord {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	return kitdaemon.NewRuntimeRecord(ServiceName, "v1", kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP,
		Address: parsed.Host,
	})
}

func clientForUnverifiedServer(
	t *testing.T,
	server *httptest.Server,
	token string,
) *Client {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: parsed.Host}
	return newClient(ep, token, server.Client())
}

func TestNewVerifiedClientProvesEndpointBeforeBearerUse(t *testing.T) {
	token := "test-secret"
	var authorizationSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authorizationSeen = true
		}
		_, _ = w.Write([]byte(`{"ok":true,"service":"kwt","version":"v1","pid":` +
			fmt.Sprint(os.Getpid()) + `}`))
	}))
	defer server.Close()
	rec := runtimeRecordForServer(t, server, token)

	_, err := NewVerifiedClient(context.Background(), rec, token)
	require.Error(t, err, "server did not return a valid proof")
	assert.False(t, authorizationSeen)
}

func TestClientDecodesDrainingProblem(t *testing.T) {
	deadline := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		require.NoError(t, json.NewEncoder(w).Encode(Problem{
			Status: http.StatusServiceUnavailable,
			Detail: "daemon is draining",
			Descriptor: service.Descriptor{
				Code: service.DaemonDraining, Message: "daemon is draining", Retryable: true,
				Details: map[string]any{"drain_deadline": deadline.Format(time.RFC3339)},
			},
		}))
	}))
	defer server.Close()

	client := clientForUnverifiedServer(t, server, "secret")
	_, err := client.Status(context.Background())
	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.DaemonDraining, typed.Code)
	assert.True(t, typed.Retryable)
	assert.Equal(t, deadline.Format(time.RFC3339), typed.Details["drain_deadline"])
}

func TestClientDecodesLegacyDeadlineBearingBusyAsDrain(t *testing.T) {
	deadline := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"type": "https://kwt.dev/problems/busy", "title": "Service Unavailable",
			"status": http.StatusServiceUnavailable, "detail": "daemon is draining",
			"code": service.Busy, "retryable": true, "drain_deadline": deadline,
		}))
	}))
	defer server.Close()

	client := clientForUnverifiedServer(t, server, "secret")
	_, err := client.Status(context.Background())
	typed := service.AsError(err)
	assert.Equal(t, service.Busy, typed.Code)
	assert.Equal(t, deadline, typed.Details["drain_deadline"])
}

func TestClientRejectsUnknownProblemCodeWithoutStatusInference(t *testing.T) {
	err := decodeProblem(http.StatusServiceUnavailable, strings.NewReader(
		`{"type":"about:blank","title":"Unavailable","status":503,"detail":"future failure","code":"future_code","message":"future failure","retryable":true}`,
	))

	typed := service.AsError(err)
	assert.Equal(t, service.DaemonTransportFailed, typed.Code)
	assert.NotEqual(t, service.Busy, typed.Code)
}

func TestClientNormalizesLegacyInternalProblemMessage(t *testing.T) {
	const secret = "legacy-daemon-password"
	err := decodeProblem(http.StatusInternalServerError, strings.NewReader(
		`{"type":"about:blank","title":"Internal Server Error","status":500,"detail":"fetch ssh://user:`+
			secret+`@example.invalid/repository","code":"internal","retryable":false}`,
	))

	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.Internal, typed.Code)
	assert.Equal(t, "internal failure", typed.Message)
	assert.NotContains(t, typed.Message, secret)
}

func TestVerifiedClientUsesBearerAfterSuccessfulProof(t *testing.T) {
	token := "test-secret"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	rec := runtimeRecordForServer(t, server, token)
	proof, err := kitdaemon.NewProof([]byte(token))
	require.NoError(t, err)
	ping, err := proof.NewPingHandler(rec)
	require.NoError(t, err)
	mux.Handle("/api/ping", ping)
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(Status{
			Service:       ServiceName,
			State:         StateReady,
			PID:           os.Getpid(),
			Version:       "v1",
			SchemaMajor:   APISchemaMajor,
			SchemaVersion: APISchemaVersion,
		})
	})

	client, err := NewVerifiedClient(context.Background(), rec, token)
	require.NoError(t, err)
	status, err := client.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, APISchemaMajor, status.SchemaMajor)
}

func TestVerifiedClientAllowsInventoryDiscoveryBeforeHeaders(t *testing.T) {
	token := "test-secret"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	rec := runtimeRecordForServer(t, server, token)
	proof, err := kitdaemon.NewProof([]byte(token))
	require.NoError(t, err)
	ping, err := proof.NewPingHandler(rec)
	require.NoError(t, err)
	mux.Handle("/api/ping", ping)
	mux.HandleFunc("/api/v1/inventory", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"freshness": "fresh"})
	})

	client, err := NewVerifiedClient(context.Background(), rec, token)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.Inventory(ctx, kwt.Request{View: kwt.ViewDashboard})

	require.NoError(t, err)
}

func TestInventoryTransportOutlivesServerRefresh(t *testing.T) {
	assert.Greater(t, inventoryRequestTimeout, kwt.DefaultRefreshTimeout)
}

func TestVerifiedClientBoundsControlPlaneResponseWait(t *testing.T) {
	token := "test-secret"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	rec := runtimeRecordForServer(t, server, token)
	proof, err := kitdaemon.NewProof([]byte(token))
	require.NoError(t, err)
	ping, err := proof.NewPingHandler(rec)
	require.NoError(t, err)
	mux.Handle("/api/ping", ping)
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2500 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(Status{State: StateReady})
	})

	client, err := NewVerifiedClient(context.Background(), rec, token)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err = client.Status(ctx)

	require.Error(t, err)
	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.DaemonTransportFailed, typed.Code)
}

func TestClientAllowsInventoryResponseLargerThanControlPlaneLimit(t *testing.T) {
	largePath := strings.Repeat("x", 2<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(kwt.Result{
			Notes: []kwt.Note{{Code: "large", Path: largePath}},
		}))
	}))
	defer server.Close()

	client := clientForUnverifiedServer(t, server, "secret")
	result, err := client.Inventory(context.Background(), kwt.Request{
		View: kwt.ViewDashboard,
	})

	require.NoError(t, err)
	require.Len(t, result.Notes, 1)
	assert.Equal(t, largePath, result.Notes[0].Path)
}

func TestClientReportsEndpointSpecificResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("response exceeds limit"))
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	err := client.doWith(
		context.Background(),
		client.inventoryHTTP,
		8,
		http.MethodGet,
		"/api/v1/inventory",
		nil,
		nil,
	)

	require.ErrorIs(t, err, ErrResponseTooLarge)
	assert.ErrorContains(t, err, "/api/v1/inventory exceeds 8 bytes")
}

func TestUnguardedRemovalContinuesWhenExpansionCaptureFails(t *testing.T) {
	original := captureRemovalExpansionContext
	captureRemovalExpansionContext = func() (kwt.ExpansionContext, error) {
		return kwt.ExpansionContext{}, fmt.Errorf("expansion unavailable")
	}
	t.Cleanup(func() { captureRemovalExpansionContext = original })
	var received kwt.RemovalRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		require.NoError(t, json.NewEncoder(w).Encode(kwt.RemovalResult{
			Path: received.Path, WorktreeRemoved: true,
		}))
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{
		RepositoryPath: "/repo", Path: "/repo/topic",
		ExpectedGeneration: "0123456789abcdef0123456789abcdef",
	})

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
	assert.Empty(t, received.Expansion.Environment)
}

func TestVersionOneUnguardedRemovalPreservesLegacyPayload(t *testing.T) {
	original := captureRemovalExpansionContext
	captureRemovalExpansionContext = func() (kwt.ExpansionContext, error) {
		return kwt.ExpansionContext{
			WorkingDirectory: "/repo/topic",
			HomeDirectory:    "/home/tester",
			Environment:      map[string]string{"FLEET_TOKEN": "secret"},
		}, nil
	}
	t.Cleanup(func() { captureRemovalExpansionContext = original })
	type legacyRemovalRequest struct {
		RepositoryPath     string `json:"repository_path"`
		Path               string `json:"path"`
		ExpectedGeneration string `json:"expected_generation"`
		Force              bool   `json:"force,omitempty"`
		DeleteBranch       bool   `json:"delete_branch,omitempty"`
		ForceDeleteBranch  bool   `json:"force_delete_branch,omitempty"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var received legacyRemovalRequest
		require.NoError(t, decoder.Decode(&received))
		assert.Equal(t, "/repo/topic", received.Path)
		require.NoError(t, json.NewEncoder(w).Encode(kwt.RemovalResult{
			Path: received.Path, WorktreeRemoved: true,
		}))
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{
		RepositoryPath: "/repo", Path: "/repo/topic",
		ExpectedGeneration: "0123456789abcdef0123456789abcdef",
	})

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
}

func TestVersionTwoUnguardedRemovalIncludesExpansionContext(t *testing.T) {
	original := captureRemovalExpansionContext
	captureRemovalExpansionContext = func() (kwt.ExpansionContext, error) {
		return kwt.ExpansionContext{
			WorkingDirectory: "/repo/topic",
			HomeDirectory:    "/home/tester",
			Environment:      map[string]string{"FLEET_TOKEN": "secret"},
		}, nil
	}
	t.Cleanup(func() { captureRemovalExpansionContext = original })
	var received kwt.RemovalRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		require.NoError(t, json.NewEncoder(w).Encode(kwt.RemovalResult{
			Path: received.Path, WorktreeRemoved: true,
		}))
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	client.capabilities = []string{CapabilityRemoval, CapabilityGuardedRemoval}

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{
		RepositoryPath: "/repo", Path: "/repo/topic",
		ExpectedGeneration: "0123456789abcdef0123456789abcdef",
	})

	require.NoError(t, err)
	assert.True(t, result.WorktreeRemoved)
	assert.Equal(t, "/repo/topic", received.Expansion.WorkingDirectory)
	assert.Equal(t, "secret", received.Expansion.Environment["FLEET_TOKEN"])
}

func TestGuardedRemovalRequiresExpansionCapture(t *testing.T) {
	original := captureRemovalExpansionContext
	captureRemovalExpansionContext = func() (kwt.ExpansionContext, error) {
		return kwt.ExpansionContext{}, fmt.Errorf("expansion unavailable")
	}
	t.Cleanup(func() { captureRemovalExpansionContext = original })
	client := &Client{}

	_, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{
		Session: &kwt.RemovalSessionCondition{SessionName: "topic", Absent: true},
	})

	assert.ErrorContains(t, err, "expansion unavailable")
}
