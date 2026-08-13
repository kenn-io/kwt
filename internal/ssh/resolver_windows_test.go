//go:build windows

package ssh

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolverWindowsInvokesOpenSSHDirectly(t *testing.T) {
	var gotArgv []string
	resolver := NewResolver(ResolverOptions{
		Executable: "ssh.exe",
		Run: func(_ context.Context, argv []string, _ string, _ []string, _ []byte) ([]byte, []byte, int, error) {
			gotArgv = append([]string(nil), argv...)
			return []byte("user deploy\nhostname build.internal\nport 2200\n"), nil, 0, nil
		},
	})

	observation, err := resolver.Resolve(context.Background(), ResolveRequest{Target: Target{
		User: "deploy", Hostname: "build.example.test", Port: 2200,
	}})
	require.NoError(t, err)
	require.Len(t, observation.route, 1)
	assert.Equal(t, []string{
		"ssh.exe", "-G", "-l", "deploy", "-p", "2200", "--", "build.example.test",
	}, gotArgv)
}
