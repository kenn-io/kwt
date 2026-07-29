# Worktree Labels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make primary and detached worktrees immediately recognizable in every TUI row label while preserving raw Git and fleet identity.

**Architecture:** Add one fleet-summary field at the command-to-TUI boundary, then derive all display text through a pure TUI helper that never mutates or replaces `rowBranch`. Keep deletion protection in the model and improve only its immediate user-facing explanation.

**Tech Stack:** Go, Bubble Tea, Testify, Git worktree metadata, kwt fleet manifests

## Global Constraints

- Primary checkouts display as `<branch> [primary]`.
- Detached checkouts display as `detached@<first-eight-commit-characters>`, or `detached` when no commit is available.
- A checkout that is both detached and primary displays as `detached@<first-eight-commit-characters> [primary]`.
- A remote-only fleet row displays `[primary]` only when it has at least one observation and every observation is primary.
- A fleet row with mixed primary and linked observations remains unmarked.
- Raw branch values, fleet refs, sorting identity, Git operations, backend APIs, and `FleetInfo.MaterializeLabel` semantics remain unchanged.
- The backend primary-worktree deletion guard remains in place.
- Production changes must follow a witnessed red-green-refactor cycle.

---

### Task 1: Retain Conservative Fleet Primary State

**Files:**

- Modify: `internal/tui/backend.go:41-60`
- Modify: `internal/cmd/tui_backend.go:789-850`
- Test: `internal/cmd/tui_test.go`

**Interfaces:**

- Consumes: `fleet.FleetRow.Observations []fleet.Observation` and `fleet.Observation.IsMain bool`
- Produces: `FleetInfo.AllPrimary bool` and `allRemoteFleetObservationsPrimary([]fleet.Observation, string) bool`

- [ ] **Step 1: Write failing adapter tests**

Add table-driven coverage for all-primary, mixed, all-linked, and empty observation lists. Add a detached-row assertion proving `MaterializeLabel` remains the raw full SHA and detached rows remain non-materializable:

```go
func TestDashboardFleetInfoSummarizesPrimaryObservations(t *testing.T) {
	tests := []struct {
		name         string
		observations []fleet.Observation
		local        bool
		want         bool
	}{
		{name: "empty", observations: nil, want: false},
		{
			name: "all primary",
			observations: []fleet.Observation{
				{HostID: "host-b", IsMain: true},
				{HostID: "host-c", IsMain: true},
			},
			want: true,
		},
		{
			name: "mixed",
			observations: []fleet.Observation{
				{HostID: "host-b", IsMain: true},
				{HostID: "host-c", IsMain: false},
			},
			want: false,
		},
		{
			name: "all linked",
			observations: []fleet.Observation{
				{HostID: "host-b"},
				{HostID: "host-c"},
			},
			want: false,
		},
		{
			name: "ignores synthesized local observation",
			observations: []fleet.Observation{
				{HostID: "host-a", IsMain: false},
				{HostID: "host-b", IsMain: true},
				{HostID: "host-c", IsMain: true},
			},
			local: true,
			want:  true,
		},
		{
			name: "local observation alone is not remote primary state",
			observations: []fleet.Observation{
				{HostID: "host-a", IsMain: true},
			},
			local: true,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := dashboardFleetInfo(fleet.FleetRow{
				ProjectIdentity: "github.com/example/kwt",
				Kind:            "branch",
				Ref:             "main",
				Branch:          "main",
				Observations:    tt.observations,
			}, fleet.StatusRow{}, "host-a", tt.local)

			require.NotNil(t, info)
			assert.Equal(t, tt.want, info.AllPrimary)
		})
	}
}

func TestDashboardFleetInfoKeepsDetachedMaterializeIdentityRaw(t *testing.T) {
	ref := strings.Repeat("a", 40)
	info := dashboardFleetInfo(fleet.FleetRow{
		ProjectIdentity: "github.com/example/kwt",
		Kind:            "detached",
		Ref:             ref,
		Observations: []fleet.Observation{{
			HostID: "host-b",
			IsMain: true,
		}},
	}, fleet.StatusRow{}, "host-a", false)

	require.NotNil(t, info)
	assert.True(t, info.AllPrimary)
	assert.Equal(t, ref, info.MaterializeLabel)
	assert.False(t, info.CanMaterialize)
}
```

- [ ] **Step 2: Run the focused tests and witness the expected compile failure**

Run:

```bash
go test ./internal/cmd -run 'TestDashboardFleetInfo(SummarizesPrimaryObservations|KeepsDetachedMaterializeIdentityRaw)$'
```

Expected: FAIL because `dashboard.FleetInfo` has no `AllPrimary` field.

- [ ] **Step 3: Add the fleet summary field and non-vacuous reducer**

Add the field immediately after `Freshness string` in `FleetInfo`:

```go
AllPrimary bool
```

Add the reducer next to the other fleet adapter helpers:

```go
func allRemoteFleetObservationsPrimary(observations []fleet.Observation, currentHost string) bool {
	found := false
	for _, observation := range observations {
		hostID := strings.TrimSpace(observation.HostID)
		if hostID == "" || hostID == currentHost {
			continue
		}
		found = true
		if !observation.IsMain {
			return false
		}
	}
	return found
}
```

Populate the new field inside the existing `dashboard.FleetInfo` literal:

```go
info := &dashboard.FleetInfo{
	ProjectIdentity: row.ProjectIdentity,
	ProjectName:     rendered.Project,
	Kind:            row.Kind,
	Ref:             ref,
	Branch:          strings.TrimSpace(row.Branch),
	Local:           local,
	Hosts:           fleetDisplayHosts(row.Observations, currentHost, local),
	Sync:            rendered.Sync,
	Dirty:           rendered.Dirty,
	Freshness:       rendered.Freshness,
	AllPrimary:      allRemoteFleetObservationsPrimary(row.Observations, currentHost),
}
```

The summary deliberately excludes the synthesized current-host observation
because `remoteProjection` later drops that host while preserving this value.
Do not change the existing `MaterializeLabel`, `CanMaterialize`, or
materialization-observation logic.

- [ ] **Step 4: Run focused and package tests**

Run:

```bash
go test ./internal/cmd -run 'TestDashboardFleetInfo(SummarizesPrimaryObservations|KeepsDetachedMaterializeIdentityRaw)$'
go test ./internal/cmd ./internal/tui
```

Expected: PASS.

- [ ] **Step 5: Commit the fleet metadata slice**

Before committing, invoke the mandatory `kenn:commit` skill. Then stage only this task's files and commit with a rationale-first message:

```bash
git add internal/tui/backend.go internal/cmd/tui_backend.go internal/cmd/tui_test.go
git commit -m "Retain fleet primary checkout state" \
  -m "Grouped fleet rows need conservative primary metadata at the TUI boundary so display code can distinguish uniformly primary observations from mixed host-specific state without inspecting fleet internals."
```

---

### Task 2: Derive Display-Only Worktree Labels

**Files:**

- Modify: `internal/tui/list.go:48-61`
- Modify: `internal/tui/list.go:131-133`
- Modify: `internal/tui/list.go:170-185`
- Modify: `internal/tui/list.go:724-734`
- Test: `internal/tui/list_test.go`
- Test: `internal/tui/model_test.go`

**Interfaces:**

- Consumes: `rowBranch(Row) string`, `Row.Entry.IsMain`, `Row.Entry.CommitHash`, `Row.Fleet.Kind`, `Row.Fleet.Ref`, and `Row.Fleet.AllPrimary`
- Produces: `shortCommitHash(string) string` and `rowDisplayBranch(Row) string`

- [ ] **Step 1: Write failing pure presentation tests**

Add a table-driven test covering ordinary, primary, detached, and fleet-only rows:

```go
func TestRowDisplayBranch(t *testing.T) {
	fullHash := "be094b1bdf4471ea60db4656f06d8fb2551ffd3d"
	tests := []struct {
		name string
		row  Row
		want string
	}{
		{
			name: "ordinary linked worktree",
			row:  testRow("kwt", "feature", "/w/kwt/feature"),
			want: "feature",
		},
		{
			name: "primary checkout",
			row: func() Row {
				row := testRow("kwt", "main", "/w/kwt/main")
				row.Entry.IsMain = true
				return row
			}(),
			want: "main [primary]",
		},
		{
			name: "detached full hash",
			row: func() Row {
				row := testRow("kwt", "HEAD", "/w/kwt/detached")
				row.Entry.CommitHash = fullHash
				return row
			}(),
			want: "detached@be094b1b",
		},
		{
			name: "detached short hash",
			row: func() Row {
				row := testRow("kwt", "HEAD", "/w/kwt/detached")
				row.Entry.CommitHash = "abc123"
				return row
			}(),
			want: "detached@abc123",
		},
		{
			name: "detached missing hash",
			row:  testRow("kwt", "HEAD", "/w/kwt/detached"),
			want: "detached",
		},
		{
			name: "detached primary checkout",
			row: func() Row {
				row := testRow("kwt", "HEAD", "/w/kwt/detached-primary")
				row.Entry.CommitHash = fullHash
				row.Entry.IsMain = true
				return row
			}(),
			want: "detached@be094b1b [primary]",
		},
		{
			name: "remote detached",
			row: Row{Fleet: &FleetInfo{
				Kind: "detached",
				Ref:  fullHash,
			}},
			want: "detached@be094b1b",
		},
		{
			name: "remote detached primary",
			row: Row{Fleet: &FleetInfo{
				Kind:       "detached",
				Ref:        fullHash,
				AllPrimary: true,
			}},
			want: "detached@be094b1b [primary]",
		},
		{
			name: "remote uniformly primary",
			row: Row{Fleet: &FleetInfo{
				Kind:       "branch",
				Ref:        "main",
				Branch:     "main",
				AllPrimary: true,
			}},
			want: "main [primary]",
		},
		{
			name: "remote mixed primary state",
			row: Row{Fleet: &FleetInfo{
				Kind:       "branch",
				Ref:        "main",
				Branch:     "main",
				AllPrimary: false,
			}},
			want: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, rowDisplayBranch(tt.row))
		})
	}
}
```

Add assertions that raw identity remains untouched and new labels are searchable:

```go
func TestDisplayBranchDoesNotReplaceRawBranchIdentity(t *testing.T) {
	row := testRow("kwt", "main", "/w/kwt/main")
	row.Entry.IsMain = true

	assert.Equal(t, "main", rowBranch(row))
	assert.Equal(t, "main [primary]", rowDisplayBranch(row))
	assert.Equal(t, "kwt:main [primary]", rowLabel(row))
}

func TestFilterRowsMatchesDisplayBranch(t *testing.T) {
	primary := testRow("kwt", "main", "/w/kwt/main")
	primary.Entry.IsMain = true
	detached := testRow("kwt", "HEAD", "/w/kwt/detached")
	detached.Entry.CommitHash = "be094b1bdf4471ea60db4656f06d8fb2551ffd3d"
	rows := []Row{primary, detached}

	require.Len(t, filterRows(rows, "primary"), 1)
	assert.Equal(t, primary.Entry.Path, rowPath(filterRows(rows, "primary")[0]))
	require.Len(t, filterRows(rows, "detached@be094b1b"), 1)
	assert.Equal(t, detached.Entry.Path, rowPath(filterRows(rows, "detached@be094b1b")[0]))
}
```

- [ ] **Step 2: Run the focused tests and witness the expected compile failure**

Run:

```bash
go test ./internal/tui -run 'Test(RowDisplayBranch|DisplayBranchDoesNotReplaceRawBranchIdentity|FilterRowsMatchesDisplayBranch)$'
```

Expected: FAIL because `rowDisplayBranch` does not exist.

- [ ] **Step 3: Implement the display-only helper**

Add the helpers after `rowBranch`:

```go
func shortCommitHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

func rowDisplayBranch(row Row) string {
	branch := rowBranch(row)
	detached := false
	commit := ""
	primary := false

	switch {
	case row.Entry != nil:
		detached = branch == "" || branch == "HEAD"
		commit = row.Entry.CommitHash
		primary = row.Entry.IsMain
	case row.Fleet != nil:
		detached = strings.EqualFold(strings.TrimSpace(row.Fleet.Kind), "detached")
		commit = row.Fleet.Ref
		primary = row.Fleet.AllPrimary
	}

	if detached {
		branch = "detached"
		if short := shortCommitHash(commit); short != "" {
			branch += "@" + short
		}
	}
	if primary {
		branch += " [primary]"
	}
	return branch
}
```

Keep `rowBranch` unchanged. Route presentation through the helper:

```go
func rowLabel(row Row) string {
	return rowRepoName(row) + ":" + rowDisplayBranch(row)
}
```

Add `rowDisplayBranch(row)` to the `filterRows` haystack without removing the raw branch:

```go
haystack := strings.ToLower(strings.Join([]string{
	rowRepoName(row),
	rowBranch(row),
	rowDisplayBranch(row),
	rowPath(row),
	rowLabel(row),
	rowFleetHaystack(row),
}, " "))
```

Use the display value only in the branch dashboard cell:

```go
dashboardColumnBranch: rowDisplayBranch(row),
```

Do not change `sortRows`, `sameCheckout`, `FleetKeyForRow`, `rowFleetHaystack`, or any materialization field.

- [ ] **Step 4: Add a rendered-dashboard regression**

Extend `internal/tui/model_test.go` with a model-level assertion proving the branch cell and selection details both use the helper:

```go
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
```

Add a fast-refresh regression proving the adapter's remote-only summary
survives when the local worktree disappears:

```go
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
```

- [ ] **Step 5: Run focused and package tests**

Run:

```bash
go test ./internal/tui -run 'Test(RowDisplayBranch|DisplayBranchDoesNotReplaceRawBranchIdentity|FilterRowsMatchesDisplayBranch|ModelRendersPrimaryAndDetachedDisplayLabels)$'
go test ./internal/tui ./internal/cmd
```

Expected: PASS. Existing fleet-key and branch-switch tests must remain green, demonstrating raw identity was not altered.

- [ ] **Step 6: Commit the presentation slice**

Before committing, invoke the mandatory `kenn:commit` skill. Then stage only this task's files:

```bash
git add internal/tui/list.go internal/tui/list_test.go internal/tui/model_test.go
git commit -m "Clarify primary and detached worktree labels" \
  -m "The dashboard needs human-readable checkout state without feeding decorated labels back into sorting, fleet matching, or Git operations. Centralize presentation while leaving raw branch identity intact."
```

---

### Task 3: Explain Protected Primary Checkout Deletion

**Files:**

- Modify: `internal/tui/model.go:1048-1055`
- Test: `internal/tui/model_test.go:1047-1060`

**Interfaces:**

- Consumes: `abbreviateHome(string) string` and `Row.Entry.Path`
- Produces: immediate TUI message `cannot delete the primary checkout: <abbreviated-path>`

- [ ] **Step 1: Replace the existing guard test with the desired message**

Update the test to use a checkout under a controlled home directory and assert the backend is never called:

```go
func TestModelDeleteRefusesPrimaryCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "code", "kwt")
	row := testRow("kwt", "main", path)
	row.Entry.IsMain = true
	backend := &fakeBackend{}
	model := NewModel(backend, "/worktrees")
	model, _ = updateModel(t, model, rowsMsg{rows: []Row{row}})

	model, cmd := updateModel(t, model, press("d"))

	require.Nil(t, cmd)
	assert.Contains(t, viewContent(model), "cannot delete the primary checkout: ~/code/kwt")
	assert.Empty(t, backend.removeCalls)
}
```

- [ ] **Step 2: Run the focused test and witness the expected assertion failure**

Run:

```bash
go test ./internal/tui -run '^TestModelDeleteRefusesPrimaryCheckout$'
```

Expected: FAIL because the TUI still renders `refusing to remove a main worktree`.

- [ ] **Step 3: Implement the specific protected-delete message**

Change only the model's immediate guard:

```go
if row.Entry.IsMain {
	m.message = fmt.Sprintf(
		"cannot delete the primary checkout: %s",
		abbreviateHome(row.Entry.Path),
	)
	return m, nil
}
```

Do not change `tuiBackend.RemoveWorktree`; its independent backend guard remains defense in depth.

- [ ] **Step 4: Run focused and full verification**

Run:

```bash
go test ./internal/tui -run '^TestModelDeleteRefusesPrimaryCheckout$'
go test ./internal/tui ./internal/cmd
make test
make build
```

Expected: every command exits successfully.

- [ ] **Step 5: Inspect the final diff for scope and identity safety**

Run:

```bash
git diff origin/main...HEAD --stat
git diff origin/main...HEAD -- internal/tui internal/cmd docs/superpowers
git status --short --branch
```

Confirm:

- no production code outside `internal/tui` and `internal/cmd/tui_backend.go` changed;
- `rowBranch`, `FleetKeyForRow`, `sameCheckout`, and `MaterializeLabel` still use raw data;
- only the planned documentation, tests, adapter field, display helper, and message changed;
- no untracked or unstaged task files remain.

- [ ] **Step 6: Commit the deletion-message slice**

Before committing, invoke the mandatory `kenn:commit` skill. Then stage the focused files:

```bash
git add internal/tui/model.go internal/tui/model_test.go
git commit -m "Explain protected primary checkout deletion" \
  -m "A generic main-worktree refusal does not tell users which checkout is protected or why the selected branch cannot be removed. Name the primary checkout and include its abbreviated path while retaining both deletion guards."
```

- [ ] **Step 7: Close the kata issue after verification**

Capture the final commit:

```bash
final_commit=$(git rev-parse HEAD)
kata label rm bmw6 needs-review --agent
kata close bmw6 --done \
  --message "Primary and detached TUI labels now expose checkout state for local and remote-only rows; protected deletion names the primary checkout path. Verified make test and make build." \
  --commit "$final_commit"
```
