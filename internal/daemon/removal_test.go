package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
	for _, code := range []service.Code{
		service.Internal,
		service.Conflict,
		service.Busy,
		service.ConnectionChanged,
	} {
		t.Run(string(code), func(t *testing.T) {
			remover := &fakeRemover{
				result: kwt.RemovalResult{
					Path: "/worktrees/topic", Branch: "topic", WorktreeRemoved: true,
				},
				err: service.NewError(
					code,
					"worktree removed but cleanup failed",
					code == service.Busy,
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
			assert.True(t, service.IsCode(err, code))
			assert.True(t, git.WorktreeWasRemoved(err))
			assert.Equal(t, "/worktrees/topic", result.Path)
			assert.True(t, result.WorktreeRemoved)
			assert.True(t, result.RegistryUnregistered)
			assert.Equal(t, "topic", result.Branch)
		})
	}
}

func TestRemovalClientPreservesKnownRemovalFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		result  kwt.RemovalResult
	}{
		{
			name: "failed before removal", message: "worktree has uncommitted changes",
			result: kwt.RemovalResult{Path: "/worktrees/topic", Branch: "topic"},
		},
		{
			name: "partial completion", message: "worktree removed but failed to delete branch",
			result: kwt.RemovalResult{
				Path: "/worktrees/topic", Branch: "topic", WorktreeRemoved: true,
				RegistryUnregistered: true,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			remover := &fakeRemover{result: test.result, err: service.NewError(
				service.RemovalFailed,
				test.message,
				false,
				map[string]any{
					"path": test.result.Path, "branch": test.result.Branch,
					"worktree_removed":      test.result.WorktreeRemoved,
					"branch_deleted":        test.result.BranchDeleted,
					"registry_unregistered": test.result.RegistryUnregistered,
				},
				errors.New("private removal cause"),
			)}
			client, closeServer := removalTestClient(t, remover)
			defer closeServer()

			result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{})

			require.Error(t, err)
			typed := service.AsError(err)
			assert.Equal(t, service.RemovalFailed, typed.Code)
			assert.Equal(t, test.message, typed.Message)
			assert.Equal(t, test.result, result)
			assert.Equal(t, test.result.WorktreeRemoved, git.WorktreeWasRemoved(err))
		})
	}
}

func TestRemovalClientHidesUnexpectedInternalCause(t *testing.T) {
	const secret = "removal-password"
	cause := errors.New("fetch ssh://user:" + secret + "@example.invalid/repository")
	remover := &fakeRemover{err: service.NewError(
		service.Internal,
		cause.Error(),
		false,
		map[string]any{
			"path": "/worktrees/topic", "branch": "topic",
			"worktree_removed": true, "branch_deleted": false,
			"registry_unregistered": true,
		},
		cause,
	)}
	client, closeServer := removalTestClient(t, remover)
	defer closeServer()

	result, err := client.RemoveWorktree(context.Background(), kwt.RemovalRequest{})

	require.Error(t, err)
	typed := service.AsError(err)
	assert.Equal(t, service.Internal, typed.Code)
	assert.Equal(t, "internal failure", typed.Message)
	assert.NotContains(t, typed.Message, secret)
	assert.True(t, result.WorktreeRemoved)
	assert.True(t, result.RegistryUnregistered)
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
	assert.True(t, service.IsCode(err, service.DaemonTransportFailed))
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
	assert.True(t, service.IsCode(err, service.DaemonTransportFailed))
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
