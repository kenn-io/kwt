package worktree_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	"go.kenn.io/kwt/worktree"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := gitcmd.New().Output(t.Context(), dir, args...)
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "--allow-empty", "-m", "initial")
	path := filepath.Join(t.TempDir(), "topic")
	git(t, root, "worktree", "add", "-b", "topic", path)
	return root, path
}

func open(t *testing.T, root string, policy worktree.LockPolicy) *worktree.Repository {
	t.Helper()
	coordinator, err := worktree.NewCoordinator(policy)
	require.NoError(t, err)
	repo, err := coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New()})
	require.NoError(t, err)
	return repo
}

func kwtPolicy() worktree.LockPolicy {
	return worktree.LockPolicy{
		FileName: "kwt-worktree.lock", ResolveCommonDirSymlinks: true,
		CreationLockName: "kwt-worktree-create.lock", CreationPathName: "kwt-worktree-create.path",
	}
}

func TestRepositoryScopeDoesNotReacquireForIdentityOrList(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	identity := worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var escaped *worktree.Scope
	var generation string
	require.NoError(t, repo.WithLock(ctx, func(scope *worktree.Scope) error {
		primary, primaryErr := repo.PrimaryPath(ctx)
		require.NoError(t, primaryErr)
		require.Equal(t, root, primary)
		escaped = scope
		out, rootErr := scope.RunGit(ctx, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
		require.NoError(t, rootErr)
		require.Equal(t, filepath.Join(root, ".git"), strings.TrimSpace(string(out)))
		var err error
		generation, err = scope.EnsureIdentity(ctx, path, identity)
		require.NoError(t, err)
		require.Regexp(t, "^[0-9a-f]{32}$", generation)
		require.NoError(t, scope.WithIdentity(ctx, path, worktree.IdentityPolicy{FileName: identity.FileName, Value: generation}, func(held *worktree.Scope) error {
			entries, listErr := held.List(ctx, identity)
			require.NoError(t, listErr)
			require.Len(t, entries, 2)
			require.Equal(t, generation, entries[1].Generation)
			return nil
		}))
		return nil
	}))
	_, err := escaped.RunGit(t.Context(), path, "config", "--local", "scope.expired", "true")
	require.Error(t, err)
	output, err := gitcmd.New().Output(t.Context(), path, "config", "--get", "scope.expired")
	require.Error(t, err)
	require.Empty(t, output)
	require.NoError(t, os.WriteFile(filepath.Join(path, "ordinary-change"), []byte("content"), 0o600))
	got, err := repo.ReadIdentity(t.Context(), path, identity.FileName)
	require.NoError(t, err)
	require.Equal(t, generation, got)
	called := false
	err = repo.WithIdentity(t.Context(), path, worktree.IdentityPolicy{FileName: identity.FileName, Value: strings.Repeat("0", 32)}, func(*worktree.Scope) error { called = true; return nil })
	require.Error(t, err)
	require.False(t, called)
}

func TestInventoryAndExplicitIdentityPreserveRegistration(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	registration := git(t, path, "rev-parse", "--absolute-git-dir")
	inventory, err := repo.Inspect(t.Context())
	require.NoError(t, err)
	require.True(t, inventory.Complete)
	require.Len(t, inventory.Entries, 2)
	require.Equal(t, worktree.GenerationMissing, inventory.Entries[1].GenerationStatus)
	require.NoFileExists(t, filepath.Join(registration, "kwt-generation"))
	identity := worktree.IdentityPolicy{FileName: "workspace-id", Value: "example-workspace"}
	value, err := repo.EnsureIdentity(t.Context(), path, identity)
	require.NoError(t, err)
	require.Equal(t, identity.Value, value)
	marker := filepath.Join(registration, identity.FileName)
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, identity.Value+"\n", string(data))
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(marker)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	_, err = repo.EnsureIdentity(t.Context(), path, worktree.IdentityPolicy{FileName: identity.FileName, Value: "different-workspace"})
	require.Error(t, err)
	data, err = os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, identity.Value+"\n", string(data))
	require.NoError(t, os.RemoveAll(path))
	inventory, err = repo.Inspect(t.Context())
	require.NoError(t, err)
	require.False(t, inventory.Entries[1].Exists)
	require.True(t, inventory.Entries[1].Prunable)
	got, err := repo.ReadIdentity(t.Context(), path, identity.FileName)
	require.NoError(t, err)
	require.Equal(t, identity.Value, got)
}

func TestInventoryHonorsActiveAndStaleCreationReservations(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, root, kwtPolicy())
	registration := git(t, path, "rev-parse", "--absolute-git-dir")
	lock := flock.New(filepath.Join(root, ".git", "kwt-worktree-create.lock"))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
	record := filepath.Join(root, ".git", "kwt-worktree-create.path")
	require.NoError(t, os.WriteFile(record, []byte(path+"\n"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entries, err := repo.List(ctx, worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].IsMain)
	require.NoFileExists(t, filepath.Join(registration, "kwt-generation"))
	called := false
	err = repo.WithIdentity(ctx, root, worktree.IdentityPolicy{FileName: "kwt-generation", Value: entries[0].Generation}, func(*worktree.Scope) error {
		called = true
		return nil
	})
	require.Error(t, err)
	require.False(t, called)
	_, err = repo.EnsureIdentity(ctx, path, worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.Error(t, err)
	require.NoError(t, lock.Unlock())
	entries, err = repo.List(ctx, worktree.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.NotEmpty(t, entries[1].Generation)
	require.NoFileExists(t, record)
}

func TestExistingLockHoldersExcludeRepositoryScopes(t *testing.T) {
	for _, name := range []string{"kwt", "external base", "bare alias", "nonbare symlink"} {
		t.Run(name, func(t *testing.T) {
			root, _ := fixture(t)
			policy := kwtPolicy()
			lockPath := filepath.Join(root, ".git", "kwt-worktree.lock")
			switch name {
			case "external base":
				policy = worktree.LockPolicy{FileName: ".kenn-forge-worktree.lock", NonBareRoot: t.TempDir(), BareUsesSuppliedPath: true}
				common := git(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
				lockPath = filepath.Join(policy.NonBareRoot, fmt.Sprintf("%x", sha256.Sum256([]byte(common))), policy.FileName)
			case "bare alias":
				bare := filepath.Join(t.TempDir(), "bare.git")
				git(t, root, "clone", "--bare", root, bare)
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(bare, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				policy = worktree.LockPolicy{FileName: ".kenn-forge-worktree.lock", BareUsesSuppliedPath: true}
				root = alias + string(os.PathSeparator) + "."
				lockPath = filepath.Join(bare, policy.FileName)
			case "nonbare symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				root = alias
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o700))
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLegacyLockHolderProcess$")
			cmd.Env = append(os.Environ(), "KWT_TEST_LEGACY_LOCK="+lockPath)
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			stdin, err := cmd.StdinPipe()
			require.NoError(t, err)
			cmd.Stderr = os.Stderr
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
			ready, err := bufio.NewReader(stdout).ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "locked\n", ready)
			repo := open(t, root, policy)
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			called := false
			err = repo.WithLock(ctx, func(*worktree.Scope) error { called = true; return nil })
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.False(t, called)
			require.NoError(t, stdin.Close())
			require.NoError(t, cmd.Wait())
			require.NoError(t, repo.WithLock(t.Context(), func(*worktree.Scope) error { called = true; return nil }))
			require.True(t, called)
		})
	}
}

func TestLegacyLockHolderProcess(t *testing.T) {
	path := os.Getenv("KWT_TEST_LEGACY_LOCK")
	if path == "" {
		return
	}
	lock := flock.New(path)
	require.NoError(t, lock.Lock())
	_, err := fmt.Fprintln(os.Stdout, "locked")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
	require.NoError(t, lock.Unlock())
}

func TestInventoryOpenedAtCommonGitDirectoryFindsPrimaryCheckout(t *testing.T) {
	root, path := fixture(t)
	repo := open(t, filepath.Join(root, ".git"), kwtPolicy())
	inventory, err := repo.Inspect(t.Context())
	require.NoError(t, err)
	require.True(t, inventory.Complete)
	require.Len(t, inventory.Entries, 2)
	require.Equal(t, root, inventory.Entries[0].Path)
	require.True(t, inventory.Entries[0].IsMain)
	require.Equal(t, path, inventory.Entries[1].Path)
	primary, err := repo.PrimaryPath(t.Context())
	require.NoError(t, err)
	require.Equal(t, root, primary)
}

// Git before 2.31 echoes --path-format=absolute as an unknown argument and then
// prints relative paths. Treating that output as a path would select a lock
// that processes opening the repository elsewhere do not share.
func TestOpenRejectsGitWithoutAbsolutePathFormat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Git shim is a POSIX shell script")
	}
	root, _ := fixture(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shim := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = rev-parse ] && [ \"$2\" = --path-format=absolute ]; then\n" +
		"  shift 2\n" +
		"  echo --path-format=absolute\n" +
		"  exec \"" + realGit + "\" rev-parse \"$@\"\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755))
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	coordinator, err := worktree.NewCoordinator(worktree.LockPolicy{FileName: ".kenn-forge-worktree.lock", NonBareRoot: t.TempDir()})
	require.NoError(t, err)
	_, err = coordinator.Open(t.Context(), worktree.RepositoryOptions{Path: root, Runner: gitcmd.New()})
	require.ErrorContains(t, err, "Git 2.31")
}
