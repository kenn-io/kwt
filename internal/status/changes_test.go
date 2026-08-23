package status

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitpkg "go.kenn.io/kwt/internal/git"
)

func TestDeriveChangeSetUsesCanonicalBucketsAndDeterministicOrder(t *testing.T) {
	files := []FileChange{
		{Path: "z-conflict", Index: FileStateConflicted, Worktree: FileStateConflicted},
		{Path: "u-untracked", Worktree: FileStateUntracked},
		{Path: "a-added", Index: FileStateAdded, Worktree: FileStateModified},
		{Path: "d-deleted", Worktree: FileStateDeleted},
		{Path: "r-renamed", OriginalPath: "old-renamed", Index: FileStateRenamed},
		{Path: "c-copied", OriginalPath: "source", Index: FileStateCopied},
		{Path: "m-modified", Worktree: FileStateModified},
	}

	got := deriveChangeSet(files)

	assert.Equal(t, ChangeStateConflicted, got.State)
	assert.Equal(t, ChangeSummary{
		Modified:  3,
		Added:     1,
		Deleted:   1,
		Untracked: 1,
		Staged:    3,
		Conflicts: 1,
	}, got.Summary)
	assert.Equal(t, []string{
		"a-added",
		"c-copied",
		"d-deleted",
		"m-modified",
		"r-renamed",
		"u-untracked",
		"z-conflict",
	}, changePaths(got.Files))
	assert.Equal(t, "z-conflict", files[0].Path, "deriveChangeSet mutated its input")
}

func TestDeriveChangeSetDoesNotCountConflictAsStaged(t *testing.T) {
	got := deriveChangeSet([]FileChange{{
		Path:     "conflict.txt",
		Index:    FileStateConflicted,
		Worktree: FileStateConflicted,
	}})

	assert.Equal(t, ChangeStateConflicted, got.State)
	assert.Equal(t, 1, got.Summary.Conflicts)
	assert.Zero(t, got.Summary.Staged)
}

func TestDeriveChangeSetStatePrecedenceAndCleanFiles(t *testing.T) {
	tests := []struct {
		name  string
		files []FileChange
		want  ChangeState
	}{
		{name: "clean", files: nil, want: ChangeStateClean},
		{
			name:  "modified",
			files: []FileChange{{Path: "file", Worktree: FileStateModified}},
			want:  ChangeStateModified,
		},
		{
			name:  "staged",
			files: []FileChange{{Path: "file", Index: FileStateModified}},
			want:  ChangeStateStaged,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveChangeSet(tt.files)

			assert.Equal(t, tt.want, got.State)
			if tt.files == nil {
				assert.NotNil(t, got.Files)
				assert.Empty(t, got.Files)
			}
		})
	}
}

func TestCoalesceFileChangesMergesCompatibleRecords(t *testing.T) {
	tests := []struct {
		name  string
		files []FileChange
		want  []FileChange
	}{
		{
			name: "independent sides",
			files: []FileChange{
				{Path: "file.txt", Index: FileStateDeleted},
				{Path: "file.txt", Worktree: FileStateUntracked},
			},
			want: []FileChange{{
				Path: "file.txt", Index: FileStateDeleted, Worktree: FileStateUntracked,
			}},
		},
		{
			name: "identical records",
			files: []FileChange{
				{Path: "new.txt", OriginalPath: "old.txt", Index: FileStateRenamed},
				{Path: "new.txt", OriginalPath: "old.txt", Index: FileStateRenamed},
			},
			want: []FileChange{{
				Path: "new.txt", OriginalPath: "old.txt", Index: FileStateRenamed,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := coalesceFileChanges(tt.files)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCoalesceFileChangesRejectsIncompatibleRecords(t *testing.T) {
	tests := []struct {
		name  string
		files []FileChange
	}{
		{
			name: "index",
			files: []FileChange{
				{Path: "file.txt", Index: FileStateAdded},
				{Path: "file.txt", Index: FileStateDeleted},
			},
		},
		{
			name: "worktree",
			files: []FileChange{
				{Path: "file.txt", Worktree: FileStateModified},
				{Path: "file.txt", Worktree: FileStateUntracked},
			},
		},
		{
			name: "original path",
			files: []FileChange{
				{Path: "file.txt", OriginalPath: "one.txt", Index: FileStateRenamed},
				{Path: "file.txt", OriginalPath: "two.txt", Index: FileStateRenamed},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := coalesceFileChanges(tt.files)

			require.Error(t, err)
			assert.ErrorContains(t, err, "incompatible "+tt.name)
			assert.ErrorContains(t, err, "file.txt")
		})
	}
}

func TestCollectChangesFromRealRepository(t *testing.T) {
	repo := newChangesTestRepository(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "modified.txt"),
		[]byte("modified after commit\n"),
		0o644,
	))
	require.NoError(t, os.Remove(filepath.Join(repo, "deleted.txt")))
	runStatusTestGit(t, repo, "mv", "rename-source.txt", "renamed.txt")
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "added.txt"),
		[]byte("staged addition\n"),
		0o644,
	))
	runStatusTestGit(t, repo, "add", "added.txt")
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "untracked.txt"),
		[]byte("untracked\n"),
		0o644,
	))

	got, err := CollectChanges(context.Background(), repo, nil)

	require.NoError(t, err)
	assert.Equal(t, ChangeStateStaged, got.State)
	assert.Equal(t, ChangeSummary{
		Modified:  2,
		Added:     1,
		Deleted:   1,
		Untracked: 1,
		Staged:    2,
	}, got.Summary)
	assert.Equal(t, []string{
		"added.txt",
		"deleted.txt",
		"modified.txt",
		"renamed.txt",
		"untracked.txt",
	}, changePaths(got.Files))
	assert.Equal(t, "rename-source.txt", got.Files[3].OriginalPath)
}

func TestCollectChangesCoalescesStagedDeletionRecreatedAsUntracked(t *testing.T) {
	repo := newChangesTestRepository(t)
	runStatusTestGit(t, repo, "rm", "deleted.txt")
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "deleted.txt"),
		[]byte("recreated after staged deletion\n"),
		0o644,
	))

	got, err := CollectChanges(context.Background(), repo, nil)

	require.NoError(t, err)
	assert.Equal(t, ChangeStateStaged, got.State)
	assert.Equal(t, ChangeSummary{Untracked: 1, Staged: 1}, got.Summary)
	assert.Equal(t, []FileChange{{
		Path: "deleted.txt", Index: FileStateDeleted, Worktree: FileStateUntracked,
	}}, got.Files)
}

func TestCollectChangesCleanRepositoryHasPresentEmptyFiles(t *testing.T) {
	repo := newChangesTestRepository(t)

	got, err := CollectChanges(context.Background(), repo, nil)

	require.NoError(t, err)
	assert.Equal(t, ChangeStateClean, got.State)
	assert.NotNil(t, got.Files)
	assert.Empty(t, got.Files)
}

func TestCollectChangesUsesOneSanitizedPorcelainV2StatusCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command recorder uses a POSIX shell")
	}
	repo := newChangesTestRepository(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	logPath := filepath.Join(wrapperDir, "git.log")
	wrapperPath := filepath.Join(wrapperDir, "git")
	require.NoError(t, os.WriteFile(wrapperPath, []byte(`#!/bin/sh
{
  for argument in "$@"; do
    printf 'arg=%s\n' "$argument"
  done
  printf 'locks=%s\n' "$GIT_OPTIONAL_LOCKS"
  printf 'locale=%s\n' "$LC_ALL"
  printf 'builtin=%s\n' "$KWT_GITHUB_TOKEN"
  printf 'custom=%s\n' "$CUSTOM_CHANGE_TOKEN"
} >> "$KWT_TEST_GIT_LOG"
exec "$KWT_TEST_REAL_GIT" "$@"
`), 0o755))
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KWT_TEST_GIT_LOG", logPath)
	t.Setenv("KWT_TEST_REAL_GIT", realGit)
	t.Setenv("KWT_GITHUB_TOKEN", "builtin-secret")
	t.Setenv("CUSTOM_CHANGE_TOKEN", "custom-secret")

	_, err = CollectChanges(
		context.Background(),
		repo,
		[]string{"CUSTOM_CHANGE_TOKEN"},
	)

	require.NoError(t, err)
	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, "arg=status\n"+
		"arg=--porcelain=v2\n"+
		"arg=-z\n"+
		"arg=--untracked-files=all\n"+
		"locks=0\n"+
		"locale=C\n"+
		"builtin=\n"+
		"custom=\n", string(log))
}

func TestCollectChangesAppliesFiveSecondBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("timeout helper uses a POSIX shell")
	}
	wrapperDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(wrapperDir, "git"),
		[]byte("#!/bin/sh\nexec sleep 60\n"),
		0o755,
	))
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	started := time.Now()

	_, err := CollectChanges(context.Background(), t.TempDir(), nil)
	elapsed := time.Since(started)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, elapsed, 4*time.Second)
	assert.Less(t, elapsed, 8*time.Second)
}

func TestCollectChangesKeepsOversizedDiagnosticsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("diagnostic helper uses a POSIX shell")
	}
	wrapperDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(wrapperDir, "git"),
		[]byte("#!/bin/sh\nhead -c 1048577 /dev/zero >&2\nexit 1\n"),
		0o755,
	))
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := CollectChanges(context.Background(), t.TempDir(), nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, gitpkg.ErrStderrLimitExceeded)
	assert.Less(t, len(err.Error()), 1024)
}

func newChangesTestRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runStatusTestGit(t, repo, "init", "-b", "main")
	runStatusTestGit(t, repo, "config", "user.name", "Test User")
	runStatusTestGit(t, repo, "config", "user.email", "test@example.com")
	for name, contents := range map[string]string{
		"modified.txt":      "original modified file\n",
		"deleted.txt":       "delete me\n",
		"rename-source.txt": "this content is long enough for rename detection without ambiguity\n",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(repo, name),
			[]byte(contents),
			0o644,
		))
	}
	runStatusTestGit(t, repo, "add", ".")
	runStatusTestGit(t, repo, "commit", "-m", "Initial commit")
	return repo
}

func changePaths(changes []FileChange) []string {
	paths := make([]string, len(changes))
	for i, change := range changes {
		paths[i] = change.Path
	}
	return paths
}
