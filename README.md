# kwt

`kwt` is a Git worktree manager with a terminal dashboard for people and a
scriptable CLI for agents and other tools. It creates isolated checkouts, opens
their tmux workspaces, shows their current state, and cleans them up safely
across all your projects.

## Work interactively

Run `kwt` inside a repository to register the project and open the dashboard:

```sh
kwt
```

The dashboard shows known projects and worktrees in one place. Create a branch
and worktree with `n`, search existing branches with `b`, attach to the selected
workspace with `enter`, and remove a worktree with `d`.

See the [quickstart](docs/get-started/quickstart.md) for the complete first-use
path. Ordinary directories can also become
[directory workspaces](docs/workflows/directory-workspaces.md).

## Automate worktrees

Agents and other tools can use plain commands and stable JSON for the same
lifecycle:

```sh
kwt add -b feature/new-ui
kwt exec feature/new-ui -- make test
kwt changes --json
kwt remove -b feature/new-ui
```

Use [agent workspaces](docs/workflows/agent-workspaces.md) for a complete
automation loop, [pull-request automation](docs/reference/pull-requests.md) for
inert imports and protected attachment, and the
[CLI reference](docs/reference/cli.md) for commands and machine contracts.

## Install

Install the current release with Go:

```sh
go install go.kenn.io/kwt/cmd/kwt@v0.5.0
```

Prebuilt macOS, Linux, and Windows archives and checksums are available from
[GitHub Releases](https://github.com/kenn-io/kwt/releases). See the
[installation guide](docs/get-started/install.md) for archive and source-build
instructions.

Requirements:

- Git 2.20 or newer. `kwt pr import` requires Git 2.42.0 or newer on macOS
  and Linux, or Git for Windows 2.53.0.windows.3 or newer. `kwt doctor`,
  `kwt prune --expired`, and `kwt prune --merged` require Git 2.31 or newer.
- tmux 2.1 or newer for workspace launch and `kwt tmux`.
- Go 1.27 or newer when installing or building from source.
- macOS 13 or newer, Linux, or Windows.

## Safety boundaries

Worktrees created from existing local or remote branches start inert. kwt does
not run repository setup, copy configured files, or launch a workspace until
you review the checkout and open it explicitly. Pull-request imports use a
protected session boundary and preserve exact push routing. Multi-machine sync
is opt-in and reports advisory state; follow the
[multi-machine guide](docs/multi-machine-sync.md) before enabling it.

## Used by Ghosthub

[Ghosthub](https://ghosthub.ai) bundles kwt to manage project registration,
linked worktrees, pull-request imports, and tmux workspaces across local and
SSH-hosted machines. Ghosthub is one consumer of the same services available to
other kwt clients and embedders.

## Go package

Applications can construct kwt's transport-neutral inventory, removal, and
generation-safe change-inspection services directly:

```go
import kwt "go.kenn.io/kwt"

inventory := kwt.NewInventoryService(kwt.InventoryServiceOptions{Source: source})
removals := kwt.NewRemovalService(kwt.RemovalServiceOptions{Home: kwtHome})
inspections := kwt.NewInspectionService(kwt.InspectionServiceOptions{
    Inventory: inventory,
})
```

The CLI and terminal dashboard use the same core services through kwt's local
daemon. The [package documentation](https://pkg.go.dev/go.kenn.io/kwt) contains
the public Go API. See [Embed and connect kwt](docs/integrations/embedding.md)
to choose between direct Go services, the local daemon or CLI, and a tmux
session endpoint.

## Configuration

Global configuration lives at `~/.config/kwt/config.toml`, or
`$KWT_HOME/config.toml` when `KWT_HOME` is set. Repository-local `.kwt.toml`
settings are trust-gated before use. `KWT_HOME` also holds `registry.json` and
`pull-requests.json`, so it isolates kwt's persistent state as a unit.

The [configuration reference](docs/reference/configuration.md) covers worktree
paths, layouts, agents, repository setup, trust, daemon and SSH policy, and
optional multi-machine settings.

## Documentation and releases

The maintained documentation lives at [kwt.sh](https://kwt.sh/). Start with:

- [Quickstart](docs/get-started/quickstart.md)
- [CLI reference](docs/reference/cli.md)
- [Configuration](docs/reference/configuration.md)
- [Changelog](docs/changelog.md)

kwt uses semantic-version tags. Pushing a `vMAJOR.MINOR.PATCH` tag runs the test
suite and publishes platform archives plus checksums. See the
[release checklist](docs/development/releasing.md) for the maintainer workflow.

To build the documentation locally:

```sh
make docs-install
make docs-build
make docs-serve
```

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).[^fork]

[^fork]:
    `kwt` began as a personalized fork of [`gwq`](https://github.com/d-kuro/gwq);
    the original project is Apache-2.0 licensed.
