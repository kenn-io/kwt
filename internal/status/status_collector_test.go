package status

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/pkg/models"
)

func TestParsePorcelainV2CountsFilesAndBranchState(t *testing.T) {
	raw := strings.Join([]string{
		"# branch.oid 0123456789abcdef",
		"# branch.head topic",
		"# branch.upstream origin/topic",
		"# branch.ab +2 -3",
		"1 .M N... 100644 100644 100644 aaaaaaa aaaaaaa tracked.txt",
		"1 A. N... 000000 100644 100644 0000000 bbbbbbb staged.txt",
		"? untracked/one.txt",
		"? untracked/two.txt",
	}, "\x00") + "\x00"

	got, err := parsePorcelainV2(raw)
	require.NoError(t, err)
	assert.Equal(t, 1, got.GitStatus.Modified)
	assert.Equal(t, 1, got.GitStatus.Staged)
	assert.Equal(t, 2, got.GitStatus.Untracked)
	assert.Equal(t, 2, got.GitStatus.Ahead)
	assert.Equal(t, 3, got.GitStatus.Behind)
	assert.Equal(t, []string{"tracked.txt", "staged.txt", "untracked/one.txt", "untracked/two.txt"}, got.Paths)
}

func TestCollectPorcelainUsesUAllForPerFileUntrackedCount(t *testing.T) {
	repo := newStatusTestRepositoryAt(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "new"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "new", "one"), []byte("1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "new", "two"), []byte("2"), 0o644))

	got, err := NewStatusCollectorWithOptions(StatusCollectorOptions{}).
		collectPorcelain(context.Background(), git.New(repo))

	require.NoError(t, err)
	assert.Equal(t, 2, got.GitStatus.Untracked)
}

func TestParsePorcelainV2WithoutUpstreamDefaultsAheadBehindToZero(t *testing.T) {
	got, err := parsePorcelainV2("# branch.oid abc\x00# branch.head main\x00")
	require.NoError(t, err)
	assert.Zero(t, got.GitStatus.Ahead)
	assert.Zero(t, got.GitStatus.Behind)
}

func TestCollectAllMarksCurrentPathByDirectoryBoundary(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main")
	mainFixPath := filepath.Join(root, "main-fix")
	require.NoError(t, os.MkdirAll(mainPath, 0755))
	require.NoError(t, os.MkdirAll(mainFixPath, 0755))
	changeDir(t, mainFixPath)

	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{})
	statuses, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{Path: mainPath, Branch: "main"},
		{Path: mainFixPath, Branch: "main-fix"},
	})

	require.NoError(t, err)
	require.Len(t, statuses, 2)
	assert.False(t, statuses[0].IsCurrent)
	assert.True(t, statuses[1].IsCurrent)
}

func TestCollectAllUsesRemoteFullPathForNestedRepository(t *testing.T) {
	baseDir := t.TempDir()
	worktreePath := filepath.Join(baseDir, "gitlab.com", "org", "team", "service", "feature-read-api")
	require.NoError(t, os.MkdirAll(worktreePath, 0755))
	runStatusTestGit(t, worktreePath, "init", "-b", "main")
	runStatusTestGit(t, worktreePath, "config", "user.name", "Test User")
	runStatusTestGit(t, worktreePath, "config", "user.email", "test@example.com")
	runStatusTestGit(t, worktreePath, "remote", "add", "origin", "https://gitlab.com/org/team/service.git")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "README.md"), []byte("# service\n"), 0644))
	runStatusTestGit(t, worktreePath, "add", ".")
	runStatusTestGit(t, worktreePath, "commit", "-m", "Initial commit")

	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{BaseDir: baseDir})
	statuses, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{Path: worktreePath, Branch: "feature/read-api"},
	})

	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "gitlab.com/org/team/service", statuses[0].Repository)
}

func TestCollectAllPrefersWorktreeRepository(t *testing.T) {
	worktreePath := t.TempDir()
	runStatusTestGit(t, worktreePath, "init", "-b", "main")
	runStatusTestGit(t, worktreePath, "remote", "add", "origin", "https://github.com/fork/repo.git")

	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{})
	statuses, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{
			Path:       worktreePath,
			Branch:     "main",
			Repository: "github.com/upstream/repo",
		},
	})

	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "github.com/upstream/repo", statuses[0].Repository)
}

// TestCollectAllRelativeDotlessRemoteFallsBackToPathIdentity pins the
// remote-provenance gate on the status surface: a relative dotless filesystem
// remote ("cache/team/repo.git" — git accepts it with no leading "./") must
// not be published as a shareable-looking "cache/team/repo" identity; the
// collector falls back to its path-derived identity instead, matching the
// identity bar kwt list and kwt projects apply.
func TestCollectAllRelativeDotlessRemoteFallsBackToPathIdentity(t *testing.T) {
	baseDir := t.TempDir()
	worktreePath := filepath.Join(baseDir, "repo-main")
	require.NoError(t, os.MkdirAll(worktreePath, 0755))
	runStatusTestGit(t, worktreePath, "init", "-b", "main")
	runStatusTestGit(t, worktreePath, "config", "user.name", "Test User")
	runStatusTestGit(t, worktreePath, "config", "user.email", "test@example.com")
	runStatusTestGit(t, worktreePath, "remote", "add", "origin", "cache/team/repo.git")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "README.md"), []byte("# repo\n"), 0644))
	runStatusTestGit(t, worktreePath, "add", ".")
	runStatusTestGit(t, worktreePath, "commit", "-m", "Initial commit")

	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{BaseDir: baseDir})
	statuses, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{Path: worktreePath, Branch: "main"},
	})

	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "repo-main", statuses[0].Repository)
}

func TestRepositoryFullPathIdentityNormalizesWindowsSeparators(t *testing.T) {
	info := &url.RepositoryInfo{FullPath: `gitlab.com\org\team\service`}

	assert.Equal(t, "gitlab.com/org/team/service", repositoryFullPathIdentity(info))
}

func TestGetLastActivityFallbackHonorsCanceledContext(t *testing.T) {
	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := collector.getLastActivityFallback(ctx, t.TempDir())

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func changeDir(t *testing.T, dir string) {
	t.Helper()

	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(oldWd))
	})
}

func runStatusTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(output))
}

func newStatusTestRepositoryAt(t *testing.T, commitTime time.Time) string {
	t.Helper()
	repo := t.TempDir()
	runStatusTestGit(t, repo, "init", "-b", "main")
	runStatusTestGit(t, repo, "config", "user.name", "Test User")
	runStatusTestGit(t, repo, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("initial\n"), 0o644))
	runStatusTestGit(t, repo, "add", "README.md")
	command := exec.Command("git", "commit", "-m", "initial")
	command.Dir = repo
	stamp := commitTime.Format(time.RFC3339)
	command.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "git commit failed: %s", output)
	return repo
}
