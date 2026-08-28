package tmux

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/template"
)

const resolverTestGeneration = "0123456789abcdef0123456789abcdef"

type fakeEndpointCommand struct {
	sessions           []string
	listErr            error
	options            map[string]string
	optionsBySession   map[string]map[string]string
	optionErrors       map[string]error
	serverPID          string
	serverPIDErr       error
	requireEnumeration bool
	listed             bool
	calls              []string
}

func (f *fakeEndpointCommand) ListSessionsContext(context.Context) ([]string, error) {
	f.calls = append(f.calls, "list-sessions")
	f.listed = true
	return append([]string(nil), f.sessions...), f.listErr
}

func (f *fakeEndpointCommand) SessionUserOptionContext(
	_ context.Context,
	session string,
	option string,
) (string, error) {
	if f.requireEnumeration && !f.listed {
		panic("session marker inspected before list-sessions")
	}
	f.calls = append(f.calls, option)
	if err := f.optionErrors[option]; err != nil {
		return "", err
	}
	if options := f.optionsBySession[session]; options != nil {
		return options[option], nil
	}
	return f.options[option], nil
}

func (f *fakeEndpointCommand) ServerPIDContext(context.Context) (string, error) {
	f.calls = append(f.calls, "display-message -p #{pid}")
	return f.serverPID, f.serverPIDErr
}

func (f *fakeEndpointCommand) AttachSession(session string) error {
	f.calls = append(f.calls, "attach-session "+session)
	return nil
}

func (f *fakeEndpointCommand) SwitchClient(session string) error {
	f.calls = append(f.calls, "switch-client "+session)
	return nil
}

func (f *fakeEndpointCommand) AttachSessionNested(_ context.Context, session string) error {
	f.calls = append(f.calls, "nested-attach "+session)
	return nil
}

func newResolverTestCommands(path, session string) (*fakeEndpointCommand, *fakeEndpointCommand) {
	matching := map[string]string{
		workspaceIdentityOption:   workspacePathIdentity(path),
		workspaceGenerationOption: resolverTestGeneration,
	}
	return &fakeEndpointCommand{
		options:      cloneStringMap(matching),
		optionErrors: make(map[string]error),
	}, &fakeEndpointCommand{
		options:            cloneStringMap(matching),
		optionErrors:       make(map[string]error),
		requireEnumeration: true,
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func resolverTestRequest() WorkspaceEndpointRequest {
	return WorkspaceEndpointRequest{
		SessionName:         "kwt-wt-widget-main-01234567",
		WorkspacePath:       "/work/widget",
		WorkspaceGeneration: resolverTestGeneration,
	}
}

func TestResolverPrefersMatchingCanonicalWhenBothEndpointsAreLive(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	canonical.sessions = []string{request.SessionName}
	defaultServer.sessions = []string{request.SessionName}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolve(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, got.Endpoint.SocketName)
	assert.True(t, got.Live)
	assert.Empty(t, defaultServer.calls, "canonical state must win before defaultServer inspection")
}

func TestResolverAdoptsPreNormalizationDollarSignSessionName(t *testing.T) {
	request := resolverTestRequest()
	suffix := "-" + template.ShortHash(request.WorkspacePath)
	request.SessionName = "kwt-wt-widget-tools-main-home" + suffix
	legacyName := "kwt-wt-widget$tools-main$home" + suffix

	for _, test := range []struct {
		name       string
		canonical  []string
		adopted    []string
		wantSocket string
	}{
		{
			name:       "canonical server",
			canonical:  []string{legacyName},
			wantSocket: KWTServerSocketName,
		},
		{
			name:    "default server",
			adopted: []string{legacyName},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			canonical, defaultServer := newResolverTestCommands(
				request.WorkspacePath, request.SessionName,
			)
			canonical.sessions = test.canonical
			defaultServer.sessions = test.adopted
			resolver := newEndpointResolver(canonical, defaultServer, nil)

			got, err := resolver.resolve(context.Background(), request)

			require.NoError(t, err)
			assert.True(t, got.Live)
			assert.Equal(t, legacyName, got.Endpoint.SessionName)
			assert.Equal(t, test.wantSocket, got.Endpoint.SocketName)
		})
	}
}

func TestResolverReturnsEveryMatchingLiveEndpoint(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	canonical.sessions = []string{request.SessionName}
	defaultServer.sessions = []string{request.SessionName}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.liveEndpoints(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, []SessionEndpoint{
		{SessionName: request.SessionName, SocketName: KWTServerSocketName},
		{SessionName: request.SessionName},
	}, got)
	assert.Equal(t, []string{
		"list-sessions",
		workspaceGenerationOption,
	}, canonical.calls)
	assert.Equal(t, []string{
		"list-sessions",
		workspaceGenerationOption,
	}, defaultServer.calls)
}

func TestResolverReturnsRenamedLiveEndpointsByWorkspaceMarkers(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	canonicalRenamed := "kwt-wt-widget-renamed-aaaaaaaa"
	defaultRenamed := "kwt-wt-widget-previous-bbbbbbbb"
	unrelated := "kwt-wt-other-main-cccccccc"
	matching := map[string]string{
		workspaceIdentityOption:   workspacePathIdentity(request.WorkspacePath),
		workspaceGenerationOption: request.WorkspaceGeneration,
	}
	canonical.sessions = []string{unrelated, canonicalRenamed}
	canonical.optionsBySession = map[string]map[string]string{
		unrelated: {
			workspaceIdentityOption:   workspacePathIdentity("/work/other"),
			workspaceGenerationOption: "fedcba9876543210fedcba9876543210",
		},
		canonicalRenamed: cloneStringMap(matching),
	}
	defaultServer.sessions = []string{defaultRenamed}
	defaultServer.optionsBySession = map[string]map[string]string{
		defaultRenamed: cloneStringMap(matching),
	}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.liveEndpoints(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, []SessionEndpoint{
		{SessionName: canonicalRenamed, SocketName: KWTServerSocketName},
		{SessionName: defaultRenamed},
	}, got)
}

func TestResolverReturnsMovedWorktreeEndpointByGeneration(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	oldSessionName := "kwt-wt-widget-old-path-aaaaaaaa"
	canonical.sessions = []string{oldSessionName}
	canonical.optionsBySession = map[string]map[string]string{
		oldSessionName: {
			workspaceIdentityOption:   workspacePathIdentity("/work/widget-before-move"),
			workspaceGenerationOption: request.WorkspaceGeneration,
		},
	}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.liveEndpoints(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, []SessionEndpoint{
		{SessionName: oldSessionName, SocketName: KWTServerSocketName},
	}, got)
}

func TestResolverTreatsGeneratedLegacyDirPrefixAsWorktree(t *testing.T) {
	request := resolverTestRequest()
	request.SessionName = "kwt-workspace-dir-owner-widget-topic-01234567"
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	canonicalSession := "kwt-wt-widget-topic-89abcdef"
	canonical.sessions = []string{canonicalSession}
	canonical.optionsBySession = map[string]map[string]string{
		canonicalSession: {
			workspaceIdentityOption:   workspacePathIdentity(request.WorkspacePath),
			workspaceGenerationOption: request.WorkspaceGeneration,
		},
	}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.liveEndpoints(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, []SessionEndpoint{
		{SessionName: canonicalSession, SocketName: KWTServerSocketName},
	}, got)
}

func TestResolverReusesMatchingEnumeratedDefaultServerSession(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	defaultServer.sessions = []string{request.SessionName}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolve(context.Background(), request)

	require.NoError(t, err)
	assert.Empty(t, got.Endpoint.SocketName)
	assert.True(t, got.Live)
	assert.Equal(t, []string{
		"list-sessions",
		workspaceIdentityOption,
		workspaceGenerationOption,
	}, defaultServer.calls)
}

func TestResolverIgnoresMismatchedDefaultServerMarkers(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	defaultServer.sessions = []string{request.SessionName}
	defaultServer.options[workspaceIdentityOption] = workspacePathIdentity("/work/other")
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolve(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, got.Endpoint.SocketName)
	assert.False(t, got.Live)
}

func TestResolverIgnoresUnmarkedDefaultServerCandidate(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	defaultServer.sessions = []string{request.SessionName}
	defaultServer.options[workspaceIdentityOption] = ""
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolve(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, got.Endpoint.SocketName)
	assert.False(t, got.Live)
}

func TestResolverRejectsMismatchedCanonicalMarkers(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	canonical.sessions = []string{request.SessionName}
	canonical.options[workspaceGenerationOption] = "fedcba9876543210fedcba9876543210"
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	_, err := resolver.resolve(context.Background(), request)

	var safety *SessionSafetyError
	require.ErrorAs(t, err, &safety)
	assert.Contains(t, safety.Reason, "different worktree generation")
}

func TestResolverFailsClosedWhenEnumeratedDefaultServerCandidateCannotBeRead(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	defaultServer.sessions = []string{request.SessionName}
	defaultServer.optionErrors[workspaceIdentityOption] = errors.New("permission denied")
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	_, err := resolver.resolve(context.Background(), request)

	require.ErrorContains(t, err, "inspect adopted default-server workspace")
	require.ErrorContains(t, err, "permission denied")
}

func TestResolverFailsOpenWhenDefaultServerEnumerationFailsBeforeDiscovery(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	defaultServer.listErr = errors.New("default server wedged")
	var diagnostics []error
	resolver := newEndpointResolver(canonical, defaultServer, func(err error) {
		diagnostics = append(diagnostics, err)
	})

	got, err := resolver.resolve(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, got.Endpoint.SocketName)
	assert.False(t, got.Live)
	require.Len(t, diagnostics, 1)
	assert.ErrorContains(t, diagnostics[0], "default-server adoption lookup degraded")
	assert.Equal(t, []string{"list-sessions"}, defaultServer.calls)
}

func TestResolverRejectsMalformedDefaultServerMarkerAfterDiscovery(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	defaultServer.sessions = []string{request.SessionName}
	defaultServer.options[workspaceIdentityOption] = "not-a-workspace-identity"
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	_, err := resolver.resolve(context.Background(), request)

	require.ErrorContains(t, err, "malformed workspace identity marker")
}

func TestResolverDirectoryWorkspaceRequiresOnlyPathIdentity(t *testing.T) {
	request := resolverTestRequest()
	request.WorkspaceGeneration = ""
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	defaultServer.sessions = []string{request.SessionName}
	delete(defaultServer.options, workspaceGenerationOption)
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolve(context.Background(), request)

	require.NoError(t, err)
	assert.Empty(t, got.Endpoint.SocketName)
	assert.Equal(t, []string{"list-sessions", workspaceIdentityOption}, defaultServer.calls)
}

func TestResolverFindsRenamedDirectoryWorkspaceByVerifiedPath(t *testing.T) {
	path := "/work/notes"
	request := WorkspaceEndpointRequest{
		SessionName:   DirWorkspaceSessionName("renamed", path),
		WorkspacePath: path,
	}
	canonical, defaultServer := newResolverTestCommands(path, request.SessionName)
	previousName := DirWorkspaceSessionName("previous", path)
	canonical.sessions = []string{previousName}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolve(context.Background(), request)

	require.NoError(t, err)
	assert.True(t, got.Live)
	assert.Equal(t, previousName, got.Endpoint.SessionName)
	assert.Equal(t, KWTServerSocketName, got.Endpoint.SocketName)
}

func TestResolverBatchEnumeratesEachEndpointOnce(t *testing.T) {
	canonical := &fakeEndpointCommand{
		options: make(map[string]string), optionErrors: make(map[string]error),
	}
	defaultServer := &fakeEndpointCommand{
		options: make(map[string]string), optionErrors: make(map[string]error),
	}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolveAll(context.Background(), []WorkspaceEndpointRequest{
		{SessionName: "one", WorkspacePath: "/work/one"},
		{SessionName: "two", WorkspacePath: "/work/two"},
	})

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, KWTServerSocketName, got[0].Endpoint.SocketName)
	assert.Equal(t, KWTServerSocketName, got[1].Endpoint.SocketName)
	assert.Equal(t, []string{"list-sessions"}, canonical.calls)
	assert.Equal(t, []string{"list-sessions"}, defaultServer.calls)
}

func TestResolverBatchPreservesLiveSessionNameAfterBranchChange(t *testing.T) {
	request := resolverTestRequest()
	suffix := "-" + template.ShortHash(request.WorkspacePath)
	request.SessionName = "kwt-wt-widget-feature-b" + suffix
	originalName := "kwt-wt-widget-feature-a" + suffix
	canonical, defaultServer := newResolverTestCommands(
		request.WorkspacePath,
		request.SessionName,
	)
	canonical.sessions = []string{originalName}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolveAllBestEffort(
		context.Background(),
		[]WorkspaceEndpointRequest{request},
	)

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NoError(t, got[0].Err)
	assert.Equal(t, canonicalEndpoint(originalName), got[0].Session.Endpoint)
	assert.True(t, got[0].Session.Live)
}

func TestResolverBatchKeepsResolvingAfterWorkspaceSafetyError(t *testing.T) {
	first := WorkspaceEndpointRequest{
		SessionName:         "kwt-wt-widget-main-01234567",
		WorkspacePath:       "/work/widget",
		WorkspaceGeneration: resolverTestGeneration,
	}
	second := WorkspaceEndpointRequest{
		SessionName:         "kwt-wt-gadget-main-89abcdef",
		WorkspacePath:       "/work/gadget",
		WorkspaceGeneration: resolverTestGeneration,
	}
	canonical, defaultServer := newResolverTestCommands(first.WorkspacePath, first.SessionName)
	canonical.sessions = []string{first.SessionName, second.SessionName}
	canonical.optionsBySession = map[string]map[string]string{
		first.SessionName: {
			workspaceIdentityOption:   workspacePathIdentity(first.WorkspacePath),
			workspaceGenerationOption: "fedcba9876543210fedcba9876543210",
		},
		second.SessionName: {
			workspaceIdentityOption:   workspacePathIdentity(second.WorkspacePath),
			workspaceGenerationOption: second.WorkspaceGeneration,
		},
	}
	resolver := newEndpointResolver(canonical, defaultServer, nil)

	got, err := resolver.resolveAllBestEffort(
		context.Background(),
		[]WorkspaceEndpointRequest{first, second},
	)

	require.NoError(t, err)
	require.Len(t, got, 2)
	var safetyErr *SessionSafetyError
	require.ErrorAs(t, got[0].Err, &safetyErr)
	assert.Equal(t, canonicalEndpoint(first.SessionName), got[0].Session.Endpoint)
	assert.False(t, got[0].Session.Live)
	require.NoError(t, got[1].Err)
	assert.Equal(t, canonicalEndpoint(second.SessionName), got[1].Session.Endpoint)
	assert.True(t, got[1].Session.Live)
	assert.Equal(t, 1, countCall(canonical.calls, "list-sessions"))
	assert.Equal(t, 1, countCall(defaultServer.calls, "list-sessions"))
}

func TestResolverBestEffortPreservesCancellation(t *testing.T) {
	request := resolverTestRequest()
	tests := []struct {
		name      string
		configure func(*fakeEndpointCommand, *fakeEndpointCommand)
	}{
		{
			name: "default server enumeration",
			configure: func(_ *fakeEndpointCommand, defaultServer *fakeEndpointCommand) {
				defaultServer.listErr = context.Canceled
			},
		},
		{
			name: "canonical marker inspection",
			configure: func(canonical *fakeEndpointCommand, _ *fakeEndpointCommand) {
				canonical.sessions = []string{request.SessionName}
				canonical.optionErrors[workspaceGenerationOption] = context.DeadlineExceeded
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			canonical, defaultServer := newResolverTestCommands(
				request.WorkspacePath, request.SessionName,
			)
			test.configure(canonical, defaultServer)
			resolver := newEndpointResolver(canonical, defaultServer, nil)

			_, err := resolver.resolveAllBestEffort(
				context.Background(), []WorkspaceEndpointRequest{request},
			)

			require.Error(t, err)
			assert.True(t,
				errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded),
			)
		})
	}
}

func TestResolverBestEffortChecksCanceledContextBeforeReturning(t *testing.T) {
	request := resolverTestRequest()
	canonical, defaultServer := newResolverTestCommands(request.WorkspacePath, request.SessionName)
	ctx, cancel := context.WithCancel(context.Background())
	defaultServer.listErr = errors.New("default server unavailable")
	resolver := newEndpointResolver(canonical, defaultServer, func(error) { cancel() })

	_, err := resolver.resolveAllBestEffort(ctx, []WorkspaceEndpointRequest{request})

	require.ErrorIs(t, err, context.Canceled)
}

func countCall(calls []string, expected string) int {
	count := 0
	for _, call := range calls {
		if call == expected {
			count++
		}
	}
	return count
}

func TestParseTMUXServerPIDUsesRightmostFields(t *testing.T) {
	pid, err := parseTMUXServerPID("/tmp/a,b/default,42,0")

	require.NoError(t, err)
	assert.Equal(t, uint64(42), pid)
}

func TestParseTMUXServerPIDRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{
		"",
		"/tmp/default,42",
		"/tmp/default,nope,0",
		"/tmp/default,042,0",
		",42,0",
		"/tmp/default,42,",
	} {
		t.Run(fmt.Sprintf("value_%q", value), func(t *testing.T) {
			_, err := parseTMUXServerPID(value)
			require.Error(t, err)
		})
	}
}

func TestAttachWorkspaceEndpointSwitchesOnMatchingServerPID(t *testing.T) {
	canonical := &fakeEndpointCommand{serverPID: "42"}

	err := attachWorkspaceEndpoint(
		context.Background(),
		canonicalEndpoint("workspace"),
		canonical,
		"/tmp/default,42,0",
	)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"display-message -p #{pid}",
		"switch-client workspace",
	}, canonical.calls)
}

func TestAttachWorkspaceEndpointNestsAcrossServers(t *testing.T) {
	canonical := &fakeEndpointCommand{serverPID: "43"}

	err := attachWorkspaceEndpoint(
		context.Background(),
		canonicalEndpoint("workspace"),
		canonical,
		"/tmp/default,42,0",
	)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"display-message -p #{pid}",
		"nested-attach workspace",
	}, canonical.calls)
}

func TestAttachWorkspaceEndpointOutsideTmuxDoesNotProbeServer(t *testing.T) {
	canonical := &fakeEndpointCommand{serverPIDErr: errors.New("must not run")}

	err := attachWorkspaceEndpoint(
		context.Background(),
		canonicalEndpoint("workspace"),
		canonical,
		"",
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"attach-session workspace"}, canonical.calls)
}

func TestAttachWorkspaceEndpointRejectsNonNumericServerPIDWithoutVersionProbe(t *testing.T) {
	canonical := &fakeEndpointCommand{serverPID: "#{pid}"}

	err := attachWorkspaceEndpoint(
		context.Background(),
		canonicalEndpoint("workspace"),
		canonical,
		"/tmp/default,42,0",
	)

	require.ErrorContains(t, err, "tmux 2.1")
	assert.Equal(t, []string{"display-message -p #{pid}"}, canonical.calls)
}
