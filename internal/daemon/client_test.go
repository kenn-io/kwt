package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
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
			Status:        http.StatusServiceUnavailable,
			Code:          "daemon_draining",
			Detail:        "daemon is draining",
			Retryable:     true,
			DrainDeadline: &deadline,
		}))
	}))
	defer server.Close()

	client := clientForUnverifiedServer(t, server, "secret")
	_, err := client.Status(context.Background())
	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.Busy, typed.Code)
	assert.True(t, typed.Retryable)
	assert.Equal(t, deadline, typed.Details["drain_deadline"])
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
