package tmux

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestRemovalSessionInspectionPreservesCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{
			name: "canceled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.ctx()
			defer cancel()
			guard := &removalSessionGuard{
				command: "tmux",
				inspect: func(context.Context, *TmuxCommand) (string, string, error) {
					return "", "no server running on /tmp/tmux/default", errors.New("tmux exited")
				},
			}

			lease, err := guard.Quiesce(ctx, RemovalSessionCondition{
				SessionName: "topic", Absent: true,
			})

			assert.Nil(t, lease)
			assert.ErrorIs(t, err, test.want)
		})
	}
}
