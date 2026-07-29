package cmd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/registry"
)

func TestAcknowledgeRemoteSourcePathDoesNotBlockOpenWhenRegistryUnavailable(
	t *testing.T,
) {
	previous := newRemoteSourceRegistry
	t.Cleanup(func() { newRemoteSourceRegistry = previous })
	newRemoteSourceRegistry = func() (*registry.Registry, error) {
		return nil, errors.New("registry unavailable")
	}

	require.NoError(t, acknowledgeRemoteSourcePath("/worktrees/ordinary"))
}
