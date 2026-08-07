package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

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
