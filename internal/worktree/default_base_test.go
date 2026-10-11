package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	gitadapter "go.kenn.io/kwt/internal/git"
)

// Git before 2.31 rejects `worktree list --expire`. Default-base selection must
// stay within kwt's documented Git 2.20 baseline.
func rejectWorktreeListExpire(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the Git shim is a POSIX shell script")
	}
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shim := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = worktree ] && [ \"$2\" = list ]; then\n" +
		"  for arg in \"$@\"; do\n" +
		"    if [ \"$arg\" = --expire ]; then echo \"error: unknown option \\`expire'\" >&2; exit 129; fi\n" +
		"  done\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755))
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDefaultWorktreeBaseUsesPrimaryBranch(t *testing.T) {
	for _, layout := range []string{"checkout-before-git-2.31", "bare"} {
		t.Run(layout, func(t *testing.T) {
			repo := NewTestRepository(t)
			gitOutput(t, repo.Path, "branch", "-m", "main", "trunk")
			path := repo.Path
			if layout == "bare" {
				path = filepath.Join(t.TempDir(), "repo.git")
				gitOutput(t, repo.Path, "clone", "--bare", repo.Path, path)
				gitOutput(t, path, "remote", "remove", "origin")
			} else {
				rejectWorktreeListExpire(t)
			}
			execution, err := gitadapter.New(path).WorktreeExecution(nil, nil)
			require.NoError(t, err)
			repository, err := gitadapter.OpenWorktrees(t.Context(), execution)
			require.NoError(t, err)

			base, err := defaultWorktreeBase(t.Context(), repository, execution)

			require.NoError(t, err)
			require.Equal(t, "refs/heads/trunk", base)
		})
	}
}
