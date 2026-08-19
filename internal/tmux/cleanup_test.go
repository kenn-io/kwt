package tmux

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWorkspaceCleanupCommand struct {
	result  workspaceCleanupResult
	err     error
	calls   int
	session string
	option  string
}

func (f *fakeWorkspaceCleanupCommand) killWorkspaceSessionWithOptionContext(
	_ context.Context,
	session string,
	option string,
) (workspaceCleanupResult, error) {
	f.calls++
	f.session = session
	f.option = option
	return f.result, f.err
}

func TestKillWorkspaceSessionIfMatching(t *testing.T) {
	worktreeRequest := WorkspaceEndpointRequest{
		SessionName:         "kwt-wt-widget-main-01234567",
		WorkspacePath:       "/work/widget",
		WorkspaceGeneration: resolverTestGeneration,
	}
	tests := []struct {
		name       string
		result     workspaceCleanupResult
		invokeErr  error
		request    WorkspaceEndpointRequest
		wantOption string
		wantCalls  int
		wantError  string
	}{
		{
			name: "matching generation", result: workspaceCleanupTerminated,
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  1,
		},
		{
			name: "replacement generation", result: workspaceCleanupMismatch,
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  1, wantError: "different worktree generation",
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
			wantOption: workspaceCleanupOption(workspacePathIdentity("/work/widget")),
			wantCalls:  1, wantError: "different workspace identity",
		},
		{
			name: "already absent", result: workspaceCleanupAbsent,
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  1,
		},
		{
			name: "invocation failure", invokeErr: context.Canceled,
			request:    worktreeRequest,
			wantOption: workspaceCleanupOption(resolverTestGeneration),
			wantCalls:  1, wantError: context.Canceled.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := &fakeWorkspaceCleanupCommand{
				result: test.result,
				err:    test.invokeErr,
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
			if test.wantCalls != 0 {
				assert.Equal(t, test.request.SessionName, command.session)
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
