package worktree_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/worktree"
)

func TestPruneRegistrationPreservesLiveAndForeignCheckouts(t *testing.T) {
	for _, replacement := range []string{"live", "missing", "foreign", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			root, path := fixture(t)
			repo := open(t, root, kwtPolicy())
			admin := git(t, path, "rev-parse", "--absolute-git-dir")
			other := filepath.Join(t.TempDir(), "another-missing-checkout")
			git(t, root, "worktree", "add", "-b", "other", other)
			otherAdmin := git(t, other, "rev-parse", "--absolute-git-dir")
			require.NoError(t, os.RemoveAll(other))
			if replacement != "live" {
				require.NoError(t, os.RemoveAll(path))
			}
			switch replacement {
			case "foreign":
				require.NoError(t, os.Mkdir(path, 0o700))
				git(t, path, "init", "-b", "foreign")
				require.NoError(t, os.WriteFile(filepath.Join(path, "notes"), []byte("keep"), 0o600))
			case "symlink":
				if err := os.Symlink(root, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			require.NoError(t, repo.WithLock(t.Context(), func(scope *worktree.Scope) error {
				state, err := scope.InspectRegistration(t.Context(), path)
				require.NoError(t, err)
				require.Equal(t, replacement == "live", state.Live)
				require.Equal(t, replacement == "symlink", state.Symlink)
				removed, err := scope.PruneRegistration(t.Context(), path)
				if replacement == "live" {
					require.Error(t, err)
					require.False(t, removed)
				} else {
					require.NoError(t, err)
					require.Equal(t, replacement != "symlink", removed)
				}
				return nil
			}))
			if replacement == "missing" || replacement == "foreign" {
				require.NoDirExists(t, admin)
			} else {
				require.DirExists(t, admin)
			}
			require.DirExists(t, otherAdmin)
			require.NotEmpty(t, git(t, root, "rev-parse", "refs/heads/topic"))
			if replacement == "foreign" {
				require.FileExists(t, filepath.Join(path, "notes"))
				require.DirExists(t, filepath.Join(path, ".git"))
			}
			if replacement == "symlink" {
				target, err := os.Readlink(path)
				require.NoError(t, err)
				require.Equal(t, root, target)
			}
		})
	}
}

func TestObserveRegistrationDoesNotTakeLockOrWriteIdentity(t *testing.T) {
	for _, replacement := range []string{"live", "missing", "foreign", "symlink", "invalid-head"} {
		t.Run(replacement, func(t *testing.T) {
			require := require.New(t)
			root, path := fixture(t)
			repo := open(t, root, kwtPolicy())
			admin := git(t, path, "rev-parse", "--absolute-git-dir")
			if replacement != "live" && replacement != "invalid-head" {
				require.NoError(os.RemoveAll(path))
			}
			switch replacement {
			case "foreign":
				require.NoError(os.Mkdir(path, 0o700))
				git(t, path, "init", "-b", "foreign")
			case "symlink":
				if err := os.Symlink(root, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "invalid-head":
				require.NoError(os.WriteFile(filepath.Join(admin, "HEAD"), []byte("invalid\n"), 0o600))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(repo.WithLock(ctx, func(scope *worktree.Scope) error {
				state, err := repo.ObserveRegistration(ctx, path, "example-id")
				require.NoError(err)
				require.Equal(replacement == "live", state.Live)
				require.Equal(replacement != "missing", state.Exists)
				require.Equal(replacement == "symlink", state.Symlink)
				require.Empty(state.Identity)
				require.NoFileExists(filepath.Join(admin, "example-id"))
				require.NoError(os.WriteFile(filepath.Join(admin, "example-id"), []byte("workspace-123\n"), 0o600))
				state, err = repo.ObserveRegistration(ctx, path, "example-id")
				require.NoError(err)
				if replacement == "symlink" {
					require.Empty(state.Identity)
				} else {
					require.Equal("workspace-123", state.Identity)
				}
				return nil
			}))
		})
	}
}

func TestQuarantinePreservesCheckoutRootsAndOrphanContents(t *testing.T) {
	for _, kind := range []string{"directory", "file", "broken-link", "live", "live-link", "nested", "bare"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			root, live := fixture(t)
			path := filepath.Join(t.TempDir(), "orphan")
			switch kind {
			case "bare":
				git(t, filepath.Dir(path), "init", "--bare", path)
			case "live":
				path = live
			case "live-link", "broken-link":
				target := live
				if kind == "broken-link" {
					target = filepath.Join(t.TempDir(), "missing")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "file":
				require.NoError(os.WriteFile(path, []byte("keep"), 0o600))
			case "nested":
				path = filepath.Join(root, "nested")
				fallthrough
			default:
				require.NoError(os.Mkdir(path, 0o700))
				require.NoError(os.WriteFile(filepath.Join(path, "notes"), []byte("keep"), 0o600))
			}
			destination := filepath.Join(t.TempDir(), "recovered")
			moved, err := worktree.Quarantine(t.Context(), worktree.QuarantineOptions{Path: path, Destination: destination, Runner: gitcmd.New()})
			require.NoError(err)
			preserve := kind == "live" || kind == "live-link"
			require.Equal(!preserve, moved)
			if preserve {
				info, err := os.Stat(path)
				require.NoError(err)
				require.True(info.IsDir())
				require.NoDirExists(destination)
				return
			}
			_, err = os.Lstat(path)
			require.ErrorIs(err, os.ErrNotExist)
			switch kind {
			case "bare":
				require.Equal("true", git(t, destination, "rev-parse", "--is-bare-repository"))
			case "broken-link":
				_, err = os.Readlink(destination)
				require.NoError(err)
			case "file":
				data, err := os.ReadFile(destination)
				require.NoError(err)
				require.Equal("keep", string(data))
			default:
				data, err := os.ReadFile(filepath.Join(destination, "notes"))
				require.NoError(err)
				require.Equal("keep", string(data))
			}
		})
	}
}

func TestBranchCleanupAndAvailabilityUseHeldScope(t *testing.T) {
	require := require.New(t)
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	git(t, root, "branch", "unused")
	require.NoError(repo.WithLock(t.Context(), func(scope *worktree.Scope) error {
		available, err := scope.BranchNameAvailable(t.Context(), "topic/child")
		require.NoError(err)
		require.False(available)
		available, err = scope.BranchNameAvailable(t.Context(), "free")
		require.NoError(err)
		require.True(available)
		removed, err := scope.RemoveBranch(t.Context(), managed.BranchRemoval{Name: "topic", Force: true})
		require.Error(err)
		require.False(removed)
		require.DirExists(path)
		removed, err = scope.RemoveBranch(t.Context(), managed.BranchRemoval{Name: "unused", Force: true})
		require.NoError(err)
		require.True(removed)
		available, err = scope.BranchNameAvailable(t.Context(), "unused")
		require.NoError(err)
		require.True(available)
		return nil
	}))
}

// A checkout that still links to its registration is not stale, even when Git
// cannot use it as a work tree, such as a bare repository's checkout without
// its per-worktree core.bare override.
func TestPruneRegistrationPreservesLinkedCheckoutGitCannotUse(t *testing.T) {
	root, _ := fixture(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, root, "clone", "--bare", root, bare)
	git(t, bare, "config", "extensions.worktreeConfig", "true")
	path := filepath.Join(t.TempDir(), "linked")
	git(t, bare, "worktree", "add", "--detach", path, "HEAD")
	admin := filepath.Join(bare, "worktrees", "linked")
	repo := open(t, bare, kwtPolicy())
	require.NoError(t, repo.WithLock(t.Context(), func(scope *worktree.Scope) error {
		state, err := scope.InspectRegistration(t.Context(), path)
		require.NoError(t, err)
		require.False(t, state.Live)
		removed, err := scope.PruneRegistration(t.Context(), path)
		require.Error(t, err)
		require.False(t, removed)
		return nil
	}))
	require.DirExists(t, admin)
	require.DirExists(t, path)
}
