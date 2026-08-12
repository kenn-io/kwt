# Short tmux Session Names Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace ceremonial standard-worktree tmux names with short repository-and-branch names while preserving verified protected pull-request endpoints.

**Architecture:** Keep naming deterministic and stateless in `internal/tmux`. Parse overlapping managed name families in most-specific-first order. Trust a persisted protected pull-request session name only after its repository, branch, path, generation, and old-or-new deterministic formula are verified.

**Tech Stack:** Go, testify, tmux naming helpers, KWT pull-request provenance, Markdown.

## Global Constraints

- Standard worktrees use `kwt-{repository}-{branch}-{path-hash8}`.
- Directory workspace names remain unchanged.
- The path hash remains eight lowercase hexadecimal characters derived from the absolute worktree path.
- Do not add a registry, collision allocator, ordinary-session adoption, rename, fallback, or dual attach path.
- Existing verified protected pull-request imports retain their persisted exact session name and socket; new imports use only the short formula.
- Use test-first development and commit each independently verified task.

---

### Task 1: Short deterministic names and parser precedence

**Files:**
- Modify: `internal/tmux/session_test.go`
- Modify: `internal/tmux/session.go`
- Modify: `internal/tmux/manager_test.go`
- Modify: `internal/tmux/manager.go`

**Interfaces:**
- Produces: `WorkspaceSessionName(info *url.RepositoryInfo, branch, worktreePath string) string`
- Produces: `MatchesWorkspaceSessionName(name string, info *url.RepositoryInfo, branch, worktreePath string) bool`
- Preserves: `DirWorkspaceSessionName(name, path string) string`

- [ ] **Step 1: Write failing naming tests**

Change the standard-worktree golden assertion to `kwt-kwt-feature-foo-9cc4e551`. Assert the name omits `github-com` and `wesm`. Add a test that `MatchesWorkspaceSessionName` accepts the new name and the prior deterministic long name for identical inputs, while rejecting an arbitrary name and a name for another path.

- [ ] **Step 2: Verify the naming tests fail**

Run:

```bash
go test ./internal/tmux -run 'TestWorkspaceSessionName|TestMatchesWorkspaceSessionName' -count=1
```

Expected: failure because the golden name is still long and the predicate does not exist.

- [ ] **Step 3: Implement the minimal naming helpers**

Make `WorkspaceSessionName` format only repository, branch, and the existing path hash. Keep the previous formula private. Implement `MatchesWorkspaceSessionName` as exact equality against the current or previous formula computed from the supplied inputs.

- [ ] **Step 4: Verify naming passes**

Run `go test ./internal/tmux -run 'TestWorkspaceSessionName|TestMatchesWorkspaceSessionName|TestDirWorkspaceSessionName' -count=1`.

- [ ] **Step 5: Write failing parser tests**

Update `TestParseWorkspaceSession` to use `kwt-kwt-feature-foo-abcd1234`. Add coverage proving `kwt-workspace-dir-notes-abcd1234` remains workspace identifier `dir-notes`, and `kwt-review-pr123-20240101120000` remains review identifier `pr123` rather than matching the eight-hex worktree suffix.

- [ ] **Step 6: Verify parser tests fail**

Run `go test ./internal/tmux -run 'TestParseWorkspaceSession|TestParseDirWorkspaceSession|TestParseLegacySessionStillWorks' -count=1`.

- [ ] **Step 7: Implement most-specific-first parsing**

Parse the 14-digit run form first, the unchanged directory-workspace form second, and the broad short worktree form last. Preserve `Context: "workspace"` and the directory identifier without its `kwt-workspace-` prefix.

- [ ] **Step 8: Verify and commit Task 1**

Run `go test ./internal/tmux -count=1`, stage only the four tmux files, and commit the naming/parser behavior.

---

### Task 2: Preserve verified protected pull-request endpoints

**Files:**
- Modify: `internal/cmd/pr_test.go`
- Modify: `internal/cmd/pr.go`
- Modify: `internal/lifecycle/source_test.go`
- Modify: `internal/lifecycle/source.go`
- Modify: `docs/reference/pull-requests.md`

**Interfaces:**
- Consumes: `tmux.MatchesWorkspaceSessionName(name, info, branch, worktreePath) bool`
- Preserves: `tmux.ProtectedWorkspaceSocketName(session, worktreeDir) string`
- Produces: inventory entries whose `SessionName` and `TmuxSocketName` use the verified persisted protected endpoint.

- [ ] **Step 1: Write a failing provenance identity test**

Extend `containsPRWorkspace` coverage with a record whose stored session name uses the previous deterministic formula while the live workspace uses the short formula. Assert it matches when repository, branch, path, and generation agree. Retain the arbitrary-name rejection and mismatched-generation rejection.

- [ ] **Step 2: Verify the provenance test fails**

Run:

```bash
go test ./internal/cmd -run 'TestTransferredProvenanceMatchesLiveWorkspaceAcrossAliases|TestContainsPRWorkspace' -count=1
```

Expected: the previous persisted name is rejected when live inventory derives the short name.

- [ ] **Step 3: Validate persisted names against authorized identities**

Update `prWorkspaceIdentityMatches` to prove the live repository is connected to the provenance record, then validate the persisted name with `tmux.MatchesWorkspaceSessionName` against canonical identities returned by `pullrequest.ProvenanceRepositoryIdentities(recorded)`. Never trust an arbitrary persisted name and do not require equality with the newly derived live name.

- [ ] **Step 4: Verify command provenance passes**

Run `go test ./internal/cmd -run 'TestTransferredProvenanceMatchesLiveWorkspaceAcrossAliases|TestContainsPRWorkspace|TestRunPRAttach' -count=1`.

- [ ] **Step 5: Write failing protected-inventory tests**

Persist an old deterministic protected name for an entry whose derived name is short. Assert `annotateProtectedSockets` replaces the entry session name with the verified persisted name and sets its derived protected socket. Add a companion case proving an arbitrary persisted name is ignored.

- [ ] **Step 6: Verify protected inventory fails**

Run `go test ./internal/lifecycle -run 'TestAnnotateProtectedSockets' -count=1`.

- [ ] **Step 7: Implement bounded inventory annotation**

Require matching canonical path, branch, generation, and connected repository identity. Validate the persisted name against authorized provenance identities with `tmux.MatchesWorkspaceSessionName`. Only then set both fields:

```go
entries[index].SessionName = workspace.SessionName
entries[index].TmuxSocketName = tmux.ProtectedWorkspaceSocketName(
    workspace.SessionName,
    workspace.Path,
)
```

- [ ] **Step 8: Update pull-request examples**

Replace old standard-worktree examples in `docs/reference/pull-requests.md` with short names such as `kwt-widget-pr-17-feature-rendering-a1b2c3d4`. Leave directory-workspace documentation unchanged.

- [ ] **Step 9: Verify and commit Task 2**

Run:

```bash
gofmt -w internal/tmux/session.go internal/tmux/session_test.go internal/tmux/manager.go internal/tmux/manager_test.go internal/cmd/pr.go internal/cmd/pr_test.go internal/lifecycle/source.go internal/lifecycle/source_test.go
go test ./internal/tmux ./internal/cmd ./internal/lifecycle ./internal/pullrequest -count=1
```

Stage only the protected-provenance, lifecycle, and documentation files and commit them.

---

### Task 3: Repository verification and handoff

**Files:**
- Review: every file changed from `origin/main`

**Interfaces:**
- Consumes: completed naming and protected-provenance behavior.
- Produces: a verified branch suitable for a KWT pull request.

- [ ] **Step 1: Run repository quality gates**

Run `make test`, `make lint`, `make build`, and `make docs-check`. All must pass.

- [ ] **Step 2: Review scope**

Run `git diff --check origin/main...HEAD`, `git diff --stat origin/main...HEAD`, and `git status --short --branch`. Confirm only planned files changed and the worktree is clean.

- [ ] **Step 3: Push and open the pull request**

Push `short-session-names`, open a concise KWT pull request, and do not poll CI. Close kata `prvh` only after verification, using the implementation commit as evidence.
