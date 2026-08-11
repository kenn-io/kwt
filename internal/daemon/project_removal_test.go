package daemon

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type fakeProjectRemover struct {
	result   kwt.ProjectRemovalResult
	err      error
	requests []kwt.ProjectRemovalRequest
}

func (f *fakeProjectRemover) RemoveProject(
	_ context.Context,
	request kwt.ProjectRemovalRequest,
) (kwt.ProjectRemovalResult, error) {
	f.requests = append(f.requests, request)
	return f.result, f.err
}

func TestProjectRemovalClientRoundTripsExactRequest(t *testing.T) {
	remover := &fakeProjectRemover{result: kwt.ProjectRemovalResult{Project: models.Project{
		Repository: "github.com/acme/widget", Name: "widget", Path: "/repo ",
	}}}
	client, closeServer := projectRemovalTestClient(t, remover)
	defer closeServer()
	request := kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		Expansion: kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home/user"},
	}

	result, err := client.RemoveProject(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, remover.result, result)
	require.Len(t, remover.requests, 1)
	assert.Equal(t, "/repo ", remover.requests[0].Path)
}

func TestProjectRemovalClientPreservesSafeLiveDetails(t *testing.T) {
	remover := &fakeProjectRemover{err: service.NewError(
		service.ProtectedSessionLive,
		"a protected project session is live",
		false,
		map[string]any{
			"session_name": "widget-pr-1", "socket_name": "kwt-pr-safe",
			"generation": "0123456789abcdef0123456789abcdef", "private": "drop-me",
		},
		nil,
	)}
	client, closeServer := projectRemovalTestClient(t, remover)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{})

	typed := service.AsError(err)
	assert.Equal(t, service.ProtectedSessionLive, typed.Code)
	assert.Equal(t, "widget-pr-1", typed.Details["session_name"])
	assert.NotContains(t, typed.Details, "private")
}

func projectRemovalTestClient(t *testing.T, remover kwt.ProjectRemover) (*Client, func()) {
	t.Helper()
	provider := &testStatusProvider{status: Status{State: StateReady}}
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = NewServer(ServerOptions{
		Token: "secret", ExpectedHost: server.Listener.Addr().String(), Status: provider,
		Shutdown:       func(context.Context, ShutdownRequest) (Status, error) { return provider.status, nil },
		ProjectRemover: remover, Gate: NewGate(time.Now()),
	})
	server.Start()
	endpoint, err := kitdaemon.ParseEndpoint(
		server.URL[len("http://"):],
		kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback},
	)
	require.NoError(t, err)
	return newClient(endpoint, "secret", server.Client()), server.Close
}
