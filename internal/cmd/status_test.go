package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/ui"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

func TestCollectWorktreeStatusesGlobalPreservesRegisteredRepository(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "repo")
	require.NoError(t, os.MkdirAll(repoDir, 0755))
	require.NoError(t, cmdStatusTestGit(repoDir, "init", "-b", "main"))
	require.NoError(t, cmdStatusTestGit(repoDir, "remote", "add", "origin", "https://github.com/fork/repo.git"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.name", "Test User"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.email", "test@example.com"))
	require.NoError(t, cmdStatusTestGit(repoDir, "commit", "--allow-empty", "-m", "initial"))

	resetStatusTestFlags(t)
	statusGlobal = true
	statusNoFetch = true
	statuses, err := collectWorktreeStatuses(context.Background(), &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir},
		Projects: []models.Project{{
			Path:       repoDir,
			Repository: "github.com/upstream/repo",
		}},
	}, ui.New(&models.UIConfig{}))

	require.NoError(t, err)
	require.Len(t, statuses, 1)
	require.Equal(t, "github.com/upstream/repo", statuses[0].Repository)
}

func TestCollectWorktreeStatusesLocalUsesCanonicalLocalRepository(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "repo")
	require.NoError(t, os.MkdirAll(repoDir, 0755))
	require.NoError(t, cmdStatusTestGit(repoDir, "init", "-b", "main"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.name", "Test User"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.email", "test@example.com"))
	require.NoError(t, cmdStatusTestGit(repoDir, "commit", "--allow-empty", "-m", "initial"))
	changeDir(t, repoDir)

	want, err := worktree.RepositoryInfoFromLocalPath(repoDir)
	require.NoError(t, err)
	resetStatusTestFlags(t)
	statusNoFetch = true
	statuses, err := collectWorktreeStatuses(context.Background(), &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir},
	}, ui.New(&models.UIConfig{}))

	require.NoError(t, err)
	require.Len(t, statuses, 1)
	require.Equal(t, want.FullPath, statuses[0].Repository)
}

func TestCollectWorktreeStatusesReturnsPerWorktreeFailure(t *testing.T) {
	repoDir := t.TempDir()
	require.NoError(t, cmdStatusTestGit(repoDir, "init", "-b", "main"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.name", "Test User"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.email", "test@example.com"))
	require.NoError(t, cmdStatusTestGit(repoDir, "commit", "--allow-empty", "-m", "initial"))
	linked := filepath.Join(t.TempDir(), "missing-linked-worktree")
	require.NoError(t, cmdStatusTestGit(repoDir, "worktree", "add", "-b", "topic", linked))
	require.NoError(t, os.RemoveAll(linked))
	changeDir(t, repoDir)
	resetStatusTestFlags(t)
	statusNoFetch = true

	statuses, err := collectWorktreeStatuses(context.Background(), &models.Config{}, ui.New(&models.UIConfig{}))

	assert.Nil(t, statuses)
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Base(linked))
}

func TestImportedWorktreeReceivesAutomaticStatusAndFleetInspection(t *testing.T) {
	resetStatusTestFlags(t)
	resetFleetCommandDeps(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repoDir := t.TempDir()
	require.NoError(t, cmdStatusTestGit(repoDir, "init", "-b", "main"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.name", "Test User"))
	require.NoError(t, cmdStatusTestGit(repoDir, "config", "user.email", "test@example.com"))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoDir, "README.md"),
		[]byte("# remote\n"),
		0644,
	))
	require.NoError(t, cmdStatusTestGit(repoDir, "add", "."))
	require.NoError(t, cmdStatusTestGit(repoDir, "commit", "-m", "Initial commit"))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoDir, "README.md"),
		[]byte("# changed\n"),
		0644,
	))
	builder := newFleetManifestBuilder()
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:                   repoDir,
		Branch:                 "main",
		UnreviewedRemoteSource: true,
	}))
	t.Chdir(repoDir)
	statusNoFetch = true
	cfg := &models.Config{
		Fleet: models.FleetConfig{HostID: "host-a"},
		Projects: []models.Project{{
			Repository: "github.com/acme/widget",
			Path:       repoDir,
		}},
	}

	statuses, err := collectWorktreeStatuses(
		context.Background(),
		cfg,
		ui.New(&models.UIConfig{}),
	)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	require.Equal(t, models.WorktreeStatusModified, statuses[0].Status)
	require.Equal(t, 1, statuses[0].GitStatus.Modified)

	manifest, err := builder.Build(context.Background(), cfg)
	require.NoError(t, err)
	require.Len(t, manifest.Worktrees, 1)
	require.Equal(t, 1, manifest.Worktrees[0].Status.Modified)
}

func resetStatusTestFlags(t *testing.T) {
	t.Helper()
	origGlobal, origNoFetch, origStaleDays := statusGlobal, statusNoFetch, statusStaleDays
	t.Cleanup(func() {
		statusGlobal, statusNoFetch, statusStaleDays = origGlobal, origNoFetch, origStaleDays
	})
	statusGlobal = false
	statusNoFetch = false
	statusStaleDays = 14
}

func cmdStatusTestGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

func TestCalculateSummary(t *testing.T) {
	tests := []struct {
		name     string
		statuses []*models.WorktreeStatus
		want     statusSummary
	}{
		{
			name:     "empty statuses",
			statuses: []*models.WorktreeStatus{},
			want:     statusSummary{Total: 0, Modified: 0, Clean: 0, Stale: 0},
		},
		{
			name: "mixed statuses",
			statuses: []*models.WorktreeStatus{
				{Status: models.WorktreeStatusClean},
				{Status: models.WorktreeStatusModified},
				{Status: models.WorktreeStatusModified},
				{Status: models.WorktreeStatusStale},
			},
			want: statusSummary{Total: 4, Modified: 2, Clean: 1, Stale: 1},
		},
		{
			name: "all clean",
			statuses: []*models.WorktreeStatus{
				{Status: models.WorktreeStatusClean},
				{Status: models.WorktreeStatusClean},
			},
			want: statusSummary{Total: 2, Modified: 0, Clean: 2, Stale: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateSummary(tt.statuses)
			if got != tt.want {
				t.Errorf("calculateSummary() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilterStatuses(t *testing.T) {
	statuses := []*models.WorktreeStatus{
		{Branch: "main", Status: models.WorktreeStatusClean},
		{Branch: "feature1", Status: models.WorktreeStatusModified},
		{Branch: "feature2", Status: models.WorktreeStatusModified},
		{Branch: "old", Status: models.WorktreeStatusStale},
	}

	tests := []struct {
		name   string
		filter string
		want   int
	}{
		{
			name:   "filter modified",
			filter: "modified",
			want:   2,
		},
		{
			name:   "filter clean",
			filter: "clean",
			want:   1,
		},
		{
			name:   "filter stale",
			filter: "stale",
			want:   1,
		},
		{
			name:   "invalid filter",
			filter: "invalid",
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterStatuses(statuses, tt.filter)
			if len(got) != tt.want {
				t.Errorf("filterStatuses() returned %d items, want %d", len(got), tt.want)
			}
		})
	}
}

func TestFormatActivity(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		time     time.Time
		expected string
	}{
		{
			name:     "zero time",
			time:     time.Time{},
			expected: "unknown",
		},
		{
			name:     "just now",
			time:     now.Add(-30 * time.Second),
			expected: "just now",
		},
		{
			name:     "minutes ago",
			time:     now.Add(-5 * time.Minute),
			expected: "5 mins ago",
		},
		{
			name:     "1 minute ago",
			time:     now.Add(-1 * time.Minute),
			expected: "1 min ago",
		},
		{
			name:     "hours ago",
			time:     now.Add(-3 * time.Hour),
			expected: "3 hours ago",
		},
		{
			name:     "1 hour ago",
			time:     now.Add(-1 * time.Hour),
			expected: "1 hour ago",
		},
		{
			name:     "days ago",
			time:     now.Add(-2 * 24 * time.Hour),
			expected: "2 days ago",
		},
		{
			name:     "1 day ago",
			time:     now.Add(-1 * 24 * time.Hour),
			expected: "1 day ago",
		},
		{
			name:     "weeks ago",
			time:     now.Add(-14 * 24 * time.Hour),
			expected: "2 weeks ago",
		},
		{
			name:     "1 week ago",
			time:     now.Add(-7 * 24 * time.Hour),
			expected: "1 week ago",
		},
		{
			name:     "months ago",
			time:     now.Add(-60 * 24 * time.Hour),
			expected: "2 months ago",
		},
		{
			name:     "1 month ago",
			time:     now.Add(-30 * 24 * time.Hour),
			expected: "1 month ago",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatActivity(tt.time)
			if got != tt.expected {
				t.Errorf("formatActivity() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFormatChanges(t *testing.T) {
	tests := []struct {
		name     string
		status   models.GitStatus
		expected string
	}{
		{
			name:     "no changes",
			status:   models.GitStatus{},
			expected: "-",
		},
		{
			name: "only added",
			status: models.GitStatus{
				Added: 5,
			},
			expected: "5 added",
		},
		{
			name: "only modified",
			status: models.GitStatus{
				Modified: 3,
			},
			expected: "3 modified",
		},
		{
			name: "only deleted",
			status: models.GitStatus{
				Deleted: 2,
			},
			expected: "2 deleted",
		},
		{
			name: "only untracked",
			status: models.GitStatus{
				Untracked: 4,
			},
			expected: "4 untracked",
		},
		{
			name: "mixed changes",
			status: models.GitStatus{
				Added:     5,
				Modified:  3,
				Deleted:   2,
				Untracked: 1,
			},
			expected: "5 added, 3 modified, 2 deleted, 1 untracked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatChanges(tt.status)
			if got != tt.expected {
				t.Errorf("formatChanges() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFormatAheadBehind(t *testing.T) {
	tests := []struct {
		name     string
		ahead    int
		behind   int
		expected string
	}{
		{
			name:     "no ahead or behind",
			ahead:    0,
			behind:   0,
			expected: "↑0 ↓0",
		},
		{
			name:     "ahead only",
			ahead:    5,
			behind:   0,
			expected: "↑5 ↓0",
		},
		{
			name:     "behind only",
			ahead:    0,
			behind:   3,
			expected: "↑0 ↓3",
		},
		{
			name:     "both ahead and behind",
			ahead:    2,
			behind:   4,
			expected: "↑2 ↓4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatAheadBehind(tt.ahead, tt.behind)
			if got != tt.expected {
				t.Errorf("formatAheadBehind() = %q, want %q", got, tt.expected)
			}
		})
	}
}
