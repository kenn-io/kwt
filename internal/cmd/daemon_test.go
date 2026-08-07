package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
)

type fakeDaemonController struct {
	observation kwtdaemon.Observation
	stopErr     error
}

func (f *fakeDaemonController) Status(context.Context) (kwtdaemon.Observation, error) {
	return f.observation, nil
}

func (f *fakeDaemonController) Start(context.Context) (kwtdaemon.Observation, error) {
	return f.observation, nil
}

func (f *fakeDaemonController) Stop(context.Context) error { return f.stopErr }

func (f *fakeDaemonController) Restart(context.Context) (kwtdaemon.Observation, error) {
	return f.observation, nil
}

func TestDaemonCommandsNeverMergeCallerRepositoryConfig(t *testing.T) {
	oldMerge := mergeCwdLocal
	t.Cleanup(func() { mergeCwdLocal = oldMerge })
	mergeCwdLocal = func() error { return errors.New("must not be called") }

	require.NoError(t, daemonCmd.PersistentPreRunE(daemonStatusCmd, nil))
}

func TestDaemonStatusRendersMachineReadableState(t *testing.T) {
	oldFactory := newDaemonController
	oldJSON := daemonStatusJSON
	t.Cleanup(func() {
		newDaemonController = oldFactory
		daemonStatusJSON = oldJSON
	})
	want := kwtdaemon.Status{
		State:        kwtdaemon.StateReady,
		PID:          42,
		Endpoint:     "127.0.0.1:43210",
		SchemaMajor:  1,
		ActiveLeases: 2,
	}
	newDaemonController = func() (daemonController, error) {
		return &fakeDaemonController{observation: kwtdaemon.Observation{
			State:  kwtdaemon.RuntimeReady,
			Status: want,
		}}, nil
	}
	daemonStatusJSON = true
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	require.NoError(t, runDaemonStatus(command, nil))
	var got kwtdaemon.Status
	require.NoError(t, json.NewDecoder(&output).Decode(&got))
	assert.Equal(t, want.State, got.State)
	assert.Equal(t, want.PID, got.PID)
	assert.Equal(t, want.Endpoint, got.Endpoint)
	assert.Equal(t, want.SchemaMajor, got.SchemaMajor)
	assert.Equal(t, want.ActiveLeases, got.ActiveLeases)
}

func TestDaemonStatusPreservesDiscoveryClassification(t *testing.T) {
	for name, test := range map[string]struct {
		observation kwtdaemon.Observation
		wantState   string
		wantPID     float64
	}{
		"incompatible": {
			observation: kwtdaemon.Observation{
				State: kwtdaemon.RuntimeIncompatible,
				Status: kwtdaemon.Status{
					State: kwtdaemon.StateReady,
					PID:   41,
				},
			},
			wantState: "incompatible",
			wantPID:   41,
		},
		"unresponsive": {
			observation: kwtdaemon.Observation{
				State: kwtdaemon.RuntimeUnresponsive,
				Record: kitdaemon.RuntimeRecord{
					PID:     42,
					Version: "v1.2.3",
					Network: kitdaemon.NetworkTCP,
					Address: "127.0.0.1:43210",
				},
			},
			wantState: "unresponsive",
			wantPID:   42,
		},
	} {
		t.Run(name, func(t *testing.T) {
			command := &cobra.Command{}
			var output bytes.Buffer
			command.SetOut(&output)
			require.NoError(t, renderDaemonStatus(
				command,
				test.observation,
				true,
			))
			var got map[string]any
			require.NoError(t, json.NewDecoder(&output).Decode(&got))
			assert.Equal(t, test.wantState, got["state"])
			assert.Equal(t, test.wantPID, got["pid"])
		})
	}
}

func TestDaemonStopIsQuietlyIdempotentWhenAbsent(t *testing.T) {
	oldFactory := newDaemonController
	t.Cleanup(func() { newDaemonController = oldFactory })
	newDaemonController = func() (daemonController, error) {
		return &fakeDaemonController{}, nil
	}
	require.NoError(t, runDaemonStop(&cobra.Command{}, nil))
}
