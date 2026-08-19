package tmux

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWorkspaceCleanupCommand struct {
	result          workspaceCleanupResult
	err             error
	identity        workspaceCleanupIdentity
	identityErr     error
	compatResult    workspaceCleanupResult
	compatErr       error
	calls           []string
	session         string
	option          string
	compatSessionID string
	compatMarker    string
	compatExpected  string
}

func (f *fakeWorkspaceCleanupCommand) killWorkspaceSessionWithOptionContext(
	_ context.Context,
	session string,
	option string,
) (workspaceCleanupResult, error) {
	f.calls = append(f.calls, "atomic")
	f.session = session
	f.option = option
	return f.result, f.err
}

func (f *fakeWorkspaceCleanupCommand) workspaceSessionCleanupIdentityContext(
	_ context.Context,
	session string,
) (workspaceCleanupIdentity, error) {
	f.calls = append(f.calls, "inspect")
	f.session = session
	return f.identity, f.identityErr
}

func (f *fakeWorkspaceCleanupCommand) killWorkspaceSessionWithObservedIdentityContext(
	_ context.Context,
	session string,
	sessionID string,
	markerOption string,
	expectedIdentity string,
) (workspaceCleanupResult, error) {
	f.calls = append(f.calls, "compat")
	f.session = session
	f.compatSessionID = sessionID
	f.compatMarker = markerOption
	f.compatExpected = expectedIdentity
	return f.compatResult, f.compatErr
}

func TestKillWorkspaceSessionIfMatching(t *testing.T) {
	worktreeRequest := WorkspaceEndpointRequest{
		SessionName:         "kwt-wt-widget-main-01234567",
		WorkspacePath:       "/work/widget",
		WorkspaceGeneration: resolverTestGeneration,
	}
	tests := []struct {
		name         string
		result       workspaceCleanupResult
		invokeErr    error
		identity     workspaceCleanupIdentity
		compatResult workspaceCleanupResult
		compatErr    error
		request      WorkspaceEndpointRequest
		wantOption   string
		wantCalls    []string
		wantError    string
	}{
		{
			name: "matching generation", result: workspaceCleanupTerminated,
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  []string{"atomic"},
		},
		{
			name: "replacement generation", result: workspaceCleanupMismatch,
			identity: workspaceCleanupIdentity{
				SessionID:           "$8",
				WorkspaceGeneration: "fedcba9876543210fedcba9876543210",
			},
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  []string{"atomic", "inspect"},
			wantError:  "different worktree generation",
		},
		{
			name: "pre-cleanup-marker generation", result: workspaceCleanupMismatch,
			identity: workspaceCleanupIdentity{
				SessionID: "$7", WorkspaceGeneration: resolverTestGeneration,
			},
			compatResult: workspaceCleanupTerminated,
			request:      worktreeRequest,
			wantOption:   workspaceCleanupOption(resolverTestGeneration),
			wantCalls:    []string{"atomic", "inspect", "compat"},
		},
		{
			name: "pre-cleanup-marker replacement race", result: workspaceCleanupMismatch,
			identity: workspaceCleanupIdentity{
				SessionID: "$7", WorkspaceGeneration: resolverTestGeneration,
			},
			compatResult: workspaceCleanupMismatch,
			request:      worktreeRequest,
			wantOption:   workspaceCleanupOption(resolverTestGeneration),
			wantCalls:    []string{"atomic", "inspect", "compat"},
			wantError:    "different worktree generation",
		},
		{
			name: "malformed requested generation",
			request: WorkspaceEndpointRequest{
				SessionName:         "kwt-wt-widget-main-01234567",
				WorkspaceGeneration: "not-a-generation",
			},
			wantError: "malformed worktree generation",
		},
		{
			name: "different directory workspace", result: workspaceCleanupMismatch,
			request: WorkspaceEndpointRequest{
				SessionName: "kwt-workspace-widget", WorkspacePath: "/work/widget",
			},
			identity: workspaceCleanupIdentity{
				SessionID:         "$9",
				WorkspaceIdentity: workspacePathIdentity("/work/replacement"),
			},
			wantOption: workspaceCleanupOption(workspacePathIdentity("/work/widget")),
			wantCalls:  []string{"atomic", "inspect"},
			wantError:  "different workspace identity",
		},
		{
			name: "already absent", result: workspaceCleanupAbsent,
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  []string{"atomic"},
		},
		{
			name: "invocation failure", invokeErr: context.Canceled,
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  []string{"atomic"}, wantError: context.Canceled.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := &fakeWorkspaceCleanupCommand{
				result:       test.result,
				err:          test.invokeErr,
				identity:     test.identity,
				compatResult: test.compatResult,
				compatErr:    test.compatErr,
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
			assert.Equal(t, test.wantCalls, command.calls)
			assert.Equal(t, test.wantOption, command.option)
			if len(test.wantCalls) != 0 {
				assert.Equal(t, test.request.SessionName, command.session)
			}
			if slices.Contains(test.wantCalls, "compat") {
				assert.Equal(t, test.identity.SessionID, command.compatSessionID)
			}
		})
	}
}

func TestQuoteTmuxCommandArgument(t *testing.T) {
	assert.Equal(
		t,
		`"=kwt-wt-topic;'\$HOME\""`,
		quoteTmuxCommandArgument(`=kwt-wt-topic;'$HOME"`),
	)
}
