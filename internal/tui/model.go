package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type inputMode int

const (
	inputNone inputMode = iota
	inputFilter
	inputProjectFilter
	inputProjectSwitch
	inputNewBranch
	inputExistingBranch
)

type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmDelete
	confirmKill
	confirmUnregister
)

type confirmState struct {
	kind  confirmKind
	row   Row
	text  string
	force bool
}

type rowsMsg struct {
	rows     []Row
	warnings []string
	err      error
}

type fastRowsMsg struct {
	rows     []Row
	warnings []string
	err      error
}

type inventoryMsg struct {
	request InventoryRequest
	result  InventoryResult
	seq     int
	err     error
}

type scopeFreshness struct {
	ObservedAt time.Time
	Current    bool
	Diagnostic error
}

// fleetRowsMsg delivers the fleet overlay for the load identified by seq;
// stale overlays (an intervening refresh bumped loadSeq) are dropped.
type fleetRowsMsg struct {
	seq      int
	rows     []Row
	warnings []string
}

type branchListMsg struct {
	row      Row
	branches []models.Branch
	err      error
}

type actionDoneMsg struct {
	message             string
	err                 error
	refresh             bool
	globalStatusRefresh bool
	anchorPath          string
	pendingPath         string
}

type removalJob struct {
	row              Row
	force            bool
	key              string
	projectIdentity  string
	workingDirectory string
}

type removalDoneMsg struct {
	job     removalJob
	err     error
	removed bool
	refresh bool
}

type openInTmuxReadyMsg struct {
	process *exec.Cmd
	message string
	err     error
}

type Model struct {
	backend Backend
	baseDir string
	keys    keyMap
	theme   theme
	input   textinput.Model

	rows                    []Row
	warnings                []string
	cursor                  int
	filter                  string
	projectPerspective      string
	projectFilter           string
	projectSwitchCursor     int
	branches                []models.Branch
	branchCursor            int
	branchRow               Row
	branchesLoading         bool
	layouts                 []string
	selectedLayout          string
	inputMode               inputMode
	confirm                 confirmState
	fetching                bool
	fetchingRequest         InventoryRequest
	inventorySeq            int
	inventoryCurrent        bool
	projectFresh            map[string]scopeFreshness
	globalFresh             scopeFreshness
	backgroundGlobalStarted bool
	now                     func() time.Time
	fleetPending            bool
	fleetCancel             context.CancelFunc
	loadSeq                 int
	pendingRefresh          bool
	showHelp                bool
	message                 string
	err                     error
	stickyError             bool
	handoff                 Handoff
	anchorPath              string
	// creating holds the destinations of worktree creations whose command has
	// not returned yet. Git publishes a worktree before copy and setup commands
	// finish, so a discovered row is not proof that creation is complete.
	creating            []string
	removalQueue        []removalJob
	removalActive       *removalJob
	removalRefreshQueue []InventoryRequest
	width               int
	height              int
}

func NewModel(backend Backend, baseDir string) Model {
	input := textinput.New()
	input.SetWidth(60)
	input.Prompt = ""

	return Model{
		backend:      backend,
		baseDir:      baseDir,
		keys:         newKeyMap(),
		theme:        newTheme(),
		input:        input,
		layouts:      backend.LayoutNames(),
		fetching:     true,
		projectFresh: make(map[string]scopeFreshness),
		now:          time.Now,
		width:        100,
		height:       30,
	}
}

// WithInitialAnchor pre-selects the row whose path matches path on the first
// rows load — typically the launch directory's workspace row, which the
// activity-descending sort would otherwise bury at the bottom of the list.
// It also scopes the initial view to the launch directory's project; Escape
// widens back to every project.
func (m Model) WithInitialAnchor(path string) Model {
	m.anchorPath = path
	return m
}

// launchPerspective resolves the project key of the row the TUI was launched
// from. Empty when launched outside any known worktree, which leaves the view
// unscoped.
func launchPerspective(rows []Row, anchorPath string) string {
	if index, ok := anchorRowIndex(rows, anchorPath); ok {
		return rowProjectKey(rows[index])
	}
	return ""
}

// anchorRowIndex resolves the row the anchor path belongs to: an exact path
// match (repo root or workspace), else the row whose directory contains the
// anchor. Containment is resolved from row paths rather than the collected
// status's IsCurrent flag so the fast load, which has no statuses, resolves
// the anchor as well as the full load does.
func anchorRowIndex(rows []Row, anchorPath string) (int, bool) {
	if index, ok := identityRowIndex(rows, anchorPath); ok {
		return index, true
	}
	if index, ok := containingRowIndex(rows, anchorPath); ok {
		return index, true
	}
	return currentRowIndex(rows)
}

func (m Model) Init() tea.Cmd {
	return m.fetchInventoryCmd(InventoryRequest{Scope: InventoryCachedDashboard}, m.inventorySeq)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case fastRowsMsg:
		return m.applyFastRows(msg)
	case inventoryMsg:
		return m.applyInventory(msg)
	case rowsMsg:
		return m.applyRows(msg, true)
	case fleetRowsMsg:
		return m.applyFleetRows(msg)
	case actionDoneMsg:
		return m.applyActionDone(msg)
	case removalDoneMsg:
		return m.applyRemovalDone(msg)
	case openInTmuxReadyMsg:
		return m.applyOpenInTmuxReady(msg)
	case branchListMsg:
		return m.applyBranchList(msg)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		return m.handlePaste(msg)
	default:
		if m.shouldForwardToTextInput() {
			return m.updateTextInput(msg)
		}
		return m, nil
	}
}

func (m Model) View() tea.View {
	var b strings.Builder

	if m.showHelp {
		b.WriteString(m.renderHelp())
	} else if m.inputMode == inputProjectSwitch {
		b.WriteString(m.renderProjectSwitchView())
	} else if m.inputMode == inputExistingBranch {
		b.WriteString(m.renderExistingBranchView())
	} else {
		b.WriteString(m.renderHeader())
		b.WriteString("\n\n")
		b.WriteString(m.renderRows())
		b.WriteString("\n")
		if warnings := m.renderWarnings(); warnings != "" {
			b.WriteString(warnings)
			b.WriteString("\n")
		}
		b.WriteString(m.renderStatusLine())
		b.WriteString("\n")
		b.WriteString(m.renderFooter())
	}

	view := tea.NewView(b.String())
	view.AltScreen = true
	return view
}

func (m Model) Handoff() Handoff {
	return m.handoff
}

func (m Model) selectedRow() Row {
	rows := m.filteredRows()
	if len(rows) == 0 {
		return Row{}
	}
	return rows[clampCursor(m.cursor, len(rows))]
}

func (m Model) filteredRows() []Row {
	return filterRows(
		filterProjectRows(
			filterProjectPerspectiveRows(m.rows, m.projectPerspective),
			m.projectFilter,
		),
		m.filter,
	)
}

func (m Model) applyFastRows(msg fastRowsMsg) (Model, tea.Cmd) {
	pendingRefresh := m.pendingRefresh
	initialAnchor := m.anchorPath
	hadRows := len(m.rows) > 0
	m.pendingRefresh = false
	next, _ := m.applyRows(rowsMsg{
		rows:     carryEnrichment(msg.rows, m.rows),
		warnings: msg.warnings,
		err:      msg.err,
	}, false)
	if !hadRows && initialAnchor != "" {
		_, exact := identityRowIndex(next.rows, initialAnchor)
		_, contained := containingRowIndex(next.rows, initialAnchor)
		if !exact && !contained {
			next.anchorPath = initialAnchor
		}
	}
	next.inventoryCurrent = false
	next = next.cancelFleetMerge()
	next.fleetPending = false
	if msg.err != nil {
		// applyRows could not act on the queued refresh because it was cleared
		// above, and no full load follows a failed fast load, so run it here or
		// the completed action's result never reaches the dashboard.
		next.pendingRefresh = pendingRefresh
		return next.startPendingRefresh()
	}
	if pendingRefresh {
		next.pendingRefresh = false
		return next.startFetch()
	}
	next.fetching = true
	return next, next.fetchRowsCmd()
}

func (m Model) applyInventory(msg inventoryMsg) (Model, tea.Cmd) {
	if msg.seq != m.inventorySeq {
		return m, nil
	}
	m.fetching = false
	if msg.err != nil {
		freshness := scopeFreshness{Diagnostic: msg.err}
		switch msg.request.Scope {
		case InventoryCurrentRepository:
			project := projectFreshnessKey(msg.request.ProjectIdentity)
			freshness.ObservedAt = m.projectFresh[project].ObservedAt
			m.projectFresh[project] = freshness
			m.message = projectRefreshErrorMessage(msg.err)
		case InventoryCachedDashboard, InventoryCurrentDashboard:
			freshness.ObservedAt = m.globalFresh.ObservedAt
			m.globalFresh = freshness
		}
		if len(m.removalRefreshQueue) > 0 {
			return m.startNextRemovalRefresh()
		}
		if msg.request.Scope == InventoryCurrentRepository && !m.backgroundGlobalStarted {
			m.backgroundGlobalStarted = true
			return m.startInventory(InventoryRequest{Scope: InventoryCurrentDashboard})
		}
		return m.startPendingRefresh()
	}

	switch msg.request.Scope {
	case InventoryCachedDashboard:
		m.globalFresh = scopeFreshness{ObservedAt: msg.result.ObservedAt}
		m = m.replaceInventoryRows(msg.result.Rows, msg.result.Warnings, false)
		if m.projectPerspective == "" && m.anchorPath != "" {
			m.projectPerspective = launchPerspective(m.rows, m.anchorPath)
		}
		if m.projectPerspective != "" {
			request := m.perspectiveRequest(m.projectPerspective)
			if request.Scope == InventoryCurrentDashboard {
				m.backgroundGlobalStarted = true
			}
			return m.startInventory(request)
		}
		m.backgroundGlobalStarted = true
		return m.startInventory(InventoryRequest{
			Scope: InventoryCurrentDashboard, CollectStatuses: true,
		})
	case InventoryCurrentRepository:
		oldRows := m.filteredRows()
		oldCursor := m.cursor
		previousRows := m.rows
		m.rows = mergeRepositoryRows(m.rows, msg.result.Rows, msg.request.ProjectIdentity)
		m.rows = mergeCreatingRows(m.rows, previousRows, m.creating)
		m.rows = m.applyRemovalState(m.rows)
		m.cursor = anchorCursorByPath(oldRows, oldCursor, m.filteredRows())
		m.warnings = msg.result.Warnings
		m.projectFresh[projectFreshnessKey(msg.request.ProjectIdentity)] = scopeFreshness{
			ObservedAt: msg.result.ObservedAt, Current: msg.result.Current,
		}
		m.inventoryCurrent = msg.result.Current
		if len(m.removalRefreshQueue) > 0 {
			return m.startNextRemovalRefresh()
		}
		if m.pendingRefresh {
			return m.startPendingRefresh()
		}
		if !m.backgroundGlobalStarted {
			m.backgroundGlobalStarted = true
			return m.startInventory(InventoryRequest{Scope: InventoryCurrentDashboard})
		}
		return m.startFleetMerge()
	case InventoryCurrentDashboard:
		if msg.request.CollectStatuses {
			m = m.replaceInventoryRows(msg.result.Rows, msg.result.Warnings, msg.result.Current)
			m = m.markProjectsFresh(msg.result.Rows, msg.result.ObservedAt, msg.result.Current)
		} else {
			m = m.mergeStructuralDashboard(msg.result.Rows, msg.result.Warnings)
		}
		m.globalFresh = scopeFreshness{ObservedAt: msg.result.ObservedAt, Current: msg.result.Current}
		m.inventoryCurrent = msg.result.Current
		if m.pendingRefresh {
			return m.startPendingRefresh()
		}
		return m.startFleetMerge()
	default:
		m.err = fmt.Errorf("unknown inventory scope %d", msg.request.Scope)
		return m, nil
	}
}

func projectRefreshErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if service.IsCode(err, service.InteractionRequired) {
		return "project config requires trust; run kwt list there once"
	}
	return err.Error()
}

func (m Model) replaceInventoryRows(rows []Row, warnings []string, current bool) Model {
	oldRows := m.filteredRows()
	oldCursor := m.cursor
	hadRows := len(m.rows) > 0
	rows = mergeCreatingRows(append([]Row(nil), rows...), m.rows, m.creating)
	rows = m.applyRemovalState(rows)
	sortRows(rows)
	m.rows = rows
	m.warnings = warnings
	m.inventoryCurrent = current
	if !hadRows && m.anchorPath != "" {
		m.projectPerspective = launchPerspective(rows, m.anchorPath)
	}
	filtered := m.filteredRows()
	if m.anchorPath != "" {
		if index, ok := anchorRowIndex(filtered, m.anchorPath); ok {
			m.cursor = index
		} else {
			m.cursor = anchorCursorByPath(oldRows, oldCursor, filtered)
		}
		if hadRows || len(rows) > 0 {
			m.anchorPath = ""
		}
	} else {
		m.cursor = anchorCursorByPath(oldRows, oldCursor, filtered)
	}
	m.cursor = clampCursor(m.cursor, len(filtered))
	m = m.cancelFleetMerge()
	m.fleetPending = false
	return m
}

func (m Model) markProjectsFresh(rows []Row, observedAt time.Time, current bool) Model {
	if m.projectFresh == nil {
		m.projectFresh = make(map[string]scopeFreshness)
	}
	for _, row := range rows {
		if row.Entry == nil {
			continue
		}
		project := rowProjectKey(row)
		if project == "" {
			continue
		}
		m.projectFresh[projectFreshnessKey(project)] = scopeFreshness{
			ObservedAt: observedAt, Current: current,
		}
	}
	return m
}

func (m Model) mergeStructuralDashboard(rows []Row, warnings []string) Model {
	previousByPath := make(map[string]Row)
	for _, row := range m.rows {
		if m.projectFresh[projectFreshnessKey(rowProjectKey(row))].Current && row.Status != nil {
			previousByPath[pathIdentity(rowPath(row))] = row
		}
	}
	markStale := func(row Row) {
		if row.Entry == nil {
			return
		}
		project := projectFreshnessKey(rowProjectKey(row))
		if project == "" {
			return
		}
		freshness := m.projectFresh[project]
		freshness.Current = false
		m.projectFresh[project] = freshness
	}
	for index := range rows {
		previous, ok := previousByPath[pathIdentity(rowPath(rows[index]))]
		if !ok {
			markStale(rows[index])
			continue
		}
		if sameCheckout(previous, rows[index]) {
			rows[index].Status = previous.Status
			continue
		}
		markStale(previous)
		markStale(rows[index])
	}
	return m.replaceInventoryRows(rows, warnings, false)
}

func sameGeneration(left, right Row) bool {
	if left.Entry == nil || right.Entry == nil {
		return false
	}
	return left.Entry.Generation == right.Entry.Generation
}

func (m Model) applyRemovalState(rows []Row) []Row {
	removing := make(map[string]bool, len(m.removalQueue)+1)
	if m.removalActive != nil {
		removing[m.removalActive.key] = true
	}
	for _, job := range m.removalQueue {
		removing[job.key] = true
	}
	for index := range rows {
		rows[index].Removing = removing[removalKey(rows[index])]
	}
	return rows
}

func (m Model) repositoryRequest(project string) InventoryRequest {
	workingDirectory := ""
	for _, row := range m.rows {
		if equalProjectKey(rowProjectKey(row), project) && row.Entry != nil {
			workingDirectory = row.Entry.Path
			if row.Entry.IsMain {
				break
			}
		}
	}
	return InventoryRequest{
		Scope: InventoryCurrentRepository, WorkingDirectory: workingDirectory,
		ProjectIdentity: project, CollectStatuses: true,
	}
}

func (m Model) perspectiveRequest(project string) InventoryRequest {
	request := m.repositoryRequest(project)
	if request.WorkingDirectory == "" {
		return InventoryRequest{Scope: InventoryCurrentDashboard}
	}
	return request
}

func (m Model) startInventory(request InventoryRequest) (Model, tea.Cmd) {
	m.loadSeq++
	m.fleetPending = false
	m = m.cancelFleetMerge()
	m.inventorySeq++
	m.fetching = true
	m.fetchingRequest = request
	return m, m.fetchInventoryCmd(request, m.inventorySeq)
}

func (m Model) startFleetMerge() (Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}
	m.fleetPending = true
	ctx, cancel := context.WithCancel(context.Background())
	m.fleetCancel = cancel
	return m, m.fleetRowsCmd(ctx, m.loadSeq, m.rows)
}

// carryEnrichment keeps what a fast load cannot know. ListFast is authoritative
// about which worktrees exist locally, but it collects no statuses and no hub
// state, so rows it lists keep the status and fleet overlay the last full load
// gave them, and remote-only rows survive until a fleet merge revises them.
// Without this a refresh would strip a populated dashboard back to bare rows,
// and a full load that then failed would leave it that way.
func carryEnrichment(fast, previous []Row) []Row {
	// An empty fast load is not a shortcut: a machine with nothing checked out
	// locally still knows the remote-only rows it learned from the hub.
	if len(previous) == 0 {
		return fast
	}
	enriched := make(map[string]Row, len(previous))
	for _, row := range previous {
		if path := rowPath(row); path != "" {
			enriched[path] = row
		}
	}

	rows := make([]Row, 0, len(fast))
	discovered := make(map[string]bool, len(fast))
	onDisk := make(map[string]bool, len(fast))
	for _, row := range fast {
		if key := FleetKeyForRow(row); key != "" {
			discovered[key] = true
		}
		if path := rowPath(row); path != "" {
			onDisk[path] = true
		}
		if before, ok := enriched[rowPath(row)]; ok && sameCheckout(before, row) {
			if before.Status != nil {
				row.Status = before.Status
			}
			row.Fleet = before.Fleet
		}
		rows = append(rows, row)
	}

	for _, row := range previous {
		// A row the fast load just found needs nothing carried: its local shape
		// supersedes whatever the hub said about it.
		if discovered[FleetKeyForRow(row)] {
			continue
		}
		if isRemoteOnly(row) {
			rows = append(rows, row)
			continue
		}
		// Matching on path, not on the fleet key: the key is derived from Fleet
		// for a row that has one and from the entry otherwise, so the two shapes
		// of one worktree do not always agree. Its path does.
		if onDisk[rowPath(row)] {
			continue
		}
		if projected, ok := remoteProjection(row); ok {
			rows = append(rows, projected)
		}
	}
	return rows
}

func mergeRepositoryRows(previous, current []Row, project string) []Row {
	oldByCheckout := make(map[string]Row)
	merged := make([]Row, 0, len(previous)+len(current))
	for _, row := range previous {
		if equalProjectKey(rowProjectKey(row), project) {
			oldByCheckout[checkoutMergeKey(row)] = row
			continue
		}
		merged = append(merged, row)
	}
	for _, row := range current {
		if !equalProjectKey(rowProjectKey(row), project) {
			continue
		}
		if old, ok := oldByCheckout[checkoutMergeKey(row)]; ok {
			row = preserveDashboardFields(old, row)
		}
		merged = append(merged, row)
	}
	sortRows(merged)
	return merged
}

func equalProjectKey(left, right string) bool {
	return projectFreshnessKey(left) == projectFreshnessKey(right) && left != "" && right != ""
}

func projectFreshnessKey(project string) string {
	return url.FoldRepositoryIdentity(project)
}

func checkoutMergeKey(row Row) string {
	generation := ""
	if row.Entry != nil {
		generation = row.Entry.Generation
	}
	return pathIdentity(rowPath(row)) + "\x00" + generation
}

func preserveDashboardFields(previous, current Row) Row {
	if previous.Entry == nil || current.Entry == nil {
		return current
	}
	entry := *current.Entry
	if entry.RepositoryURL == "" {
		entry.RepositoryURL = previous.Entry.RepositoryURL
	}
	if entry.RepositoryInfo == nil && previous.Entry.RepositoryInfo != nil {
		info := *previous.Entry.RepositoryInfo
		entry.RepositoryInfo = &info
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = previous.Entry.CreatedAt
	}
	current.Entry = &entry
	return current
}

// remoteProjection reduces a row that was local to what the hub still says about
// it, for use when the fast load no longer finds it on disk. Deleting the local
// worktree of something that also lives on other hosts should not hide those
// hosts, which is what dropping the row outright would do until a fleet merge
// re-derived it — and none follows a full load that fails.
//
// The projection keeps no local state and offers no sync: the hub reading it
// came from is now a load older than the row it replaced, so it is worth
// displaying but not worth acting on until the next merge confirms it.
func remoteProjection(row Row) (Row, bool) {
	if row.Fleet == nil {
		return Row{}, false
	}
	hosts := make([]string, 0, len(row.Fleet.Hosts))
	for _, host := range row.Fleet.Hosts {
		if host != "" && host != "local" {
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		return Row{}, false
	}
	fleetInfo := *row.Fleet
	fleetInfo.Hosts = hosts
	fleetInfo.Local = false
	fleetInfo.CanMaterialize = false
	return Row{Fleet: &fleetInfo}, true
}

// sameCheckout reports whether two rows describe the same local checkout.
// Branch, generation, and HEAD can all change while the path stays fixed.
func sameCheckout(before, now Row) bool {
	if before.Entry == nil || now.Entry == nil {
		return before.Entry == nil && now.Entry == nil
	}
	return sameGeneration(before, now) &&
		rowBranch(before) == rowBranch(now) &&
		entryCommit(before) == entryCommit(now)
}

func entryCommit(row Row) string {
	if row.Entry == nil {
		return ""
	}
	return row.Entry.CommitHash
}

func (m Model) applyRows(msg rowsMsg, refreshLayouts bool) (Model, tea.Cmd) {
	m.fetching = false
	if msg.err != nil {
		m.err = msg.err
		m.stickyError = false
		return m.startPendingRefresh()
	}
	if refreshLayouts {
		m.layouts = m.backend.LayoutNames()
		if m.selectedLayout != "" && !slices.Contains(m.layouts, m.selectedLayout) {
			m.selectedLayout = ""
		}
	}
	m.warnings = msg.warnings

	oldRows := m.filteredRows()
	oldCursor := m.cursor
	hadRows := len(m.rows) > 0
	rows := append([]Row(nil), msg.rows...)
	rows = mergeCreatingRows(rows, m.rows, m.creating)
	sortRows(rows)
	m.rows = rows
	m.inventoryCurrent = true
	m = m.markProjectsFresh(rows, m.now(), true)
	if !hadRows && m.anchorPath != "" {
		m.projectPerspective = launchPerspective(rows, m.anchorPath)
	}
	newRows := m.filteredRows()

	if m.anchorPath != "" {
		// An anchor set by a completed action holds the path the backend
		// computed, which Git may report back at its resolved spelling.
		if index, ok := identityRowIndex(newRows, m.anchorPath); ok {
			m.cursor = index
			m.anchorPath = ""
		} else if m.pendingRefresh {
			m.cursor = anchorCursorByPath(oldRows, oldCursor, newRows)
		} else if !hadRows {
			// An initial anchor pointing inside a worktree selects that
			// worktree; one that matches nothing keeps the first-load behavior
			// of selecting the current worktree.
			anchor := m.anchorPath
			m.anchorPath = ""
			if index, ok := anchorRowIndex(newRows, anchor); ok {
				m.cursor = index
			} else {
				m.cursor = anchorCursorByPath(oldRows, oldCursor, newRows)
			}
		} else {
			m.cursor = 0
			m.anchorPath = ""
		}
	} else if !hadRows {
		if index, ok := currentRowIndex(newRows); ok {
			m.cursor = index
		} else {
			m.cursor = anchorCursorByPath(oldRows, oldCursor, newRows)
		}
	} else {
		m.cursor = anchorCursorByPath(oldRows, oldCursor, newRows)
	}
	m.cursor = clampCursor(m.cursor, len(newRows))
	if !m.stickyError {
		m.err = nil
	}
	if m.pendingRefresh {
		return m.startPendingRefresh()
	}
	m.fleetPending = true
	ctx, cancel := context.WithCancel(context.Background())
	m.fleetCancel = cancel
	return m, m.fleetRowsCmd(ctx, m.loadSeq, m.rows)
}

func (m Model) applyFleetRows(msg fleetRowsMsg) (Model, tea.Cmd) {
	if msg.seq != m.loadSeq {
		return m, nil
	}
	m.fleetPending = false
	m = m.cancelFleetMerge()
	m.warnings = appendUniqueStrings(m.warnings, msg.warnings...)

	oldRows := m.filteredRows()
	oldCursor := m.cursor
	rows := append([]Row(nil), msg.rows...)
	rows = mergeCreatingRows(rows, m.rows, m.creating)
	sortRows(rows)
	m.rows = m.applyRemovalState(rows)
	m.cursor = anchorCursorByPath(oldRows, oldCursor, m.filteredRows())
	return m, nil
}

func appendUniqueStrings(existing []string, additions ...string) []string {
	seen := make(map[string]bool, len(existing)+len(additions))
	result := append([]string(nil), existing...)
	for _, value := range existing {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result
}

func (m Model) applyActionDone(msg actionDoneMsg) (Model, tea.Cmd) {
	if msg.pendingPath != "" {
		m.creating = withoutPath(m.creating, msg.pendingPath)
	}
	if msg.err != nil {
		if msg.pendingPath != "" {
			m = m.dropPendingRow(msg.pendingPath)
		}
		m.err = msg.err
		m.stickyError = msg.refresh
		m.message = ""
	} else {
		if msg.pendingPath != "" && msg.anchorPath != "" && msg.pendingPath != msg.anchorPath {
			// The preview mispredicted the destination; drop the row it added so it
			// cannot outlive the creation it stood in for.
			m = m.dropPendingRow(msg.pendingPath)
		}
		m.err = nil
		m.stickyError = false
		m.message = msg.message
		if msg.anchorPath != "" {
			m.anchorPath = msg.anchorPath
		}
	}
	if msg.globalStatusRefresh {
		m.inventoryCurrent = false
		m.globalFresh.Current = false
		for project, freshness := range m.projectFresh {
			freshness.Current = false
			m.projectFresh[project] = freshness
		}
		return m.startInventory(InventoryRequest{
			Scope: InventoryCurrentDashboard, CollectStatuses: true,
		})
	}
	if msg.refresh && m.fetching {
		m.pendingRefresh = true
		return m, nil
	}
	if msg.refresh {
		return m.startFetch()
	}
	return m, nil
}

func (m Model) applyOpenInTmuxReady(msg openInTmuxReadyMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m.applyActionDone(actionDoneMsg{err: msg.err})
	}
	if msg.process == nil {
		return m.applyActionDone(actionDoneMsg{message: msg.message, refresh: true})
	}
	return m, tea.ExecProcess(msg.process, func(err error) tea.Msg {
		return actionDoneMsg{message: msg.message, err: err, refresh: err == nil}
	})
}

func (m Model) applyBranchList(msg branchListMsg) (Model, tea.Cmd) {
	if m.inputMode != inputExistingBranch ||
		pathIdentity(rowPath(msg.row)) != pathIdentity(rowPath(m.branchRow)) {
		return m, nil
	}
	m.branchesLoading = false
	if msg.err != nil {
		m.err = msg.err
		m.stickyError = false
		return m, nil
	}
	m.branches = append([]models.Branch(nil), msg.branches...)
	m.branchCursor = clampCursor(m.branchCursor, len(m.branchOptions()))
	return m, nil
}

// dropPendingRow removes an optimistic creation row and supersedes any fleet
// overlay still in flight. That overlay carries the rows it was dispatched
// with, so applying it afterwards would put the row back — and with nothing
// left in creating to retire it, its creating flag would never clear.
func (m Model) dropPendingRow(path string) Model {
	m.rows = removeRowByPath(m.rows, path)
	m.cursor = clampCursor(m.cursor, len(m.filteredRows()))
	m = m.cancelFleetMerge()
	m.fleetPending = false
	m.loadSeq++
	return m
}

func (m Model) startPendingRefresh() (Model, tea.Cmd) {
	if !m.pendingRefresh {
		return m, nil
	}
	m.pendingRefresh = false
	return m.startFetch()
}

// startFetch begins a new load generation: any fleet overlay still in flight
// for a previous generation is cancelled, and its result dropped if it
// arrives anyway.
func (m Model) startFetch() (Model, tea.Cmd) {
	m.inventoryCurrent = false
	if m.projectPerspective != "" {
		request := m.perspectiveRequest(m.projectPerspective)
		if request.Scope == InventoryCurrentRepository {
			project := projectFreshnessKey(m.projectPerspective)
			freshness := m.projectFresh[project]
			freshness.Current = false
			m.projectFresh[project] = freshness
		} else {
			m.globalFresh.Current = false
		}
		return m.startInventory(request)
	}
	m.globalFresh.Current = false
	for project, freshness := range m.projectFresh {
		freshness.Current = false
		m.projectFresh[project] = freshness
	}
	return m.startInventory(InventoryRequest{Scope: InventoryCurrentDashboard, CollectStatuses: true})
}

// mergeCreatingRows keeps pending creations visible across a listing that has
// not discovered them yet, and re-flags the ones a listing did discover while
// their creation command is still running. Git publishes a worktree before copy
// and setup commands finish, so discovery alone does not mean creation is done.
func mergeCreatingRows(rows, current []Row, creating []string) []Row {
	// Identities cost a symlink resolution per row, so skip the merge entirely
	// unless a creation is actually pending.
	if len(creating) == 0 && !hasCreatingRow(current) {
		return rows
	}
	inFlight := make(map[string]bool, len(creating))
	for _, path := range creating {
		if key := pathIdentity(path); key != "" {
			inFlight[key] = true
		}
	}
	seen := make(map[string]bool, len(rows))
	for i := range rows {
		key := pathIdentity(rowPath(rows[i]))
		if key == "" {
			continue
		}
		seen[key] = true
		if inFlight[key] {
			rows[i].Creating = true
		}
	}
	for _, row := range current {
		key := pathIdentity(rowPath(row))
		if row.Creating && key != "" && !seen[key] {
			rows = append(rows, row)
		}
	}
	return rows
}

func hasCreatingRow(rows []Row) bool {
	for _, row := range rows {
		if row.Creating {
			return true
		}
	}
	return false
}

func withoutPath(paths []string, path string) []string {
	kept := make([]string, 0, len(paths))
	for _, candidate := range paths {
		if candidate != path {
			kept = append(kept, candidate)
		}
	}
	return kept
}

// removeRowByPath drops every row naming the given directory. It compares by
// identity because a discovery pass can replace a placeholder's path with Git's
// resolved spelling of it, and a row left behind here keeps its creating flag
// forever.
func removeRowByPath(rows []Row, path string) []Row {
	key := pathIdentity(path)
	if key == "" {
		return rows
	}
	filtered := rows[:0]
	for _, row := range rows {
		if pathIdentity(rowPath(row)) != key {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (m Model) cancelFleetMerge() Model {
	if m.fleetCancel != nil {
		m.fleetCancel()
		m.fleetCancel = nil
	}
	return m
}

func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.err != nil {
		m.err = nil
		m.stickyError = false
	}

	if m.showHelp {
		m.showHelp = false
		return m, nil
	}

	if m.confirm.kind != confirmNone {
		return m.handleConfirmKey(msg)
	}

	if m.inputMode != inputNone {
		return m.handleInputKey(msg)
	}
	if m.selectedRow().Removing && (key.Matches(msg, m.keys.Open) ||
		key.Matches(msg, m.keys.New) ||
		key.Matches(msg, m.keys.Existing) ||
		key.Matches(msg, m.keys.Delete) ||
		key.Matches(msg, m.keys.Sync) ||
		key.Matches(msg, m.keys.Shell) ||
		key.Matches(msg, m.keys.Kill)) {
		m.message = "worktree removal is in progress"
		return m, nil
	}
	if (key.Matches(msg, m.keys.New) ||
		key.Matches(msg, m.keys.Existing) ||
		key.Matches(msg, m.keys.Delete) ||
		key.Matches(msg, m.keys.Sync) ||
		key.Matches(msg, m.keys.Kill)) && !m.selectedScopeCurrent() {
		if m.selectedRow().Workspace != nil {
			m.message = "global inventory is refreshing; wait for current results"
		} else {
			m.message = "project inventory is refreshing; wait for current results"
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, quitCmd()
	case key.Matches(msg, m.keys.Down):
		m.cursor = clampCursor(m.cursor+1, len(m.filteredRows()))
	case key.Matches(msg, m.keys.Up):
		m.cursor = clampCursor(m.cursor-1, len(m.filteredRows()))
	case key.Matches(msg, m.keys.Top):
		m.cursor = 0
	case key.Matches(msg, m.keys.Bottom):
		if rows := m.filteredRows(); len(rows) > 0 {
			m.cursor = len(rows) - 1
		}
	case key.Matches(msg, m.keys.Open):
		return m.openSelected()
	case key.Matches(msg, m.keys.Layout):
		return m.cycleLayout()
	case key.Matches(msg, m.keys.New):
		return m.startNewBranch()
	case key.Matches(msg, m.keys.Existing):
		return m.startExistingBranch()
	case key.Matches(msg, m.keys.Delete):
		return m.startDelete()
	case key.Matches(msg, m.keys.Sync):
		return m.syncSelected(m.selectedRow())
	case key.Matches(msg, m.keys.Shell):
		return m.shellSelected()
	case key.Matches(msg, m.keys.Kill):
		return m.startKill()
	case key.Matches(msg, m.keys.Switch):
		return m.startProjectSwitch()
	case key.Matches(msg, m.keys.Project):
		return m.startProjectFilter()
	case key.Matches(msg, m.keys.Filter):
		return m.startFilter()
	case key.Matches(msg, m.keys.Refresh):
		if !m.fetching {
			return m.startFetch()
		}
	case key.Matches(msg, m.keys.Help):
		m.showHelp = true
	case key.Matches(msg, m.keys.Cancel):
		if m.filter != "" {
			selectedPath := rowPath(m.selectedRow())
			m.filter = ""
			m.input.SetValue("")
			if selectedPath != "" {
				m.cursor = indexByPath(m.filteredRows(), selectedPath)
			} else {
				m.cursor = clampCursor(m.cursor, len(m.filteredRows()))
			}
		} else if m.projectFilter != "" {
			selectedPath := rowPath(m.selectedRow())
			m.projectFilter = ""
			m.input.SetValue("")
			if selectedPath != "" {
				m.cursor = indexByPath(m.filteredRows(), selectedPath)
			} else {
				m.cursor = clampCursor(m.cursor, len(m.filteredRows()))
			}
		} else if m.projectPerspective != "" {
			selectedPath := rowPath(m.selectedRow())
			m.projectPerspective = ""
			if selectedPath != "" {
				m.cursor = indexByPath(m.filteredRows(), selectedPath)
			} else {
				m.cursor = clampCursor(m.cursor, len(m.filteredRows()))
			}
			return m.startFetch()
		}
	}

	return m, nil
}

func (m Model) selectedScopeCurrent() bool {
	return m.rowScopeCurrent(m.selectedRow())
}

func (m Model) rowScopeCurrent(row Row) bool {
	if row.Workspace != nil || row.Entry == nil {
		if !m.globalFresh.ObservedAt.IsZero() || m.globalFresh.Diagnostic != nil {
			return m.globalFresh.Current
		}
		return m.inventoryCurrent
	}
	project := rowProjectKey(row)
	if freshness, ok := m.projectFresh[projectFreshnessKey(project)]; ok {
		return freshness.Current
	}
	return false
}

func (m Model) handleInputKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.inputMode == inputProjectSwitch {
		return m.handleProjectSwitchKey(msg)
	}
	if m.inputMode == inputExistingBranch {
		return m.handleExistingBranchKey(msg)
	}

	if key.Matches(msg, m.keys.Cancel) {
		selectedPath := rowPath(m.selectedRow())
		wasFilter := m.inputMode == inputFilter
		wasProjectFilter := m.inputMode == inputProjectFilter
		wasNewBranch := m.inputMode == inputNewBranch
		m.inputMode = inputNone
		m.input.Blur()
		if wasNewBranch {
			m.branchRow = Row{}
		}
		if wasFilter {
			m.filter = ""
			m.input.SetValue("")
			if selectedPath != "" {
				m.cursor = indexByPath(m.filteredRows(), selectedPath)
			} else {
				m.cursor = clampCursor(m.cursor, len(m.filteredRows()))
			}
		} else if wasProjectFilter {
			m.projectFilter = ""
			m.input.SetValue("")
			if selectedPath != "" {
				m.cursor = indexByPath(m.filteredRows(), selectedPath)
			} else {
				m.cursor = clampCursor(m.cursor, len(m.filteredRows()))
			}
		}
		return m, nil
	}

	if key.Matches(msg, m.keys.Open) {
		switch m.inputMode {
		case inputNewBranch:
			branch := strings.TrimSpace(m.input.Value())
			target := m.branchRow
			m.inputMode = inputNone
			m.input.Blur()
			m.input.SetValue("")
			m.branchRow = Row{}
			if branch == "" {
				m.message = "branch name required"
				return m, nil
			}
			row, ok := m.currentBranchTarget(target)
			if !ok {
				m.message = "project inventory changed; review the current row and try again"
				return m, nil
			}
			return m.startCreateWorktree(row, branch, "", branch)
		case inputFilter:
			m.inputMode = inputNone
			m.input.Blur()
			return m, nil
		case inputProjectFilter:
			m.inputMode = inputNone
			m.input.Blur()
			return m, nil
		case inputProjectSwitch:
			return m.chooseProjectPerspective()
		}
	}

	return m.updateTextInput(msg)
}

func (m Model) startCreateWorktree(
	row Row,
	branch string,
	source string,
	display string,
) (Model, tea.Cmd) {
	if strings.TrimSpace(display) == "" {
		display = branch
	}
	planned, err := m.backend.PreviewWorktree(row, branch)
	if err != nil {
		m.err = err
		m.stickyError = false
		m.message = ""
		return m, nil
	}
	pendingPath := rowPath(planned)
	// A second row at the same path would make the placeholder and the row it
	// collides with indistinguishable, so a failed creation would roll back
	// both. Git would reject the creation anyway; say so before starting it.
	if index, ok := identityRowIndex(m.rows, pendingPath); ok {
		if m.rows[index].Creating {
			m.message = fmt.Sprintf("%s is already being created", rowLabel(m.rows[index]))
		} else {
			m.message = fmt.Sprintf("worktree already exists at %s", abbreviateHome(pendingPath))
		}
		return m, nil
	}
	if source != "" && planned.Entry != nil {
		entry := *planned.Entry
		entry.Branch = display
		planned.Entry = &entry
	}
	planned.Creating = true
	m.creating = append(m.creating, pendingPath)
	m.rows = append(m.rows, planned)
	sortRows(m.rows)
	m.cursor = indexByPath(m.filteredRows(), pendingPath)
	m.message = fmt.Sprintf("creating %s", display)
	return m, m.createWorktreeCmd(
		row,
		branch,
		source,
		display,
		pendingPath,
	)
}

func (m Model) handlePaste(msg tea.PasteMsg) (Model, tea.Cmd) {
	if m.err != nil {
		m.err = nil
		m.stickyError = false
	}
	if m.showHelp || m.confirm.kind != confirmNone || m.inputMode == inputNone {
		return m, nil
	}
	if m.inputMode == inputProjectSwitch {
		return m.appendProjectSwitchText(msg.Content), nil
	}
	if m.inputMode == inputExistingBranch {
		return m.appendExistingBranchText(msg.Content), nil
	}
	return m.updateTextInput(msg)
}

func (m Model) shouldForwardToTextInput() bool {
	return !m.showHelp &&
		m.confirm.kind == confirmNone &&
		m.inputMode != inputNone &&
		m.inputMode != inputProjectSwitch &&
		m.inputMode != inputExistingBranch
}

func (m Model) updateTextInput(msg tea.Msg) (Model, tea.Cmd) {
	oldRows := m.filteredRows()
	oldCursor := m.cursor
	next, cmd := m.input.Update(msg)
	m.input = next
	switch m.inputMode {
	case inputFilter:
		m.filter = m.input.Value()
		m.cursor = anchorCursorByPath(oldRows, oldCursor, m.filteredRows())
	case inputProjectFilter:
		m.projectFilter = m.input.Value()
		m.cursor = anchorCursorByPath(oldRows, oldCursor, m.filteredRows())
	}
	return m, cmd
}

func (m Model) handleProjectSwitchKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.Key().Code {
	case tea.KeyEsc:
		m.inputMode = inputNone
		m.input.Blur()
		m.input.SetValue("")
		return m, nil
	case tea.KeyEnter:
		return m.chooseProjectPerspective()
	case tea.KeyUp:
		m.projectSwitchCursor = clampCursor(m.projectSwitchCursor-1, len(m.projectSwitchOptions()))
		return m, nil
	case tea.KeyDown:
		m.projectSwitchCursor = clampCursor(m.projectSwitchCursor+1, len(m.projectSwitchOptions()))
		return m, nil
	case tea.KeyBackspace:
		value := []rune(m.input.Value())
		if len(value) > 0 {
			m.input.SetValue(string(value[:len(value)-1]))
			m.projectSwitchCursor = clampCursor(0, len(m.projectSwitchOptions()))
		}
		return m, nil
	}

	if msg.String() == "ctrl+c" {
		return m, quitCmd()
	}

	return m.appendProjectSwitchText(msg.Key().Text), nil
}

func (m Model) handleExistingBranchKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.Key().Code {
	case tea.KeyEsc:
		m.inputMode = inputNone
		m.input.Blur()
		m.input.SetValue("")
		m.branches = nil
		m.branchRow = Row{}
		m.branchesLoading = false
		return m, nil
	case tea.KeyEnter:
		options := m.branchOptions()
		if len(options) == 0 {
			return m, nil
		}
		branch := options[clampCursor(m.branchCursor, len(options))]
		target := m.branchRow
		m.inputMode = inputNone
		m.input.Blur()
		m.input.SetValue("")
		m.branches = nil
		m.branchRow = Row{}
		row, ok := m.currentBranchTarget(target)
		if !ok {
			m.message = "project inventory changed; review the current row and try again"
			return m, nil
		}
		return m.startCreateWorktree(
			row,
			branch.Name,
			branch.Source,
			branchDisplayLabel(branch),
		)
	case tea.KeyUp:
		m.branchCursor = clampCursor(m.branchCursor-1, len(m.branchOptions()))
		return m, nil
	case tea.KeyDown:
		m.branchCursor = clampCursor(m.branchCursor+1, len(m.branchOptions()))
		return m, nil
	case tea.KeyBackspace:
		value := []rune(m.input.Value())
		if len(value) > 0 {
			m.input.SetValue(string(value[:len(value)-1]))
			m.branchCursor = 0
		}
		return m, nil
	}
	if msg.String() == "ctrl+c" {
		return m, quitCmd()
	}
	return m.appendExistingBranchText(msg.Key().Text), nil
}

func (m Model) appendExistingBranchText(text string) Model {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsPrint(r) && !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return m
	}
	m.input.SetValue(m.input.Value() + b.String())
	m.input.CursorEnd()
	m.branchCursor = 0
	return m
}

func (m Model) appendProjectSwitchText(text string) Model {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsPrint(r) && !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return m
	}
	m.input.SetValue(m.input.Value() + b.String())
	m.input.CursorEnd()
	m.projectSwitchCursor = clampCursor(0, len(m.projectSwitchOptions()))
	return m
}

func (m Model) handleConfirmKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Cancel, m.keys.No) {
		m.confirm = confirmState{}
		m.message = "cancelled"
		return m, nil
	}
	if !key.Matches(msg, m.keys.Yes) {
		return m, nil
	}

	row := m.confirm.row
	kind := m.confirm.kind
	force := m.confirm.force
	m.confirm = confirmState{}
	current, ok := m.currentConfirmationRow(row)
	if !ok {
		m.message = "selection changed; review the current row and try again"
		return m, nil
	}
	row = current
	switch kind {
	case confirmDelete:
		return m.enqueueRemoval(row, force)
	case confirmKill:
		return m, m.killSessionCmd(row)
	case confirmUnregister:
		return m, m.unregisterWorkspaceCmd(row)
	default:
		return m, nil
	}
}

func (m Model) currentConfirmationRow(confirmed Row) (Row, bool) {
	for _, row := range m.rows {
		if !sameConfirmationTarget(confirmed, row) {
			continue
		}
		if !m.rowScopeCurrent(row) ||
			!sameCheckout(confirmed, row) ||
			row.SessionName != confirmed.SessionName ||
			row.TmuxEndpoint != confirmed.TmuxEndpoint ||
			row.SessionLive != confirmed.SessionLive {
			return Row{}, false
		}
		return row, true
	}
	return Row{}, false
}

func (m Model) currentBranchTarget(target Row) (Row, bool) {
	if target.Entry == nil {
		return Row{}, false
	}
	for _, row := range m.rows {
		if row.Entry == nil ||
			pathIdentity(rowPath(row)) != pathIdentity(rowPath(target)) ||
			!sameGeneration(target, row) ||
			!equalProjectKey(rowProjectKey(target), rowProjectKey(row)) ||
			!m.rowScopeCurrent(row) {
			continue
		}
		return row, true
	}
	return Row{}, false
}

func sameConfirmationTarget(before, now Row) bool {
	if (before.Entry == nil) != (now.Entry == nil) ||
		(before.Workspace == nil) != (now.Workspace == nil) {
		return false
	}
	return removalKey(before) == removalKey(now)
}

func (m Model) openSelected() (Model, tea.Cmd) {
	row := m.selectedRow()
	if row.Creating {
		m.message = "worktree is still being created"
		return m, nil
	}
	if row.Entry == nil && row.Workspace == nil {
		if row.Fleet != nil && row.Fleet.CanMaterialize {
			m.message = "press s to sync this worktree"
			return m, nil
		}
		m.message = "no worktree selected"
		return m, nil
	}
	if !m.selectedScopeCurrent() {
		if !row.SessionLive {
			m.message = "project inventory is refreshing; starting a workspace waits for current results"
			return m, nil
		}
		if m.backend.InsideTmux() {
			return m, m.openExistingInTmuxCmd(row)
		}
		m.handoff = Handoff{Kind: HandoffAttach, Row: row, ExistingOnly: true}
		return m, quitCmd()
	}
	if m.backend.InsideTmux() {
		return m, m.openInTmuxCmd(row)
	}
	m.handoff = Handoff{Kind: HandoffAttach, Row: row, LayoutName: m.selectedLayout}
	return m, quitCmd()
}

func (m Model) shellSelected() (Model, tea.Cmd) {
	row := m.selectedRow()
	if row.Creating {
		m.message = "worktree is still being created"
		return m, nil
	}
	if row.Entry == nil && row.Workspace == nil {
		m.message = "sync this worktree before opening a shell"
		return m, nil
	}
	if err := validateShellDirectory(rowPath(row)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.message = "selected worktree no longer exists"
		} else {
			m.message = fmt.Sprintf("cannot open shell: %v", err)
		}
		return m, nil
	}
	m.handoff = Handoff{Kind: HandoffShell, Row: row}
	return m, quitCmd()
}

var validateShellDirectory = defaultValidateShellDirectory

func defaultValidateShellDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("selected path is not a directory")
	}
	return nil
}

func (m Model) cycleLayout() (Model, tea.Cmd) {
	if len(m.layouts) == 0 {
		return m, nil
	}
	if m.selectedLayout == "" {
		m.selectedLayout = m.layouts[0]
	} else {
		next := 0
		for i, name := range m.layouts {
			if name == m.selectedLayout {
				next = i + 1
				break
			}
		}
		if next >= len(m.layouts) {
			m.selectedLayout = ""
		} else {
			m.selectedLayout = m.layouts[next]
		}
	}
	m.message = ""
	return m, nil
}

func (m Model) startNewBranch() (Model, tea.Cmd) {
	row := m.selectedRow()
	if row.Creating {
		m.message = "worktree is still being created"
		return m, nil
	}
	if row.Workspace != nil {
		m.message = "not a git worktree"
		return m, nil
	}
	if row.Entry == nil {
		m.message = "no worktree selected"
		return m, nil
	}
	m.inputMode = inputNewBranch
	m.branchRow = row
	m.input.Prompt = fmt.Sprintf("new branch in %s: ", rowRepoName(row))
	m.input.SetValue("")
	return m, m.input.Focus()
}

func (m Model) startExistingBranch() (Model, tea.Cmd) {
	row := m.selectedRow()
	if row.Creating {
		m.message = "worktree is still being created"
		return m, nil
	}
	if row.Workspace != nil {
		m.message = "not a git worktree"
		return m, nil
	}
	if row.Entry == nil {
		m.message = "no worktree selected"
		return m, nil
	}
	m.inputMode = inputExistingBranch
	m.input.Prompt = "Search: "
	m.input.SetValue("")
	m.branchRow = row
	m.branches = nil
	m.branchCursor = 0
	m.branchesLoading = true
	_ = m.input.Focus()
	return m, m.listBranchesCmd(row)
}

func (m Model) syncSelected(row Row) (Model, tea.Cmd) {
	if row.Fleet == nil {
		m.message = "nothing to sync for this row"
		return m, nil
	}
	if row.Fleet.Local {
		m.message = "nothing to sync for this row"
		return m, nil
	}
	if !row.Fleet.CanMaterialize {
		m.message = "cannot sync this worktree"
		return m, nil
	}
	m.message = fmt.Sprintf("syncing %s", rowLabel(row))
	return m, m.materializeWorktreeCmd(row)
}

func (m Model) startFilter() (Model, tea.Cmd) {
	m.inputMode = inputFilter
	m.input.Prompt = "/ filter: "
	m.input.SetValue(m.filter)
	return m, m.input.Focus()
}

func (m Model) startProjectFilter() (Model, tea.Cmd) {
	m.inputMode = inputProjectFilter
	m.input.Prompt = "p filter: "
	m.input.SetValue(m.projectFilter)
	return m, m.input.Focus()
}

func (m Model) startProjectSwitch() (Model, tea.Cmd) {
	m.inputMode = inputProjectSwitch
	m.input.SetValue("")
	m.projectSwitchCursor = indexProjectSwitchOption(m.projectSwitchOptions(), m.projectPerspective)
	m.input.Blur()
	return m, nil
}

func (m Model) chooseProjectPerspective() (Model, tea.Cmd) {
	options := m.projectSwitchOptions()
	if len(options) == 0 {
		m.message = "no matching project"
		return m, nil
	}
	selectedPath := rowPath(m.selectedRow())
	option := options[clampCursor(m.projectSwitchCursor, len(options))]
	m.projectPerspective = option.key
	m.projectFilter = ""
	m.inputMode = inputNone
	m.input.Blur()
	m.input.SetValue("")
	m.cursor = m.cursorAfterProjectChange(selectedPath)
	return m.startFetch()
}

func (m Model) cursorAfterProjectChange(selectedPath string) int {
	rows := m.filteredRows()
	if selectedPath != "" {
		if index, ok := indexByPathOK(rows, selectedPath); ok {
			return index
		}
	}
	if index, ok := currentRowIndex(rows); ok {
		return index
	}
	return clampCursor(0, len(rows))
}

func (m Model) startDelete() (Model, tea.Cmd) {
	row := m.selectedRow()
	if row.Creating {
		m.message = "worktree is still being created"
		return m, nil
	}
	if row.Workspace != nil {
		m.confirm = confirmState{
			kind: confirmUnregister,
			row:  row,
			text: fmt.Sprintf("unregister workspace %s? (directory is kept) [y/N]", row.Workspace.Name),
		}
		return m, nil
	}
	if row.Entry == nil {
		m.message = "no worktree selected"
		return m, nil
	}
	if row.Entry.IsMain {
		m.message = fmt.Sprintf(
			"cannot delete the primary checkout: %s",
			abbreviateHome(row.Entry.Path),
		)
		return m, nil
	}
	dirty := rowHasUncommittedChanges(row)
	text := fmt.Sprintf("delete %s and stop any matching workspace sessions? [y/N]", rowLabel(row))
	if dirty {
		text = fmt.Sprintf("discard changes, delete %s, and stop any matching workspace sessions? [y/N]", rowLabel(row))
	}
	m.confirm = confirmState{kind: confirmDelete, row: row, text: text, force: dirty}
	return m, nil
}

func rowHasUncommittedChanges(row Row) bool {
	if row.Status == nil {
		return false
	}
	status := row.Status.GitStatus
	if status.Added > 0 ||
		status.Modified > 0 ||
		status.Deleted > 0 ||
		status.Untracked > 0 ||
		status.Staged > 0 ||
		status.Conflicts > 0 {
		return true
	}
	switch row.Status.Status {
	case models.WorktreeStatusModified, models.WorktreeStatusStaged, models.WorktreeStatusConflict:
		return true
	default:
		return false
	}
}

func (m Model) startKill() (Model, tea.Cmd) {
	row := m.selectedRow()
	if row.Entry == nil && row.Workspace == nil {
		m.message = "no worktree selected"
		return m, nil
	}
	if !row.SessionLive {
		m.message = "no live workspace"
		return m, nil
	}
	m.confirm = confirmState{
		kind: confirmKill,
		row:  row,
		text: fmt.Sprintf("kill workspace for %s? [y/N]", rowLabel(row)),
	}
	return m, nil
}

func (m Model) fetchRowsCmd() tea.Cmd {
	backend := m.backend
	return func() tea.Msg {
		rows, warnings, err := backend.List(context.Background())
		return rowsMsg{rows: rows, warnings: warnings, err: err}
	}
}

func (m Model) fetchInventoryCmd(request InventoryRequest, seq int) tea.Cmd {
	backend := m.backend
	return func() tea.Msg {
		result, err := backend.LoadInventory(context.Background(), request)
		return inventoryMsg{request: request, result: result, seq: seq, err: err}
	}
}

func (m Model) fleetRowsCmd(ctx context.Context, seq int, rows []Row) tea.Cmd {
	backend := m.backend
	// Copy: the merge mutates row elements while the UI keeps rendering the
	// current slice.
	rows = append([]Row(nil), rows...)
	return func() tea.Msg {
		merged, warnings := backend.MergeFleet(ctx, rows)
		return fleetRowsMsg{seq: seq, rows: merged, warnings: warnings}
	}
}

func (m Model) listBranchesCmd(row Row) tea.Cmd {
	return func() tea.Msg {
		branches, err := m.backend.ListBranches(context.Background(), row)
		return branchListMsg{row: row, branches: branches, err: err}
	}
}

func (m Model) createWorktreeCmd(
	row Row,
	branch string,
	source string,
	display string,
	pendingPath string,
) tea.Cmd {
	return func() tea.Msg {
		path, err := m.backend.CreateWorktree(
			context.Background(),
			row,
			branch,
			source,
		)
		if err != nil {
			return actionDoneMsg{err: err, pendingPath: pendingPath}
		}
		message := fmt.Sprintf("created %s", display)
		if source != "" {
			message = fmt.Sprintf(
				"created %s; review it before attaching",
				display,
			)
		}
		return actionDoneMsg{
			message:     message,
			refresh:     true,
			anchorPath:  path,
			pendingPath: pendingPath,
		}
	}
}

func (m Model) materializeWorktreeCmd(row Row) tea.Cmd {
	return func() tea.Msg {
		path, err := m.backend.MaterializeWorktree(context.Background(), row)
		if err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{
			message:             fmt.Sprintf("synced %s", rowLabel(row)),
			refresh:             true,
			globalStatusRefresh: true,
			anchorPath:          path,
		}
	}
}

func (m Model) enqueueRemoval(row Row, force bool) (Model, tea.Cmd) {
	key := removalKey(row)
	if key == "" || m.removalQueued(key) {
		m.message = "worktree removal is already queued"
		return m, nil
	}
	project := rowProjectKey(row)
	job := removalJob{
		row: row, force: force, key: key, projectIdentity: project,
		workingDirectory: m.removalRefreshAnchor(project, rowPath(row)),
	}
	for index := range m.rows {
		if removalKey(m.rows[index]) == key {
			m.rows[index].Removing = true
		}
	}
	m.removalQueue = append(m.removalQueue, job)
	return m.startNextRemoval()
}

func removalKey(row Row) string {
	generation := ""
	if row.Entry != nil {
		generation = row.Entry.Generation
	}
	return pathIdentity(rowPath(row)) + "\x00" + generation
}

func (m Model) removalQueued(key string) bool {
	if m.removalActive != nil && m.removalActive.key == key {
		return true
	}
	for _, job := range m.removalQueue {
		if job.key == key {
			return true
		}
	}
	return false
}

func (m Model) removalRefreshAnchor(project, fallback string) string {
	for _, row := range m.rows {
		if row.Entry != nil && row.Entry.IsMain && equalProjectKey(rowProjectKey(row), project) {
			return row.Entry.Path
		}
	}
	return fallback
}

func (m Model) startNextRemoval() (Model, tea.Cmd) {
	if m.removalActive != nil || len(m.removalQueue) == 0 {
		return m, nil
	}
	job := m.removalQueue[0]
	m.removalQueue = m.removalQueue[1:]
	m.removalActive = &job
	return m, m.removeWorktreeCmd(job)
}

func (m Model) removeWorktreeCmd(job removalJob) tea.Cmd {
	return func() tea.Msg {
		err := m.backend.RemoveWorktree(context.Background(), job.row, job.force)
		return removalDoneMsg{
			job: job, err: err,
			removed: err == nil || git.WorktreeWasRemoved(err),
			refresh: err == nil || git.WorktreeWasRemoved(err) || actionRefreshRequired(err),
		}
	}
}

func (m Model) applyRemovalDone(msg removalDoneMsg) (Model, tea.Cmd) {
	m.removalActive = nil
	if msg.removed {
		m.inventorySeq++
		m.fetching = false
		m.loadSeq++
		m.fleetPending = false
		m = m.cancelFleetMerge()
		oldRows := m.filteredRows()
		oldCursor := m.cursor
		m.rows = removeRowByPath(m.rows, rowPath(msg.job.row))
		m.cursor = anchorCursorByPath(oldRows, oldCursor, m.filteredRows())
	} else {
		for index := range m.rows {
			if removalKey(m.rows[index]) == msg.job.key {
				m.rows[index].Removing = false
			}
		}
	}
	if msg.err != nil {
		m.err = msg.err
		m.stickyError = msg.refresh
		m.message = ""
	} else {
		m.message = fmt.Sprintf("removed %s", rowLabel(msg.job.row))
	}
	projectIdentity := msg.job.projectIdentity
	if projectIdentity == "" {
		projectIdentity = rowProjectKey(msg.job.row)
	}
	workingDirectory := msg.job.workingDirectory
	if workingDirectory == "" {
		workingDirectory = rowPath(msg.job.row)
	}
	if msg.refresh && projectIdentity != "" {
		m.removalRefreshQueue = append(m.removalRefreshQueue, InventoryRequest{
			Scope: InventoryCurrentRepository, ProjectIdentity: projectIdentity,
			WorkingDirectory: workingDirectory, CollectStatuses: true,
		})
	}
	if next, cmd := m.startNextRemoval(); cmd != nil {
		return next, cmd
	}
	return m.startNextRemovalRefresh()
}

func (m Model) startNextRemovalRefresh() (Model, tea.Cmd) {
	if len(m.removalRefreshQueue) == 0 {
		return m, nil
	}
	request := m.removalRefreshQueue[0]
	m.removalRefreshQueue = m.removalRefreshQueue[1:]
	project := projectFreshnessKey(request.ProjectIdentity)
	freshness := m.projectFresh[project]
	freshness.Current = false
	m.projectFresh[project] = freshness
	return m.startInventory(request)
}

func actionRefreshRequired(err error) bool {
	var required interface{ RefreshRequired() bool }
	return errors.As(err, &required) && required.RefreshRequired()
}

func (m Model) unregisterWorkspaceCmd(row Row) tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.UnregisterWorkspace(row); err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{message: "workspace unregistered", refresh: true}
	}
}

func (m Model) killSessionCmd(row Row) tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.KillSession(row); err != nil {
			return actionDoneMsg{err: err}
		}
		return actionDoneMsg{message: fmt.Sprintf("killed workspace for %s", rowLabel(row)), refresh: true}
	}
}

func (m Model) openInTmuxCmd(row Row) tea.Cmd {
	layoutName := m.selectedLayout
	return func() tea.Msg {
		process, err := m.backend.OpenInTmux(context.Background(), row, layoutName)
		return openInTmuxReadyMsg{
			process: process,
			message: fmt.Sprintf("attached %s", rowLabel(row)),
			err:     err,
		}
	}
}

func (m Model) openExistingInTmuxCmd(row Row) tea.Cmd {
	return func() tea.Msg {
		process, err := m.backend.OpenExistingInTmux(context.Background(), row)
		return openInTmuxReadyMsg{
			process: process,
			message: fmt.Sprintf("attached %s", rowLabel(row)),
			err:     err,
		}
	}
}

func quitCmd() tea.Cmd {
	return func() tea.Msg { return tea.Quit() }
}

func (m Model) renderHeader() string {
	repoCount := make(map[string]bool)
	for _, row := range m.rows {
		repoCount[rowRepoName(row)] = true
	}
	spinner := ""
	if m.fetching && len(m.rows) == 0 {
		spinner = " · fetching"
	} else if m.fetching {
		spinner = " · checking"
	} else if m.fleetPending {
		spinner = " · syncing"
	}
	left := fmt.Sprintf("kwt · %d %s · %d %s%s",
		len(m.rows), plural(len(m.rows), "worktree", "worktrees"),
		len(repoCount), plural(len(repoCount), "repo", "repos"),
		spinner,
	)
	right := fmt.Sprintf("P project:%s  p filter:%s  / search  L layout:%s  ? help",
		m.projectPerspectiveLabel(), m.projectFilterLabel(), m.layoutLabel())
	spaces := "  "
	if width := m.width - len(left) - len(right); width > 2 {
		spaces = strings.Repeat(" ", width)
	}
	return m.theme.header.Render(left) + spaces + m.theme.dim.Render(right)
}

func (m Model) renderRows() string {
	rows := m.filteredRows()
	if len(m.rows) == 0 && !m.fetching {
		return fmt.Sprintf("no worktrees under %s — create one with kwt add", m.baseDir)
	}
	if len(rows) == 0 {
		return "no matching worktrees"
	}

	var b strings.Builder
	columns := dashboardColumnsForWidth(m.width)
	b.WriteString(renderDashboardHeader(columns))
	b.WriteString("\n")
	now := timeNow()
	selected := clampCursor(m.cursor, len(rows))
	start, visibleRows := rowWindow(rows, selected, m.availableRowCount())
	for offset, row := range visibleRows {
		i := start + offset
		cursor := " "
		if i == selected {
			cursor = "▸"
		}
		line := cursor + " " + renderDashboardCells(columns, dashboardCellValues(row, now), m.dashboardCellStyles(row))
		if i == selected {
			line = m.theme.cursor.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) dashboardCellStyles(row Row) map[string]dashboardCellStyle {
	styles := make(map[string]dashboardCellStyle, 1)
	switch formatWorkspace(row) {
	case "live":
		styles[dashboardColumnWorkspace] = func(s string) string { return m.theme.live.Render(s) }
	case "remote", "offline":
		styles[dashboardColumnWorkspace] = func(s string) string { return m.theme.dim.Render(s) }
	}
	return styles
}

func (m Model) availableRowCount() int {
	if m.height <= 0 {
		return len(m.filteredRows())
	}
	fixedLines := 5 + m.footerHelpLines()
	count := m.height - fixedLines
	if count < 1 {
		return 1
	}
	return count
}

func rowWindow(rows []Row, cursor, maxRows int) (int, []Row) {
	if len(rows) == 0 || maxRows <= 0 || maxRows >= len(rows) {
		return 0, rows
	}
	cursor = clampCursor(cursor, len(rows))
	start := cursor - maxRows/2
	if start < 0 {
		start = 0
	}
	if start+maxRows > len(rows) {
		start = len(rows) - maxRows
	}
	return start, rows[start : start+maxRows]
}

func currentRowIndex(rows []Row) (int, bool) {
	for i, row := range rows {
		if row.Status != nil && row.Status.IsCurrent {
			return i, true
		}
	}
	return 0, false
}

func (m Model) renderWarnings() string {
	if len(m.warnings) == 0 {
		return ""
	}
	return m.theme.warning.Render("warning: " + strings.Join(m.warnings, "; "))
}

func (m Model) renderStatusLine() string {
	switch {
	case m.err != nil:
		return m.theme.error.Render(m.err.Error())
	case m.confirm.kind != confirmNone:
		return m.confirm.text
	case m.inputMode != inputNone:
		if m.inputMode == inputProjectSwitch {
			return ""
		}
		return m.input.View()
	case m.message != "":
		return m.theme.success.Render(m.message)
	case m.globalFresh.Diagnostic != nil:
		return m.theme.warning.Render(m.globalFreshnessDiagnostic())
	default:
		return m.renderSelectionDetails()
	}
}

func (m Model) globalFreshnessDiagnostic() string {
	age := "unknown age"
	if !m.globalFresh.ObservedAt.IsZero() {
		duration := max(m.now().Sub(m.globalFresh.ObservedAt), 0)
		if duration < time.Minute {
			age = fmt.Sprintf("%ds old", int(duration/time.Second))
		} else {
			age = fmt.Sprintf("%dm old", int(duration/time.Minute))
		}
	}
	return fmt.Sprintf("global inventory %s · %v", age, m.globalFresh.Diagnostic)
}

func (m Model) renderSelectionDetails() string {
	row := m.selectedRow()
	if row.Removing {
		return fmt.Sprintf("removing %s\n%s", rowLabel(row), abbreviateHome(rowPath(row)))
	}
	if row.Creating {
		return fmt.Sprintf("creating %s\n%s", rowLabel(row), abbreviateHome(rowPath(row)))
	}
	if row.Entry == nil && row.Fleet != nil {
		location := "remote"
		if row.Fleet.MaterializeHost != "" {
			location = "remote on " + row.Fleet.MaterializeHost
		}
		detail := location
		if row.Fleet.RemotePath != "" {
			detail += "\n" + abbreviateHome(row.Fleet.RemotePath)
		}
		if row.Fleet.CanMaterialize {
			detail += "\npress s to sync (branch must be pushed/fetched here)"
		}
		if row.Fleet.RemoteAhead > 0 {
			upstream := strings.TrimSpace(row.Fleet.RemoteUpstream)
			if upstream == "" {
				upstream = "upstream"
			}
			detail += fmt.Sprintf("\nsource is %d %s ahead of %s",
				row.Fleet.RemoteAhead,
				plural(row.Fleet.RemoteAhead, "commit", "commits"),
				upstream,
			)
		}
		return fmt.Sprintf("selected %s · %s", rowLabel(row), detail)
	}
	path := rowPath(row)
	if path == "" {
		return ""
	}
	workspace := "workspace offline"
	if row.SessionLive {
		workspace = "workspace live"
	}
	detail := fmt.Sprintf("selected %s · layout %s · %s", rowLabel(row), m.renderLayoutLabel(), workspace)
	if fleetDetail := renderLocalFleetDetails(row); fleetDetail != "" {
		detail += "\n" + fleetDetail
	}
	return detail + "\n" + abbreviateHome(path)
}

func renderLocalFleetDetails(row Row) string {
	if row.Fleet == nil {
		return ""
	}
	details := make([]string, 0, 3)
	otherHosts := localFleetOtherHosts(row)
	if len(otherHosts) > 0 {
		details = append(details, "also on "+strings.Join(otherHosts, ", "))
	}
	if sync := renderFleetSyncDetail(row.Fleet.Sync, otherHosts); sync != "" {
		details = append(details, sync)
	}
	if dirty := renderFleetDirtyDetail(row.Fleet.Dirty, otherHosts); dirty != "" {
		details = append(details, dirty)
	}
	return strings.Join(details, " · ")
}

func localFleetOtherHosts(row Row) []string {
	if row.Fleet == nil {
		return nil
	}
	hosts := make([]string, 0, len(row.Fleet.Hosts))
	for _, host := range row.Fleet.Hosts {
		host = strings.TrimSpace(host)
		if host == "" || host == "local" {
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts
}

func renderFleetSyncDetail(sync string, otherHosts []string) string {
	sync = strings.TrimSpace(sync)
	if sync == "" || sync == "same" {
		return ""
	}
	if different, ok := strings.CutPrefix(sync, "different: "); ok {
		host, rest, _ := strings.Cut(strings.TrimSpace(different), " ")
		rest = strings.TrimSpace(rest)
		if len(otherHosts) == 1 && host == otherHosts[0] {
			if rest == "" {
				return "head differs"
			}
			return "head differs " + rest
		}
		return "head differs from " + strings.TrimSpace(different)
	}
	return "heads " + sync
}

func renderFleetDirtyDetail(dirty string, otherHosts []string) string {
	dirty = strings.TrimSpace(dirty)
	if dirty == "" || dirty == "clean" {
		return ""
	}
	entries := parseFleetDirtyEntries(dirty)
	if len(entries) == 0 {
		return "changes " + dirty
	}
	if len(entries) == 1 {
		entry := entries[0]
		summary := compactFleetDirtySummary(entry.summary)
		if len(otherHosts) == 1 && entry.host == otherHosts[0] {
			return "remote changes " + summary
		}
		return fmt.Sprintf("changes on %s %s", entry.host, summary)
	}
	return fmt.Sprintf("remote changes on %d hosts", len(entries))
}

func (m Model) layoutLabel() string {
	if m.selectedLayout == "" {
		return "default"
	}
	return m.selectedLayout
}

func (m Model) renderLayoutLabel() string {
	return m.theme.success.Render(m.layoutLabel())
}

func (m Model) projectFilterLabel() string {
	if m.projectFilter == "" {
		return "all"
	}
	return m.projectFilter
}

const projectAllLabel = "all"

type projectSwitchOption struct {
	key     string
	display string
	count   int
}

func (m Model) projectPerspectiveLabel() string {
	if m.projectPerspective == "" {
		return projectAllLabel
	}
	for _, option := range m.projectSwitchOptionsForQuery("") {
		if option.key == m.projectPerspective {
			return option.display
		}
	}
	return m.projectPerspective
}

func (m Model) projectSwitchOptionRows() []projectSwitchOption {
	return m.projectSwitchOptions()
}

func (m Model) branchOptions() []models.Branch {
	query := strings.TrimSpace(strings.ToLower(m.input.Value()))
	if query == "" {
		return append([]models.Branch(nil), m.branches...)
	}
	tokens := strings.Fields(query)
	filtered := make([]models.Branch, 0, len(m.branches))
	for _, branch := range m.branches {
		haystack := strings.ToLower(
			branch.Name + " " + branch.Label + " " + branch.Source,
		)
		matches := true
		for _, token := range tokens {
			if !strings.Contains(haystack, token) {
				matches = false
				break
			}
		}
		if matches {
			filtered = append(filtered, branch)
		}
	}
	return filtered
}

func (m Model) projectSwitchOptions() []projectSwitchOption {
	return m.projectSwitchOptionsForQuery(m.input.Value())
}

func (m Model) projectSwitchOptionsForQuery(query string) []projectSwitchOption {
	query = strings.TrimSpace(strings.ToLower(query))
	type projectAccumulator struct {
		display string
		count   int
	}
	byKey := make(map[string]projectAccumulator)
	displayCounts := make(map[string]int)
	for _, row := range m.rows {
		key := rowProjectKey(row)
		if key == "" {
			continue
		}
		display := rowRepoName(row)
		if display == "" {
			display = key
		}
		acc := byKey[key]
		acc.display = display
		acc.count++
		byKey[key] = acc
	}
	for _, acc := range byKey {
		displayCounts[acc.display]++
	}
	options := make([]projectSwitchOption, 0, len(byKey)+1)
	options = append(options, projectSwitchOption{key: "", display: projectAllLabel, count: len(m.rows)})
	for key, acc := range byKey {
		display := acc.display
		if displayCounts[display] > 1 {
			display = key
		}
		options = append(options, projectSwitchOption{
			key:     key,
			display: display,
			count:   acc.count,
		})
	}
	sort.SliceStable(options[1:], func(i, j int) bool {
		left := options[i+1]
		right := options[j+1]
		if left.display != right.display {
			return strings.ToLower(left.display) < strings.ToLower(right.display)
		}
		return strings.ToLower(left.key) < strings.ToLower(right.key)
	})

	if query == "" {
		return options
	}
	filtered := make([]projectSwitchOption, 0, len(options))
	for _, option := range options {
		haystack := strings.ToLower(strings.Join([]string{option.display, option.key}, " "))
		if strings.Contains(haystack, query) {
			filtered = append(filtered, option)
		}
	}
	return filtered
}

func (m Model) renderProjectSwitchView() string {
	var b strings.Builder
	b.WriteString(m.theme.header.Render("Project"))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(m.theme.error.Render(m.err.Error()))
		b.WriteString("\n\n")
	}

	searchDisplay := m.input.Value()
	if searchDisplay == "" {
		searchDisplay = m.theme.dim.Render("Type to search")
	}
	b.WriteString("Search: ")
	b.WriteString(searchDisplay)
	b.WriteString("\n\n")

	options := m.projectSwitchOptionRows()
	if len(options) == 0 {
		b.WriteString(m.theme.dim.Render("  No matching projects"))
		b.WriteString("\n")
	} else {
		selected := clampCursor(m.projectSwitchCursor, len(options))
		visibleRows := m.projectSwitchVisibleRows()
		start, visibleOptions := projectSwitchWindow(options, selected, visibleRows)
		for offset, option := range visibleOptions {
			i := start + offset
			cursor := " "
			if i == selected {
				cursor = "▸"
			}
			line := fmt.Sprintf("%s %s (%d)", cursor, projectSwitchDisplayName(option), option.count)
			if i == selected {
				line = m.theme.cursor.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		if len(options) > visibleRows && visibleRows > 0 {
			end := start + len(visibleOptions)
			b.WriteString(m.theme.dim.Render(fmt.Sprintf("[showing %d-%d of %d]", start+1, end, len(options))))
			b.WriteString("\n")
		}
	}

	b.WriteString(renderHelpTable(inputHelpRows(inputProjectSwitch), m.width))
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) renderExistingBranchView() string {
	var b strings.Builder
	b.WriteString("Add existing branch")
	if name := rowRepoName(m.branchRow); name != "" {
		b.WriteString(" in ")
		b.WriteString(name)
	}
	b.WriteString("\n\n")
	b.WriteString(m.input.View())
	b.WriteString("\n\n")

	options := m.branchOptions()
	switch {
	case m.err != nil:
		b.WriteString(m.theme.error.Render(m.err.Error()))
		b.WriteString("\n")
	case m.branchesLoading:
		b.WriteString(m.theme.dim.Render("  Loading available branches…"))
		b.WriteString("\n")
	case len(options) == 0:
		b.WriteString(m.theme.dim.Render("  No matching available branches"))
		b.WriteString("\n")
	default:
		selected := clampCursor(m.branchCursor, len(options))
		visibleRows := len(options)
		if m.height > 0 {
			footerLines := helpLines(inputHelpRows(inputExistingBranch), m.width)
			visibleRows = m.height - 6 - footerLines
			if visibleRows < 1 {
				visibleRows = 1
			}
		}
		start := 0
		if visibleRows < len(options) {
			start = selected - visibleRows/2
			if start < 0 {
				start = 0
			}
			if start+visibleRows > len(options) {
				start = len(options) - visibleRows
			}
		}
		end := min(start+visibleRows, len(options))
		for index := start; index < end; index++ {
			branch := options[index]
			cursor := " "
			if index == selected {
				cursor = "▸"
			}
			label := branchDisplayLabel(branch)
			location := "local"
			if branch.IsRemote {
				location = "remote"
			}
			line := fmt.Sprintf("%s %-48s %s", cursor, label, location)
			if index == selected {
				line = m.theme.cursor.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		if len(options) > visibleRows {
			b.WriteString(m.theme.dim.Render(fmt.Sprintf(
				"[showing %d-%d of %d]",
				start+1,
				end,
				len(options),
			)))
			b.WriteString("\n")
		}
	}

	b.WriteString(renderHelpTable(inputHelpRows(inputExistingBranch), m.width))
	return strings.TrimRight(b.String(), "\n")
}

func branchDisplayLabel(branch models.Branch) string {
	if label := strings.TrimSpace(branch.Label); label != "" {
		return label
	}
	if branch.IsRemote {
		source := strings.TrimPrefix(branch.Source, "refs/remotes/")
		if source != "" {
			return fmt.Sprintf("%s (%s)", branch.Name, source)
		}
	}
	return branch.Name
}

func (m Model) projectSwitchVisibleRows() int {
	if m.height <= 0 {
		return len(m.projectSwitchOptions())
	}
	footerLines := helpLines(inputHelpRows(inputProjectSwitch), m.width)
	count := m.height - 5 - footerLines
	if count < 1 {
		return 1
	}
	return count
}

func projectSwitchWindow(options []projectSwitchOption, cursor, maxRows int) (int, []projectSwitchOption) {
	if len(options) == 0 || maxRows <= 0 || maxRows >= len(options) {
		return 0, options
	}
	cursor = clampCursor(cursor, len(options))
	start := cursor - maxRows/2
	if start < 0 {
		start = 0
	}
	if start+maxRows > len(options) {
		start = len(options) - maxRows
	}
	return start, options[start : start+maxRows]
}

func projectSwitchDisplayName(option projectSwitchOption) string {
	if option.key == "" {
		return "All"
	}
	return option.display
}

func indexProjectSwitchOption(options []projectSwitchOption, targetKey string) int {
	for i, option := range options {
		if option.key == targetKey {
			return i
		}
	}
	return 0
}

func (m Model) renderHelp() string {
	return strings.Join([]string{
		"kwt help",
		"",
		"↑/k ↓/j move    g/G top/bottom",
		"enter attach    L layout    n new    b branch    d delete    s sync    c shell    K kill workspace",
		"P project       p filter    / search    r refresh    esc cancel/clear    q quit",
		"",
		"Press any key to close help.",
	}, "\n")
}
