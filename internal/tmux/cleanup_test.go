package tmux

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWorkspaceCleanupCommand struct {
	identity   sessionCleanupIdentity
	observeErr error
	killed     []string
}

func (f *fakeWorkspaceCleanupCommand) workspaceSessionCleanupIdentityContext(
	context.Context,
	string,
) (sessionCleanupIdentity, error) {
	return f.identity, f.observeErr
}

func (f *fakeWorkspaceCleanupCommand) KillSessionIfPresentContext(
	_ context.Context,
	target string,
) error {
	f.killed = append(f.killed, target)
	return nil
}

func TestKillWorkspaceSessionIfMatching(t *testing.T) {
	worktreeRequest := WorkspaceEndpointRequest{
		SessionName:         "kwt-wt-widget-main-01234567",
		WorkspacePath:       "/work/widget",
		WorkspaceGeneration: resolverTestGeneration,
	}
	tests := []struct {
		name       string
		identity   sessionCleanupIdentity
		observeErr error
		request    WorkspaceEndpointRequest
		wantKilled []string
		wantError  string
	}{
		{
			name: "observed generation",
			identity: sessionCleanupIdentity{
				SessionID: "$7", WorkspaceGeneration: resolverTestGeneration,
			},
			request: worktreeRequest, wantKilled: []string{"$7"},
		},
		{
			name: "replacement generation",
			identity: sessionCleanupIdentity{
				SessionID: "$8", WorkspaceGeneration: "fedcba9876543210fedcba9876543210",
			},
			request: worktreeRequest, wantError: "different worktree generation",
		},
		{
			name: "malformed generation",
			identity: sessionCleanupIdentity{
				SessionID: "$8", WorkspaceGeneration: "not-a-generation",
			},
			request: worktreeRequest, wantError: "malformed worktree generation",
		},
		{
			name: "different directory workspace",
			identity: sessionCleanupIdentity{
				SessionID: "$9", WorkspaceIdentity: workspacePathIdentity("/work/replacement"),
			},
			request: WorkspaceEndpointRequest{
				SessionName: "kwt-workspace-widget", WorkspacePath: "/work/widget",
			},
			wantError: "different workspace identity",
		},
		{
			name: "already absent", identity: sessionCleanupIdentity{Absent: true},
			request: worktreeRequest,
		},
		{
			name: "observation failure", observeErr: context.Canceled,
			request: worktreeRequest, wantError: context.Canceled.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := &fakeWorkspaceCleanupCommand{
				identity: test.identity, observeErr: test.observeErr,
			}

			err := killWorkspaceSessionIfMatching(
				context.Background(), command, test.request,
			)

			if test.wantError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.ErrorContains(t, err, test.wantError)
			}
			assert.Equal(t, test.wantKilled, command.killed)
		})
	}
}

func TestClassifySessionCleanupIdentityReadsOneObservedTuple(t *testing.T) {
	workspaceIdentity := strings.Repeat("a", 64)

	identity, err := classifySessionCleanupIdentity(
		"$7\t"+workspaceIdentity+"\t"+resolverTestGeneration+"\n",
		"",
		nil,
	)

	require.NoError(t, err)
	assert.Equal(t, sessionCleanupIdentity{
		SessionID:           "$7",
		WorkspaceIdentity:   workspaceIdentity,
		WorkspaceGeneration: resolverTestGeneration,
	}, identity)
}

func TestClassifySessionCleanupIdentityTreatsExplicitAbsenceAsComplete(t *testing.T) {
	identity, err := classifySessionCleanupIdentity(
		"",
		"can't find session: workspace\n",
		fakeProbeExitError(1),
	)

	require.NoError(t, err)
	assert.True(t, identity.Absent)
}

func TestClassifySessionCleanupIdentityRejectsMalformedTuple(t *testing.T) {
	_, err := classifySessionCleanupIdentity("$7\tonly-two-fields\n", "", nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "malformed response")
}
