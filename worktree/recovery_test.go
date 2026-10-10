package worktree_test

import (
	"context"
	"errors"
	"fmt"
	gitcmd "go.kenn.io/kit/git/cmd"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/worktree"
)

func TestRecoveryRetainsIndexHeadAndRegistrationAcrossFailure(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(fmt.Sprintf("detached=%t", detached), func(t *testing.T) {
			root, path := fixture(t)
			repo := open(t, root, kwtPolicy())
			if detached {
				git(t, path, "checkout", "--detach")
			}
			git(t, path, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "saved commit")
			head := git(t, path, "rev-parse", "HEAD")
			require.NoError(t, os.WriteFile(filepath.Join(path, "staged.txt"), []byte("saved stage\n"), 0o600))
			git(t, path, "add", "staged.txt")
			metadata := git(t, path, "rev-parse", "--absolute-git-dir")
			index, err := os.ReadFile(filepath.Join(metadata, "index"))
			require.NoError(t, err)
			savedHead, err := os.ReadFile(filepath.Join(metadata, "HEAD"))
			require.NoError(t, err)
			require.NoError(t, os.RemoveAll(path))
			req := worktree.RecoveryRequest{Path: path, Branch: "wrong-branch", Mode: worktree.ReconstructRegistered, Identity: worktree.IdentityPolicy{FileName: "workspace-id", Value: "example-workspace"}}
			_, err = repo.Recover(t.Context(), req)
			require.Error(t, err)
			require.NoDirExists(t, path)
			req.Branch = ""
			marker := filepath.Join(metadata, req.Identity.FileName)
			require.NoError(t, os.Mkdir(marker, 0o700))
			_, err = repo.Recover(t.Context(), req)
			require.Error(t, err)
			require.NoDirExists(t, path)
			after, err := os.ReadFile(filepath.Join(metadata, "index"))
			require.NoError(t, err)
			require.Equal(t, index, after)
			after, err = os.ReadFile(filepath.Join(metadata, "HEAD"))
			require.NoError(t, err)
			require.Equal(t, savedHead, after)
			require.NoError(t, os.Remove(marker))
			result, err := repo.Recover(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, worktree.Recovered, result.Disposition)
			require.Empty(t, result.OwnedBranch)
			require.Equal(t, head, git(t, path, "rev-parse", "HEAD"))
			after, err = os.ReadFile(filepath.Join(metadata, "index"))
			require.NoError(t, err)
			require.Equal(t, index, after)
			data, err := os.ReadFile(filepath.Join(path, "staged.txt"))
			require.NoError(t, err)
			require.Equal(t, "saved stage\n", string(data))
			require.Equal(t, "staged.txt", git(t, path, "diff", "--cached", "--name-only"))
		})
	}
}

func TestAdoptExistingNeverCreatesOrCleans(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	require.NoError(t, os.WriteFile(filepath.Join(path, "notes"), []byte("unfinished"), 0o600))
	git(t, path, "add", "notes")
	metadata := git(t, path, "rev-parse", "--absolute-git-dir")
	before, err := os.ReadFile(filepath.Join(metadata, "index"))
	require.NoError(t, err)
	req := worktree.RecoveryRequest{Path: path, Mode: worktree.AdoptExistingOnly}
	result, err := repo.Recover(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, worktree.Adopted, result.Disposition)
	require.Empty(t, result.OwnedBranch)
	after, err := os.ReadFile(filepath.Join(metadata, "index"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	req.ExpectedHead = "not-the-observed-head"
	_, err = repo.Recover(t.Context(), req)
	require.Error(t, err)
	require.FileExists(t, filepath.Join(path, "notes"))
	require.NoError(t, os.RemoveAll(path))
	req.ExpectedHead = ""
	_, err = repo.Recover(t.Context(), req)
	require.Error(t, err)
	require.NoDirExists(t, path)
	require.DirExists(t, metadata)
	req.Path = filepath.Join(t.TempDir(), "new")
	req.Mode = worktree.ReconstructRegistered
	_, err = repo.Recover(t.Context(), req)
	require.Error(t, err)
	require.NoDirExists(t, req.Path)
	req.Path = root
	req.Mode = worktree.AdoptExistingOnly
	_, err = repo.Recover(t.Context(), req)
	require.Error(t, err)
	require.DirExists(t, root)
}

func TestSyncBaseSkipsCheckedOutAndBacksUpManagedTip(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%t", managed), func(t *testing.T) {
			root, path := fixture(t)
			repo := open(t, root, kwtPolicy())
			oldTip := git(t, path, "rev-parse", "HEAD")
			git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "remote update")
			target := git(t, root, "rev-parse", "HEAD")
			req := worktree.BaseSyncRequest{Branch: "topic", SourceRef: target, Managed: managed, BackupPrefix: "refs/kenn-forge/base-backups/"}
			sync := func() {
				require.NoError(t, repo.WithLock(t.Context(), func(s *worktree.Scope) error { return s.SyncBase(t.Context(), req) }))
			}
			sync()
			require.Equal(t, oldTip, git(t, path, "rev-parse", "HEAD"))
			git(t, path, "checkout", "--detach")
			sync()
			require.Equal(t, target, git(t, root, "rev-parse", "topic"))
			req.SourceRef = oldTip
			sync()
			expected := target
			if managed {
				expected = oldTip
				require.Equal(t, target, git(t, root, "rev-parse", req.BackupPrefix+target))
			}
			require.Equal(t, expected, git(t, root, "rev-parse", "topic"))
			req.Branch = "new-base"
			sync()
			require.Equal(t, oldTip, git(t, root, "rev-parse", "new-base"))
		})
	}
}

func TestRecoveryFailurePreservesReplacementDirectory(t *testing.T) {
	root, path := fixture(t)
	metadata := git(t, path, "rev-parse", "--absolute-git-dir")
	require.NoError(t, os.RemoveAll(path))
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	failure := errors.New("reconstruction interrupted")
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, err := r.Output(ctx, dir, args...)
		if err == nil && len(args) > 0 && args[0] == "checkout-index" {
			if err = os.Rename(path, path+"-saved"); err != nil {
				return nil, err
			}
			if err = os.Mkdir(path, 0o700); err != nil {
				return nil, err
			}
			if err = os.WriteFile(filepath.Join(path, "replacement"), []byte("keep"), 0o600); err != nil {
				return nil, err
			}
			return out, failure
		}
		return out, err
	}})
	require.NoError(t, err)
	_, err = repo.Recover(t.Context(), worktree.RecoveryRequest{Path: path, Mode: worktree.ReconstructRegistered})
	require.ErrorIs(t, err, failure)
	require.DirExists(t, metadata)
	data, err := os.ReadFile(filepath.Join(path, "replacement"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
}

func TestRecoveryReturnsCapturedIdentity(t *testing.T) {
	for _, mode := range []worktree.RecoveryMode{worktree.AdoptExistingOnly, worktree.ReconstructRegistered} {
		for _, identity := range []worktree.IdentityPolicy{
			{FileName: "kwt-generation", Generate: true},
			{FileName: "workspace-id", Value: "workspace-a"},
		} {
			t.Run(fmt.Sprintf("mode=%d/%s", mode, identity.FileName), func(t *testing.T) {
				root, path := fixture(t)
				repo := open(t, root, kwtPolicy())
				if mode == worktree.ReconstructRegistered {
					require.NoError(t, os.RemoveAll(path))
				}
				result, err := repo.Recover(t.Context(), worktree.RecoveryRequest{Path: path, Mode: mode, Identity: identity})
				require.NoError(t, err)
				persisted, err := repo.ReadIdentity(t.Context(), path, identity.FileName)
				require.NoError(t, err)
				require.NotEmpty(t, persisted)
				require.Equal(t, persisted, result.IdentityValue)
			})
		}
	}
}

// Native Git can update the base branch while kwt holds only its own lock. The
// sync must not overwrite a tip it did not read.
func TestSyncBaseRefusesBranchMovedByAnotherWriter(t *testing.T) {
	root, path := fixture(t)
	git(t, path, "checkout", "--detach")
	git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "remote update")
	target := git(t, root, "rev-parse", "HEAD")
	tree := git(t, root, "rev-parse", "topic^{tree}")
	newer := git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit-tree", tree, "-p", "topic", "-m", "written by native git")
	moved := false
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		writesTopic := len(args) > 1 && ((args[0] == "branch" && args[1] == "--force") || (args[0] == "update-ref" && slices.Contains(args, "refs/heads/topic")))
		if writesTopic && !moved {
			moved = true
			_, moveErr := r.Output(ctx, root, "update-ref", "refs/heads/topic", newer)
			require.NoError(t, moveErr)
		}
		return r.Output(ctx, dir, args...)
	}})
	require.NoError(t, err)

	err = repo.WithLock(t.Context(), func(s *worktree.Scope) error {
		return s.SyncBase(t.Context(), worktree.BaseSyncRequest{Branch: "topic", SourceRef: target, Managed: true, BackupPrefix: "refs/kenn-forge/base-backups/"})
	})

	require.True(t, moved)
	require.Error(t, err)
	require.Equal(t, newer, git(t, root, "rev-parse", "topic"))
}

// The checked-out check runs immediately before the write, so a checkout that
// lands while the old tip is backed up still stops the sync.
func TestSyncBaseSkipsBranchCheckedOutDuringBackup(t *testing.T) {
	root, path := fixture(t)
	git(t, path, "checkout", "--detach")
	git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "rewritten base")
	target := git(t, root, "rev-parse", "HEAD")
	git(t, root, "update-ref", "refs/heads/topic", git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit-tree", git(t, root, "rev-parse", "topic^{tree}"), "-p", "topic", "-m", "local only"))
	local := git(t, root, "rev-parse", "topic")
	checkedOut := false
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, runErr := r.Output(ctx, dir, args...)
		if !checkedOut && len(args) > 1 && args[0] == "update-ref" && strings.HasPrefix(args[1], "refs/kenn-forge/base-backups/") {
			checkedOut = true
			_, checkoutErr := r.Output(ctx, path, "checkout", "topic")
			require.NoError(t, checkoutErr)
		}
		return out, runErr
	}})
	require.NoError(t, err)

	require.NoError(t, repo.WithLock(t.Context(), func(s *worktree.Scope) error {
		return s.SyncBase(t.Context(), worktree.BaseSyncRequest{Branch: "topic", SourceRef: target, Managed: true, BackupPrefix: "refs/kenn-forge/base-backups/"})
	}))

	require.True(t, checkedOut)
	require.Equal(t, local, git(t, root, "rev-parse", "topic"))
	require.Equal(t, local, git(t, path, "rev-parse", "HEAD"))
}
