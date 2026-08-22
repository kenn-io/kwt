package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

type fakeBackend struct {
	rows              []Row
	fleetRows         []Row
	fleetWarnings     []string
	layoutNames       []string
	insideTmux        bool
	createPath        string
	createErr         error
	materializePath   string
	materializeErr    error
	removeErr         error
	removeWorktree    func(context.Context, Row, bool) error
	killErr           error
	openErr           error
	openProcess       *exec.Cmd
	branches          []models.Branch
	branchErr         error
	fastListCalls     int
	listCalls         int
	branchCalls       []string
	mergeFleetCalls   int
	mergeCtx          context.Context
	createCalls       []string
	createSources     []string
	materializeRows   []string
	removeCalls       []string
	removeForces      []bool
	killCalls         []string
	openCalls         []string
	openExistingCalls []string
	inventoryCalls    []InventoryRequest
	unregistered      []Row
}

type removedWithResidualFilesError struct {
	error
}

func (removedWithResidualFilesError) WorktreeRemoved() bool {
	return true
}

type refreshRequiredActionError struct {
	error
}

func (refreshRequiredActionError) RefreshRequired() bool {
	return true
}

func (b *fakeBackend) ListFast(ctx context.Context) ([]Row, []string, error) {
	b.fastListCalls++
	return append([]Row(nil), b.rows...), nil, nil
}

func (b *fakeBackend) List(ctx context.Context) ([]Row, []string, error) {
	b.listCalls++
	return append([]Row(nil), b.rows...), nil, nil
}

func (b *fakeBackend) LoadInventory(_ context.Context, request InventoryRequest) (InventoryResult, error) {
	b.inventoryCalls = append(b.inventoryCalls, request)
	if request.Scope == InventoryCachedDashboard {
		b.fastListCalls++
	} else {
		b.listCalls++
	}
	return InventoryResult{
		Rows:       append([]Row(nil), b.rows...),
		ObservedAt: time.Now(),
		Current:    request.Scope != InventoryCachedDashboard,
	}, nil
}

func TestModelLoadsCacheThenActiveProjectThenBackgroundGlobal(t *testing.T) {
	row := testRow("widget", "main", "/work/widget")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	backend := &fakeBackend{rows: []Row{row}}
	model := NewModel(backend, "/work/widget").WithInitialAnchor("/work/widget")

	cached := model.Init()()
	model, projectCmd := updateModel(t, model, cached)
	require.NotNil(t, projectCmd)
	assert.Equal(t, InventoryCachedDashboard, backend.inventoryCalls[0].Scope)

	project := projectCmd()
	_, globalCmd := updateModel(t, model, project)
	require.NotNil(t, globalCmd)
	assert.Equal(t, InventoryCurrentRepository, backend.inventoryCalls[1].Scope)
	assert.True(t, backend.inventoryCalls[1].CollectStatuses)

	_ = globalCmd()
	assert.Equal(t, InventoryCurrentDashboard, backend.inventoryCalls[2].Scope)
	assert.False(t, backend.inventoryCalls[2].CollectStatuses)
}

func TestModelLoadsCacheThenGlobalForDirectoryPerspective(t *testing.T) {
	row := Row{Workspace: &WorkspaceInfo{Name: "notes", Path: "/work/notes"}}
	backend := &fakeBackend{rows: []Row{row}}
	model := NewModel(backend, rowPath(row)).WithInitialAnchor(rowPath(row))

	cached := model.Init()()
	_, globalCmd := updateModel(t, model, cached)

	require.NotNil(t, globalCmd)
	_ = globalCmd()
	require.Len(t, backend.inventoryCalls, 2)
	assert.Equal(t, InventoryCurrentDashboard, backend.inventoryCalls[1].Scope)
	assert.False(t, backend.inventoryCalls[1].CollectStatuses)
}

func TestInitialProjectRefreshFailureStillStartsBackgroundGlobal(t *testing.T) {
	project := "github.com/acme/widget"
	row := testRow("widget", "main", "/work/widget")
	row.Entry.RepositoryInfo.FullPath = project
	model := NewModel(&fakeBackend{}, rowPath(row))
	model.rows = []Row{row}

	model, cmd := updateModel(t, model, inventoryMsg{
		request: InventoryRequest{
			Scope: InventoryCurrentRepository, ProjectIdentity: project,
		},
		err: errors.New("repository unavailable"),
	})

	require.NotNil(t, cmd)
	assert.True(t, model.backgroundGlobalStarted)
	assert.Equal(t, InventoryCurrentDashboard, model.fetchingRequest.Scope)
	assert.False(t, model.fetchingRequest.CollectStatuses)
	assert.Contains(t, model.message, "repository unavailable")
}

func TestBackgroundGlobalPreservesFreshProjectStatus(t *testing.T) {
	project := "github.com/acme/widget"
	row := testRow("widget", "topic", "/w/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "GitHub.com/Acme/Widget"
	row.Status = &models.WorktreeStatus{Path: rowPath(row), Status: models.WorktreeStatusModified}
	model := NewModel(&fakeBackend{}, "/w/widget/topic")
	model.rows = []Row{row}
	model.projectFresh = map[string]scopeFreshness{
		project: {ObservedAt: time.Now(), Current: true},
	}
	structural := row
	structural.SessionLive = true
	structural.Status = &models.WorktreeStatus{Path: rowPath(row), Status: models.WorktreeStatusUnknown}

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentDashboard},
		result:  InventoryResult{Rows: []Row{structural}, ObservedAt: time.Now(), Current: true},
	})

	assert.True(t, model.rows[0].SessionLive)
	assert.Equal(t, models.WorktreeStatusModified, model.rows[0].Status.Status)
}

func TestBackgroundGlobalMakesProjectStaleWhenHeadChanges(t *testing.T) {
	project := "github.com/acme/widget"
	row := testRow("widget", "topic", "/w/widget/topic")
	row.Entry.RepositoryInfo.FullPath = project
	row.Entry.Generation = "11111111111111111111111111111111"
	row.Entry.CommitHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	row.Status = &models.WorktreeStatus{Path: rowPath(row), Status: models.WorktreeStatusModified}
	model := currentModelWithRows(t, &fakeBackend{}, row)
	structural := row
	entry := *structural.Entry
	entry.CommitHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	structural.Entry = &entry
	structural.Status = &models.WorktreeStatus{Path: rowPath(row), Status: models.WorktreeStatusUnknown}

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentDashboard},
		result:  InventoryResult{Rows: []Row{structural}, ObservedAt: time.Now(), Current: true},
	})

	assert.False(t, model.projectFresh[projectFreshnessKey(project)].Current)
	assert.Equal(t, models.WorktreeStatusUnknown, model.rows[0].Status.Status)
}

func TestBackgroundGlobalMakesProjectStaleWhenWorktreeAppears(t *testing.T) {
	project := "github.com/acme/widget"
	existing := testRow("widget", "main", "/w/widget/main")
	existing.Entry.RepositoryInfo.FullPath = project
	added := testRow("widget", "topic", "/w/widget/topic")
	added.Entry.RepositoryInfo.FullPath = project
	model := NewModel(&fakeBackend{}, rowPath(existing))
	model.rows = []Row{existing}
	model.projectFresh[projectFreshnessKey(project)] = scopeFreshness{
		ObservedAt: time.Now(), Current: true,
	}
	existing.Status = nil
	added.Status = nil

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentDashboard},
		result: InventoryResult{
			Rows: []Row{existing, added}, ObservedAt: time.Now(), Current: true,
		},
	})
	model.cursor = indexByPath(model.filteredRows(), rowPath(added))
	model, _ = updateModel(t, model, press("d"))

	assert.Equal(t, confirmNone, model.confirm.kind)
	assert.Contains(t, model.message, "project inventory is refreshing")
}

func TestStatusFreeGlobalRefreshDoesNotAuthorizeProjectMutation(t *testing.T) {
	row := testRow("widget", "topic", "/w/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := NewModel(&fakeBackend{}, "/w/widget/topic")
	model.rows = []Row{row}

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentDashboard},
		result: InventoryResult{
			Rows: []Row{row}, ObservedAt: time.Now(), Current: true,
		},
	})
	model, _ = updateModel(t, model, press("d"))

	assert.Equal(t, confirmNone, model.confirm.kind)
	assert.Contains(t, model.message, "project inventory is refreshing")
}

func TestStatusBearingGlobalRefreshAuthorizesProjectMutation(t *testing.T) {
	row := testRow("widget", "topic", "/w/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := NewModel(&fakeBackend{}, "/w/widget/topic")

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentDashboard, CollectStatuses: true},
		result: InventoryResult{
			Rows: []Row{row}, ObservedAt: time.Now(), Current: true,
		},
	})
	model, _ = updateModel(t, model, press("d"))

	assert.Equal(t, confirmDelete, model.confirm.kind)
}

func TestStatusBearingGlobalRefreshMakesExistingProjectFreshnessStale(t *testing.T) {
	row := testRow("widget", "topic", "/w/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := currentModelWithRows(t, &fakeBackend{}, row)
	model.fetching = false

	model, refreshCmd := updateModel(t, model, press("r"))
	require.NotNil(t, refreshCmd)
	model, _ = updateModel(t, model, press("d"))

	assert.Equal(t, confirmNone, model.confirm.kind)
	assert.Contains(t, model.message, "project inventory is refreshing")
}

func TestDirectoryWorkspaceMutationRequiresCurrentGlobalScope(t *testing.T) {
	workspace := Row{Workspace: &WorkspaceInfo{Name: "notes", Path: "/work/notes"}}
	model := NewModel(&fakeBackend{}, "/work")
	model.rows = []Row{workspace}

	blocked, _ := updateModel(t, model, press("d"))
	assert.Equal(t, confirmNone, blocked.confirm.kind)
	assert.Contains(t, blocked.message, "global inventory is refreshing")

	model.globalFresh = scopeFreshness{ObservedAt: time.Now(), Current: true}
	current, _ := updateModel(t, model, press("d"))
	assert.Equal(t, confirmUnregister, current.confirm.kind)
}

func TestProjectSwitchRefreshesOnlySelectedProject(t *testing.T) {
	widget := testRow("widget", "main", "/w/widget")
	widget.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	other := testRow("other", "main", "/w/other")
	other.Entry.RepositoryInfo.FullPath = "github.com/acme/other"
	model := NewModel(&fakeBackend{}, "/w/widget")
	model.rows = []Row{widget, other}
	model.projectPerspective = "github.com/acme/widget"
	model.inputMode = inputProjectSwitch
	model.projectSwitchCursor = indexProjectSwitchOption(model.projectSwitchOptions(), "github.com/acme/other")

	model, cmd := model.chooseProjectPerspective()

	require.NotNil(t, cmd)
	assert.Equal(t, InventoryCurrentRepository, model.fetchingRequest.Scope)
	assert.Equal(t, "github.com/acme/other", model.fetchingRequest.ProjectIdentity)
}

func TestProjectPerspectiveWithoutLocalRepositoryRefreshesDashboard(t *testing.T) {
	tests := []struct {
		name string
		row  Row
	}{
		{
			name: "directory workspace",
			row:  Row{Workspace: &WorkspaceInfo{Name: "notes", Path: "/w/notes"}},
		},
		{
			name: "remote-only fleet project",
			row: Row{Fleet: &FleetInfo{
				ProjectIdentity: "github.com/acme/widget",
				ProjectName:     "widget",
				Kind:            "branch",
				Ref:             "topic",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(&fakeBackend{}, "/w")
			model.rows = []Row{tt.row}
			model.projectPerspective = rowProjectKey(tt.row)
			model.fetching = false

			model, cmd := updateModel(t, model, press("r"))

			require.NotNil(t, cmd)
			assert.Equal(t, InventoryCurrentDashboard, model.fetchingRequest.Scope)
			assert.Empty(t, model.fetchingRequest.WorkingDirectory)
			assert.False(t, model.fetchingRequest.CollectStatuses)
		})
	}
}

func TestRepositoryRefreshNormalizesProjectFreshnessIdentity(t *testing.T) {
	row := testRow("widget", "topic", "/w/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := NewModel(&fakeBackend{}, rowPath(row))
	model.rows = []Row{row}
	model.projectPerspective = rowProjectKey(row)
	model.backgroundGlobalStarted = true

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{
			Scope:           InventoryCurrentRepository,
			ProjectIdentity: "GitHub.com/Acme/Widget",
		},
		result: InventoryResult{
			Rows: []Row{row}, ObservedAt: time.Now(), Current: true,
		},
	})
	model, _ = updateModel(t, model, press("d"))

	assert.Equal(t, confirmDelete, model.confirm.kind)
}

func TestRepositoryRefreshPreservesPendingCreationAbsentFromResult(t *testing.T) {
	project := "github.com/acme/widget"
	main := testRow("widget", "main", "/w/widget")
	main.Entry.RepositoryInfo.FullPath = project
	pending := testRow("widget", "topic", "/w/widget/topic")
	pending.Entry.RepositoryInfo.FullPath = project
	pending.Creating = true
	model := NewModel(&fakeBackend{}, rowPath(main))
	model.rows = []Row{main, pending}
	model.creating = []string{rowPath(pending)}
	model.backgroundGlobalStarted = true

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{
			Scope: InventoryCurrentRepository, ProjectIdentity: project,
		},
		result: InventoryResult{
			Rows: []Row{main}, ObservedAt: time.Now(), Current: true,
		},
	})

	index, ok := identityRowIndex(model.rows, rowPath(pending))
	require.True(t, ok)
	assert.True(t, model.rows[index].Creating)
}

func TestAllProjectsStartsCurrentGlobalStatusRefresh(t *testing.T) {
	row := testRow("widget", "main", "/w/widget")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := NewModel(&fakeBackend{}, "/w/widget")
	model.rows = []Row{row}
	model.projectPerspective = "github.com/acme/widget"
	model.inputMode = inputProjectSwitch
	model.projectSwitchCursor = indexProjectSwitchOption(model.projectSwitchOptions(), "")

	model, cmd := model.chooseProjectPerspective()

	require.NotNil(t, cmd)
	assert.Equal(t, InventoryCurrentDashboard, model.fetchingRequest.Scope)
	assert.True(t, model.fetchingRequest.CollectStatuses)
}

func TestBackgroundGlobalFailureKeepsRowsAndShowsAge(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	row := testRow("widget", "main", "/w/widget")
	model := NewModel(&fakeBackend{}, "/w/widget")
	model.now = func() time.Time { return now }
	model.rows = []Row{row}
	model.globalFresh = scopeFreshness{ObservedAt: now.Add(-2 * time.Minute)}

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentDashboard},
		err:     errors.New("daemon refresh failed"),
	})

	assert.Len(t, model.rows, 1)
	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "global inventory 2m old")
	assert.Contains(t, content, "daemon refresh failed")
}

func TestModelShellOnCachedDeletedPathStaysOpen(t *testing.T) {
	backend := &fakeBackend{rows: []Row{testRow("widget", "topic", "/missing")}}
	model := NewModel(backend, "/work")
	model.rows = backend.rows
	oldValidate := validateShellDirectory
	validateShellDirectory = func(string) error { return os.ErrNotExist }
	t.Cleanup(func() { validateShellDirectory = oldValidate })

	next, cmd := updateModel(t, model, press("c"))

	assert.Nil(t, cmd)
	assert.Equal(t, HandoffNone, next.Handoff().Kind)
	assert.Contains(t, next.message, "no longer exists")
}

func TestRemovingRowDetailShowsOperationAndPath(t *testing.T) {
	row := testRow("widget", "topic", "/work/topic")
	row.Removing = true
	model := NewModel(&fakeBackend{}, "/work/topic")
	model.rows = []Row{row}

	text := stripANSI(model.renderSelectionDetails())

	assert.Contains(t, text, "removing widget:topic")
	assert.Contains(t, text, "/work/topic")
}

func TestCachedLiveAttachUsesExistingOnlyBackend(t *testing.T) {
	row := testRow("widget", "topic", "/work/topic")
	row.SessionLive = true
	backend := &fakeBackend{rows: []Row{row}, insideTmux: true}
	model := NewModel(backend, "/work")
	model.rows = backend.rows

	_, cmd := updateModel(t, model, press("enter"))

	require.NotNil(t, cmd)
	_ = cmd()
	assert.Equal(t, []string{"/work/topic"}, backend.openExistingCalls)
	assert.Empty(t, backend.openCalls)
}

func TestCachedLiveAttachOutsideTmuxMarksExistingOnlyHandoff(t *testing.T) {
	row := testRow("widget", "topic", "/work/topic")
	row.SessionLive = true
	model := NewModel(&fakeBackend{rows: []Row{row}}, "/work")
	model.rows = []Row{row}

	next, cmd := updateModel(t, model, press("enter"))

	require.NotNil(t, cmd)
	assert.Equal(t, HandoffAttach, next.Handoff().Kind)
	assert.True(t, next.Handoff().ExistingOnly)
}

func TestMergeRepositoryRowsPreservesDashboardRepositoryURL(t *testing.T) {
	oldRow := testRow("widget", "topic", "/work/topic")
	oldRow.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	oldRow.Entry.RepositoryURL = "https://github.com/acme/widget.git"
	newRow := testRow("widget", "topic", "/work/topic")
	newRow.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	previous := []Row{oldRow}
	current := []Row{newRow}
	current[0].Status = &models.WorktreeStatus{Path: "/work/topic", Status: models.WorktreeStatusModified}

	merged := mergeRepositoryRows(previous, current, "github.com/acme/widget")

	assert.Equal(t, "https://github.com/acme/widget.git", merged[0].Entry.RepositoryURL)
	assert.Equal(t, models.WorktreeStatusModified, merged[0].Status.Status)
}

func (b *fakeBackend) MergeFleet(ctx context.Context, rows []Row) ([]Row, []string) {
	b.mergeFleetCalls++
	b.mergeCtx = ctx
	return append(append([]Row(nil), rows...), b.fleetRows...), b.fleetWarnings
}

func (b *fakeBackend) CreateWorktree(
	ctx context.Context,
	row Row,
	branch string,
	source string,
) (string, error) {
	b.createCalls = append(b.createCalls, rowPath(row)+":"+branch)
	b.createSources = append(b.createSources, source)
	return b.createPath, b.createErr
}

func (b *fakeBackend) ListBranches(
	ctx context.Context,
	row Row,
) ([]models.Branch, error) {
	b.branchCalls = append(b.branchCalls, rowPath(row))
	return append([]models.Branch(nil), b.branches...), b.branchErr
}

func (b *fakeBackend) PreviewWorktree(row Row, branch string) (Row, error) {
	if b.createErr != nil {
		return Row{}, b.createErr
	}
	entry := *row.Entry
	entry.Branch = branch
	entry.Path = b.createPath
	entry.IsMain = false
	return Row{Entry: &entry}, nil
}

func (b *fakeBackend) RemoveWorktree(ctx context.Context, row Row, force bool) error {
	b.removeCalls = append(b.removeCalls, rowPath(row))
	b.removeForces = append(b.removeForces, force)
	if b.removeWorktree != nil {
		return b.removeWorktree(ctx, row, force)
	}
	return b.removeErr
}

func TestModelQueuesRemovalFIFOAndKeepsNavigationResponsive(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	backend := &fakeBackend{removeWorktree: func(_ context.Context, row Row, _ bool) error {
		started <- rowPath(row)
		<-release
		return nil
	}}
	first := testRow("widget", "one", "/work/one")
	second := testRow("widget", "two", "/work/two")
	model := currentModelWithRows(t, backend, first, second)

	model, firstCmd := confirmDeleteRow(t, model, 0)
	require.NotNil(t, firstCmd)
	model, secondCmd := confirmDeleteRow(t, model, 1)
	assert.Nil(t, secondCmd)
	assert.True(t, model.rows[0].Removing)
	assert.True(t, model.rows[1].Removing)

	messages := make(chan tea.Msg, 1)
	go func() { messages <- firstCmd() }()
	assert.Equal(t, "/work/one", <-started)
	before := model.cursor
	model, _ = updateModel(t, model, press("k"))
	assert.NotEqual(t, before, model.cursor)
	release <- struct{}{}
	model, nextCmd := updateModel(t, model, <-messages)
	require.NotNil(t, nextCmd)
	go func() { messages <- nextCmd() }()
	assert.Equal(t, "/work/two", <-started)
	release <- struct{}{}
	model, _ = updateModel(t, model, <-messages)
	assert.Nil(t, model.removalActive)
	assert.Empty(t, model.removalQueue)
}

func TestRemovalCompletionRefreshesOnlyAffectedProject(t *testing.T) {
	backend := &fakeBackend{}
	row := testRow("widget", "topic", "/work/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := currentModelWithRows(t, backend, row)
	job := removalJob{
		row: row, key: removalKey(row), projectIdentity: "github.com/acme/widget",
		workingDirectory: rowPath(row),
	}
	model.removalActive = &job

	model, cmd := updateModel(t, model, removalDoneMsg{job: job, removed: true, refresh: true})
	require.NotNil(t, cmd)
	_ = cmd()
	last := backend.inventoryCalls[len(backend.inventoryCalls)-1]
	assert.Equal(t, InventoryCurrentRepository, last.Scope)
	assert.Equal(t, "github.com/acme/widget", last.ProjectIdentity)
}

func TestNewerRemovalRefreshRejectsOlderGlobalInventory(t *testing.T) {
	project := "github.com/acme/widget"
	main := testRow("widget", "main", "/work/widget")
	main.Entry.RepositoryInfo.FullPath = project
	topic := testRow("widget", "topic", "/work/widget/topic")
	topic.Entry.RepositoryInfo.FullPath = project
	backend := &fakeBackend{rows: []Row{main, topic}}
	model := currentModelWithRows(t, backend, main, topic)
	model.backgroundGlobalStarted = true

	model, globalCmd := model.startInventory(InventoryRequest{Scope: InventoryCurrentDashboard})
	job := removalJob{
		row: topic, key: removalKey(topic), projectIdentity: project,
		workingDirectory: rowPath(main),
	}
	model.removalActive = &job
	model, projectCmd := updateModel(t, model, removalDoneMsg{
		job: job, removed: true, refresh: true,
	})
	require.NotNil(t, projectCmd)

	backend.rows = []Row{main}
	model, _ = updateModel(t, model, projectCmd())
	backend.rows = []Row{main, topic}
	model, _ = updateModel(t, model, globalCmd())

	_, restored := identityRowIndex(model.rows, rowPath(topic))
	assert.False(t, restored)
}

func TestSuccessfulRemovalImmediatelyRejectsOlderInventory(t *testing.T) {
	project := "github.com/acme/widget"
	main := testRow("widget", "main", "/work/widget")
	main.Entry.RepositoryInfo.FullPath = project
	removed := testRow("widget", "removed", "/work/widget/removed")
	removed.Entry.RepositoryInfo.FullPath = project
	next := testRow("widget", "next", "/work/widget/next")
	next.Entry.RepositoryInfo.FullPath = project
	model := currentModelWithRows(t, &fakeBackend{}, main, removed, next)
	model.backgroundGlobalStarted = true
	model, _ = model.startInventory(InventoryRequest{Scope: InventoryCurrentDashboard})
	staleSeq := model.inventorySeq
	removedJob := removalJob{
		row: removed, key: removalKey(removed), projectIdentity: project,
		workingDirectory: rowPath(main),
	}
	nextJob := removalJob{
		row: next, key: removalKey(next), projectIdentity: project,
		workingDirectory: rowPath(main),
	}
	model.removalActive = &removedJob
	model.removalQueue = []removalJob{nextJob}

	model, nextRemovalCmd := updateModel(t, model, removalDoneMsg{
		job: removedJob, removed: true, refresh: true,
	})
	require.NotNil(t, nextRemovalCmd)
	model, _ = updateModel(t, model, inventoryMsg{
		seq:     staleSeq,
		request: InventoryRequest{Scope: InventoryCurrentDashboard},
		result: InventoryResult{
			Rows: []Row{main, removed, next}, ObservedAt: time.Now(), Current: true,
		},
	})

	_, restored := identityRowIndex(model.rows, rowPath(removed))
	assert.False(t, restored)
}

func TestRemovalRejectsFleetRowsStartedBeforeDeletion(t *testing.T) {
	project := "github.com/acme/widget"
	main := testRow("widget", "main", "/work/widget")
	main.Entry.RepositoryInfo.FullPath = project
	topic := testRow("widget", "topic", "/work/widget/topic")
	topic.Entry.RepositoryInfo.FullPath = project
	model := currentModelWithRows(t, &fakeBackend{}, main, topic)
	staleFleet := fleetRowsMsg{seq: model.loadSeq, rows: []Row{main, topic}}
	job := removalJob{
		row: topic, key: removalKey(topic), projectIdentity: project,
		workingDirectory: rowPath(main),
	}
	model.removalActive = &job

	model, _ = updateModel(t, model, removalDoneMsg{
		job: job, removed: true, refresh: true,
	})
	model, _ = updateModel(t, model, staleFleet)

	_, restored := identityRowIndex(model.rows, rowPath(topic))
	assert.False(t, restored)
}

func TestRepositoryRefreshPreservesActiveRemovalState(t *testing.T) {
	project := "github.com/acme/widget"
	row := testRow("widget", "topic", "/work/widget/topic")
	row.Entry.RepositoryInfo.FullPath = project
	model := currentModelWithRows(t, &fakeBackend{}, row)
	job := removalJob{row: row, key: removalKey(row), projectIdentity: project}
	model.removalActive = &job

	model, _ = updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentRepository, ProjectIdentity: project},
		result:  InventoryResult{Rows: []Row{row}, ObservedAt: time.Now(), Current: true},
	})

	assert.True(t, model.rows[0].Removing)
}

func TestIndeterminateRemovalKeepsRowUntilScopedRefresh(t *testing.T) {
	row := testRow("widget", "topic", "/work/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := currentModelWithRows(t, &fakeBackend{}, row)
	job := removalJob{row: row, key: removalKey(row)}
	model.rows[0].Removing = true
	model.removalActive = &job

	model, _ = updateModel(t, model, removalDoneMsg{
		job: job, err: refreshRequiredActionError{errors.New("outcome unknown")}, refresh: true,
	})

	assert.NotNil(t, model.rows[0].Entry)
	assert.False(t, model.rows[0].Removing)
	assert.ErrorContains(t, model.err, "outcome unknown")
}

func currentModelWithRows(t *testing.T, backend *fakeBackend, rows ...Row) Model {
	t.Helper()
	model := NewModel(backend, rowPath(rows[0]))
	model.rows = append([]Row(nil), rows...)
	model.inventoryCurrent = true
	for _, row := range rows {
		model.projectFresh[projectFreshnessKey(rowProjectKey(row))] = scopeFreshness{
			ObservedAt: time.Now(), Current: true,
		}
	}
	return model
}

func confirmDeleteRow(t *testing.T, model Model, index int) (Model, tea.Cmd) {
	t.Helper()
	model.cursor = index
	model, _ = updateModel(t, model, press("d"))
	return updateModel(t, model, press("y"))
}

func (b *fakeBackend) MaterializeWorktree(ctx context.Context, row Row) (string, error) {
	if row.Fleet != nil {
		b.materializeRows = append(b.materializeRows, row.Fleet.ProjectIdentity+":"+row.Fleet.Ref)
	}
	return b.materializePath, b.materializeErr
}

func (b *fakeBackend) KillSession(row Row) error {
	b.killCalls = append(b.killCalls, row.SessionName)
	return b.killErr
}

func (b *fakeBackend) OpenInTmux(
	ctx context.Context,
	row Row,
	layoutName string,
) (*exec.Cmd, error) {
	b.openCalls = append(b.openCalls, rowPath(row)+":"+layoutName)
	return b.openProcess, b.openErr
}

func (b *fakeBackend) OpenExistingInTmux(_ context.Context, row Row) (*exec.Cmd, error) {
	b.openExistingCalls = append(b.openExistingCalls, rowPath(row))
	return b.openProcess, b.openErr
}

func (b *fakeBackend) UnregisterWorkspace(row Row) error {
	b.unregistered = append(b.unregistered, row)
	return nil
}

func (b *fakeBackend) LayoutNames() []string { return append([]string(nil), b.layoutNames...) }

func (b *fakeBackend) InsideTmux() bool { return b.insideTmux }

func press(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
	case "esc":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc})
	case "up":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyUp})
	case "down":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})
	case "backspace":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace})
	default:
		runes := []rune(s)
		return tea.KeyPressMsg(tea.Key{Code: runes[0], Text: s})
	}
}

func paste(s string) tea.PasteMsg {
	return tea.PasteMsg{Content: s}
}

func updateModel(t *testing.T, model Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := model.Update(msg)
	m, ok := next.(Model)
	require.True(t, ok)
	return m, cmd
}

func viewContent(model Model) string {
	return model.View().Content
}

func TestModelPaintsFastRowsBeforeLoadingFullStatus(t *testing.T) {
	backend := &fakeBackend{
		rows: []Row{testRow("kwt", "main", "/w/kwt/main")},
	}
	model := NewModel(backend, "/worktrees")

	fastCmd := model.Init()
	fastMsg := fastCmd()

	assert.Equal(t, 1, backend.fastListCalls)
	assert.Zero(t, backend.listCalls)

	model, fullCmd := updateModel(t, model, fastMsg)
	assert.Contains(t, stripANSI(viewContent(model)), "main")
	require.NotNil(t, fullCmd)

	_ = fullCmd()
	assert.Equal(t, 1, backend.listCalls)
}

func TestWorkspaceRowActions(t *testing.T) {
	row := Row{
		Workspace:   &WorkspaceInfo{Name: "notes", Path: "/Users/me/notes"},
		SessionName: "kwt-workspace-dir-notes-12345678",
		SessionLive: true,
	}
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	// Open: workspace rows hand off to attach outside tmux.
	next, _ := model.openSelected()
	assert.Equal(t, HandoffAttach, next.Handoff().Kind)

	// New branch: gated with a message.
	next, _ = model.startNewBranch()
	assert.Contains(t, next.message, "not a git worktree")

	// Sync: already gated by the row.Fleet == nil branch.
	next, _ = model.syncSelected(row)
	assert.Contains(t, next.message, "nothing to sync")

	// Kill: allowed for live sessions.
	next, _ = model.startKill()
	assert.Equal(t, confirmKill, next.confirm.kind)

	// Delete key: offers unregister, never worktree removal.
	next, _ = model.startDelete()
	require.Equal(t, confirmUnregister, next.confirm.kind)
	assert.Contains(t, next.confirm.text, "unregister")

	// Confirming with y calls Backend.UnregisterWorkspace and refreshes.
	_, cmd := updateModel(t, next, press("y"))
	require.NotNil(t, cmd)
	msg := cmd()
	done, ok := msg.(actionDoneMsg)
	require.True(t, ok)
	assert.True(t, done.refresh)
	require.Len(t, backend.unregistered, 1)
	assert.Equal(t, "notes", backend.unregistered[0].Workspace.Name)
}

func TestCachedRowsBlockMutationsUntilCurrentRowsArrive(t *testing.T) {
	row := Row{Workspace: &WorkspaceInfo{Name: "notes", Path: "/stale/notes"}}
	model := NewModel(&fakeBackend{}, "/worktrees")

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{row}})
	blocked, _ := updateModel(t, model, press("d"))

	assert.Equal(t, confirmNone, blocked.confirm.kind)
	assert.Contains(t, blocked.message, "refresh")

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})
	current, _ := updateModel(t, model, press("d"))
	assert.Equal(t, confirmUnregister, current.confirm.kind)
}

func TestCachedRowsPreserveUnresolvedInitialAnchor(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model.anchorPath = "/launch"
	cached := Row{Workspace: &WorkspaceInfo{Name: "cached", Path: "/cached"}}
	current := Row{Workspace: &WorkspaceInfo{Name: "launch", Path: "/launch"}}

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{cached}})
	assert.Equal(t, "/launch", model.anchorPath)

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{current}})
	assert.Empty(t, model.anchorPath)
	assert.Equal(t, "/launch", rowPath(model.selectedRow()))
}

func TestModelRowsMessageSortsRendersAndUsesAltScreen(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "zeta", "/w/kwt/zeta"),
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "alpha", "/w/kwt/alpha"),
	}})

	content := viewContent(model)
	assert.Contains(t, content, "kwt · 3 worktrees · 2 repos")
	assert.Contains(t, content, "/ search")
	assert.Contains(t, content, "L layout:default")
	assert.Contains(t, content, "? help")
	assert.Less(t, strings.Index(content, "kata"), strings.Index(content, "alpha"))
	assert.True(t, model.View().AltScreen)
}

func TestModelRendersBackendWarnings(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{
		rows:     []Row{testRow("kwt", "main", "/w/kwt/main")},
		warnings: []string{`multiple machines are publishing as host ID "same" (host same)`},
	})

	content := viewContent(model)
	assert.Contains(t, content, `warning: multiple machines are publishing as host ID "same" (host same)`)

	model, _ = updateModel(t, model, rowsMsg{
		rows: []Row{testRow("kwt", "main", "/w/kwt/main")},
	})
	assert.NotContains(t, viewContent(model), "warning:",
		"warnings must clear once the hub state is healthy again")
}

func TestRenderHelpTableReflowsToFitWidth(t *testing.T) {
	got := stripANSI(renderHelpTable(defaultHelpRows(Row{}), 34))
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")

	require.Greater(t, len(lines), 1)
	assert.Contains(t, got, "▕")
	assert.Contains(t, got, "P project")
	for _, line := range lines {
		assert.LessOrEqual(t, len([]rune(line)), 34, line)
	}
}

func TestModelFooterUsesAdaptiveHelpTable(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, tea.WindowSizeMsg{Width: 70, Height: 12})
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	content := stripANSI(viewContent(model))

	assert.Contains(t, content, "▕")
	assert.Contains(t, content, "P project")
	assert.NotContains(t, content, "K kill-ws  p project")
}

func TestModelSelectsCurrentWorktreeOnInitialRows(t *testing.T) {
	current := testRow("zzz-other", "main", "/repos/other")
	current.Status.IsCurrent = true
	model := NewModel(&fakeBackend{}, "/worktrees")

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		current,
	}})

	assert.Equal(t, "/repos/other", rowPath(model.selectedRow()))
}

func TestModelPolishesSingleRowDashboard(t *testing.T) {
	row := testRow("kwt", "test/layouts", "/worktrees/github.com/example/kwt/test-layouts")
	row.SessionLive = true
	model := NewModel(&fakeBackend{layoutNames: []string{"quad", "focus"}}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	content := viewContent(model)

	assert.Contains(t, content, "kwt · 1 worktree · 1 repo")
	assert.Contains(t, content, "WORKSPACE")
	assert.Contains(t, content, "live")
	assert.Contains(t, content, "\x1b[92mlive")
	assert.Contains(t, stripANSI(content), "layout default")
	assert.Contains(t, content, "selected kwt:test/layouts")
	assert.Contains(t, content, "/worktrees/github.com/example/kwt/test-layouts")
	assert.Contains(t, stripANSI(content), "c shell")
	assert.NotContains(t, stripANSI(content), "s shell")
	assert.NotContains(t, stripANSI(content), "s sync")
}

func TestModelRendersPrimaryAndDetachedDisplayLabels(t *testing.T) {
	primary := testRow("kwt", "main", "/w/kwt/main")
	primary.Entry.IsMain = true
	detached := testRow("kwt", "HEAD", "/w/kwt/detached")
	detached.Entry.CommitHash = "be094b1bdf4471ea60db4656f06d8fb2551ffd3d"

	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor(primary.Entry.Path)
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{primary, detached}})

	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "main [primary]")
	assert.Contains(t, content, "detached@be094b1b")
	assert.Contains(t, content, "selected kwt:main [primary]")
}

func TestModelRendersRemoteOnlyFleetRows(t *testing.T) {
	row := Row{Fleet: &FleetInfo{
		ProjectIdentity:  "github.com/example/kwt",
		ProjectName:      "kwt",
		Kind:             "branch",
		Ref:              "feature/studio-only",
		Branch:           "feature/studio-only",
		Local:            false,
		Hosts:            []string{"host-b"},
		Sync:             "same",
		Dirty:            "clean",
		Freshness:        "just now",
		MaterializeHost:  "host-b",
		RemotePath:       "/work/host-b/kwt/feature-studio-only",
		RemoteHead:       "bbb",
		RemoteUpstream:   "origin/feature/studio-only",
		RemoteAhead:      2,
		CanMaterialize:   true,
		MaterializeLabel: "feature/studio-only",
	}}
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	content := stripANSI(viewContent(model))

	assert.NotContains(t, content, "MACHINES")
	assert.Contains(t, content, "kwt")
	assert.Contains(t, content, "feature/studio-only")
	assert.Contains(t, content, "remote only")
	assert.NotContains(t, content, "hosts same")
	assert.Contains(t, content, "remote")
	assert.Contains(t, content, "s sync")
	assert.NotContains(t, content, "c shell")
	assert.NotContains(t, content, "m materialize")
	assert.Contains(t, content, "selected kwt:feature/studio-only")
	assert.Contains(t, content, "remote on host-b")
	assert.Contains(t, content, "press s to sync (branch must be pushed/fetched here)")
	assert.Contains(t, content, "source is 2 commits ahead of origin/feature/studio-only")
	assert.Contains(t, content, "/work/host-b/kwt/feature-studio-only")
}

func TestModelDashboardFitsHundredColumnTerminal(t *testing.T) {
	row := testRow("example-service", "very-long-feature-branch-that-needs-truncation", "/w/example-service/feature")
	row.SessionLive = true
	row.Status.LastActivity = timeNow().Add(-8 * time.Hour)
	row.Status.GitStatus.Modified = 3
	row.Fleet = &FleetInfo{
		ProjectName: "example-service",
		Local:       true,
		Hosts:       []string{"local", "host-b"},
		Sync:        "different: host-b 18h",
		Dirty:       "host-b (~3 ?3)",
		Freshness:   "18h",
	}
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, tea.WindowSizeMsg{Width: 100, Height: 12})
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	lines := strings.Split(stripANSI(viewContent(model)), "\n")
	header := findLineContaining(lines, "REPO")
	body := findLineContaining(lines, "diff host-b")

	require.NotEmpty(t, header)
	require.NotEmpty(t, body)
	assert.Contains(t, header, "WORKSPACE")
	assert.NotContains(t, header, "MACHINES")
	assert.Contains(t, body, "live")
	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "also on host-b")
	assert.Contains(t, content, "head differs 18h")
	assert.Contains(t, content, "remote changes ~3 ?3")
	assert.NotContains(t, content, "machines local")
	assert.LessOrEqual(t, visibleWidth(header), 100, header)
	assert.LessOrEqual(t, visibleWidth(body), 100, body)
}

func TestModelDashboardPreservesPrimaryMarkerWhenBranchTruncated(t *testing.T) {
	fullHash := "be094b1bdf4471ea60db4656f06d8fb2551ffd3d"
	tests := []struct {
		name  string
		width int
		row   Row
	}{
		{
			name:  "long branch",
			width: 100,
			row: func() Row {
				row := testRow("kwt", "feature/"+strings.Repeat("long-", 10), "/w/kwt/long")
				row.Entry.IsMain = true
				return row
			}(),
		},
		{
			name:  "detached in narrow dashboard",
			width: 55,
			row: func() Row {
				row := testRow("kwt", "HEAD", "/w/kwt/detached")
				row.Entry.CommitHash = fullHash
				row.Entry.IsMain = true
				return row
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(&fakeBackend{}, "/worktrees")
			model, _ = updateModel(t, model, tea.WindowSizeMsg{Width: tt.width, Height: 12})
			model, _ = updateModel(t, model, rowsMsg{rows: []Row{tt.row}})

			lines := strings.Split(stripANSI(viewContent(model)), "\n")
			headerIndex := lineIndexContaining(lines, "REPO")

			require.GreaterOrEqual(t, headerIndex, 0)
			require.Greater(t, len(lines), headerIndex+1)
			body := lines[headerIndex+1]
			assert.Contains(t, body, "[primary]")
			assert.LessOrEqual(t, visibleWidth(body), tt.width, body)
		})
	}
}

func TestModelSummarizesRemoteChangesInTableAndDetailsNameHost(t *testing.T) {
	row := testRow("kwt", "feature/remote-dirty", "/w/kwt/feature")
	row.Fleet = &FleetInfo{
		ProjectName: "kwt",
		Local:       true,
		Hosts:       []string{"local", "host-b"},
		Sync:        "same",
		Dirty:       "host-b (~3 ?3)",
		Freshness:   "5m",
	}
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	lines := strings.Split(stripANSI(viewContent(model)), "\n")
	body := findLineContaining(lines, "feature/remote-dirty")

	require.NotEmpty(t, body)
	assert.Contains(t, body, "remote ~3 ?3")
	assert.NotContains(t, body, "host-b")
	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "also on host-b")
	assert.Contains(t, content, "remote changes ~3 ?3")
	assert.NotContains(t, content, "changes host-b")
}

func TestModelCyclesLayoutSelection(t *testing.T) {
	model := NewModel(&fakeBackend{layoutNames: []string{"quad", "focus"}}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{testRow("kwt", "main", "/w/kwt/main")}})

	assert.Contains(t, stripANSI(viewContent(model)), "layout default")
	footerLine := lineIndexContaining(strings.Split(stripANSI(viewContent(model)), "\n"), "q quit")

	model, _ = updateModel(t, model, press("L"))
	assert.Equal(t, "quad", model.selectedLayout)
	assert.Contains(t, stripANSI(viewContent(model)), "selected kwt:main · layout quad · workspace offline")
	assert.Contains(t, viewContent(model), "layout \x1b")
	assert.Contains(t, viewContent(model), "/w/kwt/main")
	assert.Equal(t, footerLine, lineIndexContaining(strings.Split(stripANSI(viewContent(model)), "\n"), "q quit"))

	model, _ = updateModel(t, model, press("L"))
	assert.Equal(t, "focus", model.selectedLayout)
	assert.Contains(t, stripANSI(viewContent(model)), "selected kwt:main · layout focus · workspace offline")
	assert.Equal(t, footerLine, lineIndexContaining(strings.Split(stripANSI(viewContent(model)), "\n"), "q quit"))

	model, _ = updateModel(t, model, press("L"))
	assert.Empty(t, model.selectedLayout)
	assert.Contains(t, stripANSI(viewContent(model)), "selected kwt:main · layout default · workspace offline")
	assert.Equal(t, footerLine, lineIndexContaining(strings.Split(stripANSI(viewContent(model)), "\n"), "q quit"))
}

func TestModelRefreshesLayoutsAfterCurrentInventoryLoad(t *testing.T) {
	backend := &fakeBackend{layoutNames: []string{"quad"}}
	model := NewModel(backend, "/worktrees")
	model.selectedLayout = "quad"
	backend.layoutNames = []string{"focus"}

	model, _ = updateModel(t, model, rowsMsg{
		rows: []Row{testRow("kwt", "main", "/w/kwt/main")},
	})

	assert.Equal(t, []string{"focus"}, model.layouts)
	assert.Empty(t, model.selectedLayout)
}

func TestModelCursorFilterAndEscape(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kata", "feature", "/w/kata/feature"),
	}})

	model, _ = updateModel(t, model, press("j"))
	assert.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))

	model, _ = updateModel(t, model, press("/"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("w"))
	assert.Equal(t, "kw", model.filter)
	assert.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))
	assert.Contains(t, viewContent(model), "/ filter:")
	assert.Contains(t, viewContent(model), "kw")

	model, _ = updateModel(t, model, press("esc"))
	assert.Empty(t, model.filter)
	assert.Contains(t, viewContent(model), "/w/kwt/main")
}

func TestModelTextFilterAcceptsPaste(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kata", "feature", "/w/kata/feature"),
	}})

	model, _ = updateModel(t, model, press("/"))
	model, _ = updateModel(t, model, paste("kata"))

	assert.Equal(t, "kata", model.filter)
	assert.Equal(t, "/w/kata/feature", rowPath(model.selectedRow()))
	assert.Contains(t, stripANSI(viewContent(model)), "/ filter: kata")
}

func TestModelProjectFilterNarrowsRows(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "feature", "/w/kwt/feature"),
		testRow("kata", "main", "/w/kata/main"),
	}})

	model, _ = updateModel(t, model, press("p"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("a"))
	model, _ = updateModel(t, model, press("t"))

	assert.Equal(t, "kat", model.projectFilter)
	assert.Equal(t, "/w/kata/main", rowPath(model.selectedRow()))
	content := viewContent(model)
	assert.Contains(t, content, "p filter:")
	assert.Contains(t, content, "p filter:kat")
	assert.Contains(t, content, "kata")
	assert.NotContains(t, content, "kwt            feature")
}

func TestModelProjectFilterCombinesWithTextFilter(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature", "/w/kwt/feature"),
		testRow("kata", "feature", "/w/kata/feature"),
	}})

	model, _ = updateModel(t, model, press("p"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("w"))
	model, _ = updateModel(t, model, press("enter"))
	model, _ = updateModel(t, model, press("/"))
	model, _ = updateModel(t, model, press("f"))
	model, _ = updateModel(t, model, press("e"))
	model, _ = updateModel(t, model, press("a"))

	assert.Equal(t, "kw", model.projectFilter)
	assert.Equal(t, "fea", model.filter)
	assert.Equal(t, "/w/kwt/feature", rowPath(model.selectedRow()))
	content := viewContent(model)
	assert.Contains(t, content, "kwt            feature")
	assert.NotContains(t, content, "kwt            main")
	assert.NotContains(t, content, "kata           feature")
}

func TestModelEscapeClearsProjectFilterWhenTextFilterEmpty(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kata", "main", "/w/kata/main"),
	}})

	model, _ = updateModel(t, model, press("p"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("a"))
	model, _ = updateModel(t, model, press("enter"))
	require.Equal(t, "ka", model.projectFilter)

	model, _ = updateModel(t, model, press("esc"))

	assert.Empty(t, model.projectFilter)
	assert.Len(t, model.filteredRows(), 2)
}

func TestModelProjectPerspectiveSwitcherNarrowsRows(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("j"))
	require.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))

	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("a"))
	model, _ = updateModel(t, model, press("t"))
	model, _ = updateModel(t, model, press("enter"))

	assert.Equal(t, "kata", model.projectPerspective)
	assert.Equal(t, "/w/kata/main", rowPath(model.selectedRow()))
	content := viewContent(model)
	assert.Contains(t, content, "P project:kata")
	assert.Contains(t, content, "kata")
	assert.NotContains(t, content, "kwt            main")
}

func TestModelProjectPerspectivePickerAcceptsPaste(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, paste("kat"))
	model, _ = updateModel(t, model, press("enter"))

	assert.Equal(t, "kata", model.projectPerspective)
	assert.Equal(t, "/w/kata/main", rowPath(model.selectedRow()))
}

func TestModelProjectPerspectivePickerUsesArrowKeys(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, press("down"))
	model, _ = updateModel(t, model, press("down"))
	model, _ = updateModel(t, model, press("enter"))

	assert.Equal(t, "kwt", model.projectPerspective)
	assert.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))
}

func TestModelProjectPerspectivePickerRendersModalList(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, tea.WindowSizeMsg{Width: 80, Height: 12})
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "feature", "/w/kwt/feature"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	model, _ = updateModel(t, model, press("P"))

	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "Project")
	assert.Contains(t, content, "Search: Type to search")
	assert.Contains(t, content, "All (3)")
	assert.Contains(t, content, "kata (1)")
	assert.Contains(t, content, "kwt (2)")
	assert.Contains(t, content, "↑↓ select")
	assert.NotContains(t, content, "REPO           BRANCH")
}

func TestModelProjectPerspectivePickerDisambiguatesDuplicateRepoNames(t *testing.T) {
	left := testRow("service", "main", "/w/github.com/org-one/service/main")
	left.Entry.RepositoryInfo.Host = "github.com"
	left.Entry.RepositoryInfo.Owner = "org-one"
	left.Entry.RepositoryInfo.FullPath = "github.com/org-one/service"
	left.Status.Repository = "github.com/org-one/service"
	right := testRow("service", "main", "/w/github.com/org-two/service/main")
	right.Entry.RepositoryInfo.Host = "github.com"
	right.Entry.RepositoryInfo.Owner = "org-two"
	right.Entry.RepositoryInfo.FullPath = "github.com/org-two/service"
	right.Status.Repository = "github.com/org-two/service"

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{left, right}})
	model, _ = updateModel(t, model, press("P"))

	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "github.com/org-one/service (1)")
	assert.Contains(t, content, "github.com/org-two/service (1)")

	model, _ = updateModel(t, model, paste("org-two"))
	model, _ = updateModel(t, model, press("enter"))

	assert.Equal(t, "github.com/org-two/service", model.projectPerspective)
	assert.Equal(t, "/w/github.com/org-two/service/main", rowPath(model.selectedRow()))
	assert.Len(t, model.filteredRows(), 1)
}

func TestModelProjectPerspectivePickerShowsErrors(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, rowsMsg{err: errors.New("refresh exploded")})

	assert.Contains(t, stripANSI(viewContent(model)), "refresh exploded")
}

func TestModelProjectPerspectiveCancelKeepsCurrentProject(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model.projectPerspective = "kwt"

	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("a"))
	assert.Equal(t, len("ka"), model.input.Position())
	model, _ = updateModel(t, model, press("esc"))

	assert.Equal(t, "kwt", model.projectPerspective)
	assert.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))
}

func TestModelNewBranchUsesProjectPerspective(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kata/new-work"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("j"))
	require.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))

	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("a"))
	model, _ = updateModel(t, model, press("t"))
	model, _ = updateModel(t, model, press("enter"))
	model.projectFresh["kata"] = scopeFreshness{ObservedAt: time.Now(), Current: true}
	model.inventoryCurrent = true
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, press("f"))
	model, _ = updateModel(t, model, press("e"))
	model, _ = updateModel(t, model, press("a"))
	_, cmd := updateModel(t, model, press("enter"))

	require.NotNil(t, cmd)
	msg := cmd()
	assert.IsType(t, actionDoneMsg{}, msg)
	assert.Equal(t, []string{"/w/kata/main:fea"}, backend.createCalls)
}

func TestModelNewBranchAcceptsPaste(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kwt/pasted"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature/pasted"))
	_, cmd := updateModel(t, model, press("enter"))

	require.NotNil(t, cmd)
	_ = cmd()
	assert.Equal(t, []string{"/w/kwt/main:feature/pasted"}, backend.createCalls)
	assert.Equal(t, []string{""}, backend.createSources)
}

func TestModelBranchInputRejectsChangedTarget(t *testing.T) {
	t.Run("new branch after generation change", func(t *testing.T) {
		row := testRow("kwt", "main", "/w/kwt/main")
		row.Entry.RepositoryInfo.FullPath = "github.com/example/kwt"
		row.Entry.Generation = "11111111111111111111111111111111"
		backend := &fakeBackend{createPath: "/w/kwt/topic"}
		model := currentModelWithRows(t, backend, row)
		model.backgroundGlobalStarted = true
		model, _ = updateModel(t, model, press("n"))
		replacement := row
		entry := *replacement.Entry
		entry.Generation = "22222222222222222222222222222222"
		replacement.Entry = &entry
		model, _ = updateModel(t, model, inventoryMsg{
			request: InventoryRequest{
				Scope: InventoryCurrentRepository, ProjectIdentity: rowProjectKey(row),
			},
			result: InventoryResult{
				Rows: []Row{replacement}, ObservedAt: time.Now(), Current: true,
			},
		})
		model, _ = updateModel(t, model, paste("topic"))

		model, cmd := updateModel(t, model, press("enter"))

		assert.Nil(t, cmd)
		assert.Empty(t, backend.createCalls)
		assert.Contains(t, model.message, "changed")
	})

	t.Run("existing branch after project change", func(t *testing.T) {
		row := testRow("kwt", "main", "/w/kwt/main")
		row.Entry.RepositoryInfo.FullPath = "github.com/example/kwt"
		row.Entry.Generation = "11111111111111111111111111111111"
		backend := &fakeBackend{
			createPath: "/w/kwt/topic",
			branches:   []models.Branch{{Name: "topic", Source: "topic"}},
		}
		model := currentModelWithRows(t, backend, row)
		model, loadCmd := updateModel(t, model, press("b"))
		require.NotNil(t, loadCmd)
		model, _ = updateModel(t, model, loadCmd())
		replacement := row
		entry := *replacement.Entry
		info := *entry.RepositoryInfo
		info.FullPath = "github.com/example/replacement"
		entry.RepositoryInfo = &info
		replacement.Entry = &entry
		model, _ = updateModel(t, model, inventoryMsg{
			request: InventoryRequest{Scope: InventoryCurrentDashboard, CollectStatuses: true},
			result: InventoryResult{
				Rows: []Row{replacement}, ObservedAt: time.Now(), Current: true,
			},
		})

		model, cmd := updateModel(t, model, press("enter"))

		assert.Nil(t, cmd)
		assert.Empty(t, backend.createCalls)
		assert.Contains(t, model.message, "changed")
	})
}

func TestModelExistingBranchPickerCreatesSelectedRemote(t *testing.T) {
	backend := &fakeBackend{
		createPath: "/w/kwt/remote-ready",
		branches: []models.Branch{
			{Name: "local-ready", Source: "local-ready"},
			{
				Name:     "remote-ready",
				Source:   "refs/remotes/origin/remote-ready",
				IsRemote: true,
			},
		},
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	model, loadCmd := updateModel(t, model, press("b"))
	require.NotNil(t, loadCmd)
	model, _ = updateModel(t, model, loadCmd())

	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "local-ready")
	assert.Contains(t, content, "remote-ready")
	assert.Contains(t, content, "origin/remote-ready")
	assert.NotContains(t, content, "refs/remotes/")

	for _, value := range []string{"r", "e", "m", "o", "t", "e"} {
		model, _ = updateModel(t, model, press(value))
	}
	assert.Equal(t, len("remote"), model.input.Position())
	_, createCmd := updateModel(t, model, press("enter"))

	require.NotNil(t, createCmd)
	result := createCmd()
	assert.Contains(t, result.(actionDoneMsg).message, "review")
	assert.Equal(t, []string{"/w/kwt/main:remote-ready"}, backend.createCalls)
	assert.Equal(t, []string{"refs/remotes/origin/remote-ready"}, backend.createSources)
}

func TestModelExistingBranchPickerPreviewsDuplicateRemoteSource(t *testing.T) {
	backend := &fakeBackend{
		createPath: "/w/kwt/topic",
		branches: []models.Branch{
			{
				Name:     "topic",
				Label:    "topic (origin/topic)",
				Source:   "refs/remotes/origin/topic",
				IsRemote: true,
			},
			{
				Name:     "topic",
				Label:    "topic (upstream/topic)",
				Source:   "refs/remotes/upstream/topic",
				IsRemote: true,
			},
		},
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	model, loadCmd := updateModel(t, model, press("b"))
	require.NotNil(t, loadCmd)
	model, _ = updateModel(t, model, loadCmd())
	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "topic (origin/topic)")
	assert.Contains(t, content, "topic (upstream/topic)")

	model, _ = updateModel(t, model, paste("upstream"))
	model, createCmd := updateModel(t, model, press("enter"))

	require.NotNil(t, createCmd)
	require.NotNil(t, model.selectedRow().Entry)
	assert.Equal(t, "topic (upstream/topic)", model.selectedRow().Entry.Branch)
	_ = createCmd()
	assert.Equal(t, []string{"refs/remotes/upstream/topic"}, backend.createSources)
}

func TestModelExistingLocalBranchRequiresReviewBeforeAttaching(t *testing.T) {
	backend := &fakeBackend{
		createPath: "/w/kwt/local-ready",
		branches: []models.Branch{{
			Name:   "local-ready",
			Source: "local-ready",
		}},
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	model, loadCmd := updateModel(t, model, press("b"))
	require.NotNil(t, loadCmd)
	model, _ = updateModel(t, model, loadCmd())
	_, createCmd := updateModel(t, model, press("enter"))

	require.NotNil(t, createCmd)
	result := createCmd()
	assert.Contains(t, result.(actionDoneMsg).message, "review")
	assert.Equal(t, []string{"local-ready"}, backend.createSources)
}

func TestModelShowsNewWorktreeWhileCreateIsInFlight(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kwt/feature-fast"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-fast"))

	model, createCmd := updateModel(t, model, press("enter"))

	require.NotNil(t, createCmd)
	assert.Empty(t, backend.createCalls, "Git creation must remain asynchronous")
	assert.Equal(t, "/w/kwt/feature-fast", rowPath(model.selectedRow()))
	assert.Contains(t, stripANSI(viewContent(model)), "creating")

	_ = createCmd()
	assert.Equal(t, []string{"/w/kwt/main:feature-fast"}, backend.createCalls)
}

func TestModelRemovesOptimisticWorktreeWhenCreateFails(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kwt/feature-broken"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-broken"))
	model, createCmd := updateModel(t, model, press("enter"))
	backend.createErr = errors.New("creation failed")

	model, _ = updateModel(t, model, createCmd())

	assert.Len(t, model.rows, 1)
	assert.Equal(t, "/w/kwt/main", rowPath(model.rows[0]))
	assert.Contains(t, stripANSI(viewContent(model)), "creation failed")
}

func TestModelDropsFailedCreationFromInFlightFleetMerge(t *testing.T) {
	pendingPath := "/w/kwt/feature-broken"
	backend := &fakeBackend{createPath: pendingPath}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-broken"))
	model, createCmd := updateModel(t, model, press("enter"))
	require.NotNil(t, createCmd)

	// A load lands while the creation is still running, so the fleet merge it
	// dispatches snapshots the placeholder.
	model, fleetCmd := updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	require.NotNil(t, fleetCmd)
	staleFleetMsg := fleetCmd()

	backend.createErr = errors.New("creation failed")
	model, _ = updateModel(t, model, createCmd())
	require.Len(t, model.rows, 1, "the failed creation's row is removed")

	model, _ = updateModel(t, model, staleFleetMsg)

	assert.Len(t, model.rows, 1,
		"a fleet merge holding the failed placeholder must not restore it")
	_, restored := indexByPathOK(model.rows, pendingPath)
	assert.False(t, restored, "the removed placeholder would never clear its creating flag")
}

func TestModelKeepsCreatingWorktreeAcrossFleetRefresh(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kwt/feature-pending"}
	model := NewModel(backend, "/worktrees")
	model, fleetCmd := updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	require.NotNil(t, fleetCmd)

	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-pending"))
	model, _ = updateModel(t, model, press("enter"))
	require.Len(t, model.rows, 2)

	model, _ = updateModel(t, model, fleetCmd())

	require.Len(t, model.rows, 2)
	assert.Equal(t, "/w/kwt/feature-pending", rowPath(model.selectedRow()))
	assert.True(t, model.selectedRow().Creating)
}

func TestModelKeepsCreatedWorktreeUntilRefreshFindsIt(t *testing.T) {
	backend := &fakeBackend{
		rows:       []Row{testRow("kwt", "main", "/w/kwt/main")},
		createPath: "/w/kwt/feature-created",
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: backend.rows})

	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-created"))
	model, createCmd := updateModel(t, model, press("enter"))
	model, refreshCmd := updateModel(t, model, createCmd())
	require.NotNil(t, refreshCmd)

	model, _ = updateModel(t, model, refreshCmd())

	require.Len(t, model.rows, 2)
	assert.Equal(t, "/w/kwt/feature-created", rowPath(model.selectedRow()))
	assert.True(t, model.selectedRow().Creating,
		"the optimistic row stays pending until a fresh listing confirms it")
}

func TestModelKeepsCreatingFlagWhenDiscoveryFindsPendingWorktree(t *testing.T) {
	pendingPath := "/w/kwt/feature-discovered"
	backend := &fakeBackend{createPath: pendingPath}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-discovered"))
	model, createCmd := updateModel(t, model, press("enter"))
	require.NotNil(t, createCmd)

	// Git publishes the worktree before copy and setup commands finish, so a
	// listing can discover it while creation is still running.
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature-discovered", pendingPath),
	}})

	require.Equal(t, pendingPath, rowPath(model.selectedRow()))
	assert.True(t, model.selectedRow().Creating,
		"a discovered worktree stays pending until its creation command returns")

	model, _ = updateModel(t, model, press("enter"))
	assert.Equal(t, "worktree is still being created", model.message)
	assert.Empty(t, backend.openCalls)

	model, _ = updateModel(t, model, createCmd())
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature-discovered", pendingPath),
	}})
	assert.False(t, model.selectedRow().Creating,
		"the flag clears once the creation command returns")
}

func TestModelRejectsCreateAtAnExistingWorktreePath(t *testing.T) {
	existing := "/w/kwt/feature"
	backend := &fakeBackend{createPath: existing}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature", existing),
	}})

	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature"))
	model, createCmd := updateModel(t, model, press("enter"))

	assert.Nil(t, createCmd, "git would reject a worktree at an occupied path")
	assert.Contains(t, model.message, "worktree already exists at")
	require.Len(t, model.rows, 2, "no placeholder may share a path with an existing row")
	index, ok := indexByPathOK(model.rows, existing)
	require.True(t, ok)
	assert.False(t, model.rows[index].Creating,
		"the existing worktree must not be marked pending")
}

func TestModelRejectsSecondCreateAtAPendingPath(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kwt/feature-pending"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kata", "main", "/w/kata/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-pending"))
	model, _ = updateModel(t, model, press("enter"))
	require.Len(t, model.rows, 3)

	// The same destination reached from a different base repository while the
	// first creation is still in flight.
	model, _ = updateModel(t, model, press("G"))
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-pending"))
	model, secondCmd := updateModel(t, model, press("enter"))

	assert.Nil(t, secondCmd)
	assert.Contains(t, model.message, "is already being created")
	assert.Len(t, model.rows, 3, "the in-flight placeholder must not be duplicated")
	assert.Len(t, model.creating, 1)
}

// symlinkedWorktreeBase returns a base directory and a symlink to it, modelling
// a worktree base the user reaches through a link: PreparePath keeps the link
// spelling while Git reports the resolved one.
func symlinkedWorktreeBase(t *testing.T) (real string, link string) {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	link = filepath.Join(t.TempDir(), "base")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	return real, link
}

func TestModelMatchesPendingCreationAcrossSymlinkedPaths(t *testing.T) {
	realBase, linkBase := symlinkedWorktreeBase(t)
	previewPath := filepath.Join(linkBase, "feature")
	discoveredPath := filepath.Join(realBase, "feature")

	backend := &fakeBackend{createPath: previewPath}
	model := NewModel(backend, realBase)
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature"))
	model, createCmd := updateModel(t, model, press("enter"))
	require.NotNil(t, createCmd)
	require.Len(t, model.rows, 2)

	// Git publishes the worktree and the next listing reports its resolved path
	// while copy and setup commands are still running.
	require.NoError(t, os.MkdirAll(discoveredPath, 0o755))
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature", discoveredPath),
	}})

	require.Len(t, model.rows, 2,
		"the placeholder and the worktree it stands for must not both be listed")
	index, ok := indexByPathOK(model.rows, discoveredPath)
	require.True(t, ok)
	assert.True(t, model.rows[index].Creating,
		"the discovered worktree stays pending until its creation command returns")
}

func TestModelRejectsCreateAtASymlinkEquivalentPath(t *testing.T) {
	realBase, linkBase := symlinkedWorktreeBase(t)
	existingPath := filepath.Join(realBase, "feature")
	require.NoError(t, os.MkdirAll(existingPath, 0o755))

	backend := &fakeBackend{createPath: filepath.Join(linkBase, "feature")}
	model := NewModel(backend, realBase)
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature", existingPath),
	}})

	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature"))
	model, createCmd := updateModel(t, model, press("enter"))

	assert.Nil(t, createCmd, "the destination is occupied however it is spelled")
	assert.Contains(t, model.message, "worktree already exists at")
	assert.Len(t, model.rows, 2)
}

func TestModelRejectsDeleteWhileWorktreeIsBeingCreated(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kwt/feature-pending"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-pending"))
	model, _ = updateModel(t, model, press("enter"))
	require.True(t, model.selectedRow().Creating)

	model, _ = updateModel(t, model, press("d"))

	assert.Equal(t, confirmNone, model.confirm.kind,
		"removing a worktree mid-creation would race git worktree add and setup")
	assert.Equal(t, "worktree is still being created", model.message)
	assert.Empty(t, backend.removeCalls)
}

func TestModelRejectsNewBranchWhileWorktreeIsBeingCreated(t *testing.T) {
	backend := &fakeBackend{createPath: "/w/kwt/feature-pending"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature-pending"))
	model, _ = updateModel(t, model, press("enter"))
	require.True(t, model.selectedRow().Creating)

	model, _ = updateModel(t, model, press("n"))

	assert.Equal(t, inputNone, model.inputMode,
		"the pending directory has no repository to branch from yet")
	assert.Equal(t, "worktree is still being created", model.message)
}

func TestModelMaterializeRemoteOnlyFleetRow(t *testing.T) {
	row := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/studio-only",
		Branch:          "feature/studio-only",
		Hosts:           []string{"host-b"},
		CanMaterialize:  true,
	}}
	backend := &fakeBackend{materializePath: "/worktrees/github.com/example/kwt/feature-studio-only"}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})
	model.projectPerspective = rowProjectKey(row)

	model, cmd := updateModel(t, model, press("s"))

	require.NotNil(t, cmd)
	assert.Contains(t, model.message, "syncing kwt:feature/studio-only")
	msg := cmd()
	assert.IsType(t, actionDoneMsg{}, msg)
	assert.Equal(t, []string{"github.com/example/kwt:feature/studio-only"}, backend.materializeRows)
	done := msg.(actionDoneMsg)
	assert.True(t, done.refresh)
	assert.Equal(t, "/worktrees/github.com/example/kwt/feature-studio-only", done.anchorPath)
	assert.Contains(t, done.message, "synced kwt:feature/studio-only")

	model, refresh := updateModel(t, model, done)
	require.NotNil(t, refresh)
	assert.Equal(t, InventoryCurrentDashboard, model.fetchingRequest.Scope)
	assert.True(t, model.fetchingRequest.CollectStatuses)
	assert.Equal(t, "github.com/example/kwt", model.projectPerspective)
}

func TestModelCancelNewBranchInputKeepsExistingFilter(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kata", "feature", "/w/kata/feature"),
	}})
	model, _ = updateModel(t, model, press("/"))
	model, _ = updateModel(t, model, press("k"))
	model, _ = updateModel(t, model, press("w"))
	model, _ = updateModel(t, model, press("enter"))
	require.Equal(t, "kw", model.filter)

	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, press("esc"))

	assert.Equal(t, "kw", model.filter)
	assert.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))
}

func TestModelDeleteRefusesPrimaryCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, "code", "kwt")
	row := testRow("kwt", "main", path)
	row.Entry.IsMain = true
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, cmd := updateModel(t, model, press("d"))

	require.Nil(t, cmd)
	expectedPath := filepath.Join("~", "code", "kwt")
	assert.Contains(t, viewContent(model), "cannot delete the primary checkout: "+expectedPath)
	assert.Empty(t, backend.removeCalls)
}

func TestModelDeleteLiveWorktreeConfirmsAndCallsRemove(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	row.SessionLive = true
	row.SessionName = "kwt-workspace-kwt-feature"
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, _ = updateModel(t, model, press("d"))
	assert.Contains(t, viewContent(model), "delete kwt:feature and stop any matching workspace sessions? [y/N]")

	_, cmd := updateModel(t, model, press("y"))
	require.NotNil(t, cmd)
	msg := cmd()
	assert.IsType(t, removalDoneMsg{}, msg)
	assert.Equal(t, []string{"/w/kwt/feature"}, backend.removeCalls)
	assert.Equal(t, []bool{false}, backend.removeForces)
}

func TestModelDeleteOfflineWorktreeDisclosesMatchingSessionCleanup(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	row.SessionLive = false
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, _ = updateModel(t, model, press("d"))

	assert.Contains(t, viewContent(model), "delete kwt:feature and stop any matching workspace sessions? [y/N]")
}

func TestModelRefreshesAfterPartialRemovalWarning(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	backend := &fakeBackend{
		rows: []Row{row},
		removeErr: removedWithResidualFilesError{
			errors.New("worktree removed, but files remain"),
		},
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: backend.rows})
	done, ok := model.removeWorktreeCmd(removalJob{row: row, key: removalKey(row)})().(removalDoneMsg)
	require.True(t, ok)

	require.Error(t, done.err)
	assert.True(t, done.refresh)
	updated, refreshCmd := updateModel(t, model, done)
	require.NotNil(t, refreshCmd)
	assert.ErrorContains(t, updated.err, "files remain")
	assert.True(t, updated.fetching)

	backend.rows = nil
	updated, _ = updateModel(t, updated, refreshCmd())
	assert.Empty(t, updated.rows)
	assert.ErrorContains(t, updated.err, "files remain")

	updated, _ = updateModel(t, updated, press("j"))
	assert.NoError(t, updated.err)
}

func TestModelRefreshesAfterIndeterminateRemoval(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	backend := &fakeBackend{
		rows: []Row{row},
		removeErr: refreshRequiredActionError{
			errors.New("worktree removal outcome is indeterminate"),
		},
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: backend.rows})
	done, ok := model.removeWorktreeCmd(removalJob{row: row, key: removalKey(row)})().(removalDoneMsg)
	require.True(t, ok)

	require.Error(t, done.err)
	assert.True(t, done.refresh)
}

func TestModelDeleteDirtyWorktreeConfirmsDiscardAndForcesRemove(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	row.Status.Status = models.WorktreeStatusModified
	row.Status.GitStatus.Modified = 1
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, _ = updateModel(t, model, press("d"))
	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "discard changes, delete kwt:feature, and stop any matching workspace sessions? [y/N]")
	assert.NotContains(t, content, "kwt remove --force")

	_, cmd := updateModel(t, model, press("y"))
	require.NotNil(t, cmd)
	msg := cmd()
	assert.IsType(t, removalDoneMsg{}, msg)
	assert.Equal(t, []string{"/w/kwt/feature"}, backend.removeCalls)
	assert.Equal(t, []bool{true}, backend.removeForces)
}

func TestModelDeleteDirtyLiveWorktreeDisclosesMatchingSessionCleanup(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	row.SessionLive = true
	row.SessionName = "kwt-workspace-kwt-feature"
	row.Status.Status = models.WorktreeStatusModified
	row.Status.GitStatus.Untracked = 1
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, _ = updateModel(t, model, press("d"))

	assert.Contains(t, stripANSI(viewContent(model)), "discard changes, delete kwt:feature, and stop any matching workspace sessions? [y/N]")
}

func TestModelKillWorkspaceConfirm(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	row.SessionLive = true
	row.SessionName = "kwt-workspace-kwt-feature"
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, _ = updateModel(t, model, press("K"))
	assert.Contains(t, viewContent(model), "kill workspace for kwt:feature? [y/N]")

	_, cmd := updateModel(t, model, press("y"))
	require.NotNil(t, cmd)
	_ = cmd()
	assert.Equal(t, []string{"kwt-workspace-kwt-feature"}, backend.killCalls)
}

func TestKillConfirmationRejectsChangedOrStaleRow(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, Model, Row) Model
	}{
		{
			name: "project became stale",
			change: func(_ *testing.T, model Model, row Row) Model {
				project := projectFreshnessKey(rowProjectKey(row))
				freshness := model.projectFresh[project]
				freshness.Current = false
				model.projectFresh[project] = freshness
				return model
			},
		},
		{
			name: "worktree generation changed",
			change: func(t *testing.T, model Model, row Row) Model {
				entry := *row.Entry
				entry.Generation = "22222222222222222222222222222222"
				row.Entry = &entry
				model.backgroundGlobalStarted = true
				model, _ = updateModel(t, model, inventoryMsg{
					request: InventoryRequest{
						Scope: InventoryCurrentRepository, ProjectIdentity: rowProjectKey(row),
					},
					result: InventoryResult{
						Rows: []Row{row}, ObservedAt: time.Now(), Current: true,
					},
				})
				return model
			},
		},
		{
			name: "worktree checkout changed",
			change: func(t *testing.T, model Model, row Row) Model {
				entry := *row.Entry
				entry.Branch = "replacement"
				row.Entry = &entry
				model.backgroundGlobalStarted = true
				model, _ = updateModel(t, model, inventoryMsg{
					request: InventoryRequest{
						Scope: InventoryCurrentRepository, ProjectIdentity: rowProjectKey(row),
					},
					result: InventoryResult{
						Rows: []Row{row}, ObservedAt: time.Now(), Current: true,
					},
				})
				return model
			},
		},
		{
			name: "worktree head changed",
			change: func(t *testing.T, model Model, row Row) Model {
				entry := *row.Entry
				entry.CommitHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				row.Entry = &entry
				model.backgroundGlobalStarted = true
				model, _ = updateModel(t, model, inventoryMsg{
					request: InventoryRequest{
						Scope: InventoryCurrentRepository, ProjectIdentity: rowProjectKey(row),
					},
					result: InventoryResult{
						Rows: []Row{row}, ObservedAt: time.Now(), Current: true,
					},
				})
				return model
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := testRow("kwt", "feature", "/w/kwt/feature")
			row.Entry.Generation = "11111111111111111111111111111111"
			row.Entry.CommitHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			row.SessionLive = true
			row.SessionName = "kwt-workspace-kwt-feature"
			backend := &fakeBackend{}
			model := currentModelWithRows(t, backend, row)
			model, _ = model.startKill()
			model = tt.change(t, model, row)

			model, cmd := updateModel(t, model, press("y"))

			assert.Nil(t, cmd)
			assert.Empty(t, backend.killCalls)
			assert.Equal(t, confirmNone, model.confirm.kind)
			assert.Contains(t, model.message, "changed")
		})
	}
}

func TestModelShellAndAttachHandoffsQuitFirst(t *testing.T) {
	oldValidate := validateShellDirectory
	validateShellDirectory = func(string) error { return nil }
	t.Cleanup(func() { validateShellDirectory = oldValidate })
	row := testRow("kwt", "feature", "/w/kwt/feature")
	model := NewModel(&fakeBackend{layoutNames: []string{"quad", "focus"}}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, _ = updateModel(t, model, press("c"))
	assert.Equal(t, HandoffShell, model.Handoff().Kind)
	assert.Equal(t, "/w/kwt/feature", rowPath(model.Handoff().Row))

	model = NewModel(&fakeBackend{layoutNames: []string{"quad", "focus"}}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})
	model, _ = updateModel(t, model, press("L"))
	model, _ = updateModel(t, model, press("L"))
	model, _ = updateModel(t, model, press("enter"))
	assert.Equal(t, HandoffAttach, model.Handoff().Kind)
	assert.Equal(t, "/w/kwt/feature", rowPath(model.Handoff().Row))
	assert.Equal(t, "focus", model.Handoff().LayoutName)
}

func TestModelSyncKeyDoesNotOpenShellForLocalRow(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, cmd := updateModel(t, model, press("s"))

	require.Nil(t, cmd)
	assert.Equal(t, HandoffNone, model.Handoff().Kind)
	assert.Contains(t, stripANSI(viewContent(model)), "nothing to sync for this row")
}

func TestModelInsideTmuxAttachRunsResidentAction(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	backend := &fakeBackend{insideTmux: true, layoutNames: []string{"quad"}}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, _ = updateModel(t, model, press("L"))
	model, cmd := updateModel(t, model, press("enter"))
	require.NotNil(t, cmd)
	msg := cmd()
	ready, ok := msg.(openInTmuxReadyMsg)
	require.True(t, ok)
	assert.Nil(t, ready.process)
	assert.Equal(t, []string{"/w/kwt/feature:quad"}, backend.openCalls)
	assert.Equal(t, HandoffNone, model.Handoff().Kind)
}

func TestModelCrossServerAttachUsesBubbleTeaManagedProcess(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	process := exec.Command("tmux", "attach-session", "-t", "workspace")
	backend := &fakeBackend{insideTmux: true, openProcess: process}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, openCmd := updateModel(t, model, press("enter"))
	require.NotNil(t, openCmd)
	ready, ok := openCmd().(openInTmuxReadyMsg)
	require.True(t, ok)
	assert.Same(t, process, ready.process)

	_, managedCmd := updateModel(t, model, ready)
	require.NotNil(t, managedCmd)
	assert.Equal(t, "tea.execMsg", fmt.Sprintf("%T", managedCmd()))
}

func TestModelActionErrorDisplaysAndClearsOnNextKey(t *testing.T) {
	row := testRow("kwt", "feature", "/w/kwt/feature")
	row.SessionLive = true
	backend := &fakeBackend{killErr: errors.New("tmux exploded")}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})
	model, _ = updateModel(t, model, press("K"))
	model, cmd := updateModel(t, model, press("y"))

	require.NotNil(t, cmd)
	model, _ = updateModel(t, model, cmd())
	assert.Contains(t, viewContent(model), "tmux exploded")

	model, _ = updateModel(t, model, press("j"))
	assert.NotContains(t, viewContent(model), "tmux exploded")
}

func TestModelQueuesActionRefreshWhileFetchInFlight(t *testing.T) {
	backend := &fakeBackend{
		rows: []Row{testRow("kwt", "feature", "/w/kwt/feature")},
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: backend.rows})

	model, refreshCmd := updateModel(t, model, press("r"))
	require.NotNil(t, refreshCmd)
	require.True(t, model.fetching)

	model, actionCmd := updateModel(t, model, actionDoneMsg{
		message: "removed kwt:feature",
		refresh: true,
	})
	require.Nil(t, actionCmd)

	backend.rows = []Row{testRow("kwt", "main", "/w/kwt/main")}
	model, queuedRefreshCmd := updateModel(t, model, refreshCmd())

	require.NotNil(t, queuedRefreshCmd)
	assert.True(t, model.fetching)
	before := backend.listCalls
	msg := queuedRefreshCmd()
	assert.Equal(t, before+1, backend.listCalls)
	assert.IsType(t, inventoryMsg{}, msg)
}

func TestFastRefreshKeepsEnrichmentWhenFullLoadFails(t *testing.T) {
	enriched := testRow("kwt", "feature", "/w/kwt/feature")
	enriched.Status.GitStatus.Modified = 3
	enriched.Status.LastActivity = time.Now()
	enriched.Fleet = &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		Kind:            "branch",
		Ref:             "feature",
		Branch:          "feature",
		Hosts:           []string{"host-b"},
	}
	remoteOnly := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/remote",
		Branch:          "feature/remote",
		CanMaterialize:  true,
	}}
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{enriched}})
	model, _ = updateModel(t, model, fleetRowsMsg{seq: model.loadSeq, rows: []Row{enriched, remoteOnly}})
	require.Len(t, model.rows, 2)

	// A refresh repaints from the fast load, which reports the worktree with no
	// status and no hub state at all.
	bare := testRow("kwt", "feature", "/w/kwt/feature")
	bare.Status.GitStatus = models.GitStatus{}
	bare.Status.LastActivity = time.Time{}
	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{bare}})
	model, _ = updateModel(t, model, rowsMsg{err: errors.New("status collection failed")})

	require.Len(t, model.rows, 2, "the remote-only row must outlive a failed full load")
	index, ok := indexByPathOK(model.rows, "/w/kwt/feature")
	require.True(t, ok)
	assert.Equal(t, 3, model.rows[index].Status.GitStatus.Modified,
		"the last collected status is better than none while the load is broken")
	assert.NotNil(t, model.rows[index].Fleet, "fleet metadata must survive the fast repaint")
	assert.Contains(t, stripANSI(viewContent(model)), "status collection failed")
}

func TestFastRefreshKeepsRemoteRowsWithNoLocalWorktrees(t *testing.T) {
	remoteOnly := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/remote",
		Branch:          "feature/remote",
		CanMaterialize:  true,
	}}
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{remoteOnly}})
	require.Len(t, model.rows, 1)

	// Nothing is checked out on this machine, so the fast load reports no rows.
	model, _ = updateModel(t, model, fastRowsMsg{rows: nil})
	require.Len(t, model.rows, 1,
		"an empty local snapshot says nothing about worktrees on other hosts")

	// The full load then fails, so no fleet merge follows to put them back.
	model, _ = updateModel(t, model, rowsMsg{err: errors.New("discovery failed")})

	require.Len(t, model.rows, 1)
	assert.True(t, isRemoteOnly(model.rows[0]))
	assert.Equal(t, "feature/remote", model.rows[0].Fleet.Branch)
}

func TestFastRefreshKeepsRemotePrimaryLabelAfterLocalWorktreeDisappears(t *testing.T) {
	local := testRow("kwt", "main", "/w/kwt/main-linked")
	local.Entry.RepositoryInfo.FullPath = "github.com/example/kwt"
	local.Fleet = &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "main",
		Branch:          "main",
		Local:           true,
		Hosts:           []string{"host-b", "local"},
		AllPrimary:      true,
	}

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{local}})

	model, _ = updateModel(t, model, fastRowsMsg{rows: nil})

	require.Len(t, model.rows, 1)
	assert.True(t, isRemoteOnly(model.rows[0]))
	assert.Equal(t, []string{"host-b"}, model.rows[0].Fleet.Hosts)
	assert.True(t, model.rows[0].Fleet.AllPrimary)
	assert.Equal(t, "main [primary]", rowDisplayBranch(model.rows[0]))
}

func TestFastRefreshDropsRemoteRowThatBecameLocal(t *testing.T) {
	remoteOnly := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/remote",
		Branch:          "feature/remote",
		CanMaterialize:  true,
	}}
	local := testRow("kwt", "feature/remote", "/w/kwt/feature-remote")
	local.Entry.RepositoryInfo.FullPath = "github.com/example/kwt"

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{remoteOnly}})
	require.Len(t, model.rows, 1)

	// Materializing it makes the next fast load report it as a local worktree.
	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{local}})

	require.Len(t, model.rows, 1,
		"a materialized worktree must not appear as both a local and a remote row")
	assert.NotNil(t, model.rows[0].Entry)
}

// A hub manifest and a local clone can spell one forge identity differently;
// only the branch half of the key is case-sensitive.
func TestFastRefreshDropsRemoteRowDifferingOnlyInIdentityCase(t *testing.T) {
	remoteOnly := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/Example/KWT",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/remote",
		Branch:          "feature/remote",
		CanMaterialize:  true,
	}}
	local := testRow("kwt", "feature/remote", "/w/kwt/feature-remote")
	local.Entry.RepositoryInfo.FullPath = "github.com/example/kwt"

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{remoteOnly}})
	require.Len(t, model.rows, 1)

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{local}})

	require.Len(t, model.rows, 1,
		"owner and repository casing must not split one worktree into two rows")
	assert.NotNil(t, model.rows[0].Entry)
}

// A detached worktree is keyed by commit, not by the literal branch "HEAD",
// so its local and remote-only shapes have to agree.
func TestFastRefreshDropsDetachedRemoteRowThatBecameLocal(t *testing.T) {
	const commit = "0f1e2d3c4b5a69788796a5b4c3d2e1f0a1b2c3d4"
	remoteOnly := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "detached",
		Ref:             commit,
	}}
	local := testRow("kwt", "HEAD", "/w/kwt/detached")
	local.Entry.RepositoryInfo.FullPath = "github.com/example/kwt"
	local.Entry.CommitHash = commit

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{remoteOnly}})
	require.Len(t, model.rows, 1)

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{local}})

	require.Len(t, model.rows, 1,
		"a detached worktree must not appear as both a local and a remote row")
	assert.NotNil(t, model.rows[0].Entry)
}

func TestFastRefreshDropsEnrichmentWhenTheWorktreeSwitchedBranches(t *testing.T) {
	before := testRow("kwt", "feature", "/w/kwt/feature")
	before.Status.GitStatus.Modified = 4
	before.Fleet = &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		Kind:            "branch",
		Ref:             "feature",
		Branch:          "feature",
		Hosts:           []string{"host-b"},
	}
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{before}})

	// git switch inside the worktree: same path, different branch.
	switched := testRow("kwt", "other", "/w/kwt/feature")
	switched.Status.GitStatus = models.GitStatus{}
	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{switched}})

	require.Len(t, model.rows, 1)
	assert.Zero(t, model.rows[0].Status.GitStatus.Modified,
		"another branch's uncommitted changes must not be attributed to this one")
	assert.Nil(t, model.rows[0].Fleet,
		"hub state belongs to the branch it was collected for")
}

func TestModelRemovesFailedCreationDiscoveredAtItsResolvedPath(t *testing.T) {
	realBase, linkBase := symlinkedWorktreeBase(t)
	previewPath := filepath.Join(linkBase, "feature")
	discoveredPath := filepath.Join(realBase, "feature")

	backend := &fakeBackend{createPath: previewPath}
	model := NewModel(backend, realBase)
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	model, _ = updateModel(t, model, press("n"))
	model, _ = updateModel(t, model, paste("feature"))
	model, createCmd := updateModel(t, model, press("enter"))
	require.NotNil(t, createCmd)

	// Git published the worktree, so discovery replaces the placeholder's link
	// spelling with the resolved one before setup fails and rolls it back.
	require.NoError(t, os.MkdirAll(discoveredPath, 0o755))
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kwt", "feature", discoveredPath),
	}})
	require.Len(t, model.rows, 2)

	backend.createErr = errors.New("setup command failed")
	model, _ = updateModel(t, model, createCmd())

	assert.Len(t, model.rows, 1,
		"the rolled-back worktree must not survive under its resolved spelling")
	_, ok := indexByPathOK(model.rows, discoveredPath)
	assert.False(t, ok, "a row left here would stay marked creating forever")
}

func TestFastRefreshProjectsDeletedLocalWorktreeStillHeldElsewhere(t *testing.T) {
	shared := testRow("kwt", "feature", "/w/kwt/feature")
	shared.Fleet = &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature",
		Branch:          "feature",
		Hosts:           []string{"local", "host-b"},
		Local:           true,
	}
	other := testRow("kata", "main", "/w/kata/main")

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{shared, other}})
	require.Len(t, model.rows, 2)

	// The worktree is deleted locally but still exists on host-b, and the full
	// load that would refresh hub state then fails.
	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{other}})
	model, _ = updateModel(t, model, rowsMsg{err: errors.New("discovery failed")})

	require.Len(t, model.rows, 2,
		"deleting the local copy must not hide the hosts that still have it")
	index, ok := indexByPathOK(model.rows, "")
	require.True(t, ok, "the surviving row is remote-only, so it has no local path")
	projected := model.rows[index]
	assert.Nil(t, projected.Entry, "no local entry survives a worktree that is gone")
	assert.Nil(t, projected.Status)
	require.NotNil(t, projected.Fleet)
	assert.Equal(t, []string{"host-b"}, projected.Fleet.Hosts)
	assert.False(t, projected.Fleet.Local)
	assert.False(t, projected.Fleet.CanMaterialize,
		"syncing from a reading this stale needs a fresh merge to confirm it first")
}

func TestFastRefreshDropsDeletedWorktreeHeldNowhereElse(t *testing.T) {
	onlyLocal := testRow("kwt", "feature", "/w/kwt/feature")
	onlyLocal.Fleet = &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		Kind:            "branch",
		Ref:             "feature",
		Branch:          "feature",
		Hosts:           []string{"local"},
		Local:           true,
	}
	other := testRow("kata", "main", "/w/kata/main")

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{onlyLocal, other}})

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{other}})

	assert.Len(t, model.rows, 1,
		"a worktree no host holds is simply gone, not remote")
}

func TestFastRefreshKeepsRemoteRowOnCaseSensitiveHost(t *testing.T) {
	remoteOnly := Row{Fleet: &FleetInfo{
		ProjectIdentity: "git.example.com/srv/KWT",
		ProjectName:     "KWT",
		Kind:            "branch",
		Ref:             "feature/remote",
		Branch:          "feature/remote",
		CanMaterialize:  true,
	}}
	local := testRow("kwt", "feature/remote", "/w/kwt/feature-remote")
	local.Entry.RepositoryInfo.FullPath = "git.example.com/srv/kwt"

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{remoteOnly}})

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{local}})

	assert.Len(t, model.rows, 2,
		"an unrecognized host may distinguish these repositories by case")
}

func TestFastRefreshKeepsRemoteRowWithDifferentBranchCase(t *testing.T) {
	remoteOnly := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "Feature/Remote",
		Branch:          "Feature/Remote",
		CanMaterialize:  true,
	}}
	local := testRow("kwt", "feature/remote", "/w/kwt/feature-remote")
	local.Entry.RepositoryInfo.FullPath = "github.com/example/kwt"

	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{remoteOnly}})

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{local}})

	assert.Len(t, model.rows, 2,
		"branch names are case-sensitive, so these are two different worktrees")
}

func TestModelKeepsQueuedActionRefreshWhenFastLoadFails(t *testing.T) {
	backend := &fakeBackend{
		rows: []Row{testRow("kwt", "feature", "/w/kwt/feature")},
	}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: backend.rows})

	model, refreshCmd := updateModel(t, model, press("r"))
	require.NotNil(t, refreshCmd)
	require.True(t, model.fetching)

	model, actionCmd := updateModel(t, model, actionDoneMsg{
		message: "removed kwt:feature",
		refresh: true,
	})
	require.Nil(t, actionCmd)

	// Nothing else retries the queued refresh, so losing it here would leave the
	// removed worktree on screen until the user refreshes by hand.
	model, queuedRefreshCmd := updateModel(t, model, inventoryMsg{
		request: InventoryRequest{Scope: InventoryCurrentDashboard, CollectStatuses: true},
		seq:     model.inventorySeq,
		err:     errors.New("discovery failed"),
	})

	require.NotNil(t, queuedRefreshCmd, "a failed fast load must still run the queued refresh")
	assert.False(t, model.pendingRefresh, "the queued refresh is consumed, not re-queued")
	before := backend.listCalls
	msg := queuedRefreshCmd()
	assert.Equal(t, before+1, backend.listCalls)
	assert.IsType(t, inventoryMsg{}, msg)
}

func TestModelPreservesCreateAnchorAcrossQueuedRefresh(t *testing.T) {
	staleRows := []Row{testRow("kwt", "feature", "/w/kwt/feature")}
	newPath := "/w/kwt/new-feature"
	freshRows := []Row{
		testRow("kwt", "feature", "/w/kwt/feature"),
		testRow("kwt", "new-feature", newPath),
	}
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: staleRows})
	model, _ = updateModel(t, model, press("r"))
	model, _ = updateModel(t, model, actionDoneMsg{
		message:    "created new-feature",
		refresh:    true,
		anchorPath: newPath,
	})

	model, queuedRefreshCmd := updateModel(t, model, rowsMsg{rows: staleRows})
	require.NotNil(t, queuedRefreshCmd)
	require.Equal(t, newPath, model.anchorPath)

	model, _ = updateModel(t, model, rowsMsg{rows: freshRows})

	assert.Equal(t, newPath, rowPath(model.selectedRow()))
	assert.Empty(t, model.anchorPath)
}

func TestModelScrollsRowsWithinTerminalHeight(t *testing.T) {
	rows := make([]Row, 0, 12)
	for i := range 12 {
		branch := fmt.Sprintf("branch-%02d", i)
		rows = append(rows, testRow("kwt", branch, "/w/kwt/"+branch))
	}
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, tea.WindowSizeMsg{Width: 100, Height: 8})
	model, _ = updateModel(t, model, rowsMsg{rows: rows})
	model, _ = updateModel(t, model, press("G"))

	content := stripANSI(viewContent(model))
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")

	assert.LessOrEqual(t, len(lines), model.height)
	assert.Contains(t, content, "branch-11")
	assert.NotContains(t, content, "branch-00")
	assert.Contains(t, content, "q quit")
}

func TestModelPathAnchorsCursorAfterRefresh(t *testing.T) {
	model := NewModel(&fakeBackend{}, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kwt", "feature", "/w/kwt/feature"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})
	require.Equal(t, "/w/kwt/feature", rowPath(model.selectedRow()))

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{
		testRow("kata", "main", "/w/kata/main"),
		testRow("kwt", "feature", "/w/kwt/feature"),
		testRow("kwt", "main", "/w/kwt/main"),
	}})

	assert.Equal(t, "/w/kwt/feature", rowPath(model.selectedRow()))
	assert.Equal(t, 1, model.cursor)
}

func TestInitialAnchorSelectsLaunchWorkspaceRow(t *testing.T) {
	active := testRow("kwt", "main", "/w/kwt/main")
	active.Status.LastActivity = time.Now()
	workspace := Row{Workspace: &WorkspaceInfo{Name: "code", Path: "/Users/me/code"}}
	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor("/Users/me/code")

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{active, workspace}})

	require.NotNil(t, model.selectedRow().Workspace,
		"first load must select the launch directory's workspace row despite activity sorting")
	assert.Equal(t, "/Users/me/code", model.selectedRow().Workspace.Path)
	assert.Equal(t, "code", model.projectPerspective,
		"launching from a workspace directory must scope the view to it")

	// The anchor is consumed: after widening the view, a refresh keeps the
	// cursor by path instead of snapping back, and moving works normally.
	model, _ = updateModel(t, model, press("esc"))
	model, _ = updateModel(t, model, press("g"))
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{active, workspace}})
	require.NotNil(t, model.selectedRow().Entry, "anchor must not re-apply on later refreshes")
}

func TestInitialAnchorAppliesLaunchProjectPerspective(t *testing.T) {
	launch := testRow("kwt", "main", "/w/kwt/main")
	sibling := testRow("kwt", "feature", "/w/kwt/feature")
	other := testRow("kata", "main", "/w/kata/main")
	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor("/w/kwt/main")

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{launch, sibling, other}})

	rows := model.filteredRows()
	require.Len(t, rows, 2, "first load must show only the launch repo's worktrees")
	for _, row := range rows {
		assert.Equal(t, "kwt", rowRepoName(row))
	}
	assert.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()))

	// Escape widens the view back to every project.
	model, _ = updateModel(t, model, press("esc"))
	assert.Empty(t, model.projectPerspective)
	assert.Len(t, model.filteredRows(), 3)
	assert.Equal(t, "/w/kwt/main", rowPath(model.selectedRow()),
		"clearing the perspective must keep the selection")
}

func TestLaunchProjectPerspectiveFallsBackToCurrentRow(t *testing.T) {
	other := testRow("kwt", "main", "/w/kwt/main")
	current := testRow("kata", "feature", "/w/kata/feature")
	current.Status.IsCurrent = true
	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor("/w/kata/feature/sub/dir")

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{other, current}})

	rows := model.filteredRows()
	require.Len(t, rows, 1, "launching from inside a worktree must filter to its repo")
	assert.Equal(t, "/w/kata/feature", rowPath(rows[0]))
}

// The fast load carries no collected statuses, so IsCurrent is never set on its
// rows. Containment has to come from the row paths or launching from a
// subdirectory loses both the perspective and the selection.
func TestFastLoadResolvesAnchorInsideWorktreeWithoutStatuses(t *testing.T) {
	other := testRow("kwt", "main", "/w/kwt/main")
	other.Status.LastActivity = time.Now()
	launch := testRow("kata", "feature", "/w/kata/feature")
	launch.Status.IsCurrent = false
	backend := &fakeBackend{rows: []Row{other, launch}}
	model := NewModel(backend, "/worktrees").WithInitialAnchor("/w/kata/feature/sub/dir")

	model, _ = updateModel(t, model, fastRowsMsg{rows: backend.rows})

	rows := model.filteredRows()
	require.Len(t, rows, 1, "the fast load must scope the view to the launch repo")
	assert.Equal(t, "/w/kata/feature", rowPath(rows[0]))
	assert.Equal(t, "/w/kata/feature", rowPath(model.selectedRow()))

	// The full load that follows keeps the resolved selection.
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{other, launch}})
	assert.Equal(t, "/w/kata/feature", rowPath(model.selectedRow()))
}

func TestAnchorPrefersDeepestContainingWorktree(t *testing.T) {
	// The containing repository sorts first, so only depth-preferring
	// containment can select the nested worktree.
	repo := testRow("kwt", "main", "/w/kwt/main")
	repo.Status.LastActivity = time.Now()
	nested := testRow("dep", "main", "/w/kwt/main/vendor/dep")
	model := NewModel(&fakeBackend{}, "/worktrees").
		WithInitialAnchor("/w/kwt/main/vendor/dep/internal")

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{repo, nested}})

	assert.Equal(t, "/w/kwt/main/vendor/dep", rowPath(model.selectedRow()),
		"a nested worktree must win over the repository that contains it")
}

// The launch directory is symlink-resolved before it becomes the anchor, but
// discovered row paths are not, so the two spellings of one directory only
// match under a canonicalizing comparison.
func TestAnchorResolvesWorktreeReachedThroughSymlink(t *testing.T) {
	realDir := t.TempDir()
	worktreeDir := filepath.Join(realDir, "kata-feature")
	launchDir := filepath.Join(worktreeDir, "internal")
	require.NoError(t, os.MkdirAll(launchDir, 0o755))

	linkRoot := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkRoot); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	resolvedLaunch, err := filepath.EvalSymlinks(launchDir)
	require.NoError(t, err)

	other := testRow("kwt", "main", "/w/kwt/main")
	other.Status.LastActivity = time.Now()
	// The row path reaches the same directory through the symlink.
	launch := testRow("kata", "feature", filepath.Join(linkRoot, "kata-feature"))

	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor(resolvedLaunch)
	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{other, launch}})

	assert.Equal(t, rowPath(launch), rowPath(model.selectedRow()),
		"a symlinked ancestor must not hide the worktree containing the launch directory")
}

func TestAnchorIgnoresSiblingPathPrefix(t *testing.T) {
	sibling := testRow("kwt", "feat", "/w/kwt/feat")
	unrelated := testRow("kata", "main", "/w/kata/main")
	model := NewModel(&fakeBackend{}, "/worktrees").
		WithInitialAnchor("/w/kwt/feat-two/sub")

	model, _ = updateModel(t, model, fastRowsMsg{rows: []Row{sibling, unrelated}})

	assert.Empty(t, model.projectPerspective,
		"/w/kwt/feat must not be treated as containing /w/kwt/feat-two/sub")
	assert.Len(t, model.filteredRows(), 2)
}

func TestNoLaunchProjectPerspectiveOutsideAnyRepo(t *testing.T) {
	rows := []Row{
		testRow("kwt", "main", "/w/kwt/main"),
		testRow("kata", "main", "/w/kata/main"),
	}
	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor("/Users/me")

	model, _ = updateModel(t, model, rowsMsg{rows: rows})

	assert.Empty(t, model.projectPerspective)
	assert.Len(t, model.filteredRows(), 2,
		"launching outside any repo must show everything")
}

func TestEscapeClearsSearchFilterBeforeProjectPerspective(t *testing.T) {
	launch := testRow("kwt", "main", "/w/kwt/main")
	other := testRow("kata", "main", "/w/kata/main")
	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor("/w/kwt/main")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{launch, other}})

	model, _ = updateModel(t, model, press("/"))
	model, _ = updateModel(t, model, press("m"))
	model, _ = updateModel(t, model, press("enter"))
	require.Equal(t, "m", model.filter)

	model, _ = updateModel(t, model, press("esc"))
	assert.Empty(t, model.filter, "first escape clears the search filter")
	assert.NotEmpty(t, model.projectPerspective, "perspective survives the first escape")

	model, refreshCmd := updateModel(t, model, press("esc"))
	assert.Empty(t, model.projectPerspective, "second escape clears the perspective")
	require.NotNil(t, refreshCmd)
	assert.Equal(t, InventoryCurrentDashboard, model.fetchingRequest.Scope)
	assert.True(t, model.fetchingRequest.CollectStatuses)
}

func TestRowsLoadMergesFleetAsynchronously(t *testing.T) {
	local := testRow("kwt", "main", "/w/kwt/main")
	remote := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/kenn-io/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/remote",
		Branch:          "feature/remote",
	}}
	backend := &fakeBackend{fleetRows: []Row{remote}, fleetWarnings: []string{"hub warning"}}
	model := NewModel(backend, "/worktrees")

	model, cmd := updateModel(t, model, rowsMsg{rows: []Row{local}})

	// Local rows render immediately, before any fleet work.
	assert.Len(t, model.rows, 1)
	assert.False(t, model.fetching)
	require.NotNil(t, cmd, "applying local rows must dispatch the fleet merge command")

	msg := cmd()
	fleetMsg, ok := msg.(fleetRowsMsg)
	require.True(t, ok, "fleet merge command must produce a fleetRowsMsg, got %T", msg)
	assert.Equal(t, 1, backend.mergeFleetCalls)

	model, _ = updateModel(t, model, fleetMsg)
	assert.Len(t, model.rows, 2, "fleet-only rows must appear after the merge")
	assert.Equal(t, []string{"hub warning"}, model.warnings)
}

func TestRefreshCancelsInFlightFleetMerge(t *testing.T) {
	local := testRow("kwt", "main", "/w/kwt/main")
	backend := &fakeBackend{rows: []Row{local}}
	model := NewModel(backend, "/worktrees")

	model, fleetCmd := updateModel(t, model, rowsMsg{rows: []Row{local}})
	require.NotNil(t, fleetCmd)

	// Refresh before the merge runs: the merge's context must be cancelled so
	// the backend can abandon hub work instead of blocking the new load.
	model, _ = updateModel(t, model, press("r"))
	fleetCmd()

	require.NotNil(t, backend.mergeCtx)
	assert.ErrorIs(t, backend.mergeCtx.Err(), context.Canceled)
	_ = model
}

func TestStaleFleetMergeIsDropped(t *testing.T) {
	local := testRow("kwt", "main", "/w/kwt/main")
	remote := Row{Fleet: &FleetInfo{
		ProjectIdentity: "github.com/kenn-io/kwt",
		Kind:            "branch",
		Ref:             "feature/remote",
	}}
	backend := &fakeBackend{rows: []Row{local}, fleetRows: []Row{remote}}
	model := NewModel(backend, "/worktrees")

	model, cmd := updateModel(t, model, rowsMsg{rows: []Row{local}})
	require.NotNil(t, cmd)
	staleFleetMsg := cmd()

	// A refresh dispatched before the fleet merge lands supersedes it.
	model, _ = updateModel(t, model, press("r"))
	model, _ = updateModel(t, model, staleFleetMsg)

	assert.Len(t, model.rows, 1, "a stale fleet merge must not clobber a newer refresh")
}

func TestFleetMergePreservesCursorSelection(t *testing.T) {
	first := testRow("kwt", "feature", "/w/kwt/feature")
	second := testRow("kwt", "main", "/w/kwt/main")
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")

	model, cmd := updateModel(t, model, rowsMsg{rows: []Row{first, second}})
	model, _ = updateModel(t, model, press("G"))
	selectedPath := rowPath(model.selectedRow())

	require.NotNil(t, cmd)
	model, _ = updateModel(t, model, cmd())

	assert.Equal(t, selectedPath, rowPath(model.selectedRow()),
		"the fleet merge must keep the user's selection")
}

func TestInitialAnchorMissFallsBackToCurrentRow(t *testing.T) {
	other := testRow("kwt", "main", "/w/kwt/main")
	other.Status.LastActivity = time.Now()
	current := testRow("kata", "feature", "/w/kata/feature")
	current.Status.IsCurrent = true
	model := NewModel(&fakeBackend{}, "/worktrees").WithInitialAnchor("/nowhere/unmatched")

	model, _ = updateModel(t, model, rowsMsg{rows: []Row{other, current}})

	require.NotNil(t, model.selectedRow().Entry)
	assert.Equal(t, "/w/kata/feature", model.selectedRow().Entry.Path,
		"an unmatched initial anchor must fall back to the current worktree row")
}
