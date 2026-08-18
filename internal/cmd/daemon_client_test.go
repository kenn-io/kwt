package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/service"
)

func TestAcquireSSHLeaseDoesNotRetryDrainAfterExposingOperation(t *testing.T) {
	for _, kind := range []service.OperationEventKind{
		service.OperationEventProgress,
		service.OperationEventPrompt,
	} {
		t.Run(string(kind), func(t *testing.T) {
			oldFactory := newDaemonController
			oldAcquire := acquireSSHFromDaemon
			t.Cleanup(func() {
				newDaemonController = oldFactory
				acquireSSHFromDaemon = oldAcquire
			})
			newDaemonController = func() (daemonController, error) {
				return &fakeDaemonController{observation: kwtdaemon.Observation{
					State:  kwtdaemon.RuntimeReady,
					Client: &kwtdaemon.Client{},
					Status: kwtdaemon.Status{Capabilities: []string{
						kwtdaemon.CapabilitySSHLeaseHold,
						kwtdaemon.CapabilitySSHLifecycle,
					}},
				}}, nil
			}
			attempts := 0
			acquireSSHFromDaemon = func(
				ctx context.Context,
				_ *kwtdaemon.Client,
				_ kwt.SSHLeaseRequest,
				callbacks kwtdaemon.OperationCallbacks,
			) (kwtdaemon.SSHLeaseResult, error) {
				attempts++
				if attempts > 1 {
					return kwtdaemon.SSHLeaseResult{LeaseID: "duplicate"}, nil
				}
				event := service.OperationEvent{Kind: kind}
				if kind == service.OperationEventPrompt {
					event.Prompt = &service.OperationPrompt{ID: "prompt-1", Message: "Password:"}
				}
				require.NoError(t, callbacks.Event(event))
				if event.Prompt != nil {
					_, err := callbacks.Prompt(ctx, *event.Prompt)
					require.NoError(t, err)
				}
				return kwtdaemon.SSHLeaseResult{}, service.NewError(
					service.DaemonDraining,
					"daemon is draining",
					true,
					map[string]any{"drain_deadline": time.Now().Add(5 * time.Millisecond)},
					nil,
				)
			}
			events := 0
			prompts := 0
			_, _, err := acquireSSHLeaseViaDaemon(
				context.Background(),
				kwt.SSHLeaseRequest{},
				kwtdaemon.OperationCallbacks{
					Event: func(service.OperationEvent) error {
						events++
						return nil
					},
					Prompt: func(context.Context, service.OperationPrompt) (string, error) {
						prompts++
						return "secret", nil
					},
				},
			)
			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.DaemonDraining), err)
			assert.Equal(t, 1, attempts)
			assert.Equal(t, 1, events)
			if kind == service.OperationEventPrompt {
				assert.Equal(t, 1, prompts)
			} else {
				assert.Equal(t, 0, prompts)
			}
		})
	}
}

func TestRequireSSHLifecycleCapabilitiesRejectsDaemonWithoutLeaseHold(t *testing.T) {
	err := requireSSHLifecycleCapabilities(kwtdaemon.Observation{
		Client: &kwtdaemon.Client{},
		Status: kwtdaemon.Status{Capabilities: []string{
			kwtdaemon.CapabilitySSHLifecycle,
		}},
	})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.DaemonIncompatible))
}

func TestInventoryDrainDeadlineRequiresDaemonDrainingCode(t *testing.T) {
	deadline := time.Now().UTC().Add(time.Minute)
	tests := []struct {
		name string
		err  error
		want *time.Time
	}{
		{
			name: "inventory refresh timeout",
			err:  service.NewError(service.InventoryTimeout, "inventory refresh timed out", true, nil, nil),
		},
		{
			name: "daemon drain",
			err: service.NewError(service.DaemonDraining, "daemon is draining", true, map[string]any{
				"drain_deadline": deadline.Format(time.RFC3339Nano),
			}, nil),
			want: &deadline,
		},
		{
			name: "legacy daemon drain",
			err: service.NewError(service.Busy, "daemon is draining", true, map[string]any{
				"drain_deadline": deadline,
			}, nil),
			want: &deadline,
		},
		{
			name: "legacy busy without deadline",
			err:  service.NewError(service.Busy, "inventory is busy", true, nil, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := inventoryDrainDeadline(tt.err)
			if tt.want == nil {
				assert.False(t, ok)
				assert.Nil(t, got)
				return
			}
			require.True(t, ok)
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}

func TestWriteConfigNotesPreservesNoninteractiveWarning(t *testing.T) {
	var stderr bytes.Buffer
	writeConfigNotes(&stderr, []kwt.Note{{
		Code: "untrusted_config_skipped", Path: "/repo/.kwt.toml",
	}}, false, false)
	assert.Equal(t, "kwt: skipping untrusted local config /repo/.kwt.toml (non-interactive session)\n", stderr.String())
}

func TestWriteConfigNotesWarnsWhenUnsafeLocalConfigIsSkipped(t *testing.T) {
	var stderr bytes.Buffer
	writeConfigNotes(&stderr, []kwt.Note{{
		Code: "unsafe_config_skipped", Path: "/repo/.kwt.toml",
	}}, false, false)
	assert.Equal(t, "kwt: skipping unsafe local config /repo/.kwt.toml\n", stderr.String())
}

func TestWriteConfigNotesWarnsWhenTrustStoreIsUnavailable(t *testing.T) {
	var stderr bytes.Buffer
	writeConfigNotes(&stderr, []kwt.Note{{
		Code: "trust_store_unavailable", Path: "/home/trusted_configs.json",
	}}, false, false)
	assert.Equal(t, "kwt: failed to load trust store /home/trusted_configs.json (continuing empty)\n", stderr.String())
}

func TestTrustRequirementDecodesHTTPDetailTypes(t *testing.T) {
	err := service.NewError(service.InteractionRequired, "trust", false, map[string]any{
		"kind": "repository_config_trust", "path": "/repo/.kwt.toml",
		"digest": "abc", "size": float64(42), "preview": "[naming]", "truncated": true,
	}, nil)
	requirement, decodeErr := trustRequirement(err)
	require.NoError(t, decodeErr)
	assert.Equal(t, 42, requirement.Size)
	assert.True(t, requirement.Truncated)
}

func TestGuardedRemovalRejectsVersionOneDaemonBeforeMutation(t *testing.T) {
	oldFactory := newDaemonController
	oldRemove := removeWorktreeWithDaemonClient
	t.Cleanup(func() {
		newDaemonController = oldFactory
		removeWorktreeWithDaemonClient = oldRemove
	})

	newDaemonController = func() (daemonController, error) {
		return &fakeDaemonController{observation: kwtdaemon.Observation{
			State: kwtdaemon.RuntimeReady,
			Status: kwtdaemon.Status{
				Capabilities: []string{kwtdaemon.CapabilityRemoval},
			},
			Client: &kwtdaemon.Client{},
		}}, nil
	}
	mutated := false
	removeWorktreeWithDaemonClient = func(
		context.Context,
		*kwtdaemon.Client,
		kwt.RemovalRequest,
	) (kwt.RemovalResult, error) {
		mutated = true
		return kwt.RemovalResult{}, nil
	}

	_, err := removeWorktreeThroughDaemon(context.Background(), kwt.RemovalRequest{
		Session: &tmux.RemovalSessionCondition{},
	})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.DaemonIncompatible))
	assert.False(t, mutated, "an older daemon must be rejected before removal is dispatched")
}
