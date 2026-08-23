# Contributing

`kwt` is a Go CLI/TUI project. Keep changes small, terminal-friendly, and
verified with the repo's commands.

## Repository layout

| Path                 | Responsibility                                               |
| -------------------- | ------------------------------------------------------------ |
| `cmd/kwt`            | Main package.                                                |
| `internal/cmd`       | Cobra command wiring and real backend integration.           |
| `internal/tui`       | Bubble Tea dashboard model, rendering, and pure TUI helpers. |
| `internal/config`    | Global and local config loading, trust, and persistence.     |
| `internal/discovery` | Worktree discovery.                                          |
| `internal/status`    | Git status collection and worktree change inspection.        |
| `internal/tmux`      | tmux session, layout, and runner behavior.                   |
| `internal/worktree`  | Worktree creation, setup commands, and copied files.         |
| `pkg/models`         | Shared data models.                                          |
| `docs`               | Zensical docs and maintained design notes.                   |

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
go test ./internal/tui
go test ./internal/config ./internal/cmd ./internal/tui
```

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
