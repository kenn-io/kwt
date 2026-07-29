package tui

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

var timeNow = time.Now

func rowRepoName(row Row) string {
	if row.Workspace != nil {
		return row.Workspace.Name
	}
	if row.Entry != nil && row.Entry.RepositoryInfo != nil && row.Entry.RepositoryInfo.Repository != "" {
		return row.Entry.RepositoryInfo.Repository
	}
	if row.Fleet != nil {
		if row.Fleet.ProjectName != "" {
			return row.Fleet.ProjectName
		}
		if row.Fleet.ProjectIdentity != "" {
			return filepath.Base(row.Fleet.ProjectIdentity)
		}
	}
	if row.Entry != nil && row.Entry.Path != "" {
		return filepath.Base(row.Entry.Path)
	}
	if row.Status != nil && row.Status.Repository != "" {
		return filepath.Base(row.Status.Repository)
	}
	if row.Status != nil && row.Status.Path != "" {
		return filepath.Base(row.Status.Path)
	}
	return ""
}

func rowBranch(row Row) string {
	if row.Workspace != nil {
		return utils.TildePath(row.Workspace.Path)
	}
	if row.Entry != nil {
		return row.Entry.Branch
	}
	if row.Fleet != nil {
		if row.Fleet.Branch != "" {
			return row.Fleet.Branch
		}
		return row.Fleet.Ref
	}
	if row.Status != nil {
		return row.Status.Branch
	}
	return ""
}

// FleetKey identifies one worktree for matching hub state against what is on
// disk. The project half is folded per host, because a clone's remote URL and a
// hub manifest can spell one forge identity differently while a case-sensitive
// server can spell two repositories that way. The ref half is never folded:
// branch names are case-sensitive everywhere.
//
// This must fold identities exactly as the hub groups them, or a local worktree
// stops matching its own hub row.
func FleetKey(projectIdentity string, kind string, ref string) string {
	projectIdentity = url.FoldRepositoryIdentity(projectIdentity)
	kind = strings.ToLower(strings.TrimSpace(kind))
	ref = strings.TrimSpace(ref)
	if projectIdentity == "" || kind == "" || ref == "" {
		return ""
	}
	return projectIdentity + "\x00" + kind + "\x00" + ref
}

// FleetKeyForRow derives FleetKey from a row in whichever shape it arrived:
// reported by the hub, or discovered on disk. Both shapes of one worktree
// produce the same key, which is what lets a remote-only row be recognized once
// it becomes local. Empty when a row carries too little to be matched.
func FleetKeyForRow(row Row) string {
	if row.Fleet != nil {
		return FleetKey(row.Fleet.ProjectIdentity, row.Fleet.Kind, row.Fleet.Ref)
	}
	if row.Entry == nil || row.Entry.RepositoryInfo == nil {
		return ""
	}
	identity := row.Entry.RepositoryInfo.FullPath
	info := row.Entry.RepositoryInfo
	if identity == "" && info.Host != "" && info.Owner != "" && info.Repository != "" {
		identity = path.Join(info.Host, info.Owner, info.Repository)
	}
	if identity == "" {
		return ""
	}
	branch := strings.TrimSpace(row.Entry.Branch)
	if branch == "" || branch == "HEAD" {
		// Hub manifests key detached worktrees by commit SHA.
		return FleetKey(identity, "detached", strings.TrimSpace(row.Entry.CommitHash))
	}
	return FleetKey(identity, "branch", branch)
}

// isRemoteOnly reports whether a row exists only as hub state, with nothing on
// this machine behind it.
func isRemoteOnly(row Row) bool {
	return row.Entry == nil && row.Workspace == nil && row.Fleet != nil
}

func rowPath(row Row) string {
	if row.Workspace != nil {
		return row.Workspace.Path
	}
	if row.Entry != nil {
		return row.Entry.Path
	}
	if row.Status != nil {
		return row.Status.Path
	}
	return ""
}

func rowLabel(row Row) string {
	return rowRepoName(row) + ":" + rowBranch(row)
}

func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		leftActivity := rowLastActivity(rows[i])
		rightActivity := rowLastActivity(rows[j])
		if !leftActivity.Equal(rightActivity) {
			if leftActivity.IsZero() {
				return false
			}
			if rightActivity.IsZero() {
				return true
			}
			return leftActivity.After(rightActivity)
		}

		leftRepo := strings.ToLower(rowRepoName(rows[i]))
		rightRepo := strings.ToLower(rowRepoName(rows[j]))
		if leftRepo != rightRepo {
			return leftRepo < rightRepo
		}
		leftBranch := strings.ToLower(rowBranch(rows[i]))
		rightBranch := strings.ToLower(rowBranch(rows[j]))
		if leftBranch != rightBranch {
			return leftBranch < rightBranch
		}
		return rowPath(rows[i]) < rowPath(rows[j])
	})
}

func rowLastActivity(row Row) time.Time {
	if row.Status == nil {
		return time.Time{}
	}
	return row.Status.LastActivity
}

func filterRows(rows []Row, filter string) []Row {
	filter = strings.TrimSpace(strings.ToLower(filter))
	if filter == "" {
		return append([]Row(nil), rows...)
	}

	filtered := make([]Row, 0, len(rows))
	for _, row := range rows {
		haystack := strings.ToLower(strings.Join([]string{
			rowRepoName(row),
			rowBranch(row),
			rowPath(row),
			rowLabel(row),
			rowFleetHaystack(row),
		}, " "))
		if strings.Contains(haystack, filter) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func filterProjectRows(rows []Row, filter string) []Row {
	filter = strings.TrimSpace(strings.ToLower(filter))
	if filter == "" {
		return append([]Row(nil), rows...)
	}

	filtered := make([]Row, 0, len(rows))
	for _, row := range rows {
		haystack := strings.ToLower(strings.Join(rowProjectIdentity(row), " "))
		if strings.Contains(haystack, filter) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func filterProjectPerspectiveRows(rows []Row, project string) []Row {
	project = strings.TrimSpace(project)
	if project == "" {
		return append([]Row(nil), rows...)
	}

	// Fold as the fleet key does, so scoping to a project cannot pull in a
	// different repository that a case-sensitive host distinguishes.
	project = url.FoldRepositoryIdentity(project)
	filtered := make([]Row, 0, len(rows))
	for _, row := range rows {
		if url.FoldRepositoryIdentity(rowProjectKey(row)) == project {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func rowProjectIdentity(row Row) []string {
	parts := []string{rowRepoName(row)}
	if row.Entry != nil && row.Entry.RepositoryInfo != nil {
		info := row.Entry.RepositoryInfo
		parts = append(parts, info.Host, info.Owner, info.Repository, info.FullPath)
	}
	if row.Status != nil {
		parts = append(parts, row.Status.Repository)
	}
	if row.Fleet != nil {
		parts = append(parts,
			row.Fleet.ProjectIdentity,
			row.Fleet.ProjectName,
			strings.Join(row.Fleet.Hosts, " "),
		)
	}
	return parts
}

func rowProjectKey(row Row) string {
	if row.Entry != nil && row.Entry.RepositoryInfo != nil {
		info := row.Entry.RepositoryInfo
		if info.FullPath != "" {
			return info.FullPath
		}
		if info.Host != "" && info.Owner != "" && info.Repository != "" {
			return path.Join(info.Host, info.Owner, info.Repository)
		}
	}
	if row.Status != nil && row.Status.Repository != "" {
		return row.Status.Repository
	}
	if row.Fleet != nil {
		if row.Fleet.ProjectIdentity != "" {
			return row.Fleet.ProjectIdentity
		}
		if row.Fleet.ProjectName != "" {
			return row.Fleet.ProjectName
		}
	}
	return rowRepoName(row)
}

func formatChanges(status *models.WorktreeStatus) string {
	if status == nil || status.Status == models.WorktreeStatusUnknown {
		return "?"
	}
	gitStatus := status.GitStatus
	added := gitStatus.Added + gitStatus.Staged
	if added == 0 &&
		gitStatus.Modified == 0 &&
		gitStatus.Deleted == 0 &&
		gitStatus.Untracked == 0 &&
		gitStatus.Conflicts == 0 {
		return "clean"
	}

	parts := make([]string, 0, 5)
	if added > 0 {
		parts = append(parts, fmt.Sprintf("+%d", added))
	}
	if gitStatus.Modified > 0 {
		parts = append(parts, fmt.Sprintf("~%d", gitStatus.Modified))
	}
	if gitStatus.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("-%d", gitStatus.Deleted))
	}
	if gitStatus.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("?%d", gitStatus.Untracked))
	}
	if gitStatus.Conflicts > 0 {
		parts = append(parts, fmt.Sprintf("!%d", gitStatus.Conflicts))
	}
	return strings.Join(parts, " ")
}

func formatPushPullStatus(status *models.WorktreeStatus) (string, bool) {
	if status == nil || status.Status == models.WorktreeStatusUnknown {
		return "?", true
	}
	ahead := status.GitStatus.Ahead
	behind := status.GitStatus.Behind
	if ahead == 0 && behind == 0 {
		return "", false
	}
	parts := make([]string, 0, 2)
	if ahead > 0 {
		parts = append(parts, fmt.Sprintf("↑%d", ahead))
	}
	if behind > 0 {
		parts = append(parts, fmt.Sprintf("↓%d", behind))
	}
	return strings.Join(parts, " "), true
}

func formatRowChanges(row Row) string {
	if row.Creating {
		return "creating…"
	}
	if row.Workspace != nil {
		return "-"
	}
	if rowHasUncommittedChanges(row) {
		return formatChanges(row.Status)
	}
	if row.Fleet != nil && row.Fleet.Dirty != "" {
		if row.Status == nil || row.Fleet.Dirty != "clean" {
			return formatFleetDirty(row.Fleet.Dirty)
		}
	}
	return formatChanges(row.Status)
}

var fleetDirtyEntryPattern = regexp.MustCompile(`([^,()]+)\s+\(([^()]*)\)`)

func formatFleetDirty(dirty string) string {
	dirty = strings.TrimSpace(dirty)
	if dirty == "" || dirty == "clean" {
		return dirty
	}
	entries := parseFleetDirtyEntries(dirty)
	if len(entries) == 0 {
		return dirty
	}
	if len(entries) == 1 {
		return "remote " + compactFleetDirtySummary(entries[0].summary)
	}
	return fmt.Sprintf("%d hosts dirty", len(entries))
}

type fleetDirtyEntry struct {
	host    string
	summary string
}

func parseFleetDirtyEntries(dirty string) []fleetDirtyEntry {
	matches := fleetDirtyEntryPattern.FindAllStringSubmatch(dirty, -1)
	entries := make([]fleetDirtyEntry, 0, len(matches))
	for _, match := range matches {
		host := strings.TrimSpace(match[1])
		summary := strings.TrimSpace(match[2])
		if host == "" || summary == "" {
			continue
		}
		entries = append(entries, fleetDirtyEntry{host: host, summary: summary})
	}
	return entries
}

func compactFleetDirtySummary(summary string) string {
	parts := strings.Split(summary, ", ")
	compact := make([]string, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) < 2 {
			compact = append(compact, part)
			continue
		}
		count := fields[0]
		label := strings.TrimSuffix(strings.ToLower(fields[1]), "s")
		switch label {
		case "staged", "added":
			compact = append(compact, "+"+count)
		case "modified":
			compact = append(compact, "~"+count)
		case "deleted":
			compact = append(compact, "-"+count)
		case "untracked":
			compact = append(compact, "?"+count)
		case "conflict":
			compact = append(compact, "!"+count)
		default:
			compact = append(compact, part)
		}
	}
	return strings.Join(compact, " ")
}

func formatRowSync(row Row) string {
	if row.Creating {
		return "-"
	}
	if row.Workspace != nil {
		return "-"
	}
	if row.Fleet != nil {
		if !row.Fleet.Local {
			return "remote only"
		}
		if rowHasOtherHosts(row) {
			if row.Fleet.Sync != "" {
				return formatFleetSync(row.Fleet.Sync)
			}
			return "same"
		}
	}
	if pushPull, ok := formatPushPullStatus(row.Status); ok {
		return pushPull
	}
	return "local only"
}

func formatFleetSync(sync string) string {
	sync = strings.TrimSpace(sync)
	if sync == "same" {
		return "same"
	}
	if different, ok := strings.CutPrefix(sync, "different: "); ok {
		return "diff " + different
	}
	return sync
}

func formatMachines(row Row) string {
	if row.Fleet == nil {
		return "local"
	}
	hosts := append([]string(nil), row.Fleet.Hosts...)
	if len(hosts) == 0 {
		if row.Fleet.Local {
			return "local"
		}
		return "remote"
	}
	if !row.Fleet.Local {
		if len(hosts) == 1 {
			return hosts[0] + " only"
		}
		return strings.Join(hosts, ", ")
	}
	if len(hosts) == 1 && containsHost(hosts, "local") {
		return "local"
	}
	return strings.Join(hosts, ", ")
}

func rowHasOtherHosts(row Row) bool {
	if row.Fleet == nil {
		return false
	}
	for _, host := range row.Fleet.Hosts {
		host = strings.TrimSpace(host)
		if host != "" && host != "local" {
			return true
		}
	}
	return false
}

func formatRowActivity(row Row, now time.Time) string {
	if row.Workspace != nil {
		return "-"
	}
	if row.Status != nil {
		return formatActivityAt(row.Status.LastActivity, now)
	}
	if row.Fleet != nil && row.Fleet.Freshness != "" {
		return row.Fleet.Freshness
	}
	return "-"
}

func containsHost(hosts []string, want string) bool {
	for _, host := range hosts {
		if host == want {
			return true
		}
	}
	return false
}

func rowFleetHaystack(row Row) string {
	if row.Fleet == nil {
		return ""
	}
	return strings.Join([]string{
		row.Fleet.ProjectIdentity,
		row.Fleet.ProjectName,
		row.Fleet.Ref,
		row.Fleet.Branch,
		strings.Join(row.Fleet.Hosts, " "),
		row.Fleet.Sync,
		row.Fleet.Dirty,
		row.Fleet.RemotePath,
	}, " ")
}

func formatActivityAt(activity, now time.Time) string {
	if activity.IsZero() {
		return "-"
	}
	if activity.After(now) {
		return "now"
	}

	elapsed := now.Sub(activity)
	switch {
	case elapsed < time.Minute:
		return "now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed/time.Minute))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed/time.Hour))
	case elapsed < 21*24*time.Hour:
		return fmt.Sprintf("%dd", int(elapsed/(24*time.Hour)))
	default:
		return fmt.Sprintf("%dw", int(elapsed/(7*24*time.Hour)))
	}
}

func plural(n int, singular, many string) string {
	if n == 1 {
		return singular
	}
	return many
}

type tableColumn struct {
	key    string
	header string
	width  int
}

type tableColumnSpec struct {
	key      string
	header   string
	minWidth int
	maxWidth int
	wideOnly bool
}

const (
	dashboardColumnRepo      = "repo"
	dashboardColumnBranch    = "branch"
	dashboardColumnMachines  = "machines"
	dashboardColumnChanges   = "changes"
	dashboardColumnHeads     = "heads"
	dashboardColumnActivity  = "activity"
	dashboardColumnWorkspace = "workspace"
)

var dashboardColumnSpecs = []tableColumnSpec{
	{key: dashboardColumnRepo, header: "REPO", minWidth: 9, maxWidth: 14},
	{key: dashboardColumnBranch, header: "BRANCH", minWidth: 16, maxWidth: 32},
	{key: dashboardColumnMachines, header: "MACHINES", minWidth: 10, maxWidth: 16, wideOnly: true},
	{key: dashboardColumnChanges, header: "CHANGES", minWidth: 7, maxWidth: 12},
	{key: dashboardColumnHeads, header: "HEADS", minWidth: 8, maxWidth: 18},
	{key: dashboardColumnActivity, header: "ACTIVITY", minWidth: 8, maxWidth: 9},
	{key: dashboardColumnWorkspace, header: "WORKSPACE", minWidth: 9, maxWidth: 9},
}

func dashboardColumnsForWidth(width int) []tableColumn {
	specs := make([]tableColumnSpec, 0, len(dashboardColumnSpecs))
	showWideColumns := width <= 0 || width >= 118
	for _, spec := range dashboardColumnSpecs {
		if spec.wideOnly && !showWideColumns {
			continue
		}
		specs = append(specs, spec)
	}

	columns := make([]tableColumn, 0, len(specs))
	for _, spec := range specs {
		columnWidth := spec.maxWidth
		if width > 0 {
			columnWidth = spec.minWidth
		}
		columns = append(columns, tableColumn{key: spec.key, header: spec.header, width: columnWidth})
	}
	if width <= 0 || len(columns) == 0 {
		return columns
	}

	available := width - 2 - (len(columns) - 1)
	if available < 0 {
		available = 0
	}
	total := dashboardColumnsWidth(columns)
	if total > available {
		shrinkDashboardColumns(columns, total-available)
		return columns
	}

	extra := available - total
	growDashboardColumns(columns, specs, extra)
	return columns
}

func dashboardColumnsWidth(columns []tableColumn) int {
	total := 0
	for _, column := range columns {
		total += column.width
	}
	return total
}

func shrinkDashboardColumns(columns []tableColumn, excess int) {
	order := []string{
		dashboardColumnBranch,
		dashboardColumnHeads,
		dashboardColumnRepo,
		dashboardColumnChanges,
		dashboardColumnActivity,
		dashboardColumnWorkspace,
		dashboardColumnMachines,
	}
	for excess > 0 {
		shrank := false
		for _, key := range order {
			index := dashboardColumnIndex(columns, key)
			if index < 0 {
				continue
			}
			if columns[index].width <= 3 {
				continue
			}
			columns[index].width--
			excess--
			shrank = true
			if excess == 0 {
				break
			}
		}
		if !shrank {
			return
		}
	}
}

func growDashboardColumns(columns []tableColumn, specs []tableColumnSpec, extra int) {
	order := []string{
		dashboardColumnBranch,
		dashboardColumnRepo,
		dashboardColumnHeads,
		dashboardColumnMachines,
		dashboardColumnChanges,
		dashboardColumnActivity,
		dashboardColumnWorkspace,
	}
	for _, key := range order {
		index := dashboardColumnIndex(columns, key)
		if index < 0 {
			continue
		}
		maxWidth := dashboardColumnMaxWidth(specs, key)
		for extra > 0 && columns[index].width < maxWidth {
			columns[index].width++
			extra--
		}
		if extra == 0 {
			return
		}
	}
}

func dashboardColumnIndex(columns []tableColumn, key string) int {
	for i, column := range columns {
		if column.key == key {
			return i
		}
	}
	return -1
}

func dashboardColumnMaxWidth(specs []tableColumnSpec, key string) int {
	for _, spec := range specs {
		if spec.key == key {
			return spec.maxWidth
		}
	}
	return 0
}

type dashboardCellStyle func(string) string

func renderDashboardCells(columns []tableColumn, values map[string]string, styles map[string]dashboardCellStyle) string {
	var b strings.Builder
	for i, column := range columns {
		if i > 0 {
			b.WriteByte(' ')
		}
		cell := padRight(truncateWithEllipsis(values[column.key], column.width), column.width)
		if style := styles[column.key]; style != nil {
			cell = style(cell)
		}
		b.WriteString(cell)
	}
	return b.String()
}

func renderDashboardHeader(columns []tableColumn) string {
	values := make(map[string]string, len(columns))
	for _, column := range columns {
		values[column.key] = column.header
	}
	return "  " + renderDashboardCells(columns, values, nil)
}

func dashboardCellValues(row Row, now time.Time) map[string]string {
	return map[string]string{
		dashboardColumnRepo:      rowRepoName(row),
		dashboardColumnBranch:    rowBranch(row),
		dashboardColumnMachines:  formatMachines(row),
		dashboardColumnChanges:   formatRowChanges(row),
		dashboardColumnHeads:     formatRowSync(row),
		dashboardColumnActivity:  formatRowActivity(row, now),
		dashboardColumnWorkspace: formatWorkspace(row),
	}
}

func padRight(s string, width int) string {
	padding := width - runewidth.StringWidth(s)
	if padding <= 0 {
		return s
	}
	return s + strings.Repeat(" ", padding)
}

func formatWorkspace(row Row) string {
	if row.Creating {
		return "pending"
	}
	if row.Entry == nil && row.Fleet != nil {
		return "remote"
	}
	if row.SessionLive {
		return "live"
	}
	return "offline"
}

func anchorCursorByPath(oldRows []Row, oldCursor int, newRows []Row) int {
	if len(newRows) == 0 {
		return 0
	}
	if oldCursor < 0 || oldCursor >= len(oldRows) {
		return 0
	}
	path := rowPath(oldRows[oldCursor])
	if path == "" {
		return clampCursor(oldCursor, len(newRows))
	}
	for i, row := range newRows {
		if rowPath(row) == path {
			return i
		}
	}
	return clampCursor(oldCursor, len(newRows))
}

func indexByPath(rows []Row, path string) int {
	index, _ := indexByPathOK(rows, path)
	return index
}

// containingRowIndex returns the row whose directory contains target,
// preferring the deepest match so a nested worktree wins over the repository
// that holds it. Containment uses the same canonicalizing comparison the status
// collector uses to set IsCurrent, so the statusless fast load resolves a
// launch directory exactly as the full load would.
func containingRowIndex(rows []Row, target string) (int, bool) {
	best := -1
	bestDepth := 0
	for i, row := range rows {
		rowDir := rowPath(row)
		if rowDir == "" || !utils.IsSameOrChildPath(target, rowDir) {
			continue
		}
		// Rank on the canonical form: raw lengths would compare spellings
		// rather than depth.
		if depth := len(utils.CanonicalPath(rowDir)); depth > bestDepth {
			best = i
			bestDepth = depth
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

func indexByPathOK(rows []Row, path string) (int, bool) {
	for i, row := range rows {
		if rowPath(row) == path {
			return i, true
		}
	}
	return 0, false
}

// pathIdentity is the comparison key for rows that may be spelled differently
// while naming one directory: a worktree created under a symlinked base
// directory is reported by Git at its resolved path. A path that does not exist
// yet keeps its spelling, which is what makes a planned destination compare
// unequal to everything until Git creates it.
func pathIdentity(path string) string {
	if path == "" {
		return ""
	}
	return utils.CanonicalPath(path)
}

// identityRowIndex finds the row naming the same directory as path, whichever
// way either was spelled. Prefer indexByPathOK where both paths come from the
// same source: this one resolves symlinks per row.
func identityRowIndex(rows []Row, path string) (int, bool) {
	key := pathIdentity(path)
	if key == "" {
		return 0, false
	}
	for i, row := range rows {
		if pathIdentity(rowPath(row)) == key {
			return i, true
		}
	}
	return 0, false
}

func clampCursor(cursor, length int) int {
	if length <= 0 || cursor < 0 {
		return 0
	}
	if cursor >= length {
		return length - 1
	}
	return cursor
}

func truncateWithEllipsis(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	target := width - 3
	used := 0
	var b strings.Builder
	for _, r := range s {
		runeWidth := runewidth.RuneWidth(r)
		if used+runeWidth > target {
			break
		}
		b.WriteRune(r)
		used += runeWidth
	}
	return b.String() + "..."
}

func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}
