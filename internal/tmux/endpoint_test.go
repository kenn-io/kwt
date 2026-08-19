package tmux

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionEndpointContainsOnlyDurableAddressFields(t *testing.T) {
	typeOfEndpoint := reflect.TypeOf(SessionEndpoint{})
	got := make([]string, 0, typeOfEndpoint.NumField())
	for index := 0; index < typeOfEndpoint.NumField(); index++ {
		got = append(got, typeOfEndpoint.Field(index).Name)
	}

	assert.Equal(t, []string{"SessionName", "SocketName"}, got)
	for index := 0; index < typeOfEndpoint.NumField(); index++ {
		assert.Empty(t, typeOfEndpoint.Field(index).Tag.Get("json"))
	}
}

func TestSessionDeclaresAddressFieldsWithoutEmbeddingEndpoint(t *testing.T) {
	typeOfSession := reflect.TypeOf(Session{})
	_, embedded := typeOfSession.FieldByName("SessionEndpoint")
	assert.False(t, embedded)

	for _, name := range []string{"SessionName", "SocketName"} {
		field, found := typeOfSession.FieldByName(name)
		require.True(t, found)
		assert.Len(t, field.Index, 1)
	}
}

func TestCanonicalEndpointUsesKWTServer(t *testing.T) {
	endpoint := canonicalEndpoint("kwt-wt-widget-main-01234567")

	assert.Equal(t, "kwt-wt-widget-main-01234567", endpoint.SessionName)
	assert.Equal(t, "kwt", endpoint.SocketName)
}

func TestKillCommandHintUsesEndpointAddress(t *testing.T) {
	assert.Equal(t,
		"env -u TMUX_TMPDIR tmux -L kwt kill-session -t =workspace",
		KillCommandHint(SessionEndpoint{SessionName: "workspace", SocketName: KWTServerSocketName}),
	)
	assert.Equal(t,
		"env -u TMUX -u TMUX_PANE tmux kill-session -t =workspace",
		KillCommandHint(SessionEndpoint{SessionName: "workspace"}),
	)
}

func TestServerCommandsSeparateKWTAndDefaultEnvironmentPolicy(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/ambient/tmux")
	t.Setenv("TMUX", "/tmp/custom,42,0")
	t.Setenv("TMUX_PANE", "%7")
	t.Setenv("GHOSTHUB_AUTH", "secret")
	servers := newServerCommands(WorkspaceSessionsOptions{
		Command: "tmux", StripNames: []string{"GHOSTHUB_AUTH"},
	})

	kwtServer := servers.kwtServer().newCmd(
		context.Background(),
		[]string{"list-sessions"},
	)
	defaultServer := servers.defaultServer().newCmd(
		context.Background(),
		[]string{"list-sessions"},
	)

	assert.Equal(
		t,
		[]string{"tmux", "-L", KWTServerSocketName, "list-sessions"},
		kwtServer.Args,
	)
	assert.False(t, slices.Contains(kwtServer.Env, "TMUX_TMPDIR=/ambient/tmux"))
	assert.True(t, slices.Contains(defaultServer.Env, "TMUX_TMPDIR=/ambient/tmux"))
	assert.False(t, slices.Contains(defaultServer.Env, "TMUX=/tmp/custom,42,0"))
	assert.False(t, slices.Contains(defaultServer.Env, "TMUX_PANE=%7"))
	for _, command := range []*execCommandView{
		{env: kwtServer.Env},
		{env: defaultServer.Env},
	} {
		for _, entry := range command.env {
			assert.False(t, hasEnvName(entry, "GHOSTHUB_AUTH"))
		}
	}
}

func TestServerCommandsExplicitlyUnsetDefaultTempDir(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/ambient/tmux")
	servers := newServerCommands(WorkspaceSessionsOptions{
		Command:                 "tmux",
		DefaultServerTempDirSet: true,
	})

	command := servers.defaultServer().newCmd(
		context.Background(),
		[]string{"list-sessions"},
	)

	assert.False(t, slices.Contains(command.Env, "TMUX_TMPDIR=/ambient/tmux"))
}

func TestDefaultServerSwitchClientPreservesCurrentClientIdentity(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "/ambient/tmux")
	t.Setenv("TMUX", "/ambient/tmux/tmux-501/default,42,0")
	t.Setenv("TMUX_PANE", "%7")
	servers := newServerCommands(WorkspaceSessionsOptions{Command: "tmux"})

	command := servers.defaultServer().switchClientCmd("workspace")

	assert.True(t, slices.Contains(
		command.Env,
		"TMUX=/ambient/tmux/tmux-501/default,42,0",
	))
	assert.True(t, slices.Contains(command.Env, "TMUX_PANE=%7"))
	assert.True(t, slices.Contains(command.Env, "TMUX_TMPDIR=/ambient/tmux"))
}

func TestDefaultServerProbeStripsParentIdentityWithoutExtraNames(t *testing.T) {
	t.Setenv("TMUX", "/tmp/custom,42,0")
	t.Setenv("TMUX_PANE", "%7")

	command := newServerCommands(WorkspaceSessionsOptions{Command: "tmux"}).defaultServer().newCmd(
		context.Background(),
		[]string{"list-sessions"},
	)

	assert.False(t, slices.Contains(command.Env, "TMUX=/tmp/custom,42,0"))
	assert.False(t, slices.Contains(command.Env, "TMUX_PANE=%7"))
}

// execCommandView keeps the environment assertion above focused without
// teaching the test about exec.Cmd fields beyond the environment slice.
type execCommandView struct {
	env []string
}

func TestDefaultServerTempDirDoesNotMoveKWTServerSocket(t *testing.T) {
	servers := newServerCommands(WorkspaceSessionsOptions{
		Command: "tmux", DefaultServerTempDir: "/host/login/tmux",
	})

	assert.Empty(t, servers.kwtServer().socketTempDir)
	assert.Equal(t, "/host/login/tmux", servers.defaultServer().socketTempDir)
}

func TestServerCommandsTempDirsIsolateBothEndpoints(t *testing.T) {
	tempDir := t.TempDir()
	servers := newServerCommands(WorkspaceSessionsOptions{
		Command: "tmux", KWTServerTempDir: tempDir, DefaultServerTempDir: tempDir,
	})

	assert.Equal(t, KWTServerSocketName, servers.kwtServer().socketName)
	assert.Equal(t, tempDir, servers.kwtServer().socketTempDir)
	assert.Empty(t, servers.defaultServer().socketName)
	assert.Equal(t, tempDir, servers.defaultServer().socketTempDir)
}

func TestServerCommandsSelectOnlyOrdinaryServers(t *testing.T) {
	servers := newServerCommands(WorkspaceSessionsOptions{Command: "tmux"})

	kwtServer, err := servers.commandForEndpoint(canonicalEndpoint("workspace"))
	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, kwtServer.socketName)

	defaultServer, err := servers.commandForEndpoint(SessionEndpoint{
		SessionName: "workspace",
	})
	require.NoError(t, err)
	assert.Empty(t, defaultServer.socketName)

	_, err = servers.commandForEndpoint(SessionEndpoint{
		SessionName: "workspace",
		SocketName:  "kwt-pr-protected",
	})
	require.ErrorContains(t, err, "not an ordinary KWT tmux server")
}
