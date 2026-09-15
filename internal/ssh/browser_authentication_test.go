package ssh

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestBrowserAuthenticationStream(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	var prompts []service.OperationPrompt
	w := newBrowserAuthenticationWriter(ctx,
		LeaseRequest{promptTargetIndex: 1, promptTargetCount: 2,
			Prompt: func(_ context.Context, p service.OperationPrompt) (string, error) {
				prompts = append(prompts, p)
				return "", nil
			}}, ResolvedTarget{DisplayTarget: "relay.example.test"}, cancel)
	defer w.close()
	for _, chunk := range []string{
		"ordinary banner\n# To authenticate, visit: http://login.tailscale.com/a/example\n",
		"# To authenticate, visit: https://example.test/a/example\n",
		"# To authenticate, vi", "sit: https://login.tailscale.com/a/example\r", "\n",
		"# To authenticate, visit: https://login.tailscale.com/a/example\n",
	} {
		_, err := w.Write([]byte(chunk))
		require.NoError(t, err)
	}
	require.Len(t, prompts, 1)
	assert.Equal(t, 1, prompts[0].Details["hop_index"])
	assert.Equal(t, 2, prompts[0].Details["hop_count"])
	assert.Equal(t, "relay.example.test", prompts[0].Details["display_target"])
}

func TestBrowserAuthenticationCancellation(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "noninteractive", true: "cancelled"}[present], func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			request := LeaseRequest{}
			if present {
				request.Prompt = func(context.Context, service.OperationPrompt) (string, error) {
					return "", context.Canceled
				}
			}
			w := newBrowserAuthenticationWriter(ctx, request, ResolvedTarget{}, cancel)
			defer w.close()
			_, err := w.Write([]byte("# To authenticate, visit: https://login.tailscale.com/a/example\n"))
			require.Error(t, err)
			require.ErrorIs(t, context.Cause(ctx), err)
			if present {
				assert.ErrorIs(t, err, context.Canceled)
			} else {
				assert.Equal(t, service.SSHInteractionRequired, service.AsError(err).Code)
			}
		})
	}
}

func TestBrowserAuthenticationExpiresAfterAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		w := newBrowserAuthenticationWriter(ctx,
			LeaseRequest{Prompt: func(context.Context, service.OperationPrompt) (string, error) {
				return "", nil
			}}, ResolvedTarget{}, cancel)
		defer w.close()
		_, err := w.Write([]byte("# To authenticate, visit: https://login.tailscale.com/a/example\n"))
		require.NoError(t, err)
		<-ctx.Done()
		assert.Equal(t, service.SSHPromptTimedOut, service.AsError(context.Cause(ctx)).Code)
		assert.False(t, errors.Is(context.Cause(ctx), context.Canceled))
	})
}

func TestBrowserAuthenticationExpiresBeforeAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		w := newBrowserAuthenticationWriter(ctx, LeaseRequest{Prompt: func(ctx context.Context, _ service.OperationPrompt) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}}, ResolvedTarget{}, cancel)
		defer w.close()
		_, err := w.Write([]byte("# To authenticate, visit: https://login.tailscale.com/a/example\n"))
		require.Error(t, err)
		assert.Equal(t, service.SSHPromptTimedOut, service.AsError(err).Code)
	})
}
