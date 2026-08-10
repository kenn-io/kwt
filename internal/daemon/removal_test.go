package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/service"
)

type fakeRemover struct {
	result   kwt.RemovalResult
	err      error
	requests []kwt.RemovalRequest
}

func (f *fakeRemover) Remove(
	_ context.Context,
	request kwt.RemovalRequest,
) (kwt.RemovalResult, error) {
	f.requests = append(f.requests, request)
	return f.result, f.err
}

func TestRemovalClientRoundTripsResult(t *testing.T) {
	remover := &fakeRemover{result: kwt.RemovalResult{
		Path: "/worktrees/topic", Branch: "topic", WorktreeRemoved: true,
		BranchDeleted: true, RegistryUnregistered: true,
	}}
	client, closeServer := removalTestClient(t, remover)
	defer closeServer()

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{
		RepositoryPath: "/repos/widget", Path: "/worktrees/topic",
		ExpectedGeneration: "0123456789abcdef0123456789abcdef",
		DeleteBranch:       true,
	})

	require.NoError(t, err)
	assert.Equal(t, remover.result, result)
	require.Len(t, remover.requests, 1)
	assert.True(t, remover.requests[0].DeleteBranch)
}

func TestRemovalClientPreservesPartialResultOnError(t *testing.T) {
	remover := &fakeRemover{
		result: kwt.RemovalResult{
			Path: "/worktrees/topic", Branch: "topic", WorktreeRemoved: true,
		},
		err: service.NewError(
			service.Internal,
			"worktree removed but branch deletion failed",
			false,
			map[string]any{
				"path": "/worktrees/topic", "branch": "topic",
				"worktree_removed": true, "branch_deleted": false,
				"registry_unregistered": true,
			},
			nil,
		),
	}
	client, closeServer := removalTestClient(t, remover)
	defer closeServer()

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.Internal))
	assert.True(t, git.WorktreeWasRemoved(err))
	assert.True(t, result.WorktreeRemoved)
	assert.True(t, result.RegistryUnregistered)
	assert.Equal(t, "topic", result.Branch)
}

func TestRemovalClientReconcilesLostSuccessfulResponse(t *testing.T) {
	var reconciled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/worktrees/remove":
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack removal response: %v", err)
				return
			}
			_ = connection.Close()
		case "/api/v1/inventory":
			reconciled.Store(true)
			_ = json.NewEncoder(w).Encode(kwt.Result{Snapshot: kwt.Snapshot{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{
		RepositoryPath:     "/repos/widget",
		Path:               "/worktrees/topic",
		ExpectedGeneration: "0123456789abcdef0123456789abcdef",
	})

	require.Error(t, err)
	assert.True(t, reconciled.Load())
	assert.True(t, result.WorktreeRemoved)
	assert.True(t, git.WorktreeWasRemoved(err))
	assert.True(t, service.IsCode(err, service.TransportFailure))
}

func TestRemovalClientMarksUnreconciledResponseLossForRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack %s response: %v", r.URL.Path, err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{
		RepositoryPath:     "/repos/widget",
		Path:               "/worktrees/topic",
		ExpectedGeneration: "0123456789abcdef0123456789abcdef",
	})

	require.Error(t, err)
	assert.False(t, result.WorktreeRemoved)
	assert.False(t, git.WorktreeWasRemoved(err))
	assert.True(t, RequiresRefresh(err))
	assert.True(t, service.IsCode(err, service.TransportFailure))
}

func removalTestClient(t *testing.T, remover kwt.Remover) (*Client, func()) {
	t.Helper()
	provider := &testStatusProvider{status: Status{State: StateReady}}
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = NewServer(ServerOptions{
		Token: "secret", ExpectedHost: server.Listener.Addr().String(), Status: provider,
		Shutdown: func(context.Context, ShutdownRequest) (Status, error) { return provider.status, nil },
		Remover:  remover, Gate: NewGate(time.Now()),
	})
	server.Start()
	endpoint, err := kitdaemon.ParseEndpoint(
		server.URL[len("http://"):],
		kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback},
	)
	require.NoError(t, err)
	return newClient(endpoint, "secret", server.Client()), server.Close
}
