# Test harness isolation design

## Goal

Make every canonical Go test target safe to run from a developer's normal
shell. Tests must not read or change the user's kwt state, inherit host Git
configuration, prompt for Git credentials, or contact external Git and HTTP
services.

This work fixes the release-blocking test harness problem found while verifying
the v0.5.0 documentation. It does not change kwt's production behavior.

## Confirmed failure

`make test` currently runs `go test ./...` with the caller's environment. A
focused run of
`TestTUIBackendCreateWorktreePublishesAfterSuccessfulMutation` opened an HTTPS
tunnel to `github.com`. The fixture configures
`https://github.com/example/kwt.git` as `origin`; worktree creation then tries to
fetch the remote default branch. The fetch failure is not the behavior under
test, so the test still passes after making the connection. With normal Git
settings, the same path can ask the developer for GitHub credentials.

The command tests also contain fleet configurations that point to
`hub.example.test`. Some paths try those URLs and pass only because the request
fails. These tests depend on accidental external failure rather than a
controlled fixture.

## Chosen approach

Use both fixture correction and harness-level isolation.

Fixture correction makes transport part of the test setup whenever the test can
reach it. Harness isolation protects the developer from a missed or future
fixture. Either layer alone is insufficient: local fixtures do not prevent a
new accidental remote, while a transport block can let a test keep passing for
the wrong reason.

## Test runner boundary

Add one repository-owned runner for the Go test suite. The Makefile's `test`,
`test-verbose`, `test-coverage`, `test-coverage-report`, and `bench` targets all
use it, passing their normal `go test` arguments through unchanged.

The runner preserves the developer's normal `HOME`, `PATH`, Go toolchain, and
Go caches. It creates a private temporary directory with mode `0700`, removes
it on every exit path, and uses that directory for:

- a baseline `KWT_HOME` that cannot reach the developer's kwt registry or
  configuration;
- an empty Git global configuration;
- the outbound-request recorder's address and log.

Before running Git or tests, the runner removes every inherited `GIT_*`
variable. It then sets `GIT_CONFIG_GLOBAL` to the private empty file,
`GIT_CONFIG_NOSYSTEM=1`, and `GIT_TERMINAL_PROMPT=0`. This prevents host aliases,
credential settings, hooks, and system configuration from changing fixture
behavior, and makes any missing credential fail without interaction.

The runner downloads Go modules before it denies test transports. Module
download uses the isolated, noninteractive Git configuration but retains normal
network access and normal Go caches. The actual test command additionally sets
`GIT_ALLOW_PROTOCOL=file`, so Git fixtures can use local repositories but a
missed HTTPS, SSH, or Git remote cannot connect.

## HTTP isolation

The runner starts a small loopback-only deny proxy before `go test`. It points
`HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` at that process and sets `NO_PROXY`
for `localhost`, `127.0.0.1`, and `::1`, so tests can continue to use local test
servers.

The proxy records every request or HTTPS `CONNECT` target and rejects it. After
the test process exits, the runner fails if the recorder saw any request, even
when all Go tests passed. The error lists the attempted hosts. This catches code
paths that swallow network failures, which is the behavior that hid the GitHub
connection.

The proxy is a small standard-library Go command kept with the test tooling. It
must bind an operating-system-assigned loopback port, publish that address to
the runner, reject requests without forwarding them, and shut down when the
runner exits. It must not require Python, netcat, a fixed free port, or an
installed third-party tool.

This is a test-process boundary, not a general network sandbox. Direct sockets
from code that deliberately ignores proxy variables remain outside its scope.
Git transport denial covers the concrete credential leak, and the recorder
covers the repository's normal HTTP clients.

## Fixture corrections

Tests that can perform Git transport must use repositories under `t.TempDir()`.
For the three known TUI worktree-creation cases, create a bare local `origin`,
push the fixture's `main` branch to it, and let the production fetch path
succeed:

- `TestTUIBackendCreateWorktreePublishesAfterSuccessfulMutation`
- `TestTUIBackendWorktreeCreationUsesSelectedRepositoryConfig`
- `TestTUIBackendCreateWorktreeDoesNotExpandRepositoryLocalTemplate`

Use the existing local-bare-repository pattern already present in the command
and Git tests. Keep fake GitHub URLs where a test only parses or displays an
identity and cannot invoke transport; those URLs are useful contract data.

Fleet tests must not rely on `hub.example.test` being unreachable. A test that
does not own fleet transport should replace the transport boundary with a stub
or a loopback server and assert only its intended outcome. Tests that do own
HTTP behavior keep their controlled `httptest` servers and custom transports.
No production fallback or compatibility path is added.

## Failure behavior

The runner returns failure when any of these conditions occurs:

- module preparation fails;
- the deny proxy cannot start or publish its address;
- `go test` fails;
- an external HTTP or HTTPS request reaches the deny proxy;
- cleanup of a child process cannot be completed normally.

Test output remains visible. When both the tests and isolation check fail, the
runner reports both results and exits unsuccessfully.

## Verification

Do not add a shell test that inspects Makefile or runner text. Verify the owned
behavior by executing it.

First reproduce the original focused test through the runner and confirm that
the local bare remote makes its fetch succeed without an external request.
Then run all canonical test variants needed to exercise argument forwarding,
including coverage and a benchmark selection.

Finally, run `make test` from a hostile parent environment that supplies:

- a separate `KWT_HOME` containing a sentinel file;
- Git configuration and credential-related environment variables that would
  be unsafe to inherit;
- an outer recording proxy.

The suite must finish without a prompt, the sentinel must remain unchanged,
the outer recorder must show no GitHub or fake-hub connection, and the inner
deny recorder must report no request. Run `make build` after the harness work to
confirm that the production build remains unchanged.

## Scope

This change is limited to Go test execution, transport-capable test fixtures,
and the Makefile targets that invoke `go test`. It does not isolate builds,
documentation commands, linting, or production commands. It does not replace
`HOME`, disable normal Go caches, add a production network policy, or change
the semantics of worktree creation and fleet publishing.
