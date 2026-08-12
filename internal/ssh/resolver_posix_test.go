//go:build !windows

package ssh

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestResolverPOSIXUsesLoginShellFrameAndExplicitTarget(t *testing.T) {
	var gotArgv, gotEnvironment []string
	resolver := NewResolver(ResolverOptions{
		Executable: "/usr/bin/ssh",
		LoginShell: func() (string, error) { return "/bin/zsh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Environment: []string{
			"HOME=/Users/operator", "PATH=/usr/bin:/bin",
		},
		Run: func(_ context.Context, argv, environment []string) ([]byte, []byte, int, error) {
			gotArgv = append([]string(nil), argv...)
			gotEnvironment = append([]string(nil), environment...)
			return []byte("banner\nKWT_SSH_CONFIG_START_nonce\n" +
					"user deploy\nhostname build.internal\nport 2200\n" +
					"KWT_SSH_CONFIG_END_nonce\ntrailer\n"),
				[]byte("KWT_SSH_CONFIG_START_nonce is stderr only"), 0, nil
		},
	})

	observation, err := resolver.Resolve(context.Background(), ResolveRequest{Target: Target{
		User: "deploy", Hostname: "build.example.test", Port: 2200,
	}})
	require.NoError(t, err)
	require.Len(t, observation.route, 1)
	assert.Equal(t, "build.internal", observation.route[0].Config.Hostname)
	assert.Equal(t, []string{"/bin/zsh", "-lc"}, gotArgv[:2])
	assert.Contains(t, gotArgv[2], "'/usr/bin/ssh' '-G' '-l' 'deploy' '-p' '2200' '--' 'build.example.test'")
	assert.Equal(t, []string{"HOME=/Users/operator", "PATH=/usr/bin:/bin"}, gotEnvironment)
}

func TestResolverPOSIXResolvesProxyJumpInConnectionOrder(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	resolver := NewResolver(ResolverOptions{
		LoginShell: func() (string, error) { return "/bin/sh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Run: func(_ context.Context, argv, _ []string) ([]byte, []byte, int, error) {
			command := argv[2]
			mu.Lock()
			commands = append(commands, command)
			mu.Unlock()
			config := ""
			switch {
			case strings.Contains(command, "'relay.example.test'"):
				config = "user relay\nhostname relay.internal\nport 22\n"
			case strings.Contains(command, "'core.example.test'"):
				config = "user core\nhostname core.internal\nport 2222\n"
			default:
				config = "user deploy\nhostname build.internal\nport 22\n" +
					"proxyjump relay.example.test,core.example.test\n"
			}
			return framedResolverOutput("nonce", config), nil, 0, nil
		},
	})

	observation, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	require.Len(t, observation.route, 3)
	assert.Equal(t, "relay.example.test", observation.route[0].Target.Hostname)
	assert.Equal(t, "core.example.test", observation.route[1].Target.Hostname)
	assert.Equal(t, "build.example.test", observation.route[2].Target.Hostname)
	assert.Len(t, commands, 3)
}

func TestResolverPOSIXFailsClosedForUnreviewableRoutes(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
	}{
		{name: "proxy command", config: "user deploy\nhostname build.internal\nproxycommand ssh relay -W %h:%p\n"},
		{name: "nested jump", config: "user deploy\nhostname build.internal\nproxyjump relay.example.test\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := NewResolver(ResolverOptions{
				LoginShell: func() (string, error) { return "/bin/sh", nil },
				Nonce:      func() (string, error) { return "nonce", nil },
				Run: func(_ context.Context, argv, _ []string) ([]byte, []byte, int, error) {
					if test.name == "nested jump" && strings.Contains(argv[2], "'relay.example.test'") {
						return framedResolverOutput("nonce", "hostname relay.internal\nproxyjump edge.example.test\n"), nil, 0, nil
					}
					return framedResolverOutput("nonce", test.config), nil, 0, nil
				},
			})
			_, err := resolver.Resolve(context.Background(), ResolveRequest{
				Target: Target{Hostname: "build.example.test"},
			})
			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.SSHRouteUnreviewable))
		})
	}
}

func TestResolverPOSIXRejectsTargetBeforeInvocation(t *testing.T) {
	invoked := false
	resolver := NewResolver(ResolverOptions{
		Run: func(context.Context, []string, []string) ([]byte, []byte, int, error) {
			invoked = true
			return nil, nil, 0, nil
		},
	})
	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{User: "$(id)", Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHInvalidTarget))
	assert.False(t, invoked)
}

func TestResolverPOSIXPreservesCancellationAndRejectsMalformedFrames(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		resolver := NewResolver(ResolverOptions{
			LoginShell: func() (string, error) { return "/bin/sh", nil },
			Nonce:      func() (string, error) { return "nonce", nil },
			Run: func(context.Context, []string, []string) ([]byte, []byte, int, error) {
				return nil, nil, -1, context.Canceled
			},
		})
		_, err := resolver.Resolve(context.Background(), ResolveRequest{
			Target: Target{Hostname: "build.example.test"},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.True(t, service.IsCode(err, service.SSHResolutionFailed))
	})

	t.Run("malformed frame", func(t *testing.T) {
		resolver := NewResolver(ResolverOptions{
			LoginShell: func() (string, error) { return "/bin/sh", nil },
			Nonce:      func() (string, error) { return "nonce", nil },
			Run: func(context.Context, []string, []string) ([]byte, []byte, int, error) {
				return []byte("hostname build.internal\n"), nil, 0, nil
			},
		})
		_, err := resolver.Resolve(context.Background(), ResolveRequest{
			Target: Target{Hostname: "build.example.test"},
		})
		require.Error(t, err)
		assert.True(t, service.IsCode(err, service.SSHResolutionFailed))
	})
}

func TestResolverPOSIXPreservesDeadlineCause(t *testing.T) {
	resolver := NewResolver(ResolverOptions{
		LoginShell: func() (string, error) { return "/bin/sh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Run: func(context.Context, []string, []string) ([]byte, []byte, int, error) {
			return nil, nil, -1, context.DeadlineExceeded
		},
	})
	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func framedResolverOutput(nonce, config string) []byte {
	return []byte("KWT_SSH_CONFIG_START_" + nonce + "\n" + config +
		"KWT_SSH_CONFIG_END_" + nonce + "\n")
}
