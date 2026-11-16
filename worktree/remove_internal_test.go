package worktree

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuotePowerShellPathPreservesQuoteCharacters(t *testing.T) {
	path := `C:\worktrees\owner's ‘left’ ‚low‛ $(echo untouched)`
	require.Equal(t, `'C:\worktrees\owner''s ‘‘left’’ ‚‚low‛‛ $(echo untouched)'`, quotePowerShellPath(path))
}
