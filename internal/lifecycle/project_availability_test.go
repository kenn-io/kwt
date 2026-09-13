package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
)

func TestProjectInventoryRetainsRenamedDirectoryAndClearsIssueWhenRestored(t *testing.T) {
	root := t.TempDir()
	oldPath, newPath := filepath.Join(root, "original"), filepath.Join(root, "renamed")
	require.NoError(t, os.Mkdir(oldPath, 0o700))
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(
		"[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'Widget'\npath = %q\n", oldPath,
	)), 0o600))
	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	registration := snapshot.Projects[0]
	before, err := publishedProjectRegistrations(context.Background(), []config.ProjectRegistration{registration})
	require.NoError(t, err)
	require.Len(t, before, 1)
	assert.Empty(t, before[0].PathIssue)

	require.NoError(t, os.Rename(oldPath, newPath))
	missing, err := publishedProjectRegistrations(context.Background(), []config.ProjectRegistration{registration})
	require.NoError(t, err)
	require.Len(t, missing, 1)
	assert.Equal(t, "missing", missing[0].PathIssue)
	assert.Equal(t, before[0].RegistrationFingerprint, missing[0].RegistrationFingerprint)
	assert.Equal(t, oldPath, missing[0].Path)

	require.NoError(t, os.Rename(newPath, oldPath))
	restored, err := publishedProjectRegistrations(context.Background(), []config.ProjectRegistration{registration})
	require.NoError(t, err)
	assert.Equal(t, before, restored)
}
