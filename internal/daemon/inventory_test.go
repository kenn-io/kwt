package daemon

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kwt/service"
	publicworktree "go.kenn.io/kwt/worktree"
)

type fakeInventory struct {
	result   publicworktree.Result
	err      error
	requests []publicworktree.Request
}

func (f *fakeInventory) Query(_ context.Context, request publicworktree.Request) (publicworktree.Result, error) {
	f.requests = append(f.requests, request)
	return f.result, f.err
}

func (*fakeInventory) ApproveConfig(context.Context, publicworktree.ConfigApproval) error { return nil }

func TestInventoryClientRoundTripsResult(t *testing.T) {
	inventory := &fakeInventory{result: publicworktree.Result{Freshness: publicworktree.Fresh}}
	provider := &testStatusProvider{status: Status{State: StateReady}}
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = NewServer(ServerOptions{
		Token: "secret", ExpectedHost: server.Listener.Addr().String(), Status: provider,
		Shutdown:  func(context.Context, ShutdownRequest) (Status, error) { return provider.status, nil },
		Inventory: inventory, Gate: NewGate(time.Now()),
	})
	server.Start()
	defer server.Close()
	endpoint, err := kitdaemon.ParseEndpoint(server.URL[len("http://"):], kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	client := newClient(endpoint, "secret", server.Client())

	result, err := client.Inventory(context.Background(), publicworktree.Request{View: publicworktree.ViewProjects})
	require.NoError(t, err)
	assert.Equal(t, publicworktree.Fresh, result.Freshness)
	require.Len(t, inventory.requests, 1)
}

func TestInventoryClientPreservesTrustDetails(t *testing.T) {
	inventory := &fakeInventory{err: service.NewError(
		service.InteractionRequired, "trust required", false,
		map[string]any{"kind": "repository_config_trust", "path": "/repo/.kwt.toml", "digest": "abc"}, nil,
	)}
	provider := &testStatusProvider{status: Status{State: StateReady}}
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = NewServer(ServerOptions{
		Token: "secret", ExpectedHost: server.Listener.Addr().String(), Status: provider,
		Shutdown:  func(context.Context, ShutdownRequest) (Status, error) { return provider.status, nil },
		Inventory: inventory, Gate: NewGate(time.Now()),
	})
	server.Start()
	defer server.Close()
	endpoint, err := kitdaemon.ParseEndpoint(server.URL[len("http://"):], kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback})
	require.NoError(t, err)
	client := newClient(endpoint, "secret", server.Client())

	_, err = client.Inventory(context.Background(), publicworktree.Request{View: publicworktree.ViewProjects})
	typed := service.AsError(err)
	assert.Equal(t, service.InteractionRequired, typed.Code)
	assert.Equal(t, "/repo/.kwt.toml", typed.Details["path"])
}
