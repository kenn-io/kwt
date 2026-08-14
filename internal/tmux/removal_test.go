package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemovalSessionGuardRequiresLiveSessionToBeStoppedFirst(t *testing.T) {
	lease, err := NewRemovalSessionGuard("missing-tmux-for-live-removal").Quiesce(
		context.Background(),
		RemovalSessionCondition{
			SessionName: "topic",
			ServerPID:   "41",
			SessionID:   "$3",
			CreatedAt:   "1720000000",
		},
	)

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "stop the session")
}

func TestRemovalSessionInspectionPreservesCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{
			name: "canceled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.ctx()
			defer cancel()
			guard := &removalSessionGuard{
				command: "tmux",
				inspect: func(context.Context, *TmuxCommand) (string, string, error) {
					return "", "no server running on /tmp/tmux/default", errors.New("tmux exited")
				},
			}

			lease, err := guard.Quiesce(ctx, RemovalSessionCondition{
				SessionName: "topic", Absent: true,
			})

			assert.Nil(t, lease)
			assert.ErrorIs(t, err, test.want)
		})
	}
}

func TestRemovalSessionInspectionFailsClosedWhenListedSessionDisappears(t *testing.T) {
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(context.Context, *TmuxCommand) (string, string, error) {
			return "", "can't find session: $1", errors.New("tmux exited")
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName: "topic",
		Absent:      true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inspect tmux sessions")
}

func TestOrdinaryNamedSocketRemovalUsesSharedServerScan(t *testing.T) {
	var inspected *TmuxCommand
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			command *TmuxCommand,
		) (string, string, error) {
			inspected = command
			return removalInventoryOutput(removalSessionRow{
				SessionName: "another-session",
			}), "", nil
		},
		inspectProtected: func(
			context.Context,
			*TmuxCommand,
			string,
		) (ProtectedSessionState, error) {
			return ProtectedSessionIndeterminate, errors.New("strict protected probe used")
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:     "expected",
		SocketName:      "team-server",
		SocketDirectory: "/srv/tmux",
		Absent:          true,
	})

	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NotNil(t, inspected)
	assert.Equal(t, "team-server", inspected.socketName)
	assert.Equal(t, "/srv/tmux", inspected.socketTempDir)
}

func TestRemovalFailsClosedOnMalformedWorkspaceInventory(t *testing.T) {
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			context.Context,
			*TmuxCommand,
		) (string, string, error) {
			return "not-json", "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:   "expected",
		WorkspacePath: "/worktrees/topic",
		Absent:        true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed session inventory")
}

func TestRemovalFailsClosedWhenWorkspaceMarkerContainsDelimiter(t *testing.T) {
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			context.Context,
			*TmuxCommand,
		) (string, string, error) {
			return removalInventoryOutput(removalSessionRow{
				SessionName:         "other-session",
				WorkspaceIdentity:   "attacker|" + strings.Repeat("a", 64),
				WorkspaceGeneration: "fedcba9876543210fedcba9876543210",
			}), "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:         "expected",
		WorkspacePath:       "/worktrees/topic",
		WorkspaceGeneration: "0123456789abcdef0123456789abcdef",
		Absent:              true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed session inventory")
}

func TestRemovalRejectsDifferentNamedSessionForWorkspacePath(t *testing.T) {
	const workspacePath = "/worktrees/topic"
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			_ *TmuxCommand,
		) (string, string, error) {
			return removalInventoryOutput(removalSessionRow{
				SessionName:       "kwt-workspace-old-branch",
				WorkspaceIdentity: workspacePathIdentity(workspacePath),
			}), "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:   "kwt-workspace-new-branch",
		WorkspacePath: workspacePath,
		Absent:        true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "worktree")
}

func TestRemovalPreservesDelimiterInWorkspaceSessionName(t *testing.T) {
	const workspacePath = "/worktrees/topic"
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			_ *TmuxCommand,
		) (string, string, error) {
			return removalInventoryOutput(removalSessionRow{
				SessionName:       "kwt-wt-repo-feature|topic-deadbeef",
				WorkspaceIdentity: workspacePathIdentity(workspacePath),
			}), "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:   "kwt-wt-repo-feature|topic-deadbeef",
		WorkspacePath: workspacePath,
		Absent:        true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "started after confirmation")
}

func TestRemovalRejectsUnmarkedLegacySessionAfterBranchChange(t *testing.T) {
	const workspacePath = "/home/u/worktrees/github.com/wesm/kwt/feature/foo"
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			_ *TmuxCommand,
		) (string, string, error) {
			return removalInventoryOutput(removalSessionRow{
				SessionName: "kwt-wt-kwt-old-branch-9cc4e551",
			}), "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:   "kwt-wt-kwt-new-branch-9cc4e551",
		WorkspacePath: workspacePath,
		Absent:        true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "worktree")
}

func TestRemovalRejectsGenerationMarkedSessionAfterWorktreeMove(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			_ *TmuxCommand,
		) (string, string, error) {
			return removalInventoryOutput(removalSessionRow{
				SessionName:         "kwt-wt-kwt-topic-oldhash",
				WorkspaceIdentity:   strings.Repeat("a", 64),
				WorkspaceGeneration: generation,
			}), "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:         "kwt-wt-kwt-topic-newhash",
		WorkspacePath:       "/worktrees/moved-topic",
		WorkspaceGeneration: generation,
		Absent:              true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "worktree")
}

func TestProtectedRemovalRejectsOrdinaryGenerationMarkedSessionAfterWorktreeMove(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	ordinaryInspections := 0
	protectedInspections := 0
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			command *TmuxCommand,
		) (string, string, error) {
			ordinaryInspections++
			assert.Empty(t, command.socketName)
			return removalInventoryOutput(removalSessionRow{
				SessionName:         "kwt-wt-repo-topic-oldhash",
				WorkspaceIdentity:   strings.Repeat("a", 64),
				WorkspaceGeneration: generation,
			}), "", nil
		},
		inspectProtected: func(
			_ context.Context,
			_ *TmuxCommand,
			_ string,
		) (ProtectedSessionState, error) {
			protectedInspections++
			return ProtectedSessionAbsent, nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:             "kwt-wt-repo-topic-newhash",
		WorkspacePath:           "/worktrees/moved-topic",
		WorkspaceGeneration:     generation,
		SocketName:              "kwt-pr-protected",
		Absent:                  true,
		ProtectedSocketTopology: true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree remains live")
	assert.Equal(t, 1, ordinaryInspections)
	assert.Zero(t, protectedInspections, "ordinary ownership must fail before protected probing")
}

func TestRemovalFailsClosedOnUnmarkedKWTSessionAfterWorktreeMove(t *testing.T) {
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			_ *TmuxCommand,
		) (string, string, error) {
			return removalInventoryOutput(removalSessionRow{
				SessionName: "kwt-wt-kwt-topic-oldhash",
			}), "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:         "kwt-wt-kwt-topic-newhash",
		WorkspacePath:       "/worktrees/moved-topic",
		WorkspaceGeneration: "0123456789abcdef0123456789abcdef",
		Absent:              true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "ownership")
}

func TestRemovalFailsClosedOnUnmarkedLegacyDirHostSessionAfterMove(t *testing.T) {
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: func(
			_ context.Context,
			_ *TmuxCommand,
		) (string, string, error) {
			return removalInventoryOutput(removalSessionRow{
				SessionName: "kwt-workspace-dir-owner-repo-old-branch-oldhash",
			}), "", nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:         "kwt-workspace-dir-owner-repo-topic-newhash",
		WorkspacePath:       "/worktrees/moved-topic",
		WorkspaceGeneration: "0123456789abcdef0123456789abcdef",
		Absent:              true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	var conditionErr *RemovalSessionConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.Reason, "ownership")
}

func TestRemovalRequiresMarkersToExemptUnrelatedDirectoryWorkspaceSession(t *testing.T) {
	const directoryPath = "/registered/workspaces/notes"
	for _, test := range []struct {
		name              string
		workspaceIdentity string
		wantConflict      bool
	}{
		{name: "marked", workspaceIdentity: workspacePathIdentity(directoryPath)},
		{name: "legacy unmarked", wantConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			guard := &removalSessionGuard{
				command: "tmux",
				inspect: func(
					_ context.Context,
					_ *TmuxCommand,
				) (string, string, error) {
					return removalInventoryOutput(removalSessionRow{
						SessionName:       DirWorkspaceSessionName("notes", directoryPath),
						WorkspaceIdentity: test.workspaceIdentity,
					}), "", nil
				},
			}

			lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
				SessionName:         "kwt-wt-repo-topic-deadbeef",
				WorkspacePath:       "/worktrees/topic",
				WorkspaceGeneration: "0123456789abcdef0123456789abcdef",
				Absent:              true,
			})

			if test.wantConflict {
				assert.Nil(t, lease)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "ownership")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, lease)
		})
	}
}

func TestProtectedRemovalFailsClosedOnUnexpectedCanonicalTopology(t *testing.T) {
	var inspected []*TmuxCommand
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: absentOrdinaryRemovalInspection,
		inspectProtected: func(
			_ context.Context,
			command *TmuxCommand,
			_ string,
		) (ProtectedSessionState, error) {
			inspected = append(inspected, command)
			return ProtectedSessionIndeterminate, errors.New("unexpected protected session")
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:             "expected",
		SocketName:              "kwt-pr-protected",
		SocketDirectory:         "/tmp/legacy-tmux",
		Absent:                  true,
		ProtectedSocketTopology: true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	require.Len(t, inspected, 1, "indeterminate canonical topology must not fall through to legacy")
	assert.Equal(t, "kwt-pr-protected", inspected[0].socketName)
	assert.Empty(t, inspected[0].socketTempDir)
}

func TestProtectedRemovalCommandsStripRequestProtectedNames(t *testing.T) {
	t.Setenv("CUSTOM_FLEET_TOKEN", "secret")
	inspections := 0
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: absentOrdinaryRemovalInspection,
		inspectProtected: func(
			_ context.Context,
			command *TmuxCommand,
			_ string,
		) (ProtectedSessionState, error) {
			inspections++
			cmd := command.newCmd(context.Background(), []string{"list-sessions"})
			for _, entry := range cmd.Env {
				if hasEnvName(entry, "CUSTOM_FLEET_TOKEN") {
					t.Fatalf("protected removal command retained configured credential: %v", cmd.Env)
				}
			}
			return ProtectedSessionAbsent, nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:             "protected",
		SocketName:              "protected-socket",
		SocketDirectory:         "/legacy/tmux",
		Absent:                  true,
		ProtectedSocketTopology: true,
		ProtectedNames:          []string{"CUSTOM_FLEET_TOKEN"},
	})

	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, 2, inspections, "canonical and legacy endpoints must both be sanitized")
}

func TestProtectedRemovalChecksCanonicalBeforeLegacyEndpoint(t *testing.T) {
	var inspected []*TmuxCommand
	guard := &removalSessionGuard{
		command: "tmux",
		inspect: absentOrdinaryRemovalInspection,
		inspectProtected: func(
			_ context.Context,
			command *TmuxCommand,
			_ string,
		) (ProtectedSessionState, error) {
			inspected = append(inspected, command)
			if command.socketTempDir == "" {
				return ProtectedSessionAbsent, nil
			}
			return ProtectedSessionLive, nil
		},
	}

	lease, err := guard.Quiesce(context.Background(), RemovalSessionCondition{
		SessionName:             "expected",
		SocketName:              "kwt-pr-protected",
		SocketDirectory:         "/tmp/legacy-tmux",
		Absent:                  true,
		ProtectedSocketTopology: true,
	})

	assert.Nil(t, lease)
	require.Error(t, err)
	require.Len(t, inspected, 2)
	assert.Empty(t, inspected[0].socketTempDir)
	assert.Equal(t, "/tmp/legacy-tmux", inspected[1].socketTempDir)
}

func absentOrdinaryRemovalInspection(
	context.Context,
	*TmuxCommand,
) (string, string, error) {
	return "", "no server running on /tmp/tmux/default", errors.New("tmux exited")
}

func removalInventoryOutput(rows ...removalSessionRow) string {
	encoded, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
