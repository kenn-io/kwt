# Isolate Worktree Manager Test Registry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task with review checkpoints.

**Goal:** Keep `TestManagerAdd` from persisting temporary worktree records in the user's kwt registry.

**Architecture:** Change only `internal/worktree/worktree_test.go`. Give the test an isolated `KWT_HOME`, inject the package's existing `mockRemoteSourceState` into each manager, and assert that the isolated on-disk registry remains empty. No production behavior or registry API changes are needed.

**Tech Stack:** Go, `testing`, `testify`, kwt's existing worktree and registry test doubles.

## Global Constraints

- Preserve production registry tracking for real unreviewed worktree creation.
- Do not touch the user's default registry during tests.
- Follow the repository's test-first rule and verify focused plus full checks.
- Keep the change limited to the test harness and its regression assertion.

---

### Task 1: Isolate `TestManagerAdd` from the default registry

**Files:**

- Modify: `internal/worktree/worktree_test.go:345-415`

**Interfaces:**

- Consumes: `mockRemoteSourceState`, `Manager.openRemoteSourceState`, and `registry.New` already defined in the package.
- Produces: A `TestManagerAdd` regression test that exercises all existing cases without persisting registry entries.

- [ ] **Step 1: Write the failing regression assertion**

At the beginning of `TestManagerAdd`, set `KWT_HOME` to `t.TempDir()`. After each successful add, open `registry.New()` and assert that its list is empty. Do not inject the fake state yet; the current manager setup should persist the existing/unreviewed cases and make this test fail.

The assertion to add after the existing worktree-count check is:

```go
reg, err := registry.New()
require.NoError(t, err)
assert.Empty(t, reg.List())
```

- [ ] **Step 2: Run the focused test and verify the expected failure**

Run:

```bash
KWT_HOME="$(mktemp -d)" go test ./internal/worktree -run '^TestManagerAdd$' -count=1
```

Expected: `TestManagerAdd/WithGeneratedPath` or `TestManagerAdd/WithCustomPath` fails because the current implementation persists an unreviewed-source entry.

- [ ] **Step 3: Inject the fake remote-source state**

After creating each manager in the subtest, install the existing in-memory state:

```go
state := &mockRemoteSourceState{}
m.openRemoteSourceState = func() (remoteSourceState, error) {
	return state, nil
}
```

Keep the existing `mockGit` assertions and the isolated-registry assertion. This removes the disk-side effect while retaining the `Add` behavior under test.

- [ ] **Step 4: Run the focused test and verify it passes**

Run:

```bash
KWT_HOME="$(mktemp -d)" go test ./internal/worktree -run '^TestManagerAdd$' -count=1
```

Expected: all three subtests pass and the isolated registry remains empty.

- [ ] **Step 5: Format and inspect the focused diff**

Run:

```bash
gofmt -w internal/worktree/worktree_test.go
git diff --check
git diff -- internal/worktree/worktree_test.go
```

Expected: only the test setup and regression assertion change; no production files are modified.

- [ ] **Step 6: Run repository verification**

Run with `HOME` and temp paths redirected, but without a global `KWT_HOME` override so config tests that set `HOME` internally retain their intended behavior:

```bash
HOME="$scratch_home" XDG_CONFIG_HOME="$scratch_config" TMPDIR="$scratch_tmp" GH_CONFIG_DIR="$scratch_gh" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1 go test ./...
make build
go vet ./...
make lint
make fmt
```

Expected: all commands exit successfully and formatting produces no additional diff.
