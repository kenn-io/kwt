package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/pkg/models"
)

func testRow(repo, branch, path string) Row {
	return Row{
		Entry: &discovery.GlobalWorktreeEntry{
			RepositoryInfo: &url.RepositoryInfo{Repository: repo},
			Branch:         branch,
			Path:           path,
		},
		Status: &models.WorktreeStatus{
			Path:   path,
			Branch: branch,
			Status: models.WorktreeStatusClean,
		},
	}
}

func TestRowRepoNameFallsBackToPathBase(t *testing.T) {
	row := Row{Entry: &discovery.GlobalWorktreeEntry{Branch: "main", Path: "/tmp/no-info"}}

	assert.Equal(t, "no-info", rowRepoName(row))
	assert.Equal(t, "no-info:main", rowLabel(row))
}

func TestWorkspaceRowRendering(t *testing.T) {
	home, _ := os.UserHomeDir()
	row := Row{
		Workspace:   &WorkspaceInfo{Name: "notes", Path: filepath.Join(home, "notes")},
		SessionLive: true,
	}

	assert.Equal(t, "notes", rowRepoName(row))
	assert.Equal(t, "~/notes", rowBranch(row))
	assert.Equal(t, filepath.Join(home, "notes"), rowPath(row))
	assert.Equal(t, "local", formatMachines(row))
	assert.Equal(t, "live", formatWorkspace(row))
}

func TestSortRowsByRepoThenBranch(t *testing.T) {
	rows := []Row{
		testRow("kwt", "zeta", "/w/kwt/zeta"),
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "alpha", "/w/kwt/alpha"),
	}

	sortRows(rows)

	assert.Equal(t, []string{"kata:main", "kwt:alpha", "kwt:zeta"}, []string{
		rowLabel(rows[0]),
		rowLabel(rows[1]),
		rowLabel(rows[2]),
	})
}

func TestSortRowsByRecentActivityThenRepoBranch(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	recent := testRow("zzz", "recent", "/w/zzz/recent")
	recent.Status.LastActivity = now.Add(-5 * time.Minute)
	tieLeft := testRow("kata", "alpha", "/w/kata/alpha")
	tieLeft.Status.LastActivity = now.Add(-30 * time.Minute)
	tieRight := testRow("kwt", "beta", "/w/kwt/beta")
	tieRight.Status.LastActivity = tieLeft.Status.LastActivity
	stale := testRow("aaa", "stale", "/w/aaa/stale")
	stale.Status.LastActivity = now.Add(-2 * time.Hour)
	unknown := testRow("aaa", "unknown", "/w/aaa/unknown")

	rows := []Row{stale, unknown, tieRight, recent, tieLeft}

	sortRows(rows)

	assert.Equal(t, []string{
		"zzz:recent",
		"kata:alpha",
		"kwt:beta",
		"aaa:stale",
		"aaa:unknown",
	}, []string{
		rowLabel(rows[0]),
		rowLabel(rows[1]),
		rowLabel(rows[2]),
		rowLabel(rows[3]),
		rowLabel(rows[4]),
	})
}

func TestFilterRowsMatchesRepoBranchAndPath(t *testing.T) {
	rows := []Row{
		testRow("kwt", "feat/tui", "/work/github.com/example/kwt/feat-tui"),
		testRow("kata", "main", "/work/github.com/example/sample-app/main"),
		testRow("tools", "cleanup", "/scratch/odd-location"),
	}

	assert.Len(t, filterRows(rows, "KWT"), 1)
	assert.Equal(t, "kwt:feat/tui", rowLabel(filterRows(rows, "KWT")[0]))

	assert.Len(t, filterRows(rows, "clean"), 1)
	assert.Equal(t, "tools:cleanup", rowLabel(filterRows(rows, "clean")[0]))

	assert.Len(t, filterRows(rows, "example/sample-app"), 1)
	assert.Equal(t, "kata:main", rowLabel(filterRows(rows, "example/sample-app")[0]))
}

func TestFilterProjectRowsMatchesOnlyProjectIdentity(t *testing.T) {
	rows := []Row{
		testRow("kwt", "feature", "/work/github.com/example/kwt/feature"),
		testRow("kata", "main", "/work/github.com/example/sample-app/main"),
	}
	rows[0].Entry.RepositoryInfo.FullPath = "github.com/example/kwt"
	rows[1].Entry.RepositoryInfo.FullPath = "github.com/example/sample-app"

	assert.Equal(t, []string{"kwt:feature"}, []string{rowLabel(filterProjectRows(rows, "KWT")[0])})
	assert.Equal(t, []string{"kata:main"}, []string{rowLabel(filterProjectRows(rows, "example/sample-app")[0])})
	assert.Empty(t, filterProjectRows(rows, "feature"), "project filter must not match branch names")
}

func TestRowProjectKeyPreservesNestedRemoteNamespace(t *testing.T) {
	leftInfo, err := url.ParseRepositoryURL("https://gitlab.com/org/team-a/service.git")
	assert.NoError(t, err)
	rightInfo, err := url.ParseRepositoryURL("https://gitlab.com/org/team-b/service.git")
	assert.NoError(t, err)

	left := testRow("service", "main", "/work/gitlab.com/org/team-a/service/main")
	left.Entry.RepositoryInfo = leftInfo
	right := testRow("service", "main", "/work/gitlab.com/org/team-b/service/main")
	right.Entry.RepositoryInfo = rightInfo
	rows := []Row{left, right}

	assert.Equal(t, "gitlab.com/org/team-a/service", rowProjectKey(left))
	assert.Equal(t, "gitlab.com/org/team-b/service", rowProjectKey(right))
	assert.Equal(t, []string{left.Entry.Path}, []string{rowPath(filterProjectPerspectiveRows(
		rows,
		"gitlab.com/org/team-a/service",
	)[0])})
}

func TestFormatChanges(t *testing.T) {
	assert.Equal(t, "?", formatChanges(nil))
	assert.Equal(t, "?", formatChanges(&models.WorktreeStatus{Status: models.WorktreeStatusUnknown}))
	assert.Equal(t, "clean", formatChanges(&models.WorktreeStatus{Status: models.WorktreeStatusClean}))
	assert.Equal(t, "+2 ~1 -3 ?4", formatChanges(&models.WorktreeStatus{
		Status: models.WorktreeStatusModified,
		GitStatus: models.GitStatus{
			Added:     2,
			Modified:  1,
			Deleted:   3,
			Untracked: 4,
		},
	}))
	assert.Equal(t, "+5", formatChanges(&models.WorktreeStatus{
		Status:    models.WorktreeStatusStaged,
		GitStatus: models.GitStatus{Staged: 5},
	}))
}

func TestFormatRowChangesUsesFleetDirtyForRemoteOnlyRows(t *testing.T) {
	row := Row{Fleet: &FleetInfo{Dirty: "clean"}}

	assert.Equal(t, "clean", formatRowChanges(row))
}

func TestFormatRowChangesPrefersLocalDirtyCounts(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	row.Status.Status = models.WorktreeStatusModified
	row.Status.GitStatus.Modified = 1
	row.Fleet = &FleetInfo{
		Local: true,
		Hosts: []string{"local", "host-b"},
		Dirty: "host-b (1 modified)",
	}

	assert.Equal(t, "~1", formatRowChanges(row))
}

func TestFormatPushPullStatus(t *testing.T) {
	got, ok := formatPushPullStatus(nil)
	assert.True(t, ok)
	assert.Equal(t, "?", got)

	got, ok = formatPushPullStatus(&models.WorktreeStatus{Status: models.WorktreeStatusUnknown})
	assert.True(t, ok)
	assert.Equal(t, "?", got)

	got, ok = formatPushPullStatus(&models.WorktreeStatus{
		Status:    models.WorktreeStatusClean,
		GitStatus: models.GitStatus{},
	})
	assert.False(t, ok)
	assert.Empty(t, got)

	got, ok = formatPushPullStatus(&models.WorktreeStatus{
		Status:    models.WorktreeStatusModified,
		GitStatus: models.GitStatus{Ahead: 2, Behind: 3},
	})
	assert.True(t, ok)
	assert.Equal(t, "↑2 ↓3", got)
}

func TestFormatMachinesShowsLocalAndRemoteHosts(t *testing.T) {
	row := testRow("kwt", "main", "/w/kwt/main")
	row.Fleet = &FleetInfo{
		Local: true,
		Hosts: []string{"local", "host-b"},
	}

	assert.Equal(t, "local, host-b", formatMachines(row))
}

func TestFormatActivityAt(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	assert.Equal(t, "-", formatActivityAt(time.Time{}, now))
	assert.Equal(t, "now", formatActivityAt(now.Add(-30*time.Second), now))
	assert.Equal(t, "3m", formatActivityAt(now.Add(-3*time.Minute), now))
	assert.Equal(t, "2h", formatActivityAt(now.Add(-2*time.Hour), now))
	assert.Equal(t, "5d", formatActivityAt(now.Add(-5*24*time.Hour), now))
	assert.Equal(t, "3w", formatActivityAt(now.Add(-21*24*time.Hour), now))
}

func TestAnchorCursorByPath(t *testing.T) {
	oldRows := []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature", "/w/kwt/feature"),
	}
	newRows := []Row{
		testRow("kwt", "feature", "/w/kwt/feature"),
		testRow("kwt", "main", "/w/kwt/main"),
	}

	assert.Equal(t, 0, anchorCursorByPath(oldRows, 1, newRows))
	assert.Equal(t, 1, anchorCursorByPath(oldRows, 0, newRows))
	assert.Equal(t, 0, anchorCursorByPath(oldRows, 4, newRows))
	assert.Equal(t, 0, anchorCursorByPath(oldRows, 1, nil))
}

func TestTruncateWithEllipsis(t *testing.T) {
	assert.Equal(t, "abc", truncateWithEllipsis("abc", 5))
	assert.Equal(t, "abcdefg", truncateWithEllipsis("abcdefg", 7))
	assert.Equal(t, "abc...", truncateWithEllipsis("abcdefg", 6))
	assert.Equal(t, "...", truncateWithEllipsis("abcdefg", 3))
	assert.Equal(t, "", truncateWithEllipsis("abcdefg", 0))
}

func TestRenderRowsAlignsBodyToHeaderColumns(t *testing.T) {
	row := testRow("kwt", "test/layouts", "/w/kwt/test-layouts")
	row.SessionLive = true
	row.Status.GitStatus.Ahead = 2
	row.Status.GitStatus.Behind = 1
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	lines := strings.Split(stripANSI(viewContent(model)), "\n")
	header := findLineContaining(lines, "REPO")
	body := findLineContaining(lines, "test/layouts")

	for _, value := range []string{"kwt", "test/layouts", "clean", "↑2 ↓1", "live"} {
		assert.Equal(t, visualIndex(header, columnForValue(value)), visualIndex(body, value), value)
	}
}

func TestRenderRowsShowsLocalOnlyWhenPushPullIsZero(t *testing.T) {
	row := testRow("kwt", "main", "/w/kwt/main")
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	lines := strings.Split(stripANSI(viewContent(model)), "\n")
	body := findLineContaining(lines, "main")

	require.NotEmpty(t, body)
	assert.Contains(t, body, "local only")
	assert.NotContains(t, body, "↑0 ↓0")
	assert.NotContains(t, body, "git")
}

func columnForValue(value string) string {
	switch value {
	case "kwt":
		return "REPO"
	case "test/layouts":
		return "BRANCH"
	case "clean":
		return "CHANGES"
	case "↑2 ↓1":
		return "HEADS"
	case "live":
		return "WORKSPACE"
	default:
		return value
	}
}

func findLineContaining(lines []string, needle string) string {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func lineIndexContaining(lines []string, needle string) int {
	for i, line := range lines {
		if strings.Contains(line, needle) {
			return i
		}
	}
	return -1
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

func visibleWidth(s string) int {
	return runewidth.StringWidth(stripANSI(s))
}

func visualIndex(line, needle string) int {
	byteIndex := strings.Index(line, needle)
	if byteIndex < 0 {
		return -1
	}
	return len([]rune(line[:byteIndex]))
}
