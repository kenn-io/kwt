package worktree_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/worktree"
)

func warmRequest(t *testing.T) worktree.WarmRequest {
	t.Helper()
	return worktree.WarmRequest{Path: filepath.Join(t.TempDir(), "pool", "checkout"), PoolLockName: ".kenn-forge-worktree.lock", MarkerFile: "kenn-forge-hot-worktree", IdentityFile: "workspace-id", Revision: "HEAD"}
}

func TestWarmPreparationAndClaimLockOrder(t *testing.T) {
	root, _ := fixture(t)
	req := warmRequest(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("prepared\n"), 0o600))
	git(t, root, "add", "file")
	git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "-m", "prepared files")
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		if slices.Contains(args, "reset") {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return r.Output(ctx, dir, args...)
	}})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- repo.PrepareWarm(t.Context(), req) }()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "warm reset did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, repo.WithLock(ctx, func(*worktree.Scope) error { return nil }))
	pool := flock.New(filepath.Join(filepath.Dir(req.Path), req.PoolLockName))
	locked, err := pool.TryLock()
	require.NoError(t, err)
	require.False(t, locked)
	claim := worktree.WarmClaimRequest{Warm: req, Create: worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Mode: managed.CheckoutDetached, BaseRef: "HEAD"}, Identity: worktree.IdentityPolicy{FileName: "workspace-id", Value: "workspace-a"}}}
	_, claimed, err := repo.ClaimWarm(ctx, claim)
	require.NoError(t, err)
	require.False(t, claimed)
	close(release)
	require.NoError(t, <-done)
	before, err := os.Stat(filepath.Join(req.Path, "file"))
	require.NoError(t, err)
	// Windows loads file identity lazily; capture it while this path exists.
	require.True(t, os.SameFile(before, before))
	require.NoError(t, pool.Lock())
	defer func() { require.NoError(t, pool.Unlock()) }()
	result, claimed, err := repo.ClaimWarm(ctx, claim)
	require.NoError(t, err)
	require.True(t, claimed)
	after, err := os.Stat(filepath.Join(result.Path, "file"))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.NoDirExists(t, req.Path)
	_, claimed, err = repo.ClaimWarm(ctx, claim)
	require.NoError(t, err)
	require.False(t, claimed)
	_, err = result.Rollback(ctx, managed.RollbackUnchanged)
	require.NoError(t, err)
	require.NoDirExists(t, result.Path)
}

func TestWarmClaimUsesLatestRevisionAndPreservesChangedSpare(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	req := warmRequest(t)
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "new revision")
	head := git(t, root, "rev-parse", "HEAD")
	notes := filepath.Join(req.Path, "notes")
	require.NoError(t, os.WriteFile(notes, []byte("user work"), 0o600))
	claim := worktree.WarmClaimRequest{Warm: req, Create: worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Mode: managed.CheckoutDetached, BaseRef: "HEAD"}}}
	_, claimed, err := repo.ClaimWarm(t.Context(), claim)
	require.NoError(t, err)
	require.False(t, claimed)
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	data, err := os.ReadFile(notes)
	require.NoError(t, err)
	require.Equal(t, "user work", string(data))
	require.NoError(t, os.Remove(notes))
	result, claimed, err := repo.ClaimWarm(t.Context(), claim)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, head, git(t, result.Path, "rev-parse", "HEAD"))
}

func TestWarmResumesInterruptedBareRegistration(t *testing.T) {
	root, _ := fixture(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, root, "clone", "--bare", root, bare)
	git(t, bare, "config", "extensions.worktreeConfig", "true")
	req := warmRequest(t)
	// These are the exact on-disk artifacts left before the marker is written.
	git(t, bare, "worktree", "add", "--lock", "--reason", req.MarkerFile, "--detach", "--no-checkout", req.Path, "HEAD")
	metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
	require.NoError(t, os.WriteFile(filepath.Join(metadata, "index.lock"), nil, 0o600))
	repo := open(t, bare, kwtPolicy())
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	require.Equal(t, "false", git(t, req.Path, "rev-parse", "--is-bare-repository"))
	state, err := os.ReadFile(filepath.Join(metadata, req.MarkerFile))
	require.NoError(t, err)
	require.Equal(t, "ready\n", string(state))
	require.NoFileExists(t, filepath.Join(metadata, "locked"))
	require.NoFileExists(t, filepath.Join(metadata, "index.lock"))
}

func TestWarmCanceledRegistrationCompletesBeforeReleasingRepository(t *testing.T) {
	root, _ := fixture(t)
	req := warmRequest(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	repo, err := coordinator.Open(ctx, worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, err := r.Output(ctx, dir, args...)
		if err == nil && len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			cancel()
			if ctx.Err() != nil {
				return nil, errors.New("registration used caller cancellation")
			}
		}
		return out, err
	}})
	require.NoError(t, err)
	require.ErrorIs(t, repo.PrepareWarm(ctx, req), context.Canceled)
	metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
	reason, err := os.ReadFile(filepath.Join(metadata, "locked"))
	require.NoError(t, err)
	require.Equal(t, req.MarkerFile, strings.TrimSpace(string(reason)))
	repo = open(t, root, kwtPolicy())
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
}

func TestWarmClaimReportsCheckoutRetainedAfterCleanupFailure(t *testing.T) {
	root, _ := fixture(t)
	req := warmRequest(t)
	repo := open(t, root, kwtPolicy())
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	checkoutFailure, removeFailure := errors.New("checkout interrupted"), errors.New("cleanup unavailable")
	repo, err = coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
			return nil, removeFailure
		}
		out, err := r.Output(ctx, dir, args...)
		if err == nil && slices.Contains(args, "--no-guess") {
			return out, checkoutFailure
		}
		return out, err
	}})
	require.NoError(t, err)
	result, claimed, err := repo.ClaimWarm(t.Context(), worktree.WarmClaimRequest{Warm: req, Create: worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Mode: managed.CheckoutDetached, BaseRef: "HEAD"}}})
	require.True(t, claimed)
	require.ErrorIs(t, err, checkoutFailure)
	require.ErrorIs(t, err, removeFailure)
	require.DirExists(t, req.Path)
	require.Equal(t, req.Path, result.Path)
	remaining, err := result.Rollback(t.Context(), managed.RollbackFreshOwned)
	require.ErrorIs(t, err, managed.ErrWorktreeCleanupIncomplete)
	require.Equal(t, req.Path, remaining.Path)
	require.NotEmpty(t, remaining.Registration)
}

func TestWarmExistingBranchKeepsAttachmentSemantics(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	req := warmRequest(t)
	git(t, root, "tag", "tag-only")
	git(t, root, "branch", "available")
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	claim := worktree.WarmClaimRequest{Warm: req, Create: worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Mode: managed.CheckoutExistingBranch}}}
	for _, branch := range []string{"tag-only", "--orphan=unexpected"} {
		claim.Create.Git.Branch = branch
		_, claimed, err := repo.ClaimWarm(t.Context(), claim)
		require.Error(t, err)
		require.False(t, claimed)
		require.DirExists(t, req.Path)
	}
	claim.Create.Git.Branch = "available"
	claim.Create.Git.BaseRef = "HEAD"
	_, claimed, err := repo.ClaimWarm(t.Context(), claim)
	require.ErrorIs(t, err, managed.ErrInvalidWorktreeOptions)
	require.False(t, claimed)
	claim.Create.Git.BaseRef = ""
	result, claimed, err := repo.ClaimWarm(t.Context(), claim)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, "available", git(t, result.Path, "branch", "--show-current"))
	require.Empty(t, result.OwnedBranch)
	_, err = result.Rollback(t.Context(), managed.RollbackUnchanged)
	require.NoError(t, err)
	require.NotEmpty(t, git(t, root, "rev-parse", "refs/heads/available"))
}

func TestWarmClaimCreatesAndRollsBackNewBranch(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	req := warmRequest(t)
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	before, err := os.Stat(req.Path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, before))
	git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "advance base")
	head := git(t, root, "rev-parse", "HEAD")
	claim := worktree.WarmClaimRequest{Warm: req, Create: worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Mode: managed.CheckoutNewBranch, Branch: "new-topic", BaseRef: "HEAD"}, Identity: worktree.IdentityPolicy{FileName: "workspace-id", Value: "workspace-a"}}}
	result, claimed, err := repo.ClaimWarm(t.Context(), claim)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, "new-topic", result.OwnedBranch)
	require.Equal(t, head, git(t, result.Path, "rev-parse", "HEAD"))
	after, err := os.Stat(result.Path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	_, err = result.Rollback(t.Context(), managed.RollbackUnchanged)
	require.NoError(t, err)
	require.NoDirExists(t, result.Path)
	_, err = gitcmd.New().Output(t.Context(), root, "rev-parse", "--verify", "refs/heads/new-topic")
	require.Error(t, err)
}

func TestWarmPreparationPreservesMissingNonSpareRegistration(t *testing.T) {
	for _, state := range []string{"attached", "unmarked", "assigned", "foreign-lock"} {
		t.Run(state, func(t *testing.T) {
			root, _ := fixture(t)
			repo := open(t, root, kwtPolicy())
			req := warmRequest(t)
			if state == "attached" {
				git(t, root, "worktree", "add", "-b", "assigned", req.Path)
			} else {
				git(t, root, "worktree", "add", "--detach", req.Path, "HEAD")
			}
			metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
			switch state {
			case "assigned":
				require.NoError(t, os.WriteFile(filepath.Join(metadata, req.MarkerFile), []byte("ready\n"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(metadata, req.IdentityFile), []byte("workspace-a\n"), 0o600))
			case "foreign-lock":
				require.NoError(t, os.WriteFile(filepath.Join(metadata, req.MarkerFile), []byte("ready\n"), 0o600))
				git(t, root, "worktree", "lock", "--reason", "removable media", req.Path)
			}
			head, err := os.ReadFile(filepath.Join(metadata, "HEAD"))
			require.NoError(t, err)
			require.NoError(t, os.RemoveAll(req.Path))
			require.Error(t, repo.PrepareWarm(t.Context(), req))
			require.NoDirExists(t, req.Path)
			after, err := os.ReadFile(filepath.Join(metadata, "HEAD"))
			require.NoError(t, err)
			require.Equal(t, head, after)
		})
	}
}

func TestWarmPreparationRebuildsMissingSpare(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	req := warmRequest(t)
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	require.NoError(t, os.RemoveAll(req.Path))
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
	state, err := os.ReadFile(filepath.Join(metadata, req.MarkerFile))
	require.NoError(t, err)
	require.Equal(t, "ready\n", string(state))
}

func TestWarmPreparationKeepsSpareLockedDuringReset(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed=%t", resumed), func(t *testing.T) {
			root, _ := fixture(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("base\n"), 0o600))
			git(t, root, "add", "file")
			git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "-m", "tracked file")
			req := warmRequest(t)
			if resumed {
				// An interrupted earlier preparation left the spare unlocked.
				git(t, root, "worktree", "add", "--detach", "--no-checkout", req.Path, "HEAD")
				metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
				require.NoError(t, os.WriteFile(filepath.Join(metadata, req.MarkerFile), []byte("preparing\n"), 0o600))
			}
			other := open(t, root, kwtPolicy())
			var removeErr error
			coordinator, err := worktree.NewCoordinator(kwtPolicy())
			require.NoError(t, err)
			repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
				if slices.Contains(args, "reset") {
					require.NoError(t, other.WithLock(ctx, func(*worktree.Scope) error {
						_, removeErr = r.Output(ctx, root, "worktree", "remove", "--force", req.Path)
						return nil
					}))
				}
				return r.Output(ctx, dir, args...)
			}})
			require.NoError(t, err)
			require.NoError(t, repo.PrepareWarm(t.Context(), req))
			require.Error(t, removeErr)
			metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
			state, err := os.ReadFile(filepath.Join(metadata, req.MarkerFile))
			require.NoError(t, err)
			require.Equal(t, "ready\n", string(state))
			require.NoFileExists(t, filepath.Join(metadata, "locked"))
			data, err := os.ReadFile(filepath.Join(req.Path, "file"))
			require.NoError(t, err)
			require.Equal(t, "base\n", string(data))
		})
	}
}

func TestWarmResumeLeavesForeignLockedSpareUntouched(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	req := warmRequest(t)
	git(t, root, "worktree", "add", "--detach", "--no-checkout", req.Path, "HEAD")
	metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
	require.NoError(t, os.WriteFile(filepath.Join(metadata, req.MarkerFile), []byte("preparing\n"), 0o600))
	git(t, root, "worktree", "lock", "--reason", "another tool", req.Path)
	indexLock := filepath.Join(metadata, "index.lock")
	require.NoError(t, os.WriteFile(indexLock, nil, 0o600))

	err := repo.PrepareWarm(t.Context(), req)

	var condition *worktree.ConditionError
	require.ErrorAs(t, err, &condition)
	require.Equal(t, worktree.ReasonLocked, condition.Reason)
	require.FileExists(t, indexLock)
	state, err := os.ReadFile(filepath.Join(metadata, req.MarkerFile))
	require.NoError(t, err)
	require.Equal(t, "preparing\n", string(state))
}
