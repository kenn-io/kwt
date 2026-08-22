package tmux

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type realWorkspaceSessionsFixture struct {
	ctx        context.Context
	tempDir    string
	workspace  string
	session    string
	generation string
	servers    serverCommands
	sessions   *WorkspaceSessions
}

func newRealWorkspaceSessionsFixture(t *testing.T) *realWorkspaceSessionsFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tempDir, err := os.MkdirTemp("/tmp", "kwt-sessions-")
	require.NoError(t, err)
	workspace, err := os.MkdirTemp("/tmp", "kwt-workspace-")
	require.NoError(t, err)
	servers := newServerCommands(WorkspaceSessionsOptions{
		Command:              "tmux",
		KWTServerTempDir:     tempDir,
		DefaultServerTempDir: tempDir,
	})
	fixture := &realWorkspaceSessionsFixture{
		ctx:        context.Background(),
		tempDir:    tempDir,
		workspace:  workspace,
		session:    "kwt-wt-integration-main-01234567",
		generation: resolverTestGeneration,
		servers:    servers,
		sessions: NewWorkspaceSessions(WorkspaceSessionsOptions{
			Command:              "tmux",
			KWTServerTempDir:     tempDir,
			DefaultServerTempDir: tempDir,
		}),
	}
	t.Cleanup(func() {
		_ = fixture.servers.kwtServer().RunCommandContext(fixture.ctx, "kill-server")
		_ = fixture.servers.defaultServer().RunCommandContext(fixture.ctx, "kill-server")
		require.NoError(t, os.RemoveAll(workspace))
		require.NoError(t, os.RemoveAll(tempDir))
	})
	return fixture
}

func (f *realWorkspaceSessionsFixture) request() WorkspaceEndpointRequest {
	return WorkspaceEndpointRequest{
		SessionName:         f.session,
		WorkspacePath:       f.workspace,
		WorkspaceGeneration: f.generation,
	}
}

func (f *realWorkspaceSessionsFixture) createMatching(
	t *testing.T,
	command *TmuxCommand,
) {
	t.Helper()
	require.NoError(t, NewWorkspaceRunner(command, nil).EnsureWithGeneration(
		f.ctx,
		f.session,
		f.workspace,
		f.generation,
		BlankLayout(),
	))
}

func TestWorkspaceSessionsCreateOnlyOnKWTServer(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)

	got, err := fixture.sessions.EstablishWithGeneration(
		fixture.ctx,
		fixture.session,
		fixture.workspace,
		fixture.generation,
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, got.SocketName)
	assert.True(t, fixture.servers.kwtServer().HasSession(fixture.session))
	assert.False(t, fixture.servers.defaultServer().HasSession(fixture.session))
}

func TestWorkspaceSessionsKillSucceedsWhenCapturedSessionAlreadyExited(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.kwtServer())
	endpoint := SessionEndpoint{
		SessionName: fixture.session,
		SocketName:  KWTServerSocketName,
	}
	require.NoError(t, fixture.servers.kwtServer().KillSession(fixture.session))

	err := fixture.sessions.Kill(endpoint)

	require.NoError(t, err)
}

func TestWorkspaceSessionsKillMatchingLeavesReplacementGeneration(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.kwtServer())
	request := fixture.request()
	endpoints, err := fixture.sessions.LiveEndpoints(fixture.ctx, request)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)
	require.NoError(t, fixture.servers.kwtServer().KillSession(fixture.session))
	replacementGeneration := "fedcba9876543210fedcba9876543210"
	require.NoError(t, NewWorkspaceRunner(
		fixture.servers.kwtServer(), nil,
	).EnsureWithGeneration(
		fixture.ctx,
		fixture.session,
		fixture.workspace,
		replacementGeneration,
		BlankLayout(),
	))

	err = fixture.sessions.KillMatching(fixture.ctx, endpoints[0], request)

	require.Error(t, err)
	assert.ErrorContains(t, err, "different worktree generation")
	assert.True(t, fixture.servers.kwtServer().HasSession(fixture.session))
}

func TestWorkspaceSessionsKillMatchingTerminatesExactMatchingSession(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.session = `kwt-wt-topic;'"-01234567`
	fixture.createMatching(t, fixture.servers.kwtServer())
	request := fixture.request()
	endpoints, err := fixture.sessions.LiveEndpoints(fixture.ctx, request)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	err = fixture.sessions.KillMatching(fixture.ctx, endpoints[0], request)

	require.NoError(t, err)
	assert.False(t, fixture.servers.kwtServer().HasSession(fixture.session))
}

func TestWorkspaceSessionsAdoptVerifiedDefaultServerSession(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.defaultServer())

	got, err := fixture.sessions.EstablishWithGeneration(
		fixture.ctx,
		fixture.session,
		fixture.workspace,
		fixture.generation,
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Empty(t, got.SocketName)
	assert.True(t, fixture.servers.defaultServer().HasSession(fixture.session))
	assert.False(t, fixture.servers.kwtServer().HasSession(fixture.session))
}

func TestWorkspaceSessionsPreferKWTServerWhenBothAreValid(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.kwtServer())
	fixture.createMatching(t, fixture.servers.defaultServer())

	got, err := fixture.sessions.Resolve(fixture.ctx, fixture.request())

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, got.Endpoint.SocketName)
	assert.True(t, got.Live)
}

func TestResolveLiveRequiresExactlyOneVerifiedEndpoint(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.kwtServer())
	fixture.createMatching(t, fixture.servers.defaultServer())

	_, err := fixture.sessions.ResolveLive(fixture.ctx, fixture.request())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one live endpoint")
}

func TestWorkspaceSessionsEstablishOnKWTServerAfterAdoptedSessionExits(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.defaultServer())
	inventoryEndpoint, err := fixture.sessions.Resolve(fixture.ctx, fixture.request())
	require.NoError(t, err)
	require.Empty(t, inventoryEndpoint.Endpoint.SocketName)
	require.NoError(t, fixture.servers.defaultServer().KillSessionContext(
		fixture.ctx,
		fixture.session,
	))

	established, err := fixture.sessions.EstablishWithGeneration(
		fixture.ctx,
		fixture.session,
		fixture.workspace,
		fixture.generation,
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, established.SocketName)
	assert.True(t, fixture.servers.kwtServer().HasSession(fixture.session))
}

func TestWorkspaceSessionsRejectKWTServerMarkerMismatch(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.kwtServer())
	require.NoError(t, fixture.servers.kwtServer().RunCommandContext(
		fixture.ctx,
		"set-option",
		"-t",
		fixture.session,
		workspaceIdentityOption,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	))

	_, err := fixture.sessions.Resolve(fixture.ctx, fixture.request())

	var safety *SessionSafetyError
	require.ErrorAs(t, err, &safety)
	assert.Contains(t, safety.Error(), "different workspace identity")
}

func TestProtectedWorkspaceStillUsesUniqueSocket(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	socket := ProtectedWorkspaceSocketName(fixture.session, fixture.workspace)
	protected := NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux",
		socket,
		fixture.tempDir,
		nil,
	)
	t.Cleanup(func() {
		_ = protected.RunCommandContext(fixture.ctx, "kill-server")
	})

	require.NoError(t, NewProtectedWorkspaceRunner(protected, nil).Ensure(
		fixture.ctx,
		fixture.session,
		fixture.workspace,
		BlankLayout(),
	))
	assert.True(t, protected.HasSession(fixture.session))
	assert.False(t, fixture.servers.kwtServer().HasSession(fixture.session))
	assert.False(t, fixture.servers.defaultServer().HasSession(fixture.session))
}
