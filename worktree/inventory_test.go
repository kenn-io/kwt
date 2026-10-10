package worktree_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	"go.kenn.io/kwt/worktree"
)

func TestIdentityUsesRegisteredBacklinkAfterCheckoutDamage(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	identity := worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true}
	generation, err := repo.EnsureIdentity(t.Context(), path, identity)
	require.NoError(t, err)
	registration := git(t, path, "rev-parse", "--absolute-git-dir")
	other := filepath.Join(t.TempDir(), "other")
	git(t, root, "worktree", "add", "-b", "other", other)
	otherRegistration := git(t, other, "rev-parse", "--absolute-git-dir")
	require.NoError(t, os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+otherRegistration+"\n"), 0o600))
	got, err := repo.ReadIdentity(t.Context(), path, identity.FileName)
	require.NoError(t, err)
	require.Equal(t, generation, got)
	require.NoError(t, os.Remove(filepath.Join(path, ".git")))
	relative, err := filepath.Rel(registration, filepath.Join(path, ".git"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(registration, "gitdir"), []byte(relative+"\n"), 0o600))
	got, err = repo.ReadIdentity(t.Context(), path, identity.FileName)
	require.NoError(t, err)
	require.Equal(t, generation, got)
}

func TestIdentityDoesNotAdoptAnotherRepositoryAtRecordedPath(t *testing.T) {
	root, path := fixture(t)
	other, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	identity := worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true}
	_, err := repo.EnsureIdentity(t.Context(), path, identity)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(path))
	git(t, other, "worktree", "add", "-b", "replacement", path)
	_, err = repo.List(t.Context(), identity)
	require.ErrorIs(t, err, worktree.ErrWorktreeRepositoryMismatch)
	require.True(t, worktree.IsIncompleteInventory(err))
	registration := git(t, path, "rev-parse", "--absolute-git-dir")
	require.NoFileExists(t, filepath.Join(registration, identity.FileName))
}

func TestIdentityRejectsAmbiguousOrIncompleteRegistration(t *testing.T) {
	for _, variant := range []string{"duplicate", "missing backlink", "malformed backlink", "non-directory"} {
		t.Run(variant, func(t *testing.T) {
			root, path := fixture(t)
			repo := open(t, root, kwtPolicy())
			broken := filepath.Join(root, ".git", "worktrees", "broken")
			if variant == "non-directory" {
				require.NoError(t, os.WriteFile(broken, []byte("unexpected"), 0o600))
			} else {
				require.NoError(t, os.Mkdir(broken, 0o700))
				switch variant {
				case "duplicate":
					require.NoError(t, os.WriteFile(filepath.Join(broken, "gitdir"), []byte(filepath.Join(path, ".git")+"\n"), 0o600))
				case "malformed backlink":
					require.NoError(t, os.WriteFile(filepath.Join(broken, "gitdir"), []byte("not-a-worktree"), 0o600))
				}
			}
			_, err := repo.EnsureIdentity(t.Context(), path, worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true})
			require.Error(t, err)
			require.NoFileExists(t, filepath.Join(root, ".git", "worktrees", "topic", "kwt-generation"))
		})
	}
}

func TestGenerationRecoversPartialFileButReportsWriteFailure(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	registration := git(t, path, "rev-parse", "--absolute-git-dir")
	marker := filepath.Join(registration, "kwt-generation")
	require.NoError(t, os.WriteFile(marker, []byte("partial"), 0o600))
	identity := worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true}
	value, err := repo.EnsureIdentity(t.Context(), path, identity)
	require.NoError(t, err)
	require.Regexp(t, "^[0-9a-f]{32}$", value)
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, value+"\n", string(data))
	require.NoError(t, os.Remove(marker))
	require.NoError(t, os.Mkdir(marker, 0o700))
	_, err = repo.List(t.Context(), identity)
	require.True(t, worktree.IsIncompleteInventory(err))
	require.DirExists(t, path)
	require.Equal(t, "topic", git(t, path, "branch", "--show-current"))
}

func TestInventoryResolvesSeparateGitDirectoryAndBareContainer(t *testing.T) {
	for _, separate := range []bool{true, false} {
		t.Run(map[bool]string{true: "separate", false: "bare container"}[separate], func(t *testing.T) {
			root, _ := fixture(t)
			container := t.TempDir()
			main := filepath.Join(container, "main")
			if separate {
				git(t, root, "clone", "--separate-git-dir", filepath.Join(container, "repository.git"), root, main)
				git(t, main, "config", "core.worktree", main)
			} else {
				bare := filepath.Join(container, ".bare")
				git(t, root, "clone", "--bare", root, bare)
				git(t, bare, "worktree", "add", main, "main")
			}
			linked := filepath.Join(container, "linked")
			git(t, main, "worktree", "add", "-b", "linked", linked)
			repo := open(t, linked, kwtPolicy())
			inventory, err := repo.Inspect(t.Context())
			require.NoError(t, err)
			require.Len(t, inventory.Entries, 2)
			for _, entry := range inventory.Entries {
				require.Empty(t, entry.GitDirError)
				require.Equal(t, filepath.Base(entry.Path) == "main", entry.IsMain)
			}
		})
	}
}

func TestScopeCancellationAndExecutionPolicy(t *testing.T) {
	root, path := fixture(t)
	other, _ := fixture(t)
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	runner := gitcmd.New().WithConfig("example.value", "caller-value")
	var scopedRead bool
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{
		Path: root, Runner: runner,
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			if dir == path && slices.Equal(args, []string{"config", "--get", "example.value"}) {
				scopedRead = true
			}
			return runner.Output(ctx, dir, args...)
		},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, repo.WithLock(ctx, func(*worktree.Scope) error { t.Fatal("canceled operation ran"); return nil }), context.Canceled)
	require.NoError(t, repo.WithLock(t.Context(), func(s *worktree.Scope) error {
		output, err := s.RunGit(t.Context(), path, "config", "--get", "example.value")
		require.NoError(t, err)
		require.Equal(t, "caller-value\n", string(output))
		_, err = s.RunGit(t.Context(), other, "config", "--local", "example.written", "true")
		require.ErrorIs(t, err, worktree.ErrWorktreeRepositoryMismatch)
		return nil
	}))
	require.True(t, scopedRead, "scoped Git commands must run through the caller's RunGit")
	_, err = gitcmd.New().Output(t.Context(), other, "config", "--get", "example.written")
	require.Error(t, err)
}
