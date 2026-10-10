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
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: hooklessRunner(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
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
	repo := openHookless(t, root, kwtPolicy())
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
	repo := openHookless(t, bare, kwtPolicy())
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
	repo, err := coordinator.Open(ctx, worktree.RepositoryOptions{Path: root, Runner: hooklessRunner(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
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
	repo = openHookless(t, root, kwtPolicy())
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
}

func TestWarmClaimReportsCheckoutRetainedAfterCleanupFailure(t *testing.T) {
	root, _ := fixture(t)
	req := warmRequest(t)
	repo := openHookless(t, root, kwtPolicy())
	require.NoError(t, repo.PrepareWarm(t.Context(), req))
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	checkoutFailure, removeFailure := errors.New("checkout interrupted"), errors.New("cleanup unavailable")
	repo, err = coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: hooklessRunner(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
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
	repo := openHookless(t, root, kwtPolicy())
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
	repo := openHookless(t, root, kwtPolicy())
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
			repo := openHookless(t, root, kwtPolicy())
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
	repo := openHookless(t, root, kwtPolicy())
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
			other := openHookless(t, root, kwtPolicy())
			var removeErr error
			coordinator, err := worktree.NewCoordinator(kwtPolicy())
			require.NoError(t, err)
			repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: hooklessRunner(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
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
	repo := openHookless(t, root, kwtPolicy())
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

// Preparation writes the spare's per-worktree core.bare override, and moving
// the spare keeps it. A claim must not pay for rewriting it, but any config it
// cannot read with certainty still goes through Git.
func TestWarmClaimReusesPreparedBareOverride(t *testing.T) {
	for _, layout := range []string{"prepared", "commented", "inline-header"} {
		t.Run(layout, func(t *testing.T) {
			root, _ := fixture(t)
			bare := filepath.Join(t.TempDir(), "bare.git")
			git(t, root, "clone", "--bare", root, bare)
			git(t, bare, "config", "extensions.worktreeConfig", "true")
			req := warmRequest(t)
			configs := 0
			coordinator, err := worktree.NewCoordinator(kwtPolicy())
			require.NoError(t, err)
			repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: bare, Runner: hooklessRunner(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "config" {
					configs++
				}
				return r.Output(ctx, dir, args...)
			}})
			require.NoError(t, err)
			require.NoError(t, repo.PrepareWarm(t.Context(), req))
			metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
			// Git accepts these spellings; the fast path must leave them to Git.
			switch layout {
			case "commented":
				require.NoError(t, os.WriteFile(filepath.Join(metadata, "config.worktree"), []byte("[core]\n\tbare = false ; set by another tool\n"), 0o600))
			case "inline-header":
				require.NoError(t, os.WriteFile(filepath.Join(metadata, "config.worktree"), []byte("[core] bare = false\n"), 0o600))
			}
			require.Equal(t, "false", git(t, req.Path, "rev-parse", "--is-bare-repository"))
			configs = 0

			result, claimed, err := repo.ClaimWarm(t.Context(), worktree.WarmClaimRequest{Warm: req, Create: worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Branch: "claimed", BaseRef: "HEAD", Mode: managed.CheckoutNewBranch}}})

			require.NoError(t, err)
			require.True(t, claimed)
			require.Equal(t, "false", git(t, result.Path, "rev-parse", "--is-bare-repository"))
			if layout == "prepared" {
				require.Zero(t, configs, "a prepared override needs no Git config commands")
			} else {
				require.NotZero(t, configs, "an unrecognized config file must be resolved by Git")
			}
		})
	}
}

// The fast path may skip Git only when it reads core.bare exactly as Git would.
func TestBareOverrideSetAcceptsOnlyPlainGitLayout(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		want    bool
	}{
		"git-written":         {"[core]\n\tbare = false\n", true},
		"other sections":      {"[branch \"topic\"]\n\tremote = origin\n[core]\n\tbare = false\n", true},
		"last value wins":     {"[core]\n\tbare = false\n\tbare = true\n", false},
		"inline header value": {"[core]\n\tbare = false\n[core] bare = true\n", false},
		"include":             {"[core]\n\tbare = false\n[include]\n\tpath = other\n", false},
		"include if":          {"[core]\n\tbare = false\n[includeIf \"gitdir:/x\"]\n\tpath = other\n", false},
		"continued line":      {"[core]\n\tbare = false\n\teditor = vi \\\n", false},
		"trailing comment":    {"[core]\n\tbare = false ; note\n", false},
		"boolean shorthand":   {"[core]\n\tbare\n", false},
		"other subsection":    {"[core \"x\"]\n\tbare = false\n", false},
		"empty":               {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.worktree")
			require.NoError(t, os.WriteFile(file, []byte(tc.content), 0o600))
			require.Equal(t, tc.want, worktree.BareOverrideSet(file))
		})
	}
	require.False(t, worktree.BareOverrideSet(filepath.Join(t.TempDir(), "missing")))
}

// hooklessRunner disables native hooks, as applications that claim warm
// spares do; a claim cannot let hooks re-enter while it holds the lock.
func hooklessRunner() gitcmd.Runner {
	return gitcmd.New().WithConfig("core.hooksPath", os.DevNull)
}

func openHookless(t *testing.T, root string, policy worktree.LockPolicy) *worktree.Repository {
	t.Helper()
	coordinator, err := worktree.NewCoordinator(policy)
	require.NoError(t, err)
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: hooklessRunner()})
	require.NoError(t, err)
	return repo
}

// A claim checks out the spare while holding the repository lock. A native
// hook that lists worktrees would wait on that lock forever, so a claim that
// could run hooks declines and leaves creation to the reservation protocol.
func TestWarmClaimDeclinesWhenCheckoutHooksMayRun(t *testing.T) {
	for _, hooks := range []bool{true, false} {
		t.Run(fmt.Sprintf("hooks=%t", hooks), func(t *testing.T) {
			root, _ := fixture(t)
			req := warmRequest(t)
			require.NoError(t, openHookless(t, root, kwtPolicy()).PrepareWarm(t.Context(), req))
			repo := openHookless(t, root, kwtPolicy())
			if hooks {
				repo = open(t, root, kwtPolicy())
			}
			claim := worktree.WarmClaimRequest{Warm: req, Create: worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Branch: "claimed", BaseRef: "HEAD", Mode: managed.CheckoutNewBranch}}}

			_, claimed, err := repo.ClaimWarm(t.Context(), claim)

			require.NoError(t, err)
			require.Equal(t, !hooks, claimed)
			if hooks {
				metadata := git(t, req.Path, "rev-parse", "--absolute-git-dir")
				state, err := os.ReadFile(filepath.Join(metadata, req.MarkerFile))
				require.NoError(t, err)
				require.Equal(t, "ready\n", string(state))
			}
		})
	}
}
