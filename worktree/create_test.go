package worktree_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/worktree"
)

func TestCreateCandidateOrderAndPartialFailure(t *testing.T) {
	for _, variant := range []string{"numbered", "ancestor", "detached", "attach"} {
		t.Run(variant, func(t *testing.T) {
			root, _ := fixture(t)
			repo := open(t, root, kwtPolicy())
			candidates := []worktree.CheckoutCandidate{
				{Branch: "topic", Mode: managed.CheckoutNewBranch},
				{Branch: "kenn-forge/pr-7", Mode: managed.CheckoutNewBranch},
				{Branch: "kenn-forge/pr-7", Mode: managed.CheckoutNewBranch, Numbered: true},
				{Mode: managed.CheckoutDetached},
			}
			want := "kenn-forge/pr-7-2"
			switch variant {
			case "ancestor":
				git(t, root, "branch", "kenn-forge")
				want = "kenn-forge-2/pr-7"
			case "detached":
				git(t, root, "branch", "kenn-forge/pr-7")
				var commands strings.Builder
				for n := 2; n < 1000; n++ {
					fmt.Fprintf(&commands, "create refs/heads/kenn-forge/pr-7-%d HEAD\n", n)
				}
				_, _, err := gitcmd.New().Run(t.Context(), root, strings.NewReader(commands.String()), "update-ref", "--stdin")
				require.NoError(t, err)
				want = ""
			case "attach":
				git(t, root, "branch", "available")
				candidates = []worktree.CheckoutCandidate{{Branch: "available", Mode: managed.CheckoutExistingBranch}}
				want = "available"
			default:
				git(t, root, "branch", "kenn-forge/pr-7")
			}
			request := worktree.CreateRequest{
				Git:        managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "created"), BaseRef: "main", Checkout: managed.CheckoutIsolated},
				Candidates: candidates,
				Identity:   worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true},
			}
			if variant == "attach" {
				request.Git.BaseRef = ""
			}
			created, err := repo.Create(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, want, created.Branch)
			require.Equal(t, want, git(t, created.Path, "branch", "--show-current"))
			if variant == "attach" {
				require.Empty(t, created.OwnedBranch)
			} else {
				require.Equal(t, want, created.OwnedBranch)
			}
			_, err = created.Rollback(t.Context(), managed.RollbackUnchanged)
			require.NoError(t, err)
			require.NoDirExists(t, request.Git.Path)
			if variant == "attach" {
				require.NotEmpty(t, git(t, root, "rev-parse", "refs/heads/available"))
			}
		})
	}
}

func TestRequiredIdentityFailureKeepsCheckout(t *testing.T) {
	root, _ := fixture(t)
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "created")
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{
		Path: root, Runner: gitcmd.New(),
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			out, err := runner.Output(ctx, dir, args...)
			if len(args) > 1 && args[0] == "worktree" && args[1] == "add" && err == nil {
				registration := git(t, path, "rev-parse", "--absolute-git-dir")
				err = os.Mkdir(filepath.Join(registration, "kwt-generation"), 0o700)
			}
			return out, err
		},
	})
	require.NoError(t, err)
	partial, err := repo.Create(t.Context(), worktree.CreateRequest{
		Git:        managed.CreateWorktreeOptions{Path: path, BaseRef: "main"},
		Candidates: []worktree.CheckoutCandidate{{Branch: "created", Mode: managed.CheckoutNewBranch}, {Mode: managed.CheckoutDetached}},
		Identity:   worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, worktree.ErrIdentityUnavailable)
	require.Empty(t, partial.IdentityValue)
	require.Equal(t, path, partial.Path)
	require.DirExists(t, path)
	require.Equal(t, "created", git(t, path, "branch", "--show-current"))
	remaining, err := partial.Rollback(t.Context(), managed.RollbackFreshOwned)
	require.ErrorIs(t, err, managed.ErrWorktreeCleanupIncomplete)
	require.Equal(t, path, remaining.Path)
	require.Equal(t, git(t, path, "rev-parse", "--absolute-git-dir"), remaining.Registration)
	require.DirExists(t, path)
}

func TestScopedImportFinalizesGenerationWithoutRelocking(t *testing.T) {
	origin, _ := fixture(t)
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, origin, "clone", origin, clone)
	repo := open(t, clone, kwtPolicy())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var created worktree.CreateResult
	require.NoError(t, repo.WithLock(ctx, func(scope *worktree.Scope) error {
		var err error
		created, err = scope.Import(ctx, worktree.ImportRequest{
			Git:      managed.MergeRequestWorktreeOptions{Path: filepath.Join(t.TempDir(), "imported"), Branch: "pr-7", Number: 7, HeadBranch: "topic", HeadRepoCloneURL: origin, ProjectRepoIdentity: managed.CloneURLIdentity(origin)},
			Identity: worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true},
		})
		require.NoError(t, err)
		generation, err := scope.ReadIdentity(ctx, created.Path, "kwt-generation")
		require.NoError(t, err)
		require.Regexp(t, "^[0-9a-f]{32}$", generation)
		entries, err := scope.List(ctx, worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true})
		require.NoError(t, err)
		require.Len(t, entries, 2)
		require.Equal(t, "refs/remotes/origin/topic", git(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
		return nil
	}))
	_, err := created.Rollback(t.Context(), managed.RollbackUnchanged)
	require.NoError(t, err)
	require.NoDirExists(t, created.Path)
}

func TestRollbackRejectsActiveCreationReservation(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	created, err := repo.Create(t.Context(), worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "created"), Branch: "created", Mode: managed.CheckoutNewBranch}})
	require.NoError(t, err)
	lock := flock.New(filepath.Join(root, ".git", "kwt-worktree-create.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
	record := filepath.Join(root, ".git", "kwt-worktree-create.path")
	require.NoError(t, os.WriteFile(record, []byte(filepath.Join(t.TempDir(), "another")+"\n"), 0o600))
	before := git(t, root, "show-ref")
	remaining, err := created.Rollback(t.Context(), managed.RollbackUnchanged)
	require.Error(t, err)
	require.Equal(t, created.Path, remaining.Path)
	require.Equal(t, before, git(t, root, "show-ref"))
	require.DirExists(t, created.Path)
	require.NoError(t, lock.Unlock())
	_, err = created.Rollback(t.Context(), managed.RollbackUnchanged)
	require.NoError(t, err)
	require.NoDirExists(t, created.Path)
}

func TestBranchNameSelectionDoesNotCreateRef(t *testing.T) {
	root, _ := fixture(t)
	git(t, root, "branch", "feature")
	git(t, root, "branch", "feature-a123/topic")
	repo := open(t, root, kwtPolicy())
	before := git(t, root, "show-ref")
	name, err := repo.SelectBranchName(t.Context(), worktree.BranchNameRequest{Branch: "feature/topic", Suffixes: []string{"a123", "b456"}})
	require.NoError(t, err)
	require.Equal(t, "feature-b456/topic", name.Branch)
	require.Equal(t, 2, name.Next)
	require.Equal(t, before, git(t, root, "show-ref"))
}

func TestHookCanListWhileCreationIsReserved(t *testing.T) {
	for _, outputLimit := range []bool{false, true} {
		t.Run(strconv.FormatBool(outputLimit), func(t *testing.T) {
			root, _ := fixture(t)
			path := filepath.Join(t.TempDir(), "created")
			hooks := t.TempDir()
			report := filepath.Join(t.TempDir(), "listed")
			require.NoError(t, os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\n\"$KWT_HOOK_TEST_BINARY\" -test.run='^TestCreationListHookProcess$'\n"), 0o700))
			git(t, root, "config", "core.hooksPath", hooks)
			runner := gitcmd.New()
			runner.Env = append(runner.Env, "KWT_HOOK_TEST_BINARY="+filepath.ToSlash(os.Args[0]), "KWT_HOOK_TEST_ROOT="+root, "KWT_HOOK_TEST_PATH="+path, "KWT_HOOK_TEST_REPORT="+report, "KWT_HOOK_TEST_LIMIT="+strconv.FormatBool(outputLimit))
			if outputLimit {
				runner.StderrLimit = 4096
			}
			coordinator, err := worktree.NewCoordinator(kwtPolicy())
			require.NoError(t, err)
			repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: runner})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			created, err := repo.Create(ctx, worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: path, Branch: "created", Mode: managed.CheckoutNewBranch}, Serialization: worktree.CreateHookReentrant, Identity: worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true}})
			if outputLimit {
				require.ErrorIs(t, err, gitcmd.ErrStderrLimitExceeded)
				require.DirExists(t, path)
			} else {
				require.NoError(t, err)
				generation, err := repo.ReadIdentity(t.Context(), path, "kwt-generation")
				require.NoError(t, err)
				require.Regexp(t, "^[0-9a-f]{32}$", generation)
			}
			data, err := os.ReadFile(report)
			require.NoError(t, err)
			require.Equal(t, "2", string(data))
			require.NoFileExists(t, filepath.Join(root, ".git", "kwt-worktree-create.path"))
			if !outputLimit {
				_, err = created.Rollback(t.Context(), managed.RollbackUnchanged)
				require.NoError(t, err)
			}
		})
	}
}

func TestCreationListHookProcess(t *testing.T) {
	root := os.Getenv("KWT_HOOK_TEST_ROOT")
	if root == "" {
		return
	}
	repo := open(t, root, kwtPolicy())
	entries, err := repo.List(t.Context(), worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotEqual(t, os.Getenv("KWT_HOOK_TEST_PATH"), entry.Path)
	}
	require.NoError(t, os.WriteFile(os.Getenv("KWT_HOOK_TEST_REPORT"), []byte(strconv.Itoa(len(entries))), 0o600))
	if os.Getenv("KWT_HOOK_TEST_LIMIT") == "true" {
		_, err = os.Stderr.WriteString(strings.Repeat("x", 200000))
		require.NoError(t, err)
	}
}

func TestDeferredCheckoutAndOutputLimitCanBeRolledBack(t *testing.T) {
	for _, variant := range []string{"bare deferred", "output limit"} {
		t.Run(variant, func(t *testing.T) {
			root, _ := fixture(t)
			runner := gitcmd.New()
			if variant == "bare deferred" {
				bare := filepath.Join(t.TempDir(), "repository.git")
				git(t, root, "clone", "--bare", root, bare)
				root = bare
			} else {
				hooks := t.TempDir()
				report := filepath.Join(t.TempDir(), "listed")
				require.NoError(t, os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\n\"$KWT_HOOK_TEST_BINARY\" -test.run='^TestCreationListHookProcess$'\n"), 0o700))
				git(t, root, "config", "core.hooksPath", hooks)
				runner.Env = append(runner.Env, "KWT_HOOK_TEST_BINARY="+filepath.ToSlash(os.Args[0]), "KWT_HOOK_TEST_ROOT="+root, "KWT_HOOK_TEST_REPORT="+report, "KWT_HOOK_TEST_LIMIT=true")
				runner.StderrLimit = 4096
			}
			c, err := worktree.NewCoordinator(kwtPolicy())
			require.NoError(t, err)
			repo, err := c.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: runner})
			require.NoError(t, err)
			req := worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "created"), Branch: "created", Mode: managed.CheckoutNewBranch}}
			if variant == "bare deferred" {
				req.Git.NoCheckout = true
			} else {
				req.Serialization = worktree.CreateHookReentrant
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			created, err := repo.Create(ctx, req)
			if variant == "output limit" {
				require.ErrorIs(t, err, gitcmd.ErrStderrLimitExceeded)
			} else {
				require.NoError(t, err)
			}
			require.DirExists(t, created.Path)
			_, err = created.Rollback(t.Context(), managed.RollbackUnchanged)
			require.NoError(t, err)
			require.NoDirExists(t, req.Git.Path)
			_, err = gitcmd.New().Output(t.Context(), root, "show-ref", "--verify", "refs/heads/created")
			require.Error(t, err)
		})
	}
}

func TestScopedTrackingUsesRepositoryExecutionAndPreservesExistingUpstream(t *testing.T) {
	origin, _ := fixture(t)
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, origin, "clone", origin, clone)
	git(t, clone, "branch", "--track", "available", "origin/main")
	git(t, clone, "config", "push.default", "simple")
	repo := open(t, clone, kwtPolicy())
	created, err := repo.Create(t.Context(), worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "attached"), Branch: "available", Mode: managed.CheckoutExistingBranch, Checkout: managed.CheckoutIsolated, Upstream: managed.UpstreamPolicy{Action: managed.UpstreamLeave}}})
	require.NoError(t, err)
	require.NoError(t, repo.WithLock(t.Context(), func(scope *worktree.Scope) error {
		return scope.SetUpstream(t.Context(), managed.WorktreeUpstreamOptions{Path: created.Path, Policy: managed.UpstreamPolicy{Action: managed.UpstreamTrack, Remote: "origin", Ref: "refs/heads/main", ConfigurePush: true}})
	}))
	require.Equal(t, "refs/remotes/origin/main", git(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
	require.Equal(t, "upstream", git(t, created.Path, "config", "push.default"))
	require.Equal(t, "simple", git(t, clone, "config", "push.default"))
	_, err = created.Rollback(t.Context(), managed.RollbackUnchanged)
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", git(t, clone, "config", "branch.available.merge"))
	require.Equal(t, "simple", git(t, clone, "config", "push.default"))
}

func TestCreateRejectsNestedExecutionAndCrossRepositoryOptions(t *testing.T) {
	root, _ := fixture(t)
	other, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	path := filepath.Join(t.TempDir(), "created")
	_, err := repo.Create(t.Context(), worktree.CreateRequest{Git: managed.CreateWorktreeOptions{ProjectRoot: other, Path: path, Branch: "created"}})
	require.ErrorIs(t, err, worktree.ErrWorktreeRepositoryMismatch)
	require.NoDirExists(t, path)
	_, err = repo.Create(t.Context(), worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: path, Branch: "created", SetupScript: "setup.sh"}})
	require.Error(t, err)
	require.NoDirExists(t, path)
	require.NoError(t, repo.WithLock(t.Context(), func(scope *worktree.Scope) error {
		_, err := scope.Create(t.Context(), worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: path, Branch: "created", Mode: managed.CheckoutNewBranch}, Serialization: worktree.CreateHookReentrant})
		require.Error(t, err)
		return nil
	}))
	require.NoDirExists(t, path)
}

func TestTrustedCreateInBareRepositoryWithWorktreeConfig(t *testing.T) {
	root, _ := fixture(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, root, "clone", "--bare", root, bare)
	git(t, bare, "config", "extensions.worktreeConfig", "true")
	repo := open(t, bare, kwtPolicy())
	result, err := repo.Create(t.Context(), worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Branch: "new-topic", Mode: managed.CheckoutNewBranch}})
	require.NoError(t, err)
	require.Equal(t, "false", git(t, result.Path, "rev-parse", "--is-bare-repository"))
	require.Empty(t, git(t, result.Path, "status", "--porcelain"))
	require.Equal(t, "true", git(t, bare, "config", "--bool", "core.bare"))
}

func TestCreationReportsIdentityFromItsHeldScope(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	req := worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "workspace"), Branch: "created", Mode: managed.CheckoutNewBranch}, Identity: worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true}}
	result, err := repo.Create(t.Context(), req)
	require.NoError(t, err)
	require.NotEmpty(t, result.IdentityValue)
	observed, err := repo.ReadIdentity(t.Context(), result.Path, "kwt-generation")
	require.NoError(t, err)
	require.Equal(t, result.IdentityValue, observed)
	result.IdentityValue = strings.Repeat("0", 32)
	_, err = result.Rollback(t.Context(), managed.RollbackUnchanged)
	require.NoError(t, err)
}

func TestIsolatedCheckoutFailureCanRollbackBeforeIdentityFinalization(t *testing.T) {
	root, _ := fixture(t)
	coordinator, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	failure := errors.New("materialization failed")
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, err := r.Output(ctx, dir, args...)
		if err == nil && slices.Contains(args, "reset") {
			return out, failure
		}
		return out, err
	}})
	require.NoError(t, err)
	require.NoError(t, repo.WithLock(t.Context(), func(s *worktree.Scope) error {
		result, err := s.Create(t.Context(), worktree.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "partial"), Branch: "created", Mode: managed.CheckoutNewBranch, Checkout: managed.CheckoutIsolated}, Identity: worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true}})
		require.ErrorIs(t, err, failure)
		require.NotEmpty(t, result.Path)
		remaining, err := s.Rollback(t.Context(), result, managed.RollbackFreshOwned)
		require.NoError(t, err)
		require.Empty(t, remaining.Path)
		require.NoDirExists(t, result.Path)
		return nil
	}))
}
