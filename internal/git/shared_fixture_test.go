package git

import (
	"testing"

	"github.com/stretchr/testify/require"
	shared "go.kenn.io/kwt/worktree"
)

func openSharedWorktrees(t *testing.T, g *Git) *shared.Repository {
	t.Helper()
	repo, err := g.WorktreeRepository(t.Context(), nil)
	require.NoError(t, err)
	return repo
}
func inspectSharedWorktrees(t *testing.T, g *Git) ([]shared.Entry, error) {
	t.Helper()
	inventory, err := openSharedWorktrees(t, g).Inspect(t.Context())
	return inventory.Entries, err
}
