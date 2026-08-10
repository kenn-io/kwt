package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestDescriptorIsTheTransportNeutralErrorValue(t *testing.T) {
	descriptor := service.Descriptor{
		Code: service.DaemonDraining, Message: "the kwt daemon is draining", Retryable: true,
		Details: map[string]any{"drain_deadline": "2026-08-10T01:02:03Z"},
	}
	err := service.NewDescriptorError(descriptor, context.DeadlineExceeded)

	assert.Equal(t, descriptor, err.Descriptor)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, service.DaemonDraining, err.Code)
}

func TestErrorPreservesCategoryRetryabilityAndCause(t *testing.T) {
	cause := errors.New("locked")
	deadline := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	err := service.NewError(service.Busy, "daemon is draining", true,
		map[string]any{"drain_deadline": deadline}, cause)

	assert.ErrorIs(t, err, cause)
	assert.Equal(t, service.Busy, err.Code)
	assert.True(t, err.Retryable)
	assert.Equal(t, deadline, err.Details["drain_deadline"])
}

func TestAsErrorNormalizesUnexpectedFailures(t *testing.T) {
	got := service.AsError(errors.New("boom"))
	require.NotNil(t, got)
	assert.Equal(t, service.Internal, got.Code)
	assert.False(t, got.Retryable)
}

func TestIsCodeMatchesWrappedTypedErrors(t *testing.T) {
	err := fmt.Errorf("request failed: %w", service.NewError(service.Busy, "busy", true, nil, nil))
	assert.True(t, service.IsCode(err, service.Busy))
	assert.False(t, service.IsCode(err, service.Conflict))
}
