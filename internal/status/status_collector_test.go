package status

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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

func TestParsePorcelainV2CountsUnstagedRenameCopyAndTypeChangeAsModified(t *testing.T) {
	tests := []struct {
		name   string
		record string
	}{
		{
			name:   "rename",
			record: "2 .R N... 100644 100644 100644 aaaaaaa aaaaaaa R100 renamed.txt\x00original.txt",
		},
		{
			name:   "copy",
			record: "2 .C N... 100644 100644 100644 aaaaaaa aaaaaaa C100 copied.txt\x00original.txt",
		},
		{
			name:   "type change",
			record: "1 .T N... 100644 100644 120000 aaaaaaa bbbbbbb link.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePorcelainV2(tt.record + "\x00")
			require.NoError(t, err)
			assert.Equal(t, 1, got.GitStatus.Modified)
			assert.Equal(t, models.WorktreeStatusModified,
				NewStatusCollectorWithOptions(StatusCollectorOptions{}).
					determineWorktreeState(&got.GitStatus))
		})
	}
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

func TestStatusCollectorStripsConfiguredCredentialFromEveryGitCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command recorder uses a POSIX shell")
	}
	repo := newStatusTestRepositoryAt(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	logPath := filepath.Join(wrapperDir, "git.log")
	require.NoError(t, os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(`#!/bin/sh
if [ "${CUSTOM_FLEET_TOKEN+x}" = x ]; then
  credential=present
else
  credential=absent
fi
printf '%s|%s\n' "$*" "$credential" >> "$KWT_TEST_GIT_LOG"
exec "$KWT_TEST_REAL_GIT" "$@"
`), 0o755))
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KWT_TEST_GIT_LOG", logPath)
	t.Setenv("KWT_TEST_REAL_GIT", realGit)
	t.Setenv("CUSTOM_FLEET_TOKEN", "must-not-reach-git")

	protectedNames := []string{"CUSTOM_FLEET_TOKEN"}
	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{
		ProtectedNames: protectedNames,
	})
	protectedNames[0] = "MUTATED_AFTER_CONSTRUCTION"
	_, err = collector.CollectAll(context.Background(), []*models.Worktree{{
		Path: repo, Branch: "main",
	}})

	require.NoError(t, err)
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	require.NotEmpty(t, lines)
	for _, line := range lines {
		assert.True(t, strings.HasSuffix(line, "|absent"), "credential reached git command %q", line)
	}
	log := string(logBytes)
	assert.Contains(t, log, "status --porcelain=v2 --branch -uall -z --no-ahead-behind|absent")
	assert.Contains(t, log, "show -s --format=%ct HEAD|absent")
}

func TestParsePorcelainV2WithoutUpstreamDefaultsAheadBehindToZero(t *testing.T) {
	got, err := parsePorcelainV2("# branch.oid abc\x00# branch.head main\x00")
	require.NoError(t, err)
	assert.Zero(t, got.GitStatus.Ahead)
	assert.Zero(t, got.GitStatus.Behind)
}

func TestCollectAllBoundsWorkersAndDegradesOneRow(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	collect := func(_ context.Context, wt *models.Worktree) (*models.WorktreeStatus, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		if strings.Contains(wt.Path, "broken") {
			return nil, errors.New("status unavailable")
		}
		return &models.WorktreeStatus{Path: wt.Path, Status: models.WorktreeStatusClean}, nil
	}

	result, err := collectWorktrees(context.Background(), 2, []*models.Worktree{
		{Path: "/one"}, {Path: "/broken"}, {Path: "/three"},
	}, collect)

	require.NoError(t, err)
	assert.LessOrEqual(t, maximum.Load(), int32(2))
	require.Len(t, result.Statuses, 3)
	assert.Equal(t, models.WorktreeStatusUnknown, result.Statuses[1].Status)
	require.Len(t, result.Diagnostics, 1)
	assert.Equal(t, "/broken", result.Diagnostics[0].Path)
}

func TestLastActivityUsesRootHeadAndChangedFilesOnly(t *testing.T) {
	repo := newStatusTestRepositoryAt(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	rootTime := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	changedTime := rootTime.Add(2 * time.Hour)
	require.NoError(t, os.Chtimes(repo, rootTime, rootTime))
	changed := filepath.Join(repo, "README.md")
	require.NoError(t, os.WriteFile(changed, []byte("changed"), 0o644))
	require.NoError(t, os.Chtimes(changed, changedTime, changedTime))

	result, err := NewStatusCollectorWithOptions(StatusCollectorOptions{Workers: 1}).
		CollectAll(context.Background(), []*models.Worktree{{Path: repo, Branch: "main"}})

	require.NoError(t, err)
	assert.Equal(t, changedTime, result.Statuses[0].LastActivity)
}

func TestLastActivityUsesNewWorktreeRootForCleanOldHead(t *testing.T) {
	repo := newStatusTestRepositoryAt(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	rootTime := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(repo, rootTime, rootTime))

	result, err := NewStatusCollectorWithOptions(StatusCollectorOptions{Workers: 1}).
		CollectAll(context.Background(), []*models.Worktree{{Path: repo, Branch: "main"}})

	require.NoError(t, err)
	assert.Equal(t, rootTime, result.Statuses[0].LastActivity)
}

func TestLastActivityBoundsHeadLookup(t *testing.T) {
	root := t.TempDir()
	rootInfo, err := os.Stat(root)
	require.NoError(t, err)
	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{Workers: 1})
	collector.activityTimeout = 20 * time.Millisecond
	collector.runHead = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	started := time.Now()
	got, err := collector.lastActivity(context.Background(), root, nil)

	require.NoError(t, err)
	assert.Equal(t, rootInfo.ModTime().UTC(), got)
	assert.Less(t, time.Since(started), time.Second)
}

func TestLastActivityUsesParentDirectoryForDeletedNestedFile(t *testing.T) {
	repo := newStatusTestRepositoryAt(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	nested := filepath.Join(repo, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))
	tracked := filepath.Join(nested, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("tracked"), 0o644))
	runStatusTestGit(t, repo, "add", "nested/tracked.txt")
	runStatusTestGit(t, repo, "commit", "-m", "add nested file")
	root, err := os.Stat(repo)
	require.NoError(t, err)
	recent := root.ModTime().UTC().Add(time.Hour)
	require.NoError(t, os.Remove(tracked))
	require.NoError(t, os.Chtimes(nested, recent, recent))

	result, err := NewStatusCollectorWithOptions(StatusCollectorOptions{Workers: 1}).
		CollectAll(context.Background(), []*models.Worktree{{Path: repo, Branch: "main"}})

	require.NoError(t, err)
	assert.Equal(t, recent, result.Statuses[0].LastActivity)
}

func TestCollectAllCanceledContextReturnsNoPartialCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := NewStatusCollectorWithOptions(StatusCollectorOptions{Workers: 1}).
		CollectAll(ctx, []*models.Worktree{{Path: t.TempDir()}})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, result.Statuses)
	assert.Empty(t, result.Diagnostics)
}

func TestCollectAllMarksCurrentPathByDirectoryBoundary(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main")
	mainFixPath := filepath.Join(root, "main-fix")
	require.NoError(t, os.MkdirAll(mainPath, 0755))
	require.NoError(t, os.MkdirAll(mainFixPath, 0755))
	changeDir(t, mainFixPath)

	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{})
	result, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{Path: mainPath, Branch: "main"},
		{Path: mainFixPath, Branch: "main-fix"},
	})

	require.NoError(t, err)
	require.Len(t, result.Statuses, 2)
	assert.False(t, result.Statuses[0].IsCurrent)
	assert.True(t, result.Statuses[1].IsCurrent)
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
	result, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{Path: worktreePath, Branch: "feature/read-api"},
	})

	require.NoError(t, err)
	require.Len(t, result.Statuses, 1)
	assert.Equal(t, "gitlab.com/org/team/service", result.Statuses[0].Repository)
}

func TestCollectAllPrefersWorktreeRepository(t *testing.T) {
	worktreePath := t.TempDir()
	runStatusTestGit(t, worktreePath, "init", "-b", "main")
	runStatusTestGit(t, worktreePath, "remote", "add", "origin", "https://github.com/fork/repo.git")

	collector := NewStatusCollectorWithOptions(StatusCollectorOptions{})
	result, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{
			Path:       worktreePath,
			Branch:     "main",
			Repository: "github.com/upstream/repo",
		},
	})

	require.NoError(t, err)
	require.Len(t, result.Statuses, 1)
	assert.Equal(t, "github.com/upstream/repo", result.Statuses[0].Repository)
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
	result, err := collector.CollectAll(context.Background(), []*models.Worktree{
		{Path: worktreePath, Branch: "main"},
	})

	require.NoError(t, err)
	require.Len(t, result.Statuses, 1)
	assert.Equal(t, "repo-main", result.Statuses[0].Repository)
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
