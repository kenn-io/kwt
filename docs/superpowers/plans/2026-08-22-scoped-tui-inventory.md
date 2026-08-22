# Scoped TUI Inventory Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the TUI responsive by refreshing and collecting Git status only for the displayed project while preserving a daemon-owned global cache.

**Architecture:** Replace the backend's fast/full boolean with an explicit inventory request whose currency and status decisions are independent. The model merges validated repository-scoped rows over cached dashboard rows, tracks freshness by scope, and performs one status-free global cache refresh in the background.

**Tech Stack:** Go, Bubble Tea v2, lifecycle inventory v2, existing tmux workspace resolver, and Git porcelain v2.

## Global Constraints

- Reuse `ViewRepository` plus `WorkingDirectory`; do not add a daemon protocol field or capability version.
- The daemon remains the only persistent dashboard-cache owner.
- Repository inventory queries are non-interactive and surface `InteractionRequired` in the TUI.
- Reject empty, global-fallback, deleted-path, and repository-identity-mismatched scoped results.
- Merge scoped rows over cached rows; preserve dashboard-only repository URL and catalog fields.
- Collect status only for the displayed repository, except when all projects are explicitly displayed.
- Use `git status --porcelain=v2 --branch -uall`; absent `branch.ab` means `0/0`.
- Activity is the maximum of HEAD commit time, worktree-root mtime, and changed/untracked-file mtimes.
- Use a bounded worker pool and degrade ordinary failures per row.
- Keep command-line Git; do not introduce `go-git`.
- Invoke `kenn:using-go`, `kenn:test-scope-discipline`, and `kenn:isolate-prod` before Go tests that create repositories or invoke Git. Invoke `kenn:commit` before every commit.
- Follow test-first development and preserve path-anchored selection.

---

## File map

- Modify `internal/status/status_collector.go`: porcelain-v2 parser, bounded pool, diagnostics, and activity.
- Modify `internal/status/status_collector_test.go`: real-repository and bounded-concurrency tests.
- Modify `internal/cmd/status.go` and `internal/fleet/manifest.go`: consume the structured collector result.
- Modify `internal/tui/backend.go`: explicit inventory request and live-only attach methods.
- Modify `internal/cmd/tui_backend.go`: cached/project/global modes, scope validation, field preservation, and live endpoint resolution.
- Modify `internal/cmd/tui_daemon_inventory_test.go`: daemon request and merge contracts.
- Modify `internal/tui/model.go`: per-scope freshness, project refresh, background global refresh, and safe cached actions.
- Modify `internal/tui/model_test.go`: model sequencing, gates, selection, and shell tests.
- Modify `internal/cmd/tui.go` and `internal/cmd/tui_test.go`: outside-tmux live-only handoff.
- Modify `docs/workflows/agent-workspaces.md` and TUI help text if needed.

### Task 1: Parse Git porcelain v2 in one status command

**Files:**

- Modify: `internal/status/status_collector.go`
- Modify: `internal/status/status_collector_test.go`

**Interfaces:**

- Produces: `collectPorcelain(ctx, g) (porcelainStatus, error)`.
- Produces: `porcelainStatus{GitStatus models.GitStatus, Paths []string, Head string, Upstream string}`.
- Replaces: v1 status, untracked listing, branch/upstream lookup, and two `rev-list --count` calls.

- [ ] **Step 1: Write failing parser and real-Git tests**

```go
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
```

Add this helper in `status_collector_test.go` so all new real-Git tests are
self-contained:

```go
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
```

- [ ] **Step 2: Run parser tests and confirm missing symbols**

Run: `go test ./internal/status -run 'PorcelainV2|UAll' -count=1`

Expected: FAIL because `porcelainStatus`, `parsePorcelainV2`, and
`collectPorcelain` do not exist.

- [ ] **Step 3: Implement one porcelain command and parser**

Add:

```go
type porcelainStatus struct {
	GitStatus models.GitStatus
	Paths     []string
	Head      string
	Upstream  string
}

func (c *StatusCollector) collectPorcelain(ctx context.Context, g *git.Git) (porcelainStatus, error) {
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := g.RunWithContext(
		gitCtx, "status", "--porcelain=v2", "--branch", "-uall", "-z",
	)
	if err != nil {
		return porcelainStatus{}, err
	}
	return parsePorcelainV2(output)
}
```

`parsePorcelainV2` splits NUL records. Parse headers with exact prefixes
`# branch.oid `, `# branch.upstream `, and `# branch.ab +`; leave ahead/behind
zero when the last header is absent. Parse record kinds as follows:

```go
switch record[0] {
case '1':
	fields := strings.SplitN(record, " ", 9)
	xy, path = fields[1], fields[8]
case '2':
	fields := strings.SplitN(record, " ", 10)
	xy, path = fields[1], fields[9]
	index++ // skip the following original-path record
case 'u':
	fields := strings.SplitN(record, " ", 11)
	xy, path = fields[1], fields[10]
	status.Conflicts++
case '?':
	path = strings.TrimPrefix(record, "? ")
	status.Untracked++
}
```

For `1` and `2`, increment `Staged` when `xy[0] != '.'`; map `xy[1]` values
`M`, `A`, and `D` to `Modified`, `Added`, and `Deleted`. Append every changed or
untracked path once. Return a descriptive parse error when a record has too few
fields; do not panic on malformed Git output.

Delete `countFileStates`, `countUntrackedFiles`, `fetchRemoteStatus`,
`getCurrentBranch`, `getUpstreamBranch`, `countAheadBehind`, and `countRevList`
only after all callers use `collectPorcelain`.

- [ ] **Step 4: Run all status tests**

Run: `go test ./internal/status -count=1`

Expected: PASS.

- [ ] **Step 5: Commit porcelain collection**

Run `kenn:commit`, then commit both status files with subject:

```text
Collect worktree state with porcelain v2
```

### Task 2: Bound collection and calculate pragmatic activity

**Files:**

- Modify: `internal/status/status_collector.go`
- Modify: `internal/status/status_collector_test.go`
- Modify: `internal/cmd/status.go`
- Modify: `internal/cmd/tui_backend.go`
- Modify: `internal/fleet/manifest.go`

**Interfaces:**

- Produces: `Collection{Statuses []*models.WorktreeStatus, Diagnostics []Diagnostic}`.
- Produces: `Diagnostic{Path string, Err error}`.
- Changes: `CollectAll(context.Context, []*models.Worktree) (Collection, error)`.
- Adds: `StatusCollectorOptions.Workers int`, default `min(runtime.GOMAXPROCS(0), 8)` with a floor of 1.
- Produces: private `collectWorktrees(ctx, workers, worktrees, collect)` worker-pool function used directly by `CollectAll`.

- [ ] **Step 1: Write failing concurrency, degradation, and activity tests**

```go
func TestCollectAllBoundsWorkersAndDegradesOneRow(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	collect := func(ctx context.Context, wt *models.Worktree) (*models.WorktreeStatus, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) { break }
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
```

Add a clean-new-worktree test where HEAD is old and root mtime is new; expect
root mtime. Add a canceled-context test expecting `context.Canceled` and no
partial collection.

- [ ] **Step 2: Run focused collection tests**

Run: `go test ./internal/status -run 'BoundsWorkers|LastActivity|Canceled' -count=1`

Expected: FAIL because the result type, worker option, and activity algorithm do
not exist.

- [ ] **Step 3: Implement ordered bounded collection and diagnostics**

Add:

```go
type Diagnostic struct {
	Path string
	Err  error
}

type Collection struct {
	Statuses    []*models.WorktreeStatus
	Diagnostics []Diagnostic
}
```

Implement `collectWorktrees` with the collector callback as its last argument;
`CollectAll` passes `c.collectOne`. This keeps worker scheduling independently
testable without adding a production-only test seam to `StatusCollector`.
Preserve the existing `IsCurrent` marking after collection; the current
working-directory test already covers that contract.

Allocate `Statuses` to input length, send indexed jobs to `workers` goroutines,
and store each result at its original index. For an ordinary row error, create
an unknown status with the input path/branch/repository and append a diagnostic
under a mutex. Preserve diagnostic input order so repeated refreshes do not
reorder warning text. Make job submission cancellation-aware, close the jobs
channel exactly once, and wait for every worker before returning. Return a
top-level error only for canceled/deadline contexts.

Within `collectOne`, call `collectPorcelain` once. Calculate activity from:

```go
func (c *StatusCollector) lastActivity(ctx context.Context, path string, changed []string) (time.Time, error) {
	latest := time.Time{}
	if info, err := os.Stat(path); err == nil {
		latest = info.ModTime()
	} else {
		return time.Time{}, err
	}
	head, err := git.New(path).RunWithContext(ctx, "show", "-s", "--format=%ct", "HEAD")
	if err == nil {
		seconds, parseErr := strconv.ParseInt(strings.TrimSpace(head), 10, 64)
		if parseErr == nil && time.Unix(seconds, 0).After(latest) {
			latest = time.Unix(seconds, 0)
		}
	}
	for _, relative := range changed {
		if info, err := os.Stat(filepath.Join(path, filepath.FromSlash(relative))); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, ctx.Err()
}
```

Delete the tracked-file `ls-files` walk and untracked second command. Keep
repository identity fallback unchanged. When `FetchRemote` is false, zero the
parsed ahead/behind counts to preserve the option's existing behavior. Unlike
the current collector, return a porcelain failure from `collectOne` so the pool
can create the unknown row and its diagnostic; continue treating an unavailable
HEAD timestamp as a non-fatal activity input when root and changed-path times
are available. In
`collectTUIStatuses`, populate `models.Worktree.Repository` from the inventory
entry's canonical full path so TUI rows do not run an extra origin lookup.
Update `internal/cmd/status.go` and
`internal/fleet/manifest.go` to consume `.Statuses`; update
`collectTUIStatuses` to turn diagnostics into bounded warning strings of the
form `status unavailable for <path>: <error>`.

- [ ] **Step 4: Run every collector consumer**

Run: `go test ./internal/status ./internal/cmd ./internal/fleet -run 'Status|Manifest|TUIStatus' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit bounded pragmatic status**

Run `kenn:commit`, then commit the touched collector and consumer files with
subject:

```text
Bound worktree status collection
```

### Task 3: Split inventory currency from status collection

**Files:**

- Modify: `internal/tui/backend.go`
- Modify: `internal/cmd/tui_backend.go`
- Modify: `internal/cmd/tui_daemon_inventory_test.go`
- Modify: `internal/tui/model_test.go` fake backend methods only

**Interfaces:**

- Produces: `InventoryScope` and `InventoryRequest` in `internal/tui`.
- Produces: `InventoryResult{Rows, Warnings, ObservedAt, Current}` so the model can render real cache age.
- Replaces: `Backend.ListFast` and `Backend.List` with `Backend.LoadInventory`.

- [ ] **Step 1: Write failing backend mode tests**

Define the intended request type in tests:

```go
func TestTUIBackendInventoryModesSeparateCurrencyAndStatus(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "/launch")
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	var requests []kwt.Request
	var statusCalls int
	backend.collectStatuses = func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, []string, error) {
		statusCalls++
		return map[string]*models.WorktreeStatus{}, nil, nil
	}
	backend.queryInventory = func(_ context.Context, request kwt.Request, _ bool, _ io.Writer) (kwt.Result, error) {
		requests = append(requests, request)
		return kwt.Result{Freshness: kwt.Fresh, Snapshot: kwt.Snapshot{
			Config: &models.Config{},
			Entries: []kwt.Entry{{Path: "/work", IsMain: true, Repository: kwt.Repository{FullPath: "github.com/acme/widget"}}},
		}}, nil
	}

	_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCachedDashboard})
	_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard})
	_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard, CollectStatuses: true})

	require.Len(t, requests, 3)
	assert.False(t, requests[0].RequireCurrent)
	assert.True(t, requests[1].RequireCurrent)
	assert.True(t, requests[2].RequireCurrent)
	assert.Equal(t, 1, statusCalls)
}

func TestCurrentDashboardWithoutStatusStillAppliesConfigAndLaunchRegistration(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "/launch")
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	var registered []string
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project.Path)
		return nil
	}
	backend.registerWorkspace = nil
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{Freshness: kwt.Fresh, Snapshot: kwt.Snapshot{
			Config: &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/fresh-base"}},
			Entries: []kwt.Entry{{Path: "/launch", IsMain: true, Repository: kwt.Repository{URL: "https://github.com/acme/widget.git", FullPath: "github.com/acme/widget"}}},
			LaunchEntries: []kwt.Entry{{Path: "/launch", IsMain: true, Repository: kwt.Repository{URL: "https://github.com/acme/widget.git", FullPath: "github.com/acme/widget"}}},
		}}, nil
	}
	_, err := backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard})
	require.NoError(t, err)
	assert.Equal(t, "/fresh-base", backend.cfg.Worktree.BaseDir)
	assert.Equal(t, []string{"/launch"}, registered)
}

func TestInventoryQueryDoesNotHoldBackendConfigurationLock(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "/launch")
	entered := make(chan struct{})
	release := make(chan struct{})
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		close(entered)
		<-release
		return kwt.Result{}, nil
	}
	done := make(chan struct{})
	go func() {
		_, _ = backend.LoadInventory(context.Background(), dashboard.InventoryRequest{Scope: dashboard.InventoryCurrentDashboard})
		close(done)
	}()
	<-entered

	layouts := make(chan []string, 1)
	go func() { layouts <- backend.LayoutNames() }()
	select {
	case <-layouts:
	case <-time.After(time.Second):
		t.Fatal("inventory query held the backend configuration lock")
	}
	close(release)
	<-done
}
```

- [ ] **Step 2: Run mode tests and confirm missing interface**

Run: `go test ./internal/cmd -run 'InventoryModes|WithoutStatusStillApplies|InventoryQueryDoesNotHold' -count=1`

Expected: FAIL because `InventoryRequest` and `LoadInventory` do not exist.

- [ ] **Step 3: Add the explicit request and refactor daemon listing**

In `internal/tui/backend.go` add:

```go
type InventoryScope int

const (
	InventoryCachedDashboard InventoryScope = iota
	InventoryCurrentRepository
	InventoryCurrentDashboard
)

type InventoryRequest struct {
	Scope             InventoryScope
	WorkingDirectory  string
	ProjectIdentity   string
	CollectStatuses   bool
}

type InventoryResult struct {
	Rows       []Row
	Warnings   []string
	ObservedAt time.Time
	Current    bool
}
```

Replace both list methods on `Backend` with:

```go
LoadInventory(context.Context, InventoryRequest) (InventoryResult, error)
```

Map scopes in `tuiBackend`:

```go
switch request.Scope {
case dashboard.InventoryCachedDashboard:
	query.View = kwt.ViewDashboard
case dashboard.InventoryCurrentRepository:
	query.View = kwt.ViewRepository
	query.WorkingDirectory = request.WorkingDirectory
	interactive = false
case dashboard.InventoryCurrentDashboard:
	query.View = kwt.ViewDashboard
	query.RequireCurrent = true
default:
	return dashboard.InventoryResult{}, fmt.Errorf("unknown inventory scope %d", request.Scope)
}
```

Always set `IncludeProtectedSockets: true`. Collect status only when
`request.CollectStatuses`. For every fresh `InventoryCurrentDashboard` result,
call `applyInventoryConfigLocked`, `registerLaunchProject`, and
`registerLaunchWorkspace` before rendering, independent of status collection.
Do not render directory workspaces for `InventoryCurrentRepository`.
Copy the lifecycle result's `ObservedAt` into `InventoryResult.ObservedAt` and
set `Current` only when its freshness is `kwt.Fresh`.

Do not hold `b.mu` across the daemon query, Git status collection, or tmux
resolution. Snapshot the query/status/resolver function values, launch path,
stderr, and immutable configuration values under the lock, release it for slow
work, and reacquire only to apply a fresh dashboard config and perform launch
registration. Apply the same snapshot pattern to `MergeFleet`: do not hold
`b.mu` while publishing or reading the hub. Its merge uses the captured config
and applies only to the caller-owned row copy.

Update `fakeBackend` in `model_test.go` to record `[]InventoryRequest` and
return `InventoryResult{Rows: append([]Row(nil), b.rows...), ObservedAt:
time.Now(), Current: request.Scope != InventoryCachedDashboard}`.

- [ ] **Step 4: Run TUI backend and model tests**

Run: `go test ./internal/cmd ./internal/tui -run 'Inventory|PaintsFastRows' -count=1`

Expected: PASS after migrating existing fast/full tests to explicit requests.

- [ ] **Step 5: Commit the inventory mode split**

Run `kenn:commit`, then commit the backend interface and tests with subject:

```text
Separate inventory currency from status
```

### Task 4: Validate and merge repository-scoped refreshes

**Files:**

- Modify: `internal/cmd/tui_backend.go`
- Modify: `internal/cmd/tui_daemon_inventory_test.go`
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/model_test.go`

**Interfaces:**

- Produces: `validateRepositoryRows(rows []dashboard.Row, expected string) error`.
- Produces: `mergeRepositoryRows(previous, current []Row, project string) []Row`.
- Produces: `scopeFreshness{ObservedAt time.Time, Current bool, Diagnostic error}` plus model fields `projectFresh map[string]scopeFreshness`, `globalFresh scopeFreshness`, and `backgroundGlobalStarted bool`.

- [ ] **Step 1: Write failing scope-validation and field-preservation tests**

```go
func TestRepositoryInventoryRejectsGlobalFallback(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "/expected")
	backend.queryInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{Freshness: kwt.Fresh, Snapshot: kwt.Snapshot{Entries: []kwt.Entry{
			{Path: "/expected", IsMain: true, Repository: kwt.Repository{FullPath: "github.com/acme/expected"}},
			{Path: "/other", IsMain: true, Repository: kwt.Repository{FullPath: "github.com/acme/other"}},
		}}}, nil
	}
	_, err := backend.LoadInventory(context.Background(), dashboard.InventoryRequest{
		Scope: dashboard.InventoryCurrentRepository, WorkingDirectory: "/expected",
		ProjectIdentity: "github.com/acme/expected", CollectStatuses: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned unrelated repository")
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
```

Add deleted-working-directory and empty-result tests. Add a model test where an
`InteractionRequired` project refresh keeps cached rows, leaves the project
stale, and shows `project config requires trust; run kwt list there once`.

- [ ] **Step 2: Run scoped-refresh tests and observe wholesale replacement/fallback acceptance**

Run: `go test ./internal/cmd ./internal/tui -run 'RepositoryInventory|MergeRepository|RequiresTrust' -count=1`

Expected: FAIL because repository results are not validated or merged by scope.

- [ ] **Step 3: Implement fail-closed validation and merge-on-update**

Validation requires:

```go
func validateRepositoryRows(rows []dashboard.Row, expected string) error {
	if len(rows) == 0 {
		return fmt.Errorf("repository refresh returned no worktrees")
	}
	mainFound := false
	for _, row := range rows {
		if row.Entry == nil || !lifecycle.EqualProjectIdentity(dashboardProjectIdentity(row), expected) {
			return fmt.Errorf("repository refresh returned unrelated repository")
		}
		mainFound = mainFound || row.Entry.IsMain
	}
	if !mainFound {
		return fmt.Errorf("repository refresh returned no main worktree")
	}
	return nil
}
```

Add `dashboardProjectIdentity(row)` beside the existing command-backend
repository identity helpers; it returns `row.Entry.RepositoryInfo.FullPath`
when present and otherwise the canonical host/owner/repository join.

Use the backend's existing repository identity helpers rather than duplicating
URL parsing. Treat `InteractionRequired` specially in the model message; all
other scope errors retain the cached rows and attach their diagnostic to that
project.

`mergeRepositoryRows` removes only old rows whose project key equals the
successfully refreshed project, copies dashboard-only fields from an old row
with the same path and generation when the new value is empty, appends the
current project rows, sorts, and reanchors by path. Never let a repository
refresh add a row with an empty or mismatched project identity.

- [ ] **Step 4: Run scoped backend and model tests**

Run: `go test ./internal/cmd ./internal/tui -run 'Repository|Project.*Refresh|Anchor' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit scoped merge behavior**

Run `kenn:commit`, then commit the scoped refresh files with subject:

```text
Refresh only the active TUI project
```

### Task 5: Drive per-scope freshness and one background global pass

**Files:**

- Modify: `internal/tui/model.go`
- Modify: `internal/tui/model_test.go`

**Interfaces:**

- Consumes: `InventoryRequest` and `mergeRepositoryRows`.
- Produces: request messages tagged with scope and project so stale responses cannot mark another scope current.

- [ ] **Step 1: Write failing model sequencing tests**

```go
func TestModelLoadsCacheThenActiveProjectThenBackgroundGlobal(t *testing.T) {
	backend := &fakeBackend{rows: []Row{testRow("widget", "main", "/work/widget")}}
	model := NewModel(backend, "/work/widget")

	cached := model.Init()()
	model, projectCmd := updateModel(t, model, cached)
	require.NotNil(t, projectCmd)
	assert.Equal(t, InventoryCachedDashboard, backend.inventoryCalls[0].Scope)

	project := projectCmd()
	model, globalCmd := updateModel(t, model, project)
	require.NotNil(t, globalCmd)
	assert.Equal(t, InventoryCurrentRepository, backend.inventoryCalls[1].Scope)
	assert.True(t, backend.inventoryCalls[1].CollectStatuses)

	_ = globalCmd()
	assert.Equal(t, InventoryCurrentDashboard, backend.inventoryCalls[2].Scope)
	assert.False(t, backend.inventoryCalls[2].CollectStatuses)
}

func TestProjectSwitchRefreshesOnlySelectedProject(t *testing.T) {
	backend := &fakeBackend{}
	widget := testRow("widget", "main", "/w/widget")
	widget.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	other := testRow("other", "main", "/w/other")
	other.Entry.RepositoryInfo.FullPath = "github.com/acme/other"
	model := NewModel(backend, "/w/widget")
	model, _ = updateModel(t, model, inventoryMsg{request: InventoryRequest{Scope: InventoryCachedDashboard}, result: InventoryResult{Rows: []Row{widget, other}, ObservedAt: time.Now()}})
	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, paste("other"))
	model, _ = updateModel(t, model, press("enter"))
	request := model.fetchingRequest
	assert.Equal(t, InventoryCurrentRepository, request.Scope)
	assert.Equal(t, "github.com/acme/other", request.ProjectIdentity)
}

func TestAllProjectsStartsCurrentGlobalStatusRefresh(t *testing.T) {
	backend := &fakeBackend{}
	row := testRow("widget", "main", "/w/widget")
	row.Entry.RepositoryInfo.FullPath = "github.com/acme/widget"
	model := NewModel(backend, "/w/widget")
	model, _ = updateModel(t, model, inventoryMsg{request: InventoryRequest{Scope: InventoryCachedDashboard}, result: InventoryResult{Rows: []Row{row}, ObservedAt: time.Now()}})
	model.projectPerspective = "github.com/acme/widget"
	model, _ = updateModel(t, model, press("P"))
	model, _ = updateModel(t, model, press("up"))
	model, _ = updateModel(t, model, press("enter"))
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
		err: errors.New("daemon refresh failed"),
	})

	assert.Len(t, model.rows, 1)
	content := stripANSI(viewContent(model))
	assert.Contains(t, content, "global inventory 2m old")
	assert.Contains(t, content, "daemon refresh failed")
}

func TestBackgroundGlobalPreservesFreshProjectStatus(t *testing.T) {
	project := "github.com/acme/widget"
	row := testRow("widget", "topic", "/w/widget/topic")
	row.Entry.RepositoryInfo.FullPath = project
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
		result: InventoryResult{Rows: []Row{structural}, ObservedAt: time.Now(), Current: true},
	})

	assert.True(t, model.rows[0].SessionLive)
	assert.Equal(t, models.WorktreeStatusModified, model.rows[0].Status.Status)
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
```

- [ ] **Step 2: Run sequencing tests and observe the old global full refresh**

Run: `go test ./internal/tui -run 'CacheThenActive|ProjectSwitchRefreshes|AllProjectsStarts|BackgroundGlobalFailure|DirectoryWorkspaceMutation' -count=1`

Expected: FAIL because the model always follows cached rows with one global
`List` call and project switching only filters.

- [ ] **Step 3: Implement tagged scoped fetch messages**

Replace `rowsMsg`/`fastRowsMsg` with one message that carries its request:

```go
type inventoryMsg struct {
	request InventoryRequest
	result  InventoryResult
	err     error
}
```

Add:

```go
type scopeFreshness struct {
	ObservedAt time.Time
	Current    bool
	Diagnostic error
}
```

Add `now func() time.Time` and the global/project scope fields to `Model`;
initialize `now` to `time.Now`. A failed background dashboard request preserves
rows and the last observed timestamp, marks the scope non-current, and records
the diagnostic. Cached dashboard results initialize that timestamp from
`InventoryResult.ObservedAt` but remain non-current. Render `global inventory
<duration> old · <diagnostic>` in the status area until a later current global
result succeeds. Round age down to seconds below a minute and minutes above it;
this is product formatting owned by the model.

Initial load requests `InventoryCachedDashboard`. Once cached rows resolve
`launchPerspective`, request `InventoryCurrentRepository` with the anchored
worktree path and project identity. On success, mark only that project fresh,
merge rows, then start one `InventoryCurrentDashboard` request with
`CollectStatuses:false`. A completed background pass marks global rows fresh
and merges structural data without overwriting newer project statuses.

Project switch starts a current repository refresh for the chosen project.
Choosing the empty all-project perspective starts a current dashboard refresh
with status. Tag every response with the initiating request; discard or merge a
late response only for its own scope. Preserve the existing `loadSeq`, fleet
cancelation, pending-refresh, and path-anchor behavior.

When merging a status-free current dashboard result, accept structural,
workspace, launch, and endpoint updates but preserve status from every project
whose scoped result is already current. A status-bearing all-project result may
replace those statuses because it is itself the current displayed scope.

Replace the single `inventoryCurrent` action gate with a scope check. Worktree
mutations require `projectFresh[rowProjectKey(row)].Current`;
directory-workspace and launch-entry mutations require `globalFresh.Current`.
Cached shell and live-only attach are the exceptions implemented in Task 6.

- [ ] **Step 4: Run the complete model suite**

Run: `go test ./internal/tui -race -count=1`

Expected: PASS with no race report.

- [ ] **Step 5: Commit per-scope freshness**

Run `kenn:commit`, then commit model and tests with subject:

```text
Track TUI freshness per project
```

### Task 6: Allow safe cached shell and live-only attach

**Files:**

- Modify: `internal/tmux/workspace_sessions.go`
- Modify: `internal/tmux/workspace_sessions_integration_test.go`
- Modify: `internal/tui/backend.go`
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/model_test.go`
- Modify: `internal/cmd/tui_backend.go`
- Modify: `internal/cmd/tui.go`
- Modify: `internal/cmd/tui_test.go`

**Interfaces:**

- Produces: `WorkspaceSessions.ResolveLive(ctx, request) (SessionEndpoint, error)`.
- Produces: backend methods `OpenExistingInTmux` and `AttachExistingOutsideTmux`.
- Adds: `Handoff.ExistingOnly bool`.

- [ ] **Step 1: Write failing live-only and shell-preflight tests**

```go
func TestResolveLiveRequiresExactlyOneVerifiedEndpoint(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	fixture.createMatching(t, fixture.servers.kwtServer())
	fixture.createMatching(t, fixture.servers.defaultServer())
	_, err := fixture.sessions.ResolveLive(fixture.ctx, fixture.request())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one live endpoint")
}

func TestCachedLiveAttachNeverEstablishes(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	endpoint := tmux.SessionEndpoint{SessionName: "topic", SocketName: tmux.KWTServerSocketName}
	backend.liveEndpoints = func(context.Context, tmux.WorkspaceEndpointRequest) ([]tmux.SessionEndpoint, error) {
		return []tmux.SessionEndpoint{endpoint}, nil
	}
	wantProcess := &exec.Cmd{}
	backend.prepareResidentAttach = func(context.Context, tmux.SessionEndpoint) (*exec.Cmd, error) { return wantProcess, nil }
	var ensured bool
	backend.ensureWorktree = func(context.Context, string, string, string, models.Layout) (tmux.SessionEndpoint, error) {
		ensured = true
		return tmux.SessionEndpoint{}, nil
	}
	row := dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{Path: "/work/topic", Branch: "topic", Generation: "0123456789abcdef0123456789abcdef"},
		SessionName: "topic", SessionLive: true, TmuxEndpoint: endpoint,
	}
	process, err := backend.OpenExistingInTmux(context.Background(), row)
	require.NoError(t, err)
	assert.Same(t, wantProcess, process)
	assert.False(t, ensured)
}

func TestModelShellOnCachedDeletedPathStaysOpen(t *testing.T) {
	backend := &fakeBackend{rows: []Row{testRow("widget", "topic", "/missing")}}
	model := NewModel(backend, "/work")
	model.rows = backend.rows
	oldValidate := validateShellDirectory
	validateShellDirectory = func(string) error { return os.ErrNotExist }
	t.Cleanup(func() { validateShellDirectory = oldValidate })
	model.projectFresh = map[string]scopeFreshness{}

	next, cmd := updateModel(t, model, press("c"))

	assert.Nil(t, cmd)
	assert.Equal(t, HandoffNone, next.Handoff().Kind)
	assert.Contains(t, next.message, "no longer exists")
}
```

Add stale cached attach tests: `SessionLive:true` calls the live-only backend;
`SessionLive:false` keeps the refresh gate. Add a current-row attach test that
still uses establishment.

- [ ] **Step 2: Run cached-action tests and observe the global freshness gate**

Run: `go test ./internal/tmux ./internal/tui ./internal/cmd -run 'ResolveLive|CachedLiveAttach|ShellOnCached' -count=1`

Expected: FAIL because cached shell and attach are blocked and both attach paths
establish.

- [ ] **Step 3: Implement fail-closed live-only resolution and path checking**

Implement:

```go
func (s *WorkspaceSessions) ResolveLive(ctx context.Context, request WorkspaceEndpointRequest) (SessionEndpoint, error) {
	endpoints, err := s.LiveEndpoints(ctx, request)
	if err != nil {
		return SessionEndpoint{}, err
	}
	if len(endpoints) != 1 {
		return SessionEndpoint{}, fmt.Errorf("expected exactly one live endpoint, found %d", len(endpoints))
	}
	return endpoints[0], nil
}
```

In the command backend, build the request from exact row session name, path,
and generation. Preserve `rejectProtectedWorkspaceOpen` and
`acknowledgeRemoteSourcePath` before resolving. Then call `ResolveLive` and only
`PrepareResidentAttach` or `Attach`. Do not call layout resolution,
`Establish`, or `Ensure` on this path. The existing current attach methods
remain unchanged. Snapshot the resolver, acknowledgement, and attach function
values under `b.mu`, then release the lock before endpoint resolution or attach;
this path must remain available while inventory or fleet I/O runs.

Use these exact method shapes: add
`OpenExistingInTmux(context.Context, Row) (*exec.Cmd, error)` to the TUI
`Backend` interface, and add `AttachExistingOutsideTmux(Row) error` to the
concrete command backend. The live-only methods accept no layout because they
never create or reconfigure a workspace.

Remove `Shell` from the blanket stale gate. Before setting `HandoffShell`, call:

```go
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
```

For stale `Open`, permit only a row whose cached `SessionLive` is true and use
the live-only method. Set `Handoff.ExistingOnly` outside tmux so `runRootTUI`
calls `AttachExistingOutsideTmux`; inside tmux invoke
`OpenExistingInTmux`. Keep creation, sync, deletion, kill, and establish paths
gated by the relevant scope's freshness.

- [ ] **Step 4: Run tmux, TUI model, and command suites**

Run: `go test ./internal/tmux ./internal/tui ./internal/cmd -run 'Attach|Shell|Inventory' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit safe cached actions**

Run `kenn:commit`, then commit the live-resolution, backend, model, and tests
with subject:

```text
Keep cached TUI navigation available
```

### Task 7: Document and verify scoped TUI inventory

**Files:**

- Modify: `docs/workflows/agent-workspaces.md`
- Modify: `docs/reference/cli.md` only if its TUI key table describes refresh gating.

**Interfaces:**

- Documents: cached launch, active-project refresh, global background pass, stale action rules, and status activity semantics.

- [ ] **Step 1: Update maintained workflow documentation**

Add this user contract in plain language:

```text
The dashboard opens from its cached catalog, then refreshes only the project
you are viewing. You can search, move through rows, open a shell in an existing
directory, or attach to a session that KWT verifies is already live while that
refresh runs. Creating, deleting, syncing, killing, or starting a session waits
for current inventory.

KWT refreshes the global catalog once in the background. Choosing all projects
runs a current global refresh and collects status for the displayed rows.
```

Document that activity uses HEAD, the worktree directory, and changed/untracked
file times rather than every tracked file.

- [ ] **Step 2: Run final plan-two verification**

Run: `make fmt && make test && make build && make docs-check`

Expected: all commands PASS.

- [ ] **Step 3: Inspect the complete plan-two diff**

Run: `git diff --check && git diff --stat && git status --short`

Expected: only scoped-inventory/status/cached-action code, tests, and maintained
documentation are modified; `git diff --check` prints nothing.

- [ ] **Step 4: Commit documentation**

Run `kenn:commit`, then commit maintained docs with subject:

```text
Document responsive TUI refresh
```

- [ ] **Step 5: Record completion evidence without closing the parent issue**

Run:

```text
kata comment wbcw --body "Landed repository-scoped TUI refresh, bounded porcelain-v2 status, a status-free global cache pass, and safe cached shell/live attach. Verified make test, make build, and docs-check." --agent
```

The parent remains open until the asynchronous removal queue lands.
