package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/require"
	gitadapter "go.kenn.io/kwt/internal/git"
	shared "go.kenn.io/kwt/worktree"
)

func openSharedWorktrees(t *testing.T, g *gitadapter.Git) *shared.Repository {
	t.Helper()
	repo, err := g.WorktreeRepository(t.Context(), nil)
	require.NoError(t, err)
	return repo
}
