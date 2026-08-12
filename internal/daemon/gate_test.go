package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestGateRejectsNewWorkDuringDrainWithDeadline(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	gate := NewGate(now)
	deadline := now.Add(5 * time.Minute)
	gate.BeginDrain(deadline)

	_, err := gate.Reserve(ReservationWork, now)
	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.DaemonDraining, typed.Code)
	assert.True(t, typed.Retryable)
	assert.Equal(t, deadline, typed.Details["drain_deadline"])
}

func TestGateWaitsForReservationsOrDrainDeadline(t *testing.T) {
	now := time.Now()
	gate := NewGate(now)
	release, err := gate.Reserve(ReservationLease, now)
	require.NoError(t, err)
	gate.BeginDrain(now.Add(time.Minute))

	result := make(chan DrainResult, 1)
	go func() { result <- gate.WaitForDrain(context.Background(), now) }()
	select {
	case <-result:
		require.FailNow(t, "drain completed while a lease was active")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	assert.Equal(t, DrainReleased, <-result)
}

func TestGateDeadlineInvalidatesReservations(t *testing.T) {
	now := time.Now()
	gate := NewGate(now)
	_, err := gate.Reserve(ReservationLease, now)
	require.NoError(t, err)
	gate.BeginDrain(now.Add(-time.Millisecond))
	assert.Equal(t, DrainDeadline, gate.WaitForDrain(context.Background(), now))
}

func TestGateWaitForReleaseContinuesAfterDrainDeadline(t *testing.T) {
	now := time.Now()
	gate := NewGate(now)
	release, err := gate.Reserve(ReservationWork, now)
	require.NoError(t, err)
	gate.BeginDrain(now.Add(-time.Millisecond))
	assert.Equal(t, DrainDeadline, gate.WaitForDrain(context.Background(), now))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- gate.WaitForRelease(ctx) }()
	select {
	case <-result:
		t.Fatal("cleanup wait returned while work remained active")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	assert.True(t, <-result)
}

func TestGateIdleDecisionIgnoresHealthAndWarmResources(t *testing.T) {
	now := time.Now()
	gate := NewGate(now)
	gate.Touch(now)
	assert.False(t, gate.ShouldStopForIdle(now.Add(time.Hour), 2*time.Hour))
	assert.True(t, gate.ShouldStopForIdle(now.Add(2*time.Hour), 2*time.Hour))
	assert.False(t, gate.ShouldStopForIdle(now.Add(24*time.Hour), 0))
}
