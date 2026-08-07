package daemon

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func TestReplacementDecision(t *testing.T) {
	for _, test := range []struct {
		name    string
		client  string
		running string
		policy  string
		want    replacementDecision
	}{
		{"newer client replaces", "v1.2.0", "v1.1.0", "newer", replaceDaemon},
		{"older client reuses", "v1.1.0", "v1.2.0", "newer", reuseDaemon},
		{"same client reuses", "v1.2.0", "v1.2.0", "newer", reuseDaemon},
		{"policy never reuses", "v1.2.0", "v1.1.0", "never", reuseDaemon},
		{"development versions do not order", "dev", "v1.1.0", "newer", reuseDaemon},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(
				t,
				test.want,
				decideReplacement(test.client, test.running, test.policy),
			)
		})
	}
}

func scriptedInspector(
	t *testing.T,
	values ...Observation,
) func(context.Context) (Observation, error) {
	t.Helper()
	index := 0
	return func(context.Context) (Observation, error) {
		if index >= len(values) {
			require.FailNow(t, "unexpected extra inspection")
		}
		value := values[index]
		index++
		return value, nil
	}
}

func testControllerOptions(t *testing.T) ControllerOptions {
	t.Helper()
	return ControllerOptions{
		Home:  t.TempDir(),
		Build: Build{Version: "v1.2.0"},
		Config: models.DaemonConfig{
			AutoRestart:      "newer",
			ReplacementGrace: 50 * time.Millisecond,
		},
		PollInterval: time.Millisecond,
		StartTimeout: time.Second,
	}
}

func TestControllerStartsAbsentDaemonAndWaitsForReady(t *testing.T) {
	options := testControllerOptions(t)
	ready := Observation{State: RuntimeReady, Status: Status{Version: "v1.2.0"}}
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeAbsent},
		ready,
	)
	launches := 0
	options.Launch = func(context.Context) error {
		launches++
		return nil
	}
	got, err := NewController(options).Start(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ready.Status.Version, got.Status.Version)
	assert.Equal(t, 1, launches)
}

func TestControllerReusesCompatibleNewerDaemon(t *testing.T) {
	options := testControllerOptions(t)
	options.Inspect = scriptedInspector(t, Observation{
		State:  RuntimeReady,
		Status: Status{Version: "v1.3.0"},
	})
	launches := 0
	options.Launch = func(context.Context) error {
		launches++
		return nil
	}
	got, err := NewController(options).Start(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "v1.3.0", got.Status.Version)
	assert.Zero(t, launches)
}

func TestControllerReplacesOlderDaemonAfterDrain(t *testing.T) {
	options := testControllerOptions(t)
	old := Observation{State: RuntimeReady, Status: Status{Version: "v1.1.0"}}
	ready := Observation{State: RuntimeReady, Status: Status{Version: "v1.2.0"}}
	options.Inspect = scriptedInspector(
		t,
		old,
		Observation{State: RuntimeAbsent},
		ready,
	)
	var shutdownReason string
	options.RequestShutdown = func(
		_ context.Context,
		_ Observation,
		reason string,
	) error {
		shutdownReason = reason
		return nil
	}
	launches := 0
	options.Launch = func(context.Context) error {
		launches++
		return nil
	}
	_, err := NewController(options).Start(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "replacement", shutdownReason)
	assert.Equal(t, 1, launches)
}

func TestControllerReportsDrainProgressBeforeStarting(t *testing.T) {
	options := testControllerOptions(t)
	deadline := time.Now().Add(time.Second)
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeDraining, Status: Status{
			State:         StateDraining,
			ActiveLeases:  3,
			DrainDeadline: &deadline,
		}},
		Observation{State: RuntimeAbsent},
		Observation{State: RuntimeReady, Status: Status{Version: "v1.2.0"}},
	)
	var progress bytes.Buffer
	options.Progress = &progress
	options.Launch = func(context.Context) error { return nil }
	_, err := NewController(options).Start(context.Background())
	require.NoError(t, err)
	assert.Contains(t, progress.String(), "draining, waiting on 3 leases")
	assert.Contains(t, progress.String(), deadline.Format(time.RFC3339))
}

func TestControllerRefusesIncompatibleOrUnresponsiveOwner(t *testing.T) {
	for name, state := range map[string]RuntimeState{
		"incompatible": RuntimeIncompatible,
		"unresponsive": RuntimeUnresponsive,
	} {
		t.Run(name, func(t *testing.T) {
			options := testControllerOptions(t)
			options.Inspect = scriptedInspector(t, Observation{State: state})
			launched := false
			options.Launch = func(context.Context) error {
				launched = true
				return nil
			}
			_, err := NewController(options).Start(context.Background())
			require.Error(t, err)
			assert.False(t, launched)
		})
	}
}

func TestControllerStopAbsentIsIdempotent(t *testing.T) {
	options := testControllerOptions(t)
	options.Inspect = scriptedInspector(t, Observation{State: RuntimeAbsent})
	require.NoError(t, NewController(options).Stop(context.Background()))
}

func TestControllerRestartStopsThenStartsInvokingBinary(t *testing.T) {
	options := testControllerOptions(t)
	ready := Observation{State: RuntimeReady, Status: Status{Version: "v1.2.0"}}
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeReady, Status: Status{Version: "v1.3.0"}},
		Observation{State: RuntimeAbsent},
		Observation{State: RuntimeAbsent},
		ready,
	)
	reasons := make([]string, 0, 1)
	options.RequestShutdown = func(
		_ context.Context,
		_ Observation,
		reason string,
	) error {
		reasons = append(reasons, reason)
		return nil
	}
	options.Launch = func(context.Context) error { return nil }
	got, err := NewController(options).Restart(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"restart"}, reasons)
	assert.Equal(t, "v1.2.0", got.Status.Version)
}
