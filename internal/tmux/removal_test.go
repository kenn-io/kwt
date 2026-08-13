package tmux

import (
	"context"
	"testing"

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
