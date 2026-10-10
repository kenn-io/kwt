package pullrequest_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	managed "go.kenn.io/kit/git/managed"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	pr "go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	appworktree "go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

type transactionProvider struct{ request pr.PullRequest }

func (p transactionProvider) List(context.Context, pr.Repository, string) ([]pr.PullRequest, error) {
	return []pr.PullRequest{p.request}, nil
}
func (p transactionProvider) Get(context.Context, pr.Repository, int) (pr.PullRequest, error) {
	return p.request, nil
}

type transactionFixture struct {
	root, home, path, provenance string
	project                      pr.Project
	provider                     transactionProvider
	backend                      *pr.GitBackend
	manager                      *appworktree.Manager
	store                        *pr.FileStore
}

func transactionGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func newTransactionFixture(t *testing.T, probe bool) *transactionFixture {
	t.Helper()
	root, home := t.TempDir(), t.TempDir()
	t.Setenv("KWT_HOME", home)
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(key, "Example")
	}
	for _, key := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "example@example.com")
	}
	transactionGit(t, root, "init", "-b", "main")
	transactionGit(t, root, "commit", "--allow-empty", "-m", "initial")
	transactionGit(t, root, "branch", "feature/widgets")
	transactionGit(t, root, "remote", "add", "origin", root)
	transactionGit(t, root, "remote", "set-url", "--push", "origin", "https://github.com/acme/widget.git")
	project := pr.Project{Identity: "github.com/acme/widget", Name: "widget", Path: root}
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), fmt.Appendf(nil, "[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = %q\n", root), 0o600))
	expansion, err := lifecycle.CaptureExpansionContext()
	require.NoError(t, err)
	claim, err := lifecycle.ObserveProjectClaim(t.Context(), home, root, expansion)
	require.NoError(t, err)
	release, err := lifecycle.AcquireRequiredProjectClaim(t.Context(), home, claim)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, release()) })
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: t.TempDir(), AutoMkdir: true}}
	manager := appworktree.New(gitadapter.New(root), cfg)
	path, err := manager.PreparePathForRepository("", "pr-17-feature-widgets", project.Identity)
	require.NoError(t, err)
	state, err := registry.NewAt(home)
	require.NoError(t, err)
	provenance := filepath.Join(t.TempDir(), "state", "imports.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(provenance), 0o700))
	require.NoError(t, os.WriteFile(provenance, []byte("{\"version\":1,\"imports\":{}}\n"), 0o600))
	repository := pr.Repository{Provider: "github", Identity: project.Identity, Host: "github.com", Owner: "acme", Name: "widget", CloneURL: "https://github.com/acme/widget.git"}
	request := pr.PullRequest{ID: "github:github.com/acme/widget#17", Provider: "github", Repository: repository, Number: 17, URL: "https://github.com/acme/widget/pull/17", State: "open", HeadSHA: transactionGit(t, root, "rev-parse", "HEAD"), Source: pr.Branch{Repository: repository, Name: "feature/widgets"}, Target: pr.Branch{Repository: repository, Name: "main"}}
	if probe {
		if runtime.GOOS == "windows" {
			t.Skip("cross-process lock probe uses a POSIX Git wrapper")
		}
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		dir := t.TempDir()
		script := `#!/bin/sh
previous=
for arg in "$@"; do
 if [ "$previous" = worktree ] && { [ "$arg" = add ] || [ "$arg" = remove ]; }; then
  WORKTREE_TEST_ACTION="$arg" "$WORKTREE_TEST_BINARY" -test.run=^TestImportTransactionLockProbe$ || exit 1
 fi
 previous="$arg"
done
exec "$WORKTREE_TEST_REAL_GIT" "$@"
`
		require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
		t.Setenv("WORKTREE_TEST_BINARY", os.Args[0])
		t.Setenv("WORKTREE_TEST_REAL_GIT", realGit)
		t.Setenv("WORKTREE_TEST_ROOT", root)
		t.Setenv("WORKTREE_TEST_HOME", home)
		t.Setenv("WORKTREE_TEST_PATH", path)
		t.Setenv("WORKTREE_TEST_PROVENANCE", provenance)
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	backend := pr.NewGitBackend(gitadapter.New(root), manager, project, func() (pr.WorktreeCreationGuard, error) { return state, nil }, "")
	return &transactionFixture{root: root, home: home, path: path, provenance: provenance, project: project, provider: transactionProvider{request}, backend: backend, manager: manager, store: pr.NewFileStore(provenance)}
}

func TestImportTransactionLockProbe(t *testing.T) {
	action := os.Getenv("WORKTREE_TEST_ACTION")
	if action == "" {
		t.Skip("Git subprocess probe")
	}
	home := os.Getenv("WORKTREE_TEST_HOME")
	assertLock := func(path string, wantHeld bool) {
		lock := flock.New(path)
		acquired, err := lock.TryLock()
		require.NoError(t, err)
		if acquired {
			require.NoError(t, lock.Unlock())
		}
		require.Equal(t, !wantHeld, acquired, "lock state for %s during %s", path, action)
	}
	fences, err := os.ReadDir(filepath.Join(home, "project-locks"))
	require.NoError(t, err)
	count := 0
	for _, entry := range fences {
		if entry.Name() != "registry.lock" {
			assertLock(filepath.Join(home, "project-locks", entry.Name()), true)
			count++
		}
	}
	require.Positive(t, count)
	state, err := registry.NewAt(home)
	require.NoError(t, err)
	release, acquired, err := state.AcquireCreation(os.Getenv("WORKTREE_TEST_PATH"))
	require.NoError(t, err)
	if acquired {
		require.NoError(t, release())
	}
	require.False(t, acquired, "registry path lease must outlive Git cleanup")
	assertLock(os.Getenv("WORKTREE_TEST_PROVENANCE")+".lock", action == "add")
	assertLock(filepath.Join(os.Getenv("WORKTREE_TEST_ROOT"), ".git", "kwt-worktree.lock"), true)
}

func TestImportTransactionFinalizesIdentityWithoutDeadlock(t *testing.T) {
	f := newTransactionFixture(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := pr.NewService(f.provider, f.backend, f.store).Import(ctx, f.project, "17")
	require.NoError(t, err)
	require.Equal(t, f.path, result.Workspace.Path)
	require.NotEmpty(t, result.Workspace.Generation)
	require.Equal(t, f.provider.request.HeadSHA, transactionGit(t, f.path, "rev-parse", "HEAD"))
	require.NoError(t, f.store.View(t.Context(), func(records map[string]pr.Provenance) error {
		require.Equal(t, result.Workspace.Generation, records[f.provider.request.ID].Workspace.Generation)
		return nil
	}))
}

// afterTransactionStore keeps the real provenance lock/save and injects a
// filesystem failure only after the application's transaction has completed.
type afterTransactionStore struct {
	*pr.FileStore
	after func()
}

func (s afterTransactionStore) Update(ctx context.Context, fn func(map[string]pr.Provenance) error) error {
	return s.FileStore.Update(ctx, func(records map[string]pr.Provenance) error {
		if err := fn(records); err != nil {
			return err
		}
		s.after()
		return nil
	})
}

func TestImportProvenanceFailureRollsBackUnderFreshLock(t *testing.T) {
	f := newTransactionFixture(t, true)
	before, err := os.ReadFile(f.provenance)
	require.NoError(t, err)
	// Replacing the save destination with a directory fails os.Rename on all
	// supported platforms. Keep the previous file under its own fixture path.
	backup := f.provenance + ".saved"
	store := afterTransactionStore{FileStore: f.store, after: func() {
		require.NoError(t, os.Rename(f.provenance, backup))
		require.NoError(t, os.Mkdir(f.provenance, 0o700))
	}}
	_, err = pr.NewService(f.provider, f.backend, store).Import(t.Context(), f.project, "17")
	require.Error(t, err)
	require.ErrorContains(t, err, "rolled it back")
	require.NoDirExists(t, f.path)
	require.NotContains(t, transactionGit(t, f.root, "for-each-ref", "--format=%(refname)"), "refs/heads/pr-17-feature-widgets")
	after, readErr := os.ReadFile(backup)
	require.NoError(t, readErr)
	require.Equal(t, before, after)
	require.NoError(t, os.Remove(f.provenance))
	require.NoError(t, os.Rename(backup, f.provenance))
}

func TestImportRollbackPreservesActivePlainCreation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("paused native checkout uses a POSIX hook")
	}
	f := newTransactionFixture(t, false)
	started, release := filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "release")
	script := fmt.Sprintf("#!/bin/sh\ntouch %q\nwhile [ ! -f %q ]; do sleep 0.01; done\n", started, release)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, ".git", "hooks", "post-checkout"), []byte(script), 0o755))
	done := make(chan error, 1)
	plainPath := filepath.Join(t.TempDir(), "concurrent")
	plainCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
	launched := false
	t.Cleanup(func() {
		defer cancel()
		require.NoError(t, os.WriteFile(release, nil, 0o600))
		if launched {
			require.NoError(t, <-done)
		}
	})
	before, err := os.ReadFile(f.provenance)
	require.NoError(t, err)
	backup := f.provenance + ".saved"
	var beforeRefs string
	store := afterTransactionStore{FileStore: f.store, after: func() {
		launched = true
		go func() {
			_, err := f.manager.Create(plainCtx, appworktree.CreateOptions{Branch: "concurrent", Path: plainPath, NewBranch: true, RequireGeneration: true})
			done <- err
		}()
		require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 5*time.Second, 10*time.Millisecond)
		beforeRefs = transactionGit(t, f.root, "for-each-ref", "--format=%(refname) %(objectname)")
		require.NoError(t, os.Rename(f.provenance, backup))
		require.NoError(t, os.Mkdir(f.provenance, 0o700))
	}}
	_, err = pr.NewService(f.provider, f.backend, store).Import(t.Context(), f.project, "17")
	require.ErrorContains(t, err, "rollback failed")
	require.ErrorIs(t, err, managed.ErrWorktreeCleanupIncomplete)
	require.DirExists(t, f.path)
	after, readErr := os.ReadFile(backup)
	require.NoError(t, readErr)
	require.Equal(t, before, after)
	require.Equal(t, beforeRefs, transactionGit(t, f.root, "for-each-ref", "--format=%(refname) %(objectname)"))
}
