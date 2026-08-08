# Isolate Worktree Manager Test Registry Design

## Goal

Prevent `internal/worktree` tests from writing registry entries to the user's
default kwt registry when exercising existing/unreviewed worktree creation.

## Root cause

`Manager.Add` uses the remote-source registry for the default
`createBranch=false` path. `TestManagerAdd` constructs a manager without
replacing its `openRemoteSourceState` dependency, so the test opens the real
registry. The test directories are removed by `t.TempDir`, but the registry
records survive and later appear as stale entries.

## Design

Keep production behavior unchanged. The test will set `KWT_HOME` to a
test-owned directory and assert that the test leaves that registry empty. Each
subtest will inject the existing `mockRemoteSourceState`, matching the adjacent
remote-source tests. This makes the test independent of filesystem-backed
registry state while the explicit empty-registry assertion catches regressions
that reintroduce persistence.

## Error handling

The test fails immediately if the isolated registry cannot be opened or if any
entry is persisted. The fake state continues to exercise creation ownership,
generation finalization, and worktree result behavior without involving disk
state.

## Verification

Run the focused `TestManagerAdd` test in a scratch environment, then run the
full Go test suite, build, vet, and the repository's lint/format checks. The
focused test must pass without creating entries in the user's registry.
