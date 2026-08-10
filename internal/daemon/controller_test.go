package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func TestReplacementDecision(t *testing.T) {
	for _, test := range []struct {
		name           string
		client         Build
		running        Build
		advertisedTime bool
		policy         string
		want           replacementDecision
	}{
		{
			name:   "newer semantic client replaces",
			client: Build{Version: "v1.2.0"}, running: Build{Version: "v1.1.0"},
			policy: "newer", want: replaceDaemon,
		},
		{
			name:   "older semantic client reuses",
			client: Build{Version: "v1.1.0"}, running: Build{Version: "v1.2.0"},
			policy: "newer", want: reuseDaemon,
		},
		{
			name:           "newer SHA timestamp replaces",
			client:         Build{Version: "sha-new", Revision: "new", RevisionTime: "2026-08-09T12:00:01Z"},
			running:        Build{Version: "sha-old", Revision: "old", RevisionTime: "2026-08-09T12:00:00Z"},
			advertisedTime: true, policy: "newer", want: replaceDaemon,
		},
		{
			name:           "unknown order reuses",
			client:         Build{Version: "sha-new", Revision: "new", RevisionTime: "2026-08-09T12:00:00Z"},
			running:        Build{Version: "sha-old", Revision: "old", RevisionTime: "2026-08-09T12:00:00Z"},
			advertisedTime: true, policy: "newer", want: reuseDaemon,
		},
		{
			name:   "policy never reuses",
			client: Build{Version: "v1.2.0"}, running: Build{Version: "v1.1.0"},
			policy: "never", want: reuseDaemon,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(
				t,
				test.want,
				decideReplacement(
					test.client,
					test.running,
					test.advertisedTime,
					test.policy,
				),
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
		Home: t.TempDir(),
		Build: Build{
			Version:  "v1.2.0",
			Revision: strings.Repeat("a", 40),
		},
		Config: models.DaemonConfig{
			AutoRestart:      "newer",
			ReplacementGrace: 50 * time.Millisecond,
		},
		PollInterval: time.Millisecond,
		StartTimeout: time.Second,
	}
}

func matchingBuildStatus(options ControllerOptions) Status {
	return Status{
		Version:      options.Build.Version,
		Revision:     options.Build.Revision,
		RevisionTime: options.Build.RevisionTime,
	}
}

func TestControllerStartsAbsentDaemonAndWaitsForReady(t *testing.T) {
	options := testControllerOptions(t)
	ready := Observation{State: RuntimeReady, Status: Status{Version: "v1.2.0"}}
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeAbsent},
		Observation{State: RuntimeStarting},
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

func TestControllerLaunchFailsWhenDaemonReportsFailed(t *testing.T) {
	options := testControllerOptions(t)
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeAbsent},
		Observation{State: RuntimeFailed},
	)
	options.Launch = func(context.Context) error { return nil }

	_, err := NewController(options).Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed state")
}

func TestControllerWaitsForExistingStartingDaemon(t *testing.T) {
	options := testControllerOptions(t)
	ready := Observation{State: RuntimeReady, Status: Status{Version: "v1.2.0"}}
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeStarting},
		Observation{State: RuntimeStarting},
		ready,
	)
	launched := false
	options.Launch = func(context.Context) error {
		launched = true
		return nil
	}

	got, err := NewController(options).Start(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ready, got)
	assert.False(t, launched)
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
	) (Status, error) {
		shutdownReason = reason
		return Status{}, nil
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

func TestControllerReplacesOlderSHAStampedDaemon(t *testing.T) {
	options := testControllerOptions(t)
	options.Build = Build{
		Version: "sha-new", Revision: "new", RevisionTime: "2026-08-09T12:00:01Z",
	}
	old := Observation{
		State: RuntimeReady,
		Status: Status{
			Version: "sha-old", Revision: "old", RevisionTime: "2026-08-09T12:00:00Z",
		},
		Record: runtimeRecordWithRevisionTime("2026-08-09T12:00:00Z"),
	}
	ready := Observation{State: RuntimeReady, Status: matchingBuildStatus(options)}
	options.Inspect = scriptedInspector(
		t,
		old,
		Observation{State: RuntimeAbsent},
		ready,
	)
	shutdown := false
	deadline := time.Now().Add(time.Second)
	var progress bytes.Buffer
	options.Progress = &progress
	options.RequestShutdown = func(context.Context, Observation, string) (Status, error) {
		shutdown = true
		return Status{
			State: StateDraining, ActiveLeases: 2, DrainDeadline: &deadline,
		}, nil
	}
	options.Launch = func(context.Context) error { return nil }

	got, err := NewController(options).Start(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ready, got)
	assert.True(t, shutdown)
	assert.Contains(t, progress.String(), "draining, waiting on 2 leases")
}

func TestControllerReportsDrainProgressBeforeStarting(t *testing.T) {
	options := testControllerOptions(t)
	deadline := time.Now().Add(time.Second)
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeDraining, Status: Status{
			Version:       options.Build.Version,
			Revision:      options.Build.Revision,
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

func TestControllerStopRequestsShutdownForNonReadyOwner(t *testing.T) {
	for name, state := range map[string]RuntimeState{
		"starting": RuntimeStarting,
		"failed":   RuntimeFailed,
	} {
		t.Run(name, func(t *testing.T) {
			options := testControllerOptions(t)
			options.Inspect = scriptedInspector(
				t,
				Observation{State: state},
				Observation{State: RuntimeAbsent},
			)
			shutdown := false
			options.RequestShutdown = func(
				_ context.Context,
				_ Observation,
				_ string,
			) (Status, error) {
				shutdown = true
				return Status{}, nil
			}

			require.NoError(t, NewController(options).Stop(context.Background()))
			assert.True(t, shutdown)
		})
	}
}

func TestControllerStopUsesRunningDaemonDrainDeadline(t *testing.T) {
	options := testControllerOptions(t)
	options.Config.ReplacementGrace = time.Millisecond
	options.CleanupAllowance = time.Millisecond
	deadline := time.Now().Add(50 * time.Millisecond)
	draining := Observation{State: RuntimeDraining, Status: Status{
		State:         StateDraining,
		DrainDeadline: &deadline,
	}}
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeReady},
		draining,
		draining,
		draining,
		draining,
		draining,
		Observation{State: RuntimeAbsent},
	)
	options.RequestShutdown = func(
		_ context.Context,
		_ Observation,
		_ string,
	) (Status, error) {
		return Status{}, nil
	}

	require.NoError(t, NewController(options).Stop(context.Background()))
}

func TestControllerRestartStopsThenStartsInvokingBinary(t *testing.T) {
	options := testControllerOptions(t)
	ready := Observation{State: RuntimeReady, Status: matchingBuildStatus(options)}
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeReady, Status: matchingBuildStatus(options)},
		Observation{State: RuntimeAbsent},
		Observation{State: RuntimeAbsent},
		ready,
	)
	reasons := make([]string, 0, 1)
	options.RequestShutdown = func(
		_ context.Context,
		_ Observation,
		reason string,
	) (Status, error) {
		reasons = append(reasons, reason)
		return Status{}, nil
	}
	options.Launch = func(context.Context) error { return nil }
	got, err := NewController(options).Restart(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"restart"}, reasons)
	assert.Equal(t, "v1.2.0", got.Status.Version)
}

func TestControllerRestartRecoversFailedDaemon(t *testing.T) {
	options := testControllerOptions(t)
	ready := Observation{State: RuntimeReady, Status: matchingBuildStatus(options)}
	options.Inspect = scriptedInspector(
		t,
		Observation{State: RuntimeFailed, Status: matchingBuildStatus(options)},
		Observation{State: RuntimeAbsent},
		Observation{State: RuntimeAbsent},
		ready,
	)
	shutdown := false
	options.RequestShutdown = func(
		_ context.Context,
		_ Observation,
		_ string,
	) (Status, error) {
		shutdown = true
		return Status{}, nil
	}
	options.Launch = func(context.Context) error { return nil }

	got, err := NewController(options).Restart(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ready, got)
	assert.True(t, shutdown)
}

func TestControllerRestartRejectsUnknownBuildOrder(t *testing.T) {
	options := testControllerOptions(t)
	options.Build = Build{
		Version: "sha-new", Revision: "new", RevisionTime: "2026-08-09T12:00:00Z",
	}
	options.Inspect = scriptedInspector(t, Observation{
		State: RuntimeReady,
		Status: Status{
			Version: "sha-old", Revision: "old", RevisionTime: "2026-08-09T12:00:00Z",
		},
		Record: runtimeRecordWithRevisionTime("2026-08-09T12:00:00Z"),
	})
	shutdown := false
	options.RequestShutdown = func(context.Context, Observation, string) (Status, error) {
		shutdown = true
		return Status{}, nil
	}

	_, err := NewController(options).Restart(context.Background())
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.DaemonBuildOrderUnknown))
	assert.False(t, shutdown)
}

func TestControllerUnknownBuildCannotReplaceDrainingDaemon(t *testing.T) {
	for name, act := range map[string]func(*Controller) error{
		"start": func(controller *Controller) error {
			_, err := controller.Start(context.Background())
			return err
		},
		"restart": func(controller *Controller) error {
			_, err := controller.Restart(context.Background())
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := testControllerOptions(t)
			options.Build = Build{
				Version: "sha-new", Revision: "new", RevisionTime: "2026-08-09T12:00:00Z",
			}
			options.Inspect = scriptedInspector(t, Observation{
				State: RuntimeDraining,
				Status: Status{
					Version: "sha-old", Revision: "old", RevisionTime: "2026-08-09T12:00:00Z",
				},
				Record: runtimeRecordWithRevisionTime("2026-08-09T12:00:00Z"),
			})
			launched := false
			options.Launch = func(context.Context) error {
				launched = true
				return nil
			}

			err := act(NewController(options))
			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.DaemonBuildOrderUnknown))
			assert.False(t, launched)
		})
	}
}

func runtimeRecordWithRevisionTime(value string) kitdaemon.RuntimeRecord {
	return kitdaemon.RuntimeRecord{Metadata: map[string]string{metadataRevisionTime: value}}
}

func TestControllerRestartRefusesToDowngradeNewerDaemon(t *testing.T) {
	options := testControllerOptions(t)
	options.Inspect = scriptedInspector(t, Observation{
		State:  RuntimeReady,
		Status: Status{Version: "v1.3.0"},
	})
	shutdown := false
	options.RequestShutdown = func(
		_ context.Context,
		_ Observation,
		_ string,
	) (Status, error) {
		shutdown = true
		return Status{}, nil
	}

	_, err := NewController(options).Restart(context.Background())
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.DaemonDowngradeRefused))
	assert.False(t, shutdown)
}

func TestControllerOlderVersionCannotReplaceDrainingNewerDaemon(t *testing.T) {
	for name, act := range map[string]func(*Controller) error{
		"start": func(controller *Controller) error {
			_, err := controller.Start(context.Background())
			return err
		},
		"restart": func(controller *Controller) error {
			_, err := controller.Restart(context.Background())
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := testControllerOptions(t)
			options.Inspect = scriptedInspector(t, Observation{
				State:  RuntimeDraining,
				Status: Status{Version: "v1.3.0"},
			})
			launched := false
			options.Launch = func(context.Context) error {
				launched = true
				return nil
			}

			err := act(NewController(options))
			require.Error(t, err)
			assert.False(t, launched)
		})
	}
}
