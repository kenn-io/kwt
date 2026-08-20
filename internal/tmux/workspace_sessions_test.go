package tmux

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

type recordingWorkspaceEnsurer struct {
	ensureSession    string
	ensurePath       string
	ensureGeneration string
	repairErr        error
	repaired         bool
	ensured          bool
}

func (r *recordingWorkspaceEnsurer) RepairExisting(
	_ context.Context,
	session, path string,
	_ models.Layout,
) error {
	r.repaired = true
	r.ensureSession = session
	r.ensurePath = path
	return r.repairErr
}

func (r *recordingWorkspaceEnsurer) RepairExistingWithGeneration(
	_ context.Context,
	session, path, generation string,
	_ models.Layout,
) error {
	r.repaired = true
	r.ensureSession = session
	r.ensurePath = path
	r.ensureGeneration = generation
	return r.repairErr
}

func (r *recordingWorkspaceEnsurer) Ensure(
	_ context.Context,
	session, path string,
	_ models.Layout,
) error {
	r.ensured = true
	r.ensureSession = session
	r.ensurePath = path
	return nil
}

func (r *recordingWorkspaceEnsurer) EnsureWithGeneration(
	_ context.Context,
	session, path, generation string,
	_ models.Layout,
) error {
	r.ensured = true
	r.ensureSession = session
	r.ensurePath = path
	r.ensureGeneration = generation
	return nil
}

type recordingWorkspaceAttachCommand struct {
	verb            string
	session         string
	preparedSession string
	serverPID       string
}

func (r *recordingWorkspaceAttachCommand) ServerPIDContext(context.Context) (string, error) {
	return r.serverPID, nil
}

func (r *recordingWorkspaceAttachCommand) AttachSession(session string) error {
	r.verb = "attach-session"
	r.session = session
	return nil
}

func (r *recordingWorkspaceAttachCommand) SwitchClient(session string) error {
	r.verb = "switch-client"
	r.session = session
	return nil
}

func (r *recordingWorkspaceAttachCommand) AttachSessionNested(
	_ context.Context,
	session string,
) error {
	r.verb = "nested-attach"
	r.session = session
	return nil
}

func (r *recordingWorkspaceAttachCommand) AttachSessionNestedCommand(
	_ context.Context,
	session string,
) *exec.Cmd {
	r.preparedSession = session
	return exec.Command(os.Args[0], "attach-session", "-t", session)
}

func TestWorkspaceSessionsEstablishesOnResolvedCanonicalEndpoint(t *testing.T) {
	ensurer := &recordingWorkspaceEnsurer{}
	var selected SessionEndpoint
	runner := &WorkspaceSessions{
		resolveSession: func(context.Context, WorkspaceEndpointRequest) (resolvedWorkspaceSession, error) {
			return newResolvedWorkspaceSession("workspace", false, false), nil
		},
		workspaceForEndpoint: func(endpoint SessionEndpoint) (workspaceSessionEnsurer, error) {
			selected = endpoint
			return ensurer, nil
		},
	}

	got, err := runner.EstablishWithGeneration(
		context.Background(),
		"workspace",
		"/work/widget",
		resolverTestGeneration,
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Equal(t, KWTServerSocketName, selected.SocketName)
	assert.Equal(t, KWTServerSocketName, got.SocketName)
	assert.Equal(t, "workspace", ensurer.ensureSession)
	assert.Equal(t, resolverTestGeneration, ensurer.ensureGeneration)
}

func TestWorkspaceSessionsEstablishesOnResolvedAdoptedEndpoint(t *testing.T) {
	ensurer := &recordingWorkspaceEnsurer{}
	var selected SessionEndpoint
	runner := &WorkspaceSessions{
		resolveSession: func(context.Context, WorkspaceEndpointRequest) (resolvedWorkspaceSession, error) {
			return newResolvedWorkspaceSession("workspace", true, true), nil
		},
		workspaceForEndpoint: func(endpoint SessionEndpoint) (workspaceSessionEnsurer, error) {
			selected = endpoint
			return ensurer, nil
		},
	}

	got, err := runner.Establish(
		context.Background(),
		"workspace",
		"/work/widget",
		BlankLayout(),
	)

	require.NoError(t, err)
	assert.Empty(t, selected.SocketName)
	assert.Empty(t, got.SocketName)
	assert.Empty(t, ensurer.ensureGeneration)
	assert.True(t, ensurer.repaired)
}

func TestWorkspaceSessionsRepairsResolvedSessionName(t *testing.T) {
	for _, test := range []struct {
		name       string
		adopted    bool
		generation string
	}{
		{name: "canonical"},
		{name: "canonical with generation", generation: resolverTestGeneration},
		{name: "adopted", adopted: true},
		{name: "adopted with generation", adopted: true, generation: resolverTestGeneration},
	} {
		t.Run(test.name, func(t *testing.T) {
			ensurer := &recordingWorkspaceEnsurer{}
			runner := &WorkspaceSessions{
				resolveSession: func(
					context.Context,
					WorkspaceEndpointRequest,
				) (resolvedWorkspaceSession, error) {
					return newResolvedWorkspaceSession(
						"existing-session", true, test.adopted,
					), nil
				},
				workspaceForEndpoint: func(
					SessionEndpoint,
				) (workspaceSessionEnsurer, error) {
					return ensurer, nil
				},
			}

			got, err := runner.establish(
				context.Background(),
				"requested-session",
				"/work/widget",
				test.generation,
				BlankLayout(),
			)

			require.NoError(t, err)
			assert.Equal(t, "existing-session", got.SessionName)
			assert.Equal(t, "existing-session", ensurer.ensureSession)
			assert.Equal(t, test.generation, ensurer.ensureGeneration)
			assert.True(t, ensurer.repaired)
			assert.False(t, ensurer.ensured)
		})
	}
}

func TestWorkspaceSessionsNeverCreatesReplacementOnExitedResolvedEndpoint(t *testing.T) {
	for _, test := range []struct {
		name         string
		resolvedName string
		adopted      bool
		generation   string
	}{
		{
			name: "adopted endpoint", resolvedName: "workspace", adopted: true,
			generation: resolverTestGeneration,
		},
		{name: "canonical alias", resolvedName: "legacy$session"},
	} {
		t.Run(test.name, func(t *testing.T) {
			disappeared := &recordingWorkspaceEnsurer{repairErr: errWorkspaceSessionAbsent}
			canonical := &recordingWorkspaceEnsurer{}
			resolveCalls := 0
			runner := &WorkspaceSessions{
				resolveSession: func(
					_ context.Context,
					request WorkspaceEndpointRequest,
				) (resolvedWorkspaceSession, error) {
					assert.Equal(t, "workspace", request.SessionName)
					resolveCalls++
					if resolveCalls == 1 {
						return newResolvedWorkspaceSession(
							test.resolvedName, true, test.adopted,
						), nil
					}
					return newResolvedWorkspaceSession("workspace", false, false), nil
				},
				workspaceForEndpoint: func(
					endpoint SessionEndpoint,
				) (workspaceSessionEnsurer, error) {
					if endpoint != canonicalEndpoint("workspace") {
						return disappeared, nil
					}
					return canonical, nil
				},
			}

			got, err := runner.establish(
				context.Background(),
				"workspace",
				"/work/widget",
				test.generation,
				BlankLayout(),
			)

			require.NoError(t, err)
			assert.Equal(t, 2, resolveCalls)
			assert.True(t, disappeared.repaired)
			assert.False(t, disappeared.ensured)
			assert.True(t, canonical.ensured)
			assert.Equal(t, "workspace", canonical.ensureSession)
			assert.Equal(t, test.generation, canonical.ensureGeneration)
			assert.Equal(t, canonicalEndpoint("workspace"), got)
		})
	}
}

func TestWorkspaceSessionsRoutesAttachmentByClientMembership(t *testing.T) {
	for _, test := range []struct {
		name      string
		tmuxValue string
		serverPID string
		wantVerb  string
	}{
		{
			name:     "outside tmux",
			wantVerb: "attach-session",
		},
		{
			name:      "same server",
			tmuxValue: "/tmp/default,42,0",
			serverPID: "42",
			wantVerb:  "switch-client",
		},
		{
			name:      "different server",
			tmuxValue: "/tmp/default,42,0",
			serverPID: "43",
			wantVerb:  "nested-attach",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := &recordingWorkspaceAttachCommand{serverPID: test.serverPID}
			runner := &WorkspaceSessions{
				attachForEndpoint: func(SessionEndpoint) (workspaceAttachCommand, error) {
					return command, nil
				},
				tmuxEnvironment: func() string { return test.tmuxValue },
			}
			endpoint := canonicalEndpoint("workspace")

			err := runner.Attach(context.Background(), endpoint)

			require.NoError(t, err)
			assert.Equal(t, test.wantVerb, command.verb)
			assert.Equal(t, "workspace", command.session)
		})
	}
}

func TestWorkspaceSessionsPreparesCrossServerAttachForResidentUI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, test := range []struct {
		name            string
		serverPID       string
		wantVerb        string
		wantPrepared    string
		wantProcessArgs []string
	}{
		{
			name:      "same server switches resident client",
			serverPID: "42",
			wantVerb:  "switch-client",
		},
		{
			name:            "different server returns nested attach process",
			serverPID:       "43",
			wantPrepared:    "workspace",
			wantProcessArgs: []string{os.Args[0], "attach-session", "-t", "workspace"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := &recordingWorkspaceAttachCommand{serverPID: test.serverPID}
			runner := &WorkspaceSessions{
				attachForEndpoint: func(SessionEndpoint) (workspaceAttachCommand, error) {
					return command, nil
				},
				tmuxEnvironment: func() string { return "/tmp/default,42,0" },
			}

			process, err := runner.PrepareResidentAttach(
				context.Background(), canonicalEndpoint("workspace"),
			)

			require.NoError(t, err)
			assert.Equal(t, test.wantVerb, command.verb)
			assert.Equal(t, test.wantPrepared, command.preparedSession)
			if test.wantProcessArgs == nil {
				assert.Nil(t, process)
			} else {
				require.NotNil(t, process)
				assert.Equal(t, test.wantProcessArgs, process.Args)
			}
		})
	}
}
