# Test Harness Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make local and CI Go tests fail closed instead of contacting external Git or HTTP services or inheriting developer kwt and Git state.

**Architecture:** A standard-library Go runner owns environment cleanup, Git version enforcement, module preparation, a loopback deny proxy, child-process execution, and cleanup. Existing transport-capable fixtures become local, then Makefile and both CI workflows invoke the same runner with their existing flags and package selections.

**Tech Stack:** Go 1.27 standard library, Git 2.32+, `net/http`, `os/exec`, local bare Git repositories, `httptest`, Make, and GitHub Actions.

## Global Constraints

- Do not change production kwt behavior.
- Keep kwt's general runtime minimum at Git 2.20; Git 2.32 is required only by the test runner.
- Preserve the caller's `HOME`, `PATH`, Go toolchain, `GOCACHE`, and `GOMODCACHE`.
- Strip inherited `GIT_*` variables, use an empty global Git config, disable system Git config, and prohibit interactive credentials.
- Permit only Git's `file` transport during the test phase.
- Allow loopback HTTP test servers and reject every proxied external HTTP or HTTPS request.
- Support Unix and Windows without a POSIX shell or external proxy utility.
- Keep Windows CI's explicit package list.
- Do not add tests that inspect Makefile or workflow text; execute those entry points instead.
- Load `kenn:commit` before every commit. Never amend or squash.

---

### Task 1: Build and test the isolated runner core

**Files:**
- Create: `internal/testharness/runner.go`
- Create: `internal/testharness/runner_test.go`

**Interfaces:**
- Produces: `func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`
- Owns: Git version parsing, environment replacement, deny-proxy lifecycle, and child `go` commands.

- [ ] **Step 1: Write failing Git-version and environment tests**

Create table tests for `parseGitVersion` with `2.31.8`, `2.32.0`, and
`2.55.0.windows.1`. Add `isolatedEnvironment` coverage that starts with
`GIT_DIR`, mixed-case proxy variables, normal `HOME` and `PATH`, then asserts
that normal variables remain while inherited Git and proxy values are replaced
with scratch paths, noninteractive Git, and file-only transport.

- [ ] **Step 2: Verify the tests fail for missing runner functions**

Run:

```sh
go test ./internal/testharness
```

Expected: compile failure naming `parseGitVersion` or `isolatedEnvironment`.

- [ ] **Step 3: Implement minimal version and environment policy**

In `runner.go`, define a private semantic Git version parser and an environment
builder. Compare major and minor numerically, return a direct error for Git
older than 2.32, remove keys case-insensitively on Windows-safe semantics, and
append exactly the runner-owned values:

```go
GIT_CONFIG_GLOBAL=<scratch gitconfig>
GIT_CONFIG_NOSYSTEM=1
GIT_TERMINAL_PROMPT=0
GIT_ALLOW_PROTOCOL=file
KWT_HOME=<scratch kwt>
HTTP_PROXY=http://<deny proxy>
HTTPS_PROXY=http://<deny proxy>
ALL_PROXY=http://<deny proxy>
NO_PROXY=localhost,127.0.0.1,::1
```

Keep a preparation variant that omits `GIT_ALLOW_PROTOCOL` and retains the
caller's proxy settings for `go mod download`.

- [ ] **Step 4: Write a failing deny-proxy behavior test**

Start the wished-for recorder with an OS-assigned loopback port. Send an HTTP
request and an HTTPS-style `CONNECT` request through it, then assert that both
hosts are recorded, both requests are rejected, and shutdown returns.

- [ ] **Step 5: Verify the proxy test fails for the missing recorder**

Run `go test ./internal/testharness -run 'TestDenyProxy' -v`.

Expected: compile failure naming the recorder constructor.

- [ ] **Step 6: Implement the deny proxy**

Use `net.Listen("tcp", "127.0.0.1:0")` and `http.Server`. Record `Method` plus
`Host` under a mutex, return `502 Bad Gateway`, and never dial upstream. `Run`
will later:

1. create and defer removal of a scratch directory;
2. create an empty Git config and `kwt` directory;
3. run `git version` under the sanitized preparation environment;
4. fail if the version is older than 2.32;
5. run `go mod download` under the preparation environment;
6. start the deny proxy;
7. run `go test` with caller arguments under the isolated test environment;
8. stop the proxy, print every recorded request, and return failure if any was recorded;
9. preserve the test command's stdout, stderr, and failure even when the isolation check also fails.

- [ ] **Step 7: Write failing runner orchestration tests**

Inject the executable lookup and child-command boundary. Add one test where a
reported Git version below 2.32 prevents module preparation, and one where the
child test command exits successfully after making a request through the deny
proxy. Assert that `Run` returns failure in both cases and lists the attempted
host for the second case.

- [ ] **Step 8: Verify the orchestration tests fail, then complete `Run`**

Run the two tests by exact name. Expected before the orchestration code is
complete: assertion failure because `Run` returns success or invokes the wrong
child step. Add only the injected seams and the nine-step sequencing above
needed to make them pass.

- [ ] **Step 9: Verify the runner package passes**

Run `go test ./internal/testharness -v`.

Expected: all runner-owned behavior passes with no leaked goroutine or process.

- [ ] **Step 10: Commit the runner core**

Load `kenn:commit`, stage the two runner files, and commit with a subject such as
`Isolate Go test processes from host state`.

### Task 2: Correct transport-capable fixtures

**Files:**
- Modify: `internal/cmd/tui_test.go`
- Modify: `internal/cmd/add_test.go`
- Modify: `internal/fleet/client_test.go`

**Interfaces:**
- Consumes: the existing `runTUITestGit` helper and fleet dependency seams.
- Produces: Git fetches that succeed against local bare repositories and fleet failures owned by loopback fixtures.

- [ ] **Step 1: Add a local-origin fixture helper**

Add `addLocalTUIOrigin(t, repoPath) string`, used by three tests. It creates
`origin.git` below `t.TempDir()`, initializes it bare with `main`, adds it as the
fixture's origin, and pushes `main`. Do not add a helper for a single test.

- [ ] **Step 2: Replace the three fetch-capable GitHub origins**

Use the local helper in:

- `TestTUIBackendCreateWorktreePublishesAfterSuccessfulMutation`
- `TestTUIBackendWorktreeCreationUsesSelectedRepositoryConfig`
- `TestTUIBackendCreateWorktreeDoesNotExpandRepositoryLocalTemplate`

Run each test by exact name with `GIT_ALLOW_PROTOCOL=file` and isolated Git
configuration. Expected: all three pass and their new worktree directories
exist.

- [ ] **Step 3: Make command config use a loopback fleet failure**

In `initCommandTestConfig`, start an `httptest.Server` that returns
`503 Service Unavailable`, register `server.Close` with `t.Cleanup`, and write
its URL into `hub_url`. This keeps incidental best-effort publication local and
failed without relying on DNS.

- [ ] **Step 4: Clean the fleet proxy re-exec environment**

Extend `TestClientRejectsPlaintextTailnetBeforeEnvironmentProxy` to strip
`HTTPS_PROXY`, `ALL_PROXY`, and their lower-case forms before installing its
controlled HTTP proxy variables.

- [ ] **Step 5: Run focused fixture packages**

Run:

```sh
go test ./internal/cmd ./internal/fleet
```

Expected: both packages pass without external transport.

- [ ] **Step 6: Commit fixture corrections**

Load `kenn:commit`, stage only the three test files, and commit with a subject
such as `Keep test transports inside local fixtures`.

### Task 3: Add the executable runner and route repository entry points

**Files:**
- Create: `internal/testharness/cmd/main.go`
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `testharness.Run` and arbitrary arguments after `--`.
- Produces: `go run ./internal/testharness/cmd -- <go-test-arguments>` for local and CI callers.

- [ ] **Step 1: Add the command entry point**

Implement only argument forwarding and process exit:

```go
func main() {
    os.Exit(testharness.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
```

Reject an empty argument list with a usage error from `Run`.

- [ ] **Step 2: Exercise the command before changing configuration**

Run:

```sh
go run ./internal/testharness/cmd -- -run '^TestTUIBackendCreateWorktreePublishesAfterSuccessfulMutation$' ./internal/cmd
```

Expected: the runner completes successfully and reports no outbound request.

- [ ] **Step 3: Route all Makefile Go test targets through the command**

Define one Make variable for `go run ./internal/testharness/cmd --` and use it
for `test`, `test-verbose`, `test-coverage`, `test-coverage-report`, and `bench`.
Keep their current test flags and coverage report generation.

- [ ] **Step 4: Route CI and release tests through the command**

Change Linux CI to pass `./...` and Windows CI to pass its unchanged explicit
package list after the runner command. Remove the separate CI module-download
step because the runner owns it. Change the tag release workflow's test step to
the same runner so a release cannot bypass isolation.

- [ ] **Step 5: Validate configuration by executing it**

Run:

```sh
make test
make test-coverage
go run ./internal/testharness/cmd -- -run '^$' -bench '^$' ./internal/testharness
```

Expected: tests and coverage pass; the benchmark-shaped invocation accepts its
flags and package argument. Do not add Makefile or YAML content assertions.

- [ ] **Step 6: Commit entry-point adoption**

Load `kenn:commit`, stage the command, Makefile, and two workflow files, and
commit with a subject such as `Run local and CI tests through isolation`.

### Task 4: Prove hostile-state isolation and close the blocker

**Files:**
- Modify only if behavioral verification exposes a real fixture dependency.

**Interfaces:**
- Verifies: user kwt state, Git state, credential prompts, Git transport, HTTP proxy recording, Windows-style explicit package arguments, and normal builds.

- [ ] **Step 1: Prepare hostile ambient state outside the worktree**

Create an owner-private temporary directory containing a sentinel `KWT_HOME`, a
global Git config with a failing alias, credential helper, identity, and
`url.*.insteadOf` rewrite, plus an outer loopback recorder. Preserve normal
`HOME`, `PATH`, and Go caches.

- [ ] **Step 2: Run the canonical suite under hostile state**

Run `make test` with the hostile `KWT_HOME`, `GIT_*`, and proxy variables.

Expected: no terminal prompt; tests pass; the sentinel contents and timestamps
remain unchanged; the runner reports no proxied request. If a test relied on
global identity or URL rewriting, correct that fixture locally and repeat.

- [ ] **Step 3: Run the CI-equivalent explicit selection**

Invoke the runner with the exact Windows package list from `ci.yml` on the
current platform.

Expected: the selection passes with no outbound request. The GitHub Windows job
will provide the platform-specific execution after push; do not claim a local
Windows run.

- [ ] **Step 4: Re-run runner failure observables**

Run the focused runner tests from Task 1 that reject old Git and fail after a
recorded outbound request. Expected: both pass against the final entry points.

- [ ] **Step 5: Run build and final diff checks**

Run:

```sh
make build
git diff --check
git status --short
```

Expected: the production binary builds, the diff is clean, and only planned
files remain changed.

- [ ] **Step 6: Commit any verification-driven fixture repair**

If Step 2 found a real ambient dependency, load `kenn:commit` and commit only
that focused repair. If no files changed, do not create an empty commit.

- [ ] **Step 7: Update kata issue `x52x`**

After fresh verification and final commits, close the issue with the exact
commits and behavioral evidence. If any required platform or isolation behavior
remains unverified, leave it open with `needs-review` and a precise comment.

### Task 5: Return to the v0.5.0 release documentation

**Files:**
- Continue from: `docs/superpowers/plans/2026-08-28-v0.5.0-release-documentation.md`

**Interfaces:**
- Consumes: a canonical `make test` that no longer risks external credentials or state.
- Produces: the already-approved changelog and Zensical documentation reset.

- [ ] **Step 1: Resume the release documentation plan at Task 1**

Follow the existing plan from its changelog source audit through homepage,
reader journeys, integration documentation, rendered inspection, and final
release checks.

- [ ] **Step 2: Keep the release boundary**

Commit accepted documentation, remove temporary `docs/superpowers` planning
artifacts at the plan's final gate, and stop before creating `v0.5.0` or a
GitHub release.
