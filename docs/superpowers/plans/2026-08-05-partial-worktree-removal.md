# Partial Worktree Removal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Treat confirmed Git deregistration with residual files as partial success so kwt completes bookkeeping, refreshes the dashboard, and shows an actionable warning without deleting the remaining directory.

**Architecture:** The Git package will expose a typed error and classifier for the state where a registered worktree disappears from Git inventory despite a failed command. CLI and TUI consumers will use that classification to run successful-removal bookkeeping, while the TUI model refreshes inventory and retains the warning.

**Tech Stack:** Go, `os/exec`, Cobra command tests, Bubble Tea model tests, Testify assertions.

## Global Constraints

- Preserve every residual directory after generation-conditional removal; never recursively clean this outcome.
- Do not discover or terminate processes and do not retry deletion.
- Preserve ordinary success and hard-failure behavior.
- Write and run each regression before its production change.
- Use focused `go test`, then `make test` and `make build`.

---

## File Map

- `internal/git/git_worktree.go`: defines and emits the typed partial-removal result.
- `internal/git/git_test.go`: exercises deregistration followed by residual-directory failure.
- `internal/worktree/worktree.go`: continues requested branch deletion after confirmed deregistration.
- `internal/worktree/worktree_test.go`: verifies branch deletion is attempted and the warning is retained.
- `internal/cmd/remove.go`: counts partial removals as completed for CLI bookkeeping.
- `internal/cmd/remove_test.go`: verifies CLI cleanup, publication, warning, and preservation.
- `internal/cmd/tui_backend.go`: completes backend bookkeeping for partial removal.
- `internal/cmd/tui_test.go`: verifies backend bookkeeping and warning propagation.
- `internal/tui/model.go`: refreshes inventory when completed removal carries a warning.
- `internal/tui/model_test.go`: verifies warning visibility during refresh.

### Task 1: Classify Confirmed Git Deregistration

**Files:**
- Modify: `internal/git/git_worktree.go:854-888`
- Test: `internal/git/git_test.go:1237-1280`

**Interfaces:**
- Produces: `func WorktreeWasRemoved(err error) bool`
- Produces: an error implementing `WorktreeRemoved() bool`, `Unwrap() error`, and actionable `Error() string`

- [ ] **Step 1: Strengthen the failing regression**

Update `TestConditionalRemoveWorktreePreservesDirectoryAfterGitDeregistersWorktree` after its wrapper invokes real Git, recreates a residual file, and exits nonzero:

```go
err = g.RemoveWorktree(worktreePath, false, generation)

require.Error(t, err)
assert.True(t, WorktreeWasRemoved(err))
assert.ErrorContains(t, err, "worktree removed, but files remain at "+worktreePath)
assert.ErrorContains(t, err, "stop processes using that directory, then delete it")
assert.FileExists(t, filepath.Join(worktreePath, "replacement"))
```

This catches returning an unclassified generic error after Git deregistration.

- [ ] **Step 2: Run the focused test and verify RED**

```bash
go test ./internal/git -run '^TestConditionalRemoveWorktreePreservesDirectoryAfterGitDeregistersWorktree$' -count=1
```

Expected: FAIL to compile because `WorktreeWasRemoved` is undefined.

- [ ] **Step 3: Add the minimal typed result**

Import `errors` and add near `removeWorktree`:

```go
type incompleteWorktreeRemovalError struct {
	path  string
	cause error
}

func (e *incompleteWorktreeRemovalError) Error() string {
	return fmt.Sprintf(
		"worktree removed, but files remain at %s; stop processes using that directory, then delete it",
		e.path,
	)
}

func (e *incompleteWorktreeRemovalError) Unwrap() error { return e.cause }
func (e *incompleteWorktreeRemovalError) WorktreeRemoved() bool { return true }

func WorktreeWasRemoved(err error) bool {
	var removed interface{ WorktreeRemoved() bool }
	return errors.As(err, &removed) && removed.WorktreeRemoved()
}
```

Replace only the conditional deregistration error with:

```go
return &incompleteWorktreeRemovalError{path: path, cause: err}
```

Keep unconditional cleanup and still-registered failures unchanged.

- [ ] **Step 4: Run focused and package tests and verify GREEN**

```bash
go test ./internal/git -run 'Test(ConditionalRemoveWorktreePreservesDirectoryAfterGitDeregistersWorktree|RemoveWorktreeCleansDirectoryAfterGitDeregistersWorktree|RemoveWorktreeRejectsChangedGeneration)$' -count=1
go test ./internal/git -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the classification**

```bash
git add internal/git/git_worktree.go internal/git/git_test.go
git commit -m "Classify incomplete worktree deletion"
```

Use the mandatory commit skill for the message body and attribution.

### Task 2: Complete CLI and TUI Bookkeeping

**Files:**
- Modify: `internal/worktree/worktree.go:437-465`
- Test: `internal/worktree/worktree_test.go`
- Modify: `internal/cmd/remove.go:218-270,389-480`
- Test: `internal/cmd/remove_test.go`
- Modify: `internal/cmd/tui_backend.go:1268-1310`
- Test: `internal/cmd/tui_test.go:1899-1992`

**Interfaces:**
- Consumes: `git.WorktreeWasRemoved(err error) bool`
- Produces: `Manager.RemoveWithBranch` continues requested branch deletion and returns the partial warning
- Produces: CLI removal counts partial outcomes in `removed`
- Produces: `tuiBackend.RemoveWorktree` returns the original warning after bookkeeping

- [ ] **Step 1: Add a failing manager branch-cleanup test**

Add `deletedBranches []string` to `mockGit`, append the branch in `DeleteBranch`, and define a test-local error implementing `WorktreeRemoved() bool`. Call `RemoveWithBranch` with branch deletion enabled and assert:

```go
require.ErrorIs(t, err, partialErr)
assert.Equal(t, []string{"feature"}, mockG.deletedBranches)
```

This catches returning before an explicitly requested branch deletion after Git has already deregistered the worktree.

- [ ] **Step 2: Run the manager test and verify RED**

```bash
go test ./internal/worktree -run '^TestManagerRemoveWithBranchContinuesAfterPartialRemoval$' -count=1
```

Expected: FAIL because `DeleteBranch` is not called.

- [ ] **Step 3: Continue branch cleanup after semantic completion**

Retain the worktree error, return immediately only when it is not classified as removed, and return the partial warning after successful branch deletion:

```go
removalErr := m.git.RemoveWorktree(path, forceWorktree, ifGeneration)
if removalErr != nil && !git.WorktreeWasRemoved(removalErr) {
	return removalErr
}
if deleteBranch && branch != "" {
	if err := m.git.DeleteBranch(branch, forceBranch); err != nil {
		branchErr := fmt.Errorf("worktree removed but failed to delete branch: %w", err)
		if removalErr != nil {
			return errors.Join(removalErr, branchErr)
		}
		return branchErr
	}
}
return removalErr
```

Import `go.kenn.io/kwt/internal/git`; the existing standard-library `errors` import supports the combined-error case without changing ordinary branch-failure behavior.

- [ ] **Step 4: Run the manager regressions and verify GREEN**

```bash
go test ./internal/worktree -run 'TestManager(RemoveWithBranchContinuesAfterPartialRemoval|Remove)$' -count=1
```

Expected: PASS.

- [ ] **Step 5: Add a failing CLI behavior test**

Add a POSIX-only test that creates a real worktree and registry entry, puts a `git` wrapper first on `PATH`, and makes only `git worktree remove` run real Git, recreate `residual`, and exit nonzero. Invoke `runRemove` with the real generation condition and assert:

```go
require.ErrorContains(t, err, "worktree removed, but files remain at "+worktreePath)
assert.Equal(t, 1, publishCalls)
assert.FileExists(t, filepath.Join(worktreePath, "residual"))
_, registered := refreshedRegistry.Get(worktreePath)
assert.False(t, registered)
```

This catches deciding completion solely from `os.Stat(path)` when residual files remain.

- [ ] **Step 6: Run the CLI test and verify RED**

```bash
go test ./internal/cmd -run '^TestRemoveLocalCompletesBookkeepingAfterGitDeregistersWithResidualFiles$' -count=1
```

Expected: FAIL because publication remains zero and the registry record remains.

- [ ] **Step 7: Recognize semantic completion in local and global CLI paths**

```go
func worktreeRemovalCompleted(path string, err error) bool {
	return worktreePathRemoved(path) || git.WorktreeWasRemoved(err)
}
```

Use this predicate in every local/global error branch that currently increments `removed` and unregisters only when `worktreePathRemoved` is true. Preserve warning return/printing behavior.

- [ ] **Step 8: Run the CLI regressions and verify GREEN**

```bash
go test ./internal/cmd -run 'TestRemove(LocalCompletesBookkeepingAfterGitDeregistersWithResidualFiles|LocalPublishesWhenWorktreeRemovedButBranchDeleteFails|GlobalPublishesWhenWorktreeRemovedButBranchDeleteFails)$' -count=1
```

Expected: PASS.

- [ ] **Step 9: Add a failing TUI backend behavior test**

Add a POSIX-only backend regression using the same real-Git wrapper pattern, a real registry entry, and a fleet publisher counter. Call `backend.RemoveWorktree` and assert the warning, one publication, preserved residual file, and absent registry record.

This catches returning immediately on every non-dirty removal error before cleanup.

- [ ] **Step 10: Run the backend test and verify RED**

```bash
go test ./internal/cmd -run '^TestTUIBackendCompletesBookkeepingAfterGitDeregistersWithResidualFiles$' -count=1
```

Expected: FAIL because publication and registry cleanup do not run.

- [ ] **Step 11: Make backend cleanup conditional on semantic completion**

```go
removalErr := b.removeWorktreeFromRoot(repoRoot, row.Entry.Path, force, generation)
if removalErr != nil && !git.WorktreeWasRemoved(removalErr) {
	if strings.Contains(removalErr.Error(), "contains modified or untracked files") ||
		strings.Contains(removalErr.Error(), "has local changes") {
		return fmt.Errorf("worktree has uncommitted changes")
	}
	return removalErr
}

unregisterWorktreeRecord(registryRecord)
publishTUIFleetBestEffort(ctx, b.cfg)
if row.SessionLive && row.SessionName != "" {
	if err := b.tmux.KillSession(row.SessionName); err != nil {
		return err
	}
}
return removalErr
```

- [ ] **Step 12: Run focused and package tests and verify GREEN**

```bash
go test ./internal/cmd -run 'Test(TUIBackendCompletesBookkeepingAfterGitDeregistersWithResidualFiles|TUIBackendRemoveWorktreePublishesAfterSuccessfulMutation|TUIBackendRemoveWorktreeUnregistersLegacyEntry|TUIBackendRemoveWorktreeRejectsReplacementGeneration|RemoveLocalCompletesBookkeepingAfterGitDeregistersWithResidualFiles)$' -count=1
go test ./internal/cmd -count=1
```

Expected: PASS.

- [ ] **Step 13: Commit consumer bookkeeping**

```bash
git add internal/worktree/worktree.go internal/worktree/worktree_test.go internal/cmd/remove.go internal/cmd/remove_test.go internal/cmd/tui_backend.go internal/cmd/tui_test.go
git commit -m "Complete bookkeeping after Git deregistration"
```

Use the mandatory commit skill for the message body and attribution.

### Task 3: Refresh the TUI While Showing the Warning

**Files:**
- Modify: `internal/tui/model.go:470-505,1340-1347`
- Test: `internal/tui/model_test.go`

**Interfaces:**
- Consumes: `git.WorktreeWasRemoved(err error) bool`
- Produces: `actionDoneMsg{err: err, refresh: true}` for partial removal
- Produces: `applyActionDone` retains `m.err` while beginning or queuing refresh

- [ ] **Step 1: Add a failing model regression**

Configure `fakeBackend.removeErr` with a small test error implementing `WorktreeRemoved() bool`, execute `removeWorktreeCmd`, apply its result, and assert:

```go
require.Error(t, done.err)
assert.True(t, done.refresh)
updated, refreshCmd := updateModel(t, model, done)
require.NotNil(t, refreshCmd)
assert.ErrorContains(t, updated.err, "files remain")
assert.True(t, updated.fetching)
```

This catches returning before honoring `actionDoneMsg.refresh` when `err` is non-nil.

- [ ] **Step 2: Run the model test and verify RED**

```bash
go test ./internal/tui -run '^TestModelRefreshesAfterPartialRemovalWarning$' -count=1
```

Expected: FAIL because `done.refresh` is false and no refresh starts.

- [ ] **Step 3: Mark and honor warning refreshes**

Import `go.kenn.io/kwt/internal/git`. In `removeWorktreeCmd`:

```go
if err := m.backend.RemoveWorktree(context.Background(), row, force); err != nil {
	return actionDoneMsg{err: err, refresh: git.WorktreeWasRemoved(err)}
}
```

In `applyActionDone`, retain pending-row rollback, set `m.err`, clear `m.message`, then pass through the existing `refresh && fetching`, `refresh`, and no-refresh branches. Do not clear `m.err` before refresh begins.

- [ ] **Step 4: Run focused and package tests and verify GREEN**

```bash
go test ./internal/tui -run 'Test(ModelRefreshesAfterPartialRemovalWarning|ModelQueuesActionRefreshWhileFetchInFlight|ModelDeleteLiveWorktreeConfirmsAndCallsRemove)$' -count=1
go test ./internal/tui -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit TUI refresh behavior**

```bash
git add internal/tui/model.go internal/tui/model_test.go
git commit -m "Refresh dashboard after partial removal"
```

Use the mandatory commit skill for the message body and attribution.

### Task 4: Final Verification and Issue Closure

**Files:**
- Verify all files changed by Tasks 1-3
- Update: kata issue `0d7a`

**Interfaces:**
- Consumes: all completed behavior from Tasks 1-3
- Produces: verified repository state and a closed issue referencing the implementation

- [ ] **Step 1: Run repository verification**

```bash
gofmt -w internal/git/git_worktree.go internal/git/git_test.go internal/worktree/worktree.go internal/worktree/worktree_test.go internal/cmd/remove.go internal/cmd/remove_test.go internal/cmd/tui_backend.go internal/cmd/tui_test.go internal/tui/model.go internal/tui/model_test.go
git diff --check
make test
make build
```

Expected: every command exits zero.

- [ ] **Step 2: Review the implementation range**

```bash
git status --short --branch
git diff 62b3dee..HEAD --stat
git diff 62b3dee..HEAD
```

Confirm the diff contains only the typed result, consumer bookkeeping, TUI refresh, and focused regressions.

- [ ] **Step 3: Close the kata issue with evidence**

```bash
kata close 0d7a --done \
  --message "Classified Git deregistration with residual files as partial success; CLI and TUI now complete bookkeeping, refresh inventory, preserve files, and show an actionable warning. Verified focused regressions, make test, and make build." \
  --commit "$(git rev-parse HEAD)"
```

Expected: issue `0d7a` closes successfully against the latest implementation commit.
