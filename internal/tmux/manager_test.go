package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSessionManagerCommand struct {
	detailed  []*SessionInfo
	listErr   error
	has       bool
	serverPID string
	calls     []string
}

func (r *recordingSessionManagerCommand) SetOptionContext(
	_ context.Context, sessionName, option string, value any,
) error {
	r.calls = append(r.calls, fmt.Sprintf("set-option:%s:%s:%v", sessionName, option, value))
	return nil
}

func (r *recordingSessionManagerCommand) RunCommandContext(
	_ context.Context,
	args ...string,
) error {
	r.calls = append(r.calls, "run-command:"+strings.Join(args, " "))
	return nil
}

func (r *recordingSessionManagerCommand) RunCommandOutputContext(
	_ context.Context,
	args ...string,
) (string, error) {
	r.calls = append(r.calls, "run-command-output:"+strings.Join(args, " "))
	if len(args) > 0 && args[0] == "new-session" {
		r.has = true
		return "%1\n", nil
	}
	if len(args) > 0 && args[0] == "show-options" {
		return "/bin/sh\n", nil
	}
	return "", nil
}

func (r *recordingSessionManagerCommand) GlobalEnvironmentContext(
	context.Context,
) (string, error) {
	r.calls = append(r.calls, "global-environment")
	return "", nil
}

func (r *recordingSessionManagerCommand) SessionEnvironmentContext(
	context.Context,
	string,
) (string, error) {
	r.calls = append(r.calls, "session-environment")
	return "", nil
}

func (r *recordingSessionManagerCommand) ListSessionsDetailed() ([]*SessionInfo, error) {
	r.calls = append(r.calls, "list-sessions")
	return r.detailed, r.listErr
}

func (r *recordingSessionManagerCommand) KillSession(sessionName string) error {
	r.calls = append(r.calls, "kill-session:"+sessionName)
	return nil
}

func (r *recordingSessionManagerCommand) HasSession(sessionName string) bool {
	r.calls = append(r.calls, "has-session:"+sessionName)
	return r.has
}

func (r *recordingSessionManagerCommand) AttachSession(sessionName string) error {
	r.calls = append(r.calls, "attach-session:"+sessionName)
	return nil
}

func (r *recordingSessionManagerCommand) SwitchClient(sessionName string) error {
	r.calls = append(r.calls, "switch-client:"+sessionName)
	return nil
}

func (r *recordingSessionManagerCommand) AttachSessionNested(
	_ context.Context, sessionName string,
) error {
	r.calls = append(r.calls, "nested-attach:"+sessionName)
	return nil
}

func (r *recordingSessionManagerCommand) ServerPIDContext(context.Context) (string, error) {
	r.calls = append(r.calls, "server-pid")
	return r.serverPID, nil
}

func TestSessionManagerCreatesOnlyOnKWTServer(t *testing.T) {
	kwtServer := &recordingSessionManagerCommand{}
	defaultServer := &recordingSessionManagerCommand{}
	manager := newSessionManagerWithCommands(DefaultSessionConfig(), kwtServer, defaultServer)

	session, err := manager.CreateSession(context.Background(), SessionOptions{
		Context: "run", Identifier: "test", WorkingDir: "/work",
	})

	require.NoError(t, err)
	assert.NotEmpty(t, kwtServer.calls)
	assert.Empty(t, defaultServer.calls)
	assert.Equal(t, KWTServerSocketName, session.SocketName)
}

func TestSessionManagerNormalizesDollarSignsInGeneratedSessionName(t *testing.T) {
	kwtServer := &recordingSessionManagerCommand{}
	manager := newSessionManagerWithCommands(
		DefaultSessionConfig(), kwtServer, &recordingSessionManagerCommand{},
	)

	session, err := manager.CreateSession(context.Background(), SessionOptions{
		Context: "run$local", Identifier: "build$target", WorkingDir: "/work",
	})

	require.NoError(t, err)
	assert.NotContains(t, session.SessionName, "$")
	assert.Contains(t, session.SessionName, "kwt-run-local-build-target-")
}

func TestSessionManagerRejectsWorkspaceNamespaceContexts(t *testing.T) {
	for _, sessionContext := range []string{
		"wt",
		"wt-build",
		"workspace",
		"workspace-review",
	} {
		t.Run(sessionContext, func(t *testing.T) {
			kwtServer := &recordingSessionManagerCommand{}
			manager := newSessionManagerWithCommands(
				DefaultSessionConfig(), kwtServer, &recordingSessionManagerCommand{},
			)

			_, err := manager.CreateSession(context.Background(), SessionOptions{
				Context: sessionContext, Identifier: "standalone", WorkingDir: "/work",
			})

			require.ErrorContains(t, err, "reserved for workspace sessions")
			assert.Empty(t, kwtServer.calls)
		})
	}
}

func TestSessionManagerUnionsKWTAndDefaultServerEndpoints(t *testing.T) {
	kwtServer := &recordingSessionManagerCommand{detailed: []*SessionInfo{{
		Name: "kwt-run-kwt-20260101120000",
	}}}
	defaultServer := &recordingSessionManagerCommand{detailed: []*SessionInfo{{
		Name: "kwt-run-default-20260101120000",
	}}}
	manager := newSessionManagerWithCommands(DefaultSessionConfig(), kwtServer, defaultServer)

	sessions, err := manager.ListSessions()

	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, KWTServerSocketName, sessions[0].SocketName)
	assert.Empty(t, sessions[1].SocketName)
}

func TestSessionManagerKeepsEqualNamesFromBothEndpoints(t *testing.T) {
	info := &SessionInfo{Name: "kwt-run-same-20260101120000"}
	manager := newSessionManagerWithCommands(
		DefaultSessionConfig(),
		&recordingSessionManagerCommand{detailed: []*SessionInfo{info}},
		&recordingSessionManagerCommand{detailed: []*SessionInfo{info}},
	)

	sessions, err := manager.ListSessions()

	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.NotEqual(t, sessions[0].SocketName, sessions[1].SocketName)
}

func TestSessionManagerFailsWhenDefaultServerInventoryIsIncomplete(t *testing.T) {
	manager := newSessionManagerWithCommands(
		DefaultSessionConfig(),
		&recordingSessionManagerCommand{},
		&recordingSessionManagerCommand{listErr: errors.New("permission denied")},
	)

	_, err := manager.ListSessions()

	require.ErrorContains(t, err, "permission denied")
}

func TestSessionManagerAttachAndKillUseSelectedEndpoint(t *testing.T) {
	kwtServer := &recordingSessionManagerCommand{has: true}
	defaultServer := &recordingSessionManagerCommand{has: true}
	manager := newSessionManagerWithCommands(DefaultSessionConfig(), kwtServer, defaultServer)
	manager.tmuxEnvironment = func() string { return "" }
	adoptedSession := &Session{
		SessionName: "adopted",
	}

	require.NoError(t, manager.AttachSessionDirect(adoptedSession))
	require.NoError(t, manager.KillSessionDirect(adoptedSession))
	assert.Contains(t, defaultServer.calls, "attach-session:adopted")
	assert.Contains(t, defaultServer.calls, "kill-session:adopted")
	assert.Empty(t, kwtServer.calls)
}

func TestSessionManagerStripsConfiguredCredentialFromEveryAttachPath(t *testing.T) {
	t.Setenv("CUSTOM_FLEET_TOKEN", "secret")
	manager := NewSessionManager(nil, "CUSTOM_FLEET_TOKEN")

	for endpoint, rawCommand := range map[string]sessionManagerCommand{
		"kwt":     manager.kwtServer,
		"default": manager.defaultServer,
	} {
		command, ok := rawCommand.(*TmuxCommand)
		require.True(t, ok)
		commands := map[string]*exec.Cmd{
			"direct": command.attachSessionCmd("workspace"),
			"nested": command.attachSessionNestedCmd(context.Background(), "workspace"),
			"switch": command.switchClientCmd("workspace"),
		}
		for path, attach := range commands {
			for _, entry := range attach.Env {
				assert.Falsef(
					t,
					hasEnvName(entry, "CUSTOM_FLEET_TOKEN"),
					"%s %s attach leaked configured credential: %v",
					endpoint,
					path,
					attach.Env,
				)
			}
		}
	}
}

func TestSessionManagerNestsAttachmentAcrossServers(t *testing.T) {
	canonical := &recordingSessionManagerCommand{has: true, serverPID: "43"}
	manager := newSessionManagerWithCommands(
		DefaultSessionConfig(), canonical, &recordingSessionManagerCommand{},
	)
	manager.tmuxEnvironment = func() string { return "/tmp/default,42,0" }

	err := manager.AttachSessionDirect(&Session{
		SessionName: "canonical", SocketName: KWTServerSocketName,
	})

	require.NoError(t, err)
	assert.Contains(t, canonical.calls, "nested-attach:canonical")
}

func TestParseWorkspaceSession(t *testing.T) {
	sm := NewSessionManager(nil)
	info := &SessionInfo{
		Name:           "kwt-wt-kwt-feature-foo-abcd1234",
		Created:        "1700000000",
		WorkingDir:     "/wt",
		CurrentCommand: "zsh",
	}
	got := sm.parseSessionFromTmux(info)
	require.NotNil(t, got)
	assert.Equal(t, "workspace", got.Context)
	assert.Equal(t, "kwt-feature-foo", got.Identifier)
	assert.Equal(t, info.Name, got.SessionName)
	assert.Equal(t, "/wt", got.WorkingDir)
	// StartTime comes from tmux's #{session_created}, parsed by parseCreated.
	assert.Equal(t, time.Unix(1700000000, 0), got.StartTime)
}

func TestParseDirWorkspaceSession(t *testing.T) {
	sm := NewSessionManager(nil)
	info := &SessionInfo{
		Name:    "kwt-workspace-dir-notes-abcd1234",
		Created: "1700000000",
	}

	got := sm.parseSessionFromTmux(info)

	require.NotNil(t, got)
	assert.Equal(t, "workspace", got.Context)
	assert.Equal(t, "dir-notes", got.Identifier)
}

func TestParseLegacySessionStillWorks(t *testing.T) {
	sm := NewSessionManager(nil)
	info := &SessionInfo{
		Name:    "kwt-review-pr123-20240101120000",
		Created: "1700000000",
	}
	got := sm.parseSessionFromTmux(info)
	require.NotNil(t, got)
	assert.Equal(t, "review", got.Context)
	assert.Equal(t, "pr123", got.Identifier)
}

func TestNonKwtSessionIgnored(t *testing.T) {
	sm := NewSessionManager(nil)
	assert.Nil(t, sm.parseSessionFromTmux(&SessionInfo{Name: "random-session"}))
}

func TestParseCreated(t *testing.T) {
	// Valid and whitespace-padded values parse to the exact unix second;
	// a negative value is still a valid (pre-epoch) time.
	assert.Equal(t, time.Unix(1700000000, 0), parseCreated("1700000000"))
	assert.Equal(t, time.Unix(1700000000, 0), parseCreated("  1700000000  "))
	assert.Equal(t, time.Unix(-1, 0), parseCreated("-1"))

	// Empty/garbage/overflow must not panic; they fall back to ~now.
	for _, bad := range []string{"", "garbage", "12.5", "99999999999999999999999999"} {
		assert.WithinDuration(t, time.Now(), parseCreated(bad), time.Minute,
			"parseCreated(%q) must fall back to now without panicking", bad)
	}
}
