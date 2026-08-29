# Contributing

`kwt` is a Go CLI/TUI project. Keep changes small, terminal-friendly, and
verified with the repo's commands.

## Repository layout

| Path                   | Responsibility                                               |
| ---------------------- | ------------------------------------------------------------ |
| `cmd/kwt`              | Main package.                                                |
| `kwt.go`, `ssh.go`     | Public embeddable service surface.                           |
| `service`              | Shared operation and typed-error contracts.                  |
| `internal/cmd`         | Cobra command wiring and real backend integration.           |
| `internal/daemon`      | Same-machine service host and client.                        |
| `internal/lifecycle`   | Inventory and guarded lifecycle services.                    |
| `internal/ssh`         | OpenSSH route, prompt, and lease ownership.                  |
| `internal/tui`         | Bubble Tea dashboard model, rendering, and pure TUI helpers. |
| `internal/config`      | Global and local config loading, trust, and persistence.     |
| `internal/discovery`   | Worktree discovery.                                          |
| `internal/status`      | Git status collection and worktree change inspection.        |
| `internal/tmux`        | tmux session, layout, and runner behavior.                   |
| `internal/worktree`    | Worktree creation, setup commands, and copied files.         |
| `internal/testharness` | Isolated Go test runner.                                     |
| `pkg/models`           | Shared data models.                                          |
| `docs`                 | Zensical docs and maintained design notes.                   |

## Local installation

Install the current checkout into the shared Go bin directory with:

```sh
make install
```

The target defaults to `$(go env GOPATH)/bin`, even when a toolchain manager
sets a private `GOBIN`. Override it when needed with
`make INSTALL_DIR=/custom/bin install`. After installing, verify that sibling
repositories resolve the refreshed binary:

```sh
command -v kwt
kwt --version
```

## Local checks

```sh
make test
make build
```

Focused package tests are useful while iterating:

```sh
make test TEST_PACKAGES=./internal/tui
make test TEST_PACKAGES="./internal/config ./internal/cmd ./internal/tui"
```

The supported Make and CI entrypoints start the dependency-free bootstrap from
an explicit platform and toolchain environment. Bootstrap compilation and its
own tests do not inherit ambient proxies, Git settings, Go authentication,
private-module settings, `KWT_HOME`, or custom token variables. Root
toolchain and module downloads use `proxy.golang.org` and `sum.golang.org`;
private and regional module mirrors are intentionally not used by test
commands.

The bootstrap passes the caller's `KWT_HOME` only to the inner runner so it can
identify the configured `fleet.token_env`. If that setting names a required
platform variable such as `PATH`, `HOME`, or `LANG`, the runner stops before
module preparation or tests. A relative `KWT_HOME` is resolved before the
runner changes to the repository root.

The inner test runner requires Git 2.32 or newer. After modules are available,
it isolates kwt and Git state, restricts inherited Git commands to local file
transport, and records requests from proxy-aware HTTP clients. Direct sockets,
custom transports, and subprocesses that replace the guarded environment are
outside this boundary; it is not an operating-system network sandbox. Use the
documented Make entrypoints rather than invoking the Go bootstrap directly.

## OpenSSH projection maintenance

Kwt's route identity retains every normalized `ssh -G` directive, but
execution replays only the positive policy documented as
`kwt.openssh.projection.v1`. Total replay is not valid: supported OpenSSH
versions emit entries such as `Host` that are not accepted as command-line
options.

Whenever CI's supported OpenSSH version changes, review new and changed
`ssh -G` directives against `internal/ssh/testdata/projection_v1.json` and the
pinned Ghosthub parity matrix. A directive absent from the positive set remains
identity-only. Before a projection policy ships, correct that policy and its
parity evidence in place. After release, adding, removing, or changing
projection handling requires a new policy version and matching Ghosthub parity
evidence. Do not pin tests to an incidental vendor version string.

## Docs

Install the docs toolchain:

```sh
make docs-install
```

Build or preview:

```sh
make docs-build
make docs-serve
```

`make docs-check` runs the same strict Zensical build used for docs
verification. Pull requests run that check in CI. `make docs-deploy` deploys the
generated `docs/site` output to the `kwt-docs` Vercel project. Override
`VERCEL_SCOPE` or `VERCEL_PROJECT` when deploying a fork.

Website binaries live on the orphan `website-assets` branch rather than in the
documentation history. The docs targets fetch and materialize the required
asset set into the ignored `docs/assets` directory before Zensical runs. Update
and push that branch before building or deploying a refreshed screenshot.

## Releases

Version tags publish platform archives and checksums through GoReleaser. See
[Releasing kwt](releasing.md) for the complete maintainer checklist. Do not move
or replace an existing release tag.

## Test discipline

Tests should fail when protected behavior breaks. Prefer assertions over
observable outputs, persisted config, command results, rendered TUI state, exit
codes, and handoff intent. Avoid tests that merely mirror implementation logic,
grep source text, prove framework behavior, or pin absence of deleted code.
