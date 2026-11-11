package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

func TestAcquireSSHLeaseRequiresConnectionCapabilityForExtraMasters(t *testing.T) {
	oldFactory := newDaemonController
	oldAcquire := acquireSSHFromDaemon
	t.Cleanup(func() {
		newDaemonController = oldFactory
		acquireSSHFromDaemon = oldAcquire
	})
	lifecycleOnly := []string{
		kwtdaemon.CapabilitySSHLeaseHold,
		kwtdaemon.CapabilitySSHLifecycle,
	}
	var forwarded []int
	acquireSSHFromDaemon = func(
		_ context.Context,
		_ *kwtdaemon.Client,
		request kwt.SSHLeaseRequest,
		_ kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, error) {
		forwarded = append(forwarded, request.Connection)
		return kwtdaemon.SSHLeaseResult{LeaseID: "lease"}, nil
	}
	useCapabilities := func(capabilities []string) {
		newDaemonController = func() (daemonController, error) {
			return &fakeDaemonController{observation: kwtdaemon.Observation{
				State:  kwtdaemon.RuntimeReady,
				Client: &kwtdaemon.Client{},
				Status: kwtdaemon.Status{Capabilities: capabilities},
			}}, nil
		}
	}

	useCapabilities(lifecycleOnly)
	_, _, err := acquireSSHLeaseViaDaemon(
		t.Context(), kwt.SSHLeaseRequest{}, kwtdaemon.OperationCallbacks{},
	)
	require.NoError(t, err, "the shared master works on an older daemon")
	_, _, err = acquireSSHLeaseViaDaemon(
		t.Context(), kwt.SSHLeaseRequest{Connection: 1}, kwtdaemon.OperationCallbacks{},
	)
	assert.True(t, service.IsCode(err, service.DaemonIncompatible), err)

	useCapabilities(append(lifecycleOnly, kwtdaemon.CapabilitySSHLeaseConnections))
	_, _, err = acquireSSHLeaseViaDaemon(
		t.Context(), kwt.SSHLeaseRequest{Connection: 1}, kwtdaemon.OperationCallbacks{},
	)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, forwarded)
}

func TestSSHLeaseForwardsConnectionIndex(t *testing.T) {
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldJSON, oldIdentity, oldConnection := sshLeaseJSON, sshLeaseRouteIdentity, sshLeaseConnection
	t.Cleanup(func() {
		acquireSSHLeaseThroughDaemon = oldAcquire
		sshLeaseJSON, sshLeaseRouteIdentity, sshLeaseConnection = oldJSON, oldIdentity, oldConnection
	})
	sshLeaseJSON = true
	sshLeaseRouteIdentity = "route-one"
	sshLeaseConnection = 3
	control := &fakeSSHLeaseControl{}
	var requested int
	acquireSSHLeaseThroughDaemon = func(
		_ context.Context,
		request kwt.SSHLeaseRequest,
		_ kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		requested = request.Connection
		return kwtdaemon.SSHLeaseResult{LeaseID: "lease-1"}, control, nil
	}
	command, _, _ := sshResolveTestCommand()
	command.SetIn(strings.NewReader(""))

	require.NoError(t, runSSHLease(command, []string{"build.example.test"}))
	assert.Equal(t, 3, requested)
}
