package daemon

import (
	"context"
	"encoding/json"
	"net/http"
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
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home/user"},
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

func TestProjectRemovalClientReconcilesLostSuccessfulResponse(t *testing.T) {
	expansion := kwt.ExpansionContext{
		WorkingDirectory: "/request/work", HomeDirectory: "/request/home",
		Environment: map[string]string{"PROJECT_ROOT": "/request/repo"},
	}
	var inventoryRequest kwt.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/remove":
			connection, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = connection.Close()
		case "/api/v1/inventory":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&inventoryRequest))
			require.NoError(t, json.NewEncoder(w).Encode(kwt.Result{
				Snapshot: kwt.Snapshot{Projects: nil}, Freshness: kwt.Fresh,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	request := kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            expansion,
	}

	result, err := client.RemoveProject(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, "/repo ", result.Project.Path)
	assert.Equal(t, "github.com/acme/widget", result.Project.Repository)
	assert.Equal(t, expansion, inventoryRequest.Expansion)
}

func TestProjectRemovalClientPreservesLostResponseWhenRegistrationRemains(t *testing.T) {
	client, closeServer := lostProjectRemovalClient(t, []kwt.Project{{
		Path: "/repo ", Repository: "github.com/acme/widget",
		RegistrationFingerprint: "v1:1111111111111111111111111111111111111111111111111111111111111111",
	}}, false)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home"},
	})

	assert.True(t, service.IsCode(err, service.DaemonTransportFailed))
	assert.False(t, RequiresRefresh(err))
}

func TestProjectRemovalClientReconcilesEquivalentRepositoryCase(t *testing.T) {
	client, closeServer := lostProjectRemovalClient(t, []kwt.Project{{
		Path: "/repo ", Repository: "github.com/Acme/Widget",
		RegistrationFingerprint: "v1:1111111111111111111111111111111111111111111111111111111111111111",
	}}, false)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home"},
	})

	assert.True(t, service.IsCode(err, service.DaemonTransportFailed))
	assert.False(t, service.IsCode(err, service.RegistrationChanged))
}

func TestProjectRemovalClientDistinguishesLocalIdentityTrailingWhitespace(t *testing.T) {
	client, closeServer := lostProjectRemovalClient(t, []kwt.Project{{
		Path: "/repo ", Repository: "local/repo",
		RegistrationFingerprint: "v1:1111111111111111111111111111111111111111111111111111111111111111",
	}}, false)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "local/repo ",
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home"},
	})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestProjectRemovalClientReportsReplacementAfterLostResponse(t *testing.T) {
	client, closeServer := lostProjectRemovalClient(t, []kwt.Project{{
		Path: "/repo ", Repository: "github.com/acme/replacement",
		RegistrationFingerprint: "v1:2222222222222222222222222222222222222222222222222222222222222222",
	}}, false)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home"},
	})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestProjectRemovalClientReportsSameRepositoryReplacementAfterLostResponse(t *testing.T) {
	client, closeServer := lostProjectRemovalClient(t, []kwt.Project{{
		Path: "/repo ", Repository: "github.com/acme/widget",
		RegistrationFingerprint: "v1:2222222222222222222222222222222222222222222222222222222222222222",
	}}, false)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home"},
	})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestProjectRemovalClientRequiresRefreshForAmbiguousReconciliation(t *testing.T) {
	project := kwt.Project{
		Path: "/repo ", Repository: "github.com/acme/widget",
		RegistrationFingerprint: "v1:1111111111111111111111111111111111111111111111111111111111111111",
	}
	client, closeServer := lostProjectRemovalClient(t, []kwt.Project{project, project}, false)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		ExpectedRegistration: project.RegistrationFingerprint,
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home"},
	})

	assert.True(t, service.IsCode(err, service.DaemonTransportFailed))
	assert.True(t, RequiresRefresh(err))
}

func TestProjectRemovalClientMarksUnreconciledResponseLossForRefresh(t *testing.T) {
	client, closeServer := lostProjectRemovalClient(t, nil, true)
	defer closeServer()

	_, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		ExpectedRegistration: "v1:1111111111111111111111111111111111111111111111111111111111111111",
		Expansion:            kwt.ExpansionContext{WorkingDirectory: "/work", HomeDirectory: "/home"},
	})

	assert.True(t, service.IsCode(err, service.DaemonTransportFailed))
	assert.True(t, RequiresRefresh(err))
}

func lostProjectRemovalClient(
	t *testing.T,
	projects []kwt.Project,
	failInventory bool,
) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/inventory" && !failInventory {
			require.NoError(t, json.NewEncoder(w).Encode(kwt.Result{
				Snapshot: kwt.Snapshot{Projects: projects}, Freshness: kwt.Fresh,
			}))
			return
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		require.NoError(t, err)
		_ = connection.Close()
	}))
	return clientForUnverifiedServer(t, server, "secret"), server.Close
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
