# Asynchronous TUI Removal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users continue navigating the TUI while confirmed worktree removals run through one first-in, first-out background queue.

**Architecture:** The Bubble Tea model owns queue order and optimistic `removing…` row state, with at most one removal command active. The command backend prepares an immutable removal operation under its configuration lock, releases the lock, and performs daemon mutation, fleet publication, and session cleanup outside it.

**Tech Stack:** Go, Bubble Tea v2 commands/messages, existing daemon removal API, lifecycle generation guards, and tmux endpoint cleanup.

## Global Constraints

- Implement this plan after `2026-08-22-scoped-tui-inventory.md`.
- Keep one active TUI removal; do not add concurrent removal workers.
- Keep daemon/Git project locking authoritative across processes and agents.
- Preserve generation, repository identity, protected-session, ownership, and indeterminate-outcome checks.
- Do not kill a tmux endpoint unless worktree removal is known to have happened.
- Do not claim removal after an unobservable daemon outcome.
- Navigation, search, safe shell, and verified live attach remain available for other rows.
- Reject any action that targets a queued or actively removing row.
- Invoke `kenn:using-go` and `kenn:test-scope-discipline` before Go changes/tests, and `kenn:commit` before every commit.
- Follow test-first development and keep path-anchored selection.

---

## File map

- Modify `internal/tui/backend.go`: add `Row.Removing` only; removal backend signature stays synchronous.
- Modify `internal/tui/list.go` and `internal/tui/list_test.go`: render `removing…` status.
- Modify `internal/tui/model.go`: FIFO queue, messages, optimistic rows, duplicate/action gates, and scoped reconciliation.
- Modify `internal/tui/model_test.go`: controlled removal worker and responsive-interaction tests.
- Modify `internal/cmd/tui_backend.go`: prepare immutable removal operation under lock, run it outside lock.
- Modify `internal/cmd/tui_test.go`: lock-release and cleanup safety tests.
- Modify `docs/workflows/agent-workspaces.md`: queue behavior and exceptional failures.

### Task 1: Render explicit removal state

**Files:**

- Modify: `internal/tui/backend.go`
- Modify: `internal/tui/list.go`
- Modify: `internal/tui/list_test.go`
- Modify: `internal/tui/model.go` detail rendering only

**Interfaces:**

- Produces: `Row.Removing bool`.
- Produces: visible status label `removing…` with precedence over Git status and activity.

- [ ] **Step 1: Write failing row-rendering tests**

```go
func TestRemovingRowUsesOperationStatus(t *testing.T) {
	row := testRow("widget", "topic", "/work/topic")
	row.Removing = true
	row.Status = &models.WorktreeStatus{Status: models.WorktreeStatusModified}

	assert.Equal(t, "removing…", formatRowChanges(row))
	model := NewModel(&fakeBackend{}, "/work/topic")
	model.rows = []Row{row}
	text := stripANSI(viewContent(model))
	assert.Contains(t, text, "removing…")
	assert.NotContains(t, text, "modified")
}
```

Add a model detail test expecting `removing widget:topic` and its abbreviated
path for the selected row.

- [ ] **Step 2: Run rendering tests and observe the missing field**

Run: `go test ./internal/tui -run 'RemovingRow|Removing.*Detail' -count=1`

Expected: FAIL because `Row.Removing` does not exist.

- [ ] **Step 3: Add removal state with explicit precedence**

Add `Removing bool` next to `Creating bool` on `Row`. In every status/detail
switch, check `Removing` before `Creating` and normal status:

```go
if row.Removing {
	return "removing…"
}
if row.Creating {
	return "creating…"
}
```

Do not treat a removing row as absent until the worker reports known success.

- [ ] **Step 4: Run list and model rendering tests**

Run: `go test ./internal/tui -run 'Removing|Creating|Status' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit removal presentation**

Run `kenn:commit`, then commit row/list/model rendering files with subject:

```text
Show worktrees being removed
```

### Task 2: Queue one removal at a time in the model

**Files:**

- Modify: `internal/tui/model.go`
- Modify: `internal/tui/model_test.go`

**Interfaces:**

- Produces: `removalJob{row Row, force bool, key string}`.
- Produces: `removalDoneMsg{job removalJob, err error, removed bool, refresh bool}`.
- Adds model fields `removalQueue []removalJob` and `removalActive *removalJob`.

- [ ] **Step 1: Write failing FIFO and responsive-interaction tests**

Extend `fakeBackend` with a controlled removal function:

```go
removeWorktree func(context.Context, Row, bool) error

func (b *fakeBackend) RemoveWorktree(ctx context.Context, row Row, force bool) error {
	b.removeCalls = append(b.removeCalls, rowPath(row))
	b.removeForces = append(b.removeForces, force)
	if b.removeWorktree != nil {
		return b.removeWorktree(ctx, row, force)
	}
	return b.removeErr
}
```

Then add:

```go
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
	assert.Nil(t, secondCmd, "the second job waits in the model queue")
	assert.True(t, model.rows[0].Removing)
	assert.True(t, model.rows[1].Removing)

	firstMessages := make(chan tea.Msg, 1)
	go func() { firstMessages <- firstCmd() }()
	assert.Equal(t, "/work/one", <-started)

	before := model.cursor
	model, _ = updateModel(t, model, press("j"))
	assert.NotEqual(t, before, model.cursor, "navigation remains responsive")
	select {
	case got := <-started:
		t.Fatalf("second removal started early: %s", got)
	default:
	}

	release <- struct{}{}
	firstDone := <-firstMessages
	model, nextCmd := updateModel(t, model, firstDone)
	require.NotNil(t, nextCmd)
	go func() { firstMessages <- nextCmd() }()
	assert.Equal(t, "/work/two", <-started)
	release <- struct{}{}
	model, _ = updateModel(t, model, <-firstMessages)
	assert.Nil(t, model.removalActive)
	assert.Empty(t, model.removalQueue)
}

func TestModelRemovalFailureRestoresRowAndContinuesQueue(t *testing.T) {
	backend := &fakeBackend{}
	model := currentModelWithRows(t, backend,
		testRow("widget", "one", "/work/one"),
		testRow("widget", "two", "/work/two"),
	)
	first := removalJob{row: model.rows[0], key: removalKey(model.rows[0])}
	second := removalJob{row: model.rows[1], key: removalKey(model.rows[1])}
	model.rows[0].Removing = true
	model.rows[1].Removing = true
	model.removalActive = &first
	model.removalQueue = []removalJob{second}

	model, next := updateModel(t, model, removalDoneMsg{job: first, err: errors.New("remove failed")})
	assert.False(t, rowByPath(model.rows, "/work/one").Removing)
	require.NotNil(t, next)
	assert.ErrorContains(t, model.err, "remove failed")
}
```

Add these helpers beside the tests; `testRow` already exists in the package:

```go
func currentModelWithRows(t *testing.T, backend *fakeBackend, rows ...Row) Model {
	t.Helper()
	model := NewModel(backend, rowPath(rows[0]))
	model.rows = append([]Row(nil), rows...)
	model.projectFresh = map[string]scopeFreshness{}
	for _, row := range rows {
		model.projectFresh[rowProjectKey(row)] = scopeFreshness{ObservedAt: time.Now(), Current: true}
	}
	return model
}

func confirmDeleteRow(t *testing.T, model Model, index int) (Model, tea.Cmd) {
	t.Helper()
	model.cursor = index
	model, _ = updateModel(t, model, press("d"))
	return updateModel(t, model, press("y"))
}

func rowByPath(rows []Row, path string) Row {
	for _, row := range rows {
		if rowPath(row) == path { return row }
	}
	return Row{}
}
```

Add tests that duplicate confirmation is rejected, shell/open/delete on the
same removing row is rejected, and shell/open on a different safe row still
works.

- [ ] **Step 2: Run queue tests and observe immediate parallel commands**

Run: `go test ./internal/tui -run 'QueuesRemoval|RemovalFailure|RemovingRow.*Rejected' -count=1`

Expected: FAIL because confirmation returns an independent removal command each
time and rows have no queue identity.

- [ ] **Step 3: Implement queue state and messages**

Add:

```go
type removalJob struct {
	row   Row
	force bool
	key   string
}

type removalDoneMsg struct {
	job     removalJob
	err     error
	removed bool
	refresh bool
}
```

Use `pathIdentity(rowPath(row)) + "\x00" + row.Entry.Generation` as the key.
On confirmed deletion, reject an existing key; otherwise mark the matching row
`Removing`, append the job, and call:

```go
func (m Model) startNextRemoval() (Model, tea.Cmd) {
	if m.removalActive != nil || len(m.removalQueue) == 0 {
		return m, nil
	}
	job := m.removalQueue[0]
	m.removalQueue = m.removalQueue[1:]
	m.removalActive = &job
	return m, m.removeWorktreeCmd(job)
}
```

The command returns:

```go
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
```

When handling completion, clear `removalActive`. On known success, drop the
local row immediately; on failure, clear only its `Removing` flag. Preserve
typed error/sticky-refresh behavior. Start the next job even after failure.
Also start a scoped refresh when `refresh` is true; use `tea.Batch` when both a
next removal and refresh command exist. Re-anchor selection by path after row
drop/reorder.

Reject all actions on `row.Removing`. Keep navigation, filtering, and actions
on other rows available.

- [ ] **Step 4: Run the complete model suite with race detection**

Run: `go test ./internal/tui -race -count=1`

Expected: PASS with no race report.

- [ ] **Step 5: Commit the FIFO model queue**

Run `kenn:commit`, then commit model and tests with subject:

```text
Queue TUI worktree removals
```

### Task 3: Release the backend lock before slow removal work

**Files:**

- Modify: `internal/cmd/tui_backend.go`
- Modify: `internal/cmd/tui_test.go`

**Interfaces:**

- Produces: immutable `tuiRemovalOperation` prepared under `b.mu`.
- Keeps: `Backend.RemoveWorktree(context.Context, Row, bool) error` synchronous for one job.

- [ ] **Step 1: Write a failing lock-release test**

```go
func TestTUIBackendRemovalDoesNotBlockInventoryConfiguration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repository := newTUITestRepo(t)
	worktree := filepath.Join(t.TempDir(), "topic")
	runTUITestGit(t, repository, "branch", "topic")
	runTUITestGit(t, repository, "worktree", "add", worktree, "topic")
	generation := tuiTestWorktreeGeneration(t, repository, worktree)
	backend := newTUIBackendWithLaunchDir(&models.Config{}, repository)
	backend.liveEndpoints = func(context.Context, tmux.WorkspaceEndpointRequest) ([]tmux.SessionEndpoint, error) { return nil, nil }
	entered := make(chan struct{})
	release := make(chan struct{})
	backend.removeWorktree = func(context.Context, kwt.RemovalRequest) (kwt.RemovalResult, error) {
		close(entered)
		<-release
		return kwt.RemovalResult{WorktreeRemoved: true}, nil
	}
	done := make(chan error, 1)
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Path: worktree, Branch: "topic", Generation: generation,
	}}
	go func() { done <- backend.RemoveWorktree(context.Background(), row, false) }()
	<-entered

	applied := make(chan error, 1)
	go func() { applied <- backend.applyInventoryConfig(&models.Config{}) }()
	select {
	case err := <-applied:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("inventory configuration blocked behind worktree removal")
	}

	close(release)
	require.NoError(t, <-done)
}
```

Add tests proving an indeterminate failure does not run endpoint cleanup and a
known removal with cleanup failure still returns a
`removedWorktreeCleanupError`.

- [ ] **Step 2: Run backend removal tests and observe lock contention**

Run: `go test ./internal/cmd -run 'RemovalDoesNotBlock|RemoveWorktree.*Cleanup' -count=1`

Expected: FAIL by the one-second timeout because `RemoveWorktree` defers
`b.mu.Unlock()` until every slow step completes.

- [ ] **Step 3: Prepare immutable operation state under lock**

Add:

```go
type tuiRemovalOperation struct {
	request               kwt.RemovalRequest
	row                   dashboard.Row
	endpointRequest       tmux.WorkspaceEndpointRequest
	config                models.Config
	protectedNames        []string
	remove                func(context.Context, kwt.RemovalRequest) (kwt.RemovalResult, error)
	liveEndpoints         func(context.Context, tmux.WorkspaceEndpointRequest) ([]tmux.SessionEndpoint, error)
	cleanupEndpoint       func(context.Context, tmux.SessionEndpoint, tmux.WorkspaceEndpointRequest) error
	killProtectedEndpoint func(context.Context, tmux.SessionEndpoint, tmux.WorkspaceEndpointRequest, []string) error
}
```

Refactor `RemoveWorktree` to call `prepareRemoval` while holding `b.mu`, unlock,
then call `operation.run(ctx)`. `prepareRemoval` validates main/generation,
resolves the authoritative repository root, copies `*b.cfg`, clones
`protectedNames`, captures function values, and builds the endpoint request.
No captured closure may read mutable `b` fields after unlock.

Move publication, live endpoint discovery, protected/direct cleanup, and error
joining into `tuiRemovalOperation.run`. Preserve this order:

1. daemon removal;
2. if outcome is unobservable and not known removed, publish for reconciliation
   only when existing typed logic requires it, then return without cleanup;
3. publish known mutation;
4. resolve matching endpoints;
5. clean each endpoint;
6. return residual cleanup error marked as worktree removed.

Change protected cleanup to an injected function that receives the captured
protected-name slice; it must not read `b.protectedNames` after unlock.

- [ ] **Step 4: Run removal tests under the race detector**

Run: `go test ./internal/cmd -run 'TUIBackend.*Remove|RemovalDoesNotBlock' -race -count=1`

Expected: PASS with no timeout or race report.

- [ ] **Step 5: Commit backend lock narrowing**

Run `kenn:commit`, then commit backend and tests with subject:

```text
Release TUI inventory during removal
```

### Task 4: Reconcile queued completion with scoped refresh

**Files:**

- Modify: `internal/tui/model.go`
- Modify: `internal/tui/model_test.go`

**Interfaces:**

- Consumes: per-project freshness and `InventoryRequest` from plan two.
- Produces: project-scoped refresh after each known or indeterminate removal outcome.

- [ ] **Step 1: Write failing scoped-reconciliation tests**

```go
func TestRemovalCompletionRefreshesOnlyAffectedProject(t *testing.T) {
	backend := &fakeBackend{}
	row := testRow("widget", "topic", "/work/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := currentModelWithRows(t, backend, row)
	job := removalJob{row: row, key: removalKey(row)}
	model.removalActive = &job

	model, cmd := updateModel(t, model, removalDoneMsg{job: job, removed: true, refresh: true})
	require.NotNil(t, cmd)
	_ = cmd()
	last := backend.inventoryCalls[len(backend.inventoryCalls)-1]
	assert.Equal(t, InventoryCurrentRepository, last.Scope)
	assert.Equal(t, "github.com/acme/widget", last.ProjectIdentity)
}

func TestIndeterminateRemovalKeepsRowUntilScopedRefresh(t *testing.T) {
	row := testRow("widget", "topic", "/work/widget/topic")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := currentModelWithRows(t, &fakeBackend{}, row)
	job := removalJob{row: row, key: removalKey(row)}
	model.removalActive = &job

	model, _ = updateModel(t, model, removalDoneMsg{
		job: job, err: refreshRequiredActionError{errors.New("outcome unknown")}, refresh: true,
	})

	assert.NotNil(t, rowByPath(model.rows, rowPath(row)).Entry)
	assert.False(t, rowByPath(model.rows, rowPath(row)).Removing)
	assert.ErrorContains(t, model.err, "outcome unknown")
}
```

- [ ] **Step 2: Run reconciliation tests and observe global refresh/drop behavior**

Run: `go test ./internal/tui -run 'RemovalCompletionRefreshes|IndeterminateRemovalKeeps' -count=1`

Expected: FAIL until removal completion uses the affected project's explicit
inventory request and distinguishes known removal from unknown outcome.

- [ ] **Step 3: Bind refresh to the queued row's project identity**

Store the project's stable identity and working directory on `removalJob` when
enqueuing:

```go
type removalJob struct {
	row              Row
	force            bool
	key              string
	projectIdentity  string
	workingDirectory string
}
```

Choose `workingDirectory` from the main-worktree row for the same project when
it is present, falling back to the candidate path only when no main row is
available. A non-main deletion therefore retains a live repository anchor for
the reconciliation request.

After completion with `refresh:true`, request
`InventoryRequest{Scope: InventoryCurrentRepository, ProjectIdentity:
job.projectIdentity, WorkingDirectory: job.workingDirectory,
CollectStatuses:true}` even if the user
has switched perspectives. Merge the result into only that project and do not
steal cursor focus from the currently displayed project. If the repository
path disappeared, preserve the global cached catalog and attach the refresh
diagnostic to the affected project.

- [ ] **Step 4: Run full TUI verification**

Run: `go test ./internal/tui ./internal/cmd -race -count=1`

Expected: PASS with no race report.

- [ ] **Step 5: Commit scoped removal reconciliation**

Run `kenn:commit`, then commit model/tests with subject:

```text
Reconcile removals by project
```

### Task 5: Document, verify, and close the issue

**Files:**

- Modify: `docs/workflows/agent-workspaces.md`

**Interfaces:**

- Documents: FIFO queue, `removing…`, responsive actions, and failure restoration.

- [ ] **Step 1: Update workflow documentation**

Add:

```text
Confirmed deletions enter one background queue. The row immediately shows
removing… while navigation and safe actions on other rows remain available.
KWT processes removals in confirmation order. A failed removal restores the
row and reports the error; later queued removals still run.
```

- [ ] **Step 2: Run final repository verification**

Run: `make fmt && make test && make build && make docs-check`

Expected: all commands PASS.

- [ ] **Step 3: Review the complete three-plan implementation diff**

Run: `git diff --check && git diff --stat && git status --short`

Expected: only accepted maintenance/TUI code, tests, and maintained docs are
present; `git diff --check` prints nothing.

- [ ] **Step 4: Commit documentation**

Run `kenn:commit`, then commit maintained docs with subject:

```text
Document queued worktree removal
```

- [ ] **Step 5: Close the kata issue with fresh evidence**

After all three plans are implemented and the final verification remains
green, run:

```text
kata close wbcw --done --message "Made maintenance progress visible, scoped TUI refresh and status to displayed projects, kept safe cached actions available, and queued removals without blocking navigation." --commit "$(git rev-parse HEAD)" --evidence "tests:make test" --evidence "build:make build" --evidence "docs:make docs-check"
```
