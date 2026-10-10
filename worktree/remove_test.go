package worktree_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/worktree"
)

func removalFixture(t *testing.T) (string, *worktree.Repository, worktree.RemovalRequest) {
	t.Helper()
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	generation, err := repo.EnsureIdentity(t.Context(), path, worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	return root, repo, worktree.RemovalRequest{
		Path: path, Authority: worktree.MatchingIdentity, Identity: worktree.IdentityPolicy{FileName: "kwt-generation", Value: generation},
		Conditions: &worktree.RemovalConditions{ExpectedGitDir: git(t, path, "rev-parse", "--absolute-git-dir"), Generation: generation, Head: git(t, path, "rev-parse", "HEAD"), Branch: "topic", RequireClean: true},
	}
}

func TestRemovalRevalidatesConditionsInsideClaim(t *testing.T) {
	for _, kind := range []string{"generation", "head", "backlink", "branch", "repository", "upstream repository", "upstream branch", "staged", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			root, repo, req := removalFixture(t)
			git(t, root, "remote", "add", "origin", "https://example.com/team/project.git")
			git(t, root, "remote", "add", "source", "https://example.com/team/source.git")
			git(t, req.Path, "config", "branch.topic.remote", "source")
			git(t, req.Path, "config", "branch.topic.merge", "refs/heads/topic")
			req.Conditions.RepositoryIdentity = "example.com/team/project"
			req.Conditions.UpstreamRepository = "example.com/team/source"
			req.Conditions.UpstreamBranch = "topic"
			req.Conditions.MatchRepositoryIdentity = func(remote, expected string) bool { return remote == "https://"+expected+".git" }
			reason := worktree.ReasonGenerationChanged
			req.Claim = func(_ context.Context, preflight func() error, remove func() (worktree.RemovalResult, error)) (bool, error) {
				require.NoError(t, preflight())
				require.NoError(t, preflight())
				switch kind {
				case "generation":
					require.NoError(t, os.WriteFile(filepath.Join(req.Conditions.ExpectedGitDir, "kwt-generation"), []byte(strings.Repeat("0", 32)), 0o600))
				case "head":
					git(t, req.Path, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "changed")
					reason = worktree.ReasonHeadChanged
				case "backlink":
					require.NoError(t, os.WriteFile(filepath.Join(req.Path, ".git"), []byte("gitdir: "+filepath.Join(root, ".git")+"\n"), 0o600))
					reason = worktree.ReasonBacklinkChanged
				case "branch":
					git(t, req.Path, "checkout", "-b", "replacement")
					reason = worktree.ReasonBranchChanged
				case "repository":
					git(t, req.Path, "remote", "set-url", "origin", "https://example.com/team/replaced.git")
					reason = worktree.ReasonRepositoryChanged
				case "upstream repository":
					git(t, req.Path, "remote", "set-url", "source", "https://example.com/team/replaced.git")
					reason = worktree.ReasonUpstreamRepositoryChanged
				case "upstream branch":
					git(t, req.Path, "config", "branch.topic.merge", "refs/heads/changed")
					reason = worktree.ReasonUpstreamBranchChanged
				case "staged":
					require.NoError(t, os.WriteFile(filepath.Join(req.Path, "staged.txt"), []byte("keep"), 0o600))
					git(t, req.Path, "add", "staged.txt")
					reason = worktree.ReasonDirty
				case "ignored":
					require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("ignored.txt\n"), 0o600))
					require.NoError(t, os.WriteFile(filepath.Join(req.Path, "ignored.txt"), []byte("keep"), 0o600))
					reason = worktree.ReasonDirty
				}
				_, err := remove()
				return true, err
			}
			req.Conditions.IncludeIgnored = true
			result, err := repo.Remove(t.Context(), req)
			var condition *worktree.ConditionError
			require.ErrorAs(t, err, &condition)
			require.Equal(t, reason, condition.Reason)
			require.True(t, result.Claimed)
			require.False(t, result.CheckoutRemoved)
			require.Equal(t, req.Path, result.Remaining.Path)
			require.DirExists(t, req.Path)
			require.DirExists(t, req.Conditions.ExpectedGitDir)
		})
	}
}

func TestRemovalClaimAndBranchOwnership(t *testing.T) {
	root, repo, req := removalFixture(t)
	req.DeleteObservedBranch = true
	var escaped func() (worktree.RemovalResult, error)
	req.Claim = func(_ context.Context, preflight func() error, remove func() (worktree.RemovalResult, error)) (bool, error) {
		escaped = remove
		require.NoError(t, preflight())
		_, cleanupErr := remove()
		require.NoError(t, cleanupErr)
		require.NoDirExists(t, req.Path)
		_, cleanupErr = remove()
		require.Error(t, cleanupErr)
		return true, nil
	}
	result, err := repo.Remove(t.Context(), req)
	require.NoError(t, err)
	require.True(t, result.Claimed)
	require.True(t, result.CheckoutRemoved)
	require.True(t, result.RegistrationRemoved)
	require.Equal(t, []string{"topic"}, result.BranchesRemoved)
	_, expiredErr := escaped()
	require.Error(t, expiredErr)
	_, err = gitcmd.New().Output(t.Context(), root, "show-ref", "--verify", "refs/heads/topic")
	require.Error(t, err)
}

func TestRemovalConditionsWithoutGeneration(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	req := worktree.RemovalRequest{Path: path, Conditions: &worktree.RemovalConditions{Branch: "other"}}
	_, err := repo.Remove(t.Context(), req)
	var condition *worktree.ConditionError
	require.ErrorAs(t, err, &condition)
	require.Equal(t, worktree.ReasonBranchChanged, condition.Reason)
	require.DirExists(t, path)
	req.Conditions.Branch = "topic"
	req.Conditions.ExpectedGitDir = git(t, path, "rev-parse", "--absolute-git-dir")
	result, err := repo.Remove(t.Context(), req)
	require.NoError(t, err)
	require.True(t, result.CheckoutRemoved)
	require.True(t, result.RegistrationRemoved)
	require.NoDirExists(t, path)
}

func TestRemovalDoesNothingWhenClaimWasReplaced(t *testing.T) {
	_, repo, req := removalFixture(t)
	req.Claim = func(_ context.Context, _ func() error, _ func() (worktree.RemovalResult, error)) (bool, error) {
		return false, nil
	}
	result, err := repo.Remove(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.Claimed)
	require.False(t, result.CheckoutRemoved)
	require.DirExists(t, req.Path)
}

func TestExactRemovalPreservesRecordedSymlink(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	alias := filepath.Join(t.TempDir(), "recorded")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	before := git(t, root, "worktree", "list", "--porcelain")
	refs := git(t, root, "show-ref")
	result, err := repo.Remove(t.Context(), worktree.RemovalRequest{Path: alias, Authority: worktree.ExactRegisteredPath, Force: true, Branches: []managed.BranchRemoval{{Name: "topic", Force: true}}})
	require.NoError(t, err)
	require.Equal(t, worktree.PreserveSymlinkTarget, result.Disposition)
	require.False(t, result.CheckoutRemoved)
	info, err := os.Lstat(alias)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	require.DirExists(t, path)
	require.Equal(t, before, git(t, root, "worktree", "list", "--porcelain"))
	require.Equal(t, refs, git(t, root, "show-ref"))
	result, err = repo.Remove(t.Context(), worktree.RemovalRequest{Path: path, Authority: worktree.ExactRegisteredPath})
	require.NoError(t, err)
	require.True(t, result.CheckoutRemoved)
}

func TestForceRemovalRetainsIdentityGuard(t *testing.T) {
	_, repo, req := removalFixture(t)
	req.Force = true
	req.Conditions = nil
	req.Identity.Value = strings.Repeat("f", 32)
	require.NoError(t, os.WriteFile(filepath.Join(req.Path, "untracked"), []byte("keep"), 0o600))
	_, err := repo.Remove(t.Context(), req)
	require.Error(t, err)
	require.FileExists(t, filepath.Join(req.Path, "untracked"))
}

func TestRemovalReportsPartialEffectsWithoutDeletingBranches(t *testing.T) {
	for _, variant := range []string{"checkout remnant", "branch failure"} {
		t.Run(variant, func(t *testing.T) {
			root, path := fixture(t)
			registration := git(t, path, "rev-parse", "--absolute-git-dir")
			expectedOID := git(t, path, "rev-parse", "HEAD")
			c, err := worktree.NewCoordinator(kwtPolicy())
			require.NoError(t, err)
			boundaryFailure := errors.New("fixture removal failure")
			repo, err := c.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
				if variant == "branch failure" && len(args) > 1 && args[0] == "branch" && (args[1] == "-d" || args[1] == "-D") {
					return nil, boundaryFailure
				}
				out, err := r.Output(ctx, dir, args...)
				if variant == "checkout remnant" && len(args) > 1 && args[0] == "worktree" && args[1] == "remove" && err == nil {
					require.NoError(t, os.Mkdir(path, 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(path, "keep"), []byte("residue"), 0o600))
					return out, boundaryFailure
				}
				return out, err
			}})
			require.NoError(t, err)
			var duringClaim worktree.RemovalResult
			result, err := repo.Remove(t.Context(), worktree.RemovalRequest{Path: path, Authority: worktree.ExactRegisteredPath, DeleteObservedBranch: true,
				Claim: func(_ context.Context, _ func() error, remove func() (worktree.RemovalResult, error)) (bool, error) {
					var err error
					duringClaim, err = remove()
					require.True(t, duringClaim.RegistrationRemoved)
					require.Equal(t, variant == "branch failure", duringClaim.CheckoutRemoved)
					return true, err
				},
			})
			require.ErrorIs(t, err, boundaryFailure)
			require.True(t, result.RegistrationRemoved)
			require.NoDirExists(t, registration)
			require.Equal(t, variant == "branch failure", result.CheckoutRemoved)
			require.Empty(t, result.BranchesRemoved)
			require.Equal(t, expectedOID, git(t, root, "rev-parse", "refs/heads/topic"))
			if variant == "checkout remnant" {
				require.FileExists(t, filepath.Join(path, "keep"))
			}
		})
	}
}

func TestMaintenanceRepairsBeforePruningAndRejectsUnreviewedChanges(t *testing.T) {
	for _, unexpected := range []bool{false, true} {
		t.Run(map[bool]string{true: "unexpected", false: "reviewed"}[unexpected], func(t *testing.T) {
			root, live := fixture(t)
			missing := filepath.Join(t.TempDir(), "missing")
			git(t, root, "worktree", "add", "-b", "missing", missing)
			require.NoError(t, os.RemoveAll(missing))
			moved := filepath.Join(t.TempDir(), "moved")
			require.NoError(t, os.Rename(root, moved))
			repo := open(t, moved, kwtPolicy())
			before, err := repo.Inspect(t.Context())
			require.NoError(t, err)
			expected := make([]worktree.StructuralCondition, 0, len(before.Entries))
			for _, entry := range before.Entries {
				expected = append(expected, worktree.StructuralCondition{Path: entry.Path, GitDir: entry.GitDir, DotGitTarget: entry.DotGitTarget, Generation: entry.Generation, Exists: entry.Exists})
			}
			if unexpected {
				extra := filepath.Join(t.TempDir(), "extra")
				git(t, moved, "worktree", "add", "-b", "extra", extra)
				require.NoError(t, os.RemoveAll(extra))
			}
			after, err := repo.Maintain(t.Context(), worktree.MaintenanceRequest{Expected: expected, Repair: true, Prune: true})
			if unexpected {
				require.Error(t, err)
				require.DirExists(t, filepath.Join(moved, ".git", "worktrees", "missing"))
			} else {
				require.NoError(t, err)
				require.Len(t, after.Entries, 2)
				require.Equal(t, "topic", git(t, live, "branch", "--show-current"))
				require.NoDirExists(t, filepath.Join(moved, ".git", "worktrees", "missing"))
			}
		})
	}
}

func TestMaintenanceRejectsActiveCreation(t *testing.T) {
	root, _ := fixture(t)
	repo := open(t, root, kwtPolicy())
	lock := flock.New(filepath.Join(root, ".git", "kwt-worktree-create.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "kwt-worktree-create.path"), []byte(filepath.Join(t.TempDir(), "creating")), 0o600))
	_, err := repo.Maintain(t.Context(), worktree.MaintenanceRequest{Prune: true})
	require.Error(t, err)
}

func TestRemovalReportsEffectsAfterCancellation(t *testing.T) {
	root, path := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, err := worktree.NewCoordinator(kwtPolicy())
	require.NoError(t, err)
	repo, err := c.Open(ctx, worktree.RepositoryOptions{Path: root, Runner: gitcmd.New(), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, err := r.Output(ctx, dir, args...)
		if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" && err == nil {
			cancel()
			return out, ctx.Err()
		}
		return out, err
	}})
	require.NoError(t, err)
	result, err := repo.Remove(ctx, worktree.RemovalRequest{Path: path, Authority: worktree.ExactRegisteredPath, DeleteObservedBranch: true})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, result.CheckoutRemoved)
	require.True(t, result.RegistrationRemoved)
	require.Equal(t, []string{"topic"}, result.BranchesRemaining)
	require.NotEmpty(t, git(t, root, "rev-parse", "refs/heads/topic"))
}

func TestRemovalOfMissingCheckoutKeepsOtherRegistrations(t *testing.T) {
	root, repo, req := removalFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	git(t, root, "worktree", "add", "-b", "other", other)
	otherRegistration := git(t, other, "rev-parse", "--absolute-git-dir")
	require.NoError(t, os.RemoveAll(req.Path))
	require.NoError(t, os.RemoveAll(other))
	req.Conditions = nil
	result, err := repo.Remove(t.Context(), req)
	require.NoError(t, err)
	require.True(t, result.CheckoutRemoved)
	require.True(t, result.RegistrationRemoved)
	require.DirExists(t, otherRegistration)
}

func TestExactRemovalRejectsForeignReplacementAndSubdirectory(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	subdir := filepath.Join(path, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o700))
	_, err := repo.Remove(t.Context(), worktree.RemovalRequest{Path: subdir, Force: true})
	require.Error(t, err)
	require.DirExists(t, subdir)
	other, _ := fixture(t)
	require.NoError(t, os.RemoveAll(path))
	git(t, other, "worktree", "add", "-b", "replacement", path)
	_, err = repo.Remove(t.Context(), worktree.RemovalRequest{Path: path, Force: true})
	require.ErrorIs(t, err, worktree.ErrWorktreeRepositoryMismatch)
	require.Equal(t, "replacement", git(t, path, "branch", "--show-current"))
}

func TestRemoveMissingArtifactsStillDeletesRequestedBranches(t *testing.T) {
	for _, expected := range []string{"current", "moved"} {
		t.Run(expected, func(t *testing.T) {
			root, _ := fixture(t)
			git(t, root, "branch", "leftover")
			oid := git(t, root, "rev-parse", "leftover")
			if expected == "moved" {
				git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "advance")
				git(t, root, "branch", "-f", "leftover", "HEAD")
			}
			repo := open(t, root, kwtPolicy())
			result, err := repo.Remove(t.Context(), worktree.RemovalRequest{
				Path:     filepath.Join(t.TempDir(), "gone"),
				Branches: []managed.BranchRemoval{{Name: "leftover", ExpectedOID: oid, Force: true}},
			})
			require.True(t, result.CheckoutRemoved)
			require.True(t, result.RegistrationRemoved)
			if expected == "current" {
				require.NoError(t, err)
				require.Equal(t, []string{"leftover"}, result.BranchesRemoved)
				require.Empty(t, result.BranchesRemaining)
				require.Empty(t, result.Remaining.Branch)
				_, err = gitcmd.New().Output(t.Context(), root, "rev-parse", "--verify", "refs/heads/leftover")
				require.Error(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, []string{"leftover"}, result.BranchesRemaining)
			require.Equal(t, "leftover", result.Remaining.Branch)
			require.NotEmpty(t, git(t, root, "rev-parse", "refs/heads/leftover"))
		})
	}
}
