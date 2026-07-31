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
| `internal/status`    | Git status collection.                                       |
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
verification. `make docs-deploy` deploys the docs site with Vercel.

## Test discipline

Tests should fail when protected behavior breaks. Prefer assertions over
observable outputs, persisted config, command results, rendered TUI state, exit
codes, and handoff intent. Avoid tests that merely mirror implementation logic,
grep source text, prove framework behavior, or pin absence of deleted code.
