package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

func TestSSHResolveCommandPassesStructuredTargetAndPrintsJSON(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldJSON, oldUser, oldPort := sshResolveJSON, sshResolveUser, sshResolvePort
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		sshResolveJSON, sshResolveUser, sshResolvePort = oldJSON, oldUser, oldPort
	})
	sshResolveJSON, sshResolveUser, sshResolvePort = true, "deploy", 2200
	var got kwt.SSHResolveRequest
	resolveSSHThroughDaemon = func(_ context.Context, request kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error) {
		got = request
		return kwt.SSHRouteSnapshot{
			LogicalTarget: request.Target, RouteIdentity: "route-identity",
			ProjectionPolicy: kwt.SSHProjectionPolicyV1,
			ObservedAt:       time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC),
		}, nil
	}
	command, stdout, stderr := sshResolveTestCommand()

	err := runSSHResolve(command, []string{"build.example.test"})
	require.NoError(t, err)
	assert.Equal(t, kwt.SSHResolveRequest{Target: kwt.SSHTarget{
		Hostname: "build.example.test", User: "deploy", Port: 2200,
	}}, got)
	var snapshot kwt.SSHRouteSnapshot
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &snapshot))
	assert.Equal(t, "route-identity", snapshot.RouteIdentity)
	assert.Empty(t, stderr.String())
}

func TestSSHResolveCommandPrintsCredentialFreeHumanRoute(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldJSON := sshResolveJSON
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		sshResolveJSON = oldJSON
	})
	sshResolveJSON = false
	resolveSSHThroughDaemon = func(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{Targets: []kwt.SSHResolvedTarget{{
			DisplayTarget: "relay@[2001:db8::42]:2200",
		}, {
			DisplayTarget: "deploy@build.internal:22",
		}}}, nil
	}
	command, stdout, _ := sshResolveTestCommand()
	require.NoError(t, runSSHResolve(command, []string{"build.example.test"}))
	assert.Equal(t, "relay@[2001:db8::42]:2200\ndeploy@build.internal:22\n", stdout.String())
}

func TestSSHResolveCommandPreservesStableJSONErrors(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldJSON := sshResolveJSON
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		sshResolveJSON = oldJSON
	})
	sshResolveJSON = true
	resolveSSHThroughDaemon = func(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{}, service.NewError(
			service.SSHInvalidTarget, "invalid SSH target", false, nil, errors.New("private"),
		)
	}
	command, stdout, stderr := sshResolveTestCommand()
	err := runSSHResolve(command, []string{"bad"})
	require.Error(t, err)
	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 2, coded.ExitCode())
	assert.NotEqual(t, 255, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.SSHInvalidTarget, envelope.Error.Code)
	assert.NotContains(t, stdout.String(), "private")
	assert.Contains(t, stderr.String(), "kwt ssh resolve: ssh_invalid_target")
}

func TestRequireSSHResolveCapabilityFailsClosed(t *testing.T) {
	err := requireSSHResolveCapability(kwtdaemon.Observation{})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.DaemonIncompatible))
}

func sshResolveTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	root := &cobra.Command{Use: "kwt"}
	command := &cobra.Command{Use: "resolve"}
	root.AddCommand(command)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	return command, &stdout, &stderr
}
