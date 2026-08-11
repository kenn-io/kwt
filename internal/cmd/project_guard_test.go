package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/service"
)

func TestGuardedProjectOperationRejectsRemovedRegistration(t *testing.T) {
	home := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = '"+projectPath+"'\n"),
		0o600,
	))
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	guard, err := observeGuardedProjectOperation(
		context.Background(), home, projectPath, expansion,
	)
	require.NoError(t, err)

	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	changed, err := config.CompareAndSwapProjectAt(
		home, snapshot.Projects[0], nil,
	)
	require.NoError(t, err)
	require.True(t, changed)
	called := false

	err = guard.run(context.Background(), func() error {
		called = true
		return nil
	})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, called)
}
