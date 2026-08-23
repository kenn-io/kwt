# kwt

`kwt` is a focused Git worktree manager for cross-project development and
tmux-backed agent workspaces.

Run `kwt` from a terminal to open the dashboard. The dashboard shows known
projects and worktrees, lets you create/delete worktrees, and attaches to a
tmux workspace for the selected branch.

## Used by Ghosthub

[Ghosthub](https://ghosthub.ai) bundles kwt as its worktree helper. The native
macOS terminal uses kwt to register projects, manage linked-worktree lifecycle,
and resolve canonical tmux sessions for local and SSH-hosted workspaces. No
separate system kwt installation is required for Ghosthub's bundled workflow;
the standalone CLI remains available for terminal-first use.

## Go package

Applications such as Forge embed kwt through the module root:

```go
import kwt "go.kenn.io/kwt"

inventory := kwt.NewInventoryService(kwt.InventoryServiceOptions{Source: source})
removals := kwt.NewRemovalService(kwt.RemovalServiceOptions{Home: kwtHome})
inspections := kwt.NewInspectionService(kwt.InspectionServiceOptions{
    Inventory: inventory,
})
```

The root package contains transport-neutral inventory, lifecycle, and
generation-safe change-inspection contracts. The CLI/TUI adapt them to kwt's
local daemon; Go applications can construct the same services directly without
Huma, Cobra, or daemon ownership.

## Install

```bash
go install go.kenn.io/kwt/cmd/kwt@latest
```

Pin a version with `@v0.3.0`. Release automation was introduced after that tag;
newer versions also publish prebuilt archives and checksums on
[GitHub Releases](https://github.com/kenn-io/kwt/releases).

From source:

```bash
git clone https://github.com/kenn-io/kwt.git
cd kwt
go build -o kwt ./cmd/kwt
```

## Daily Use

```bash
# Open the cross-project dashboard
kwt
kwt tui

# Create a worktree and launch its default workspace
kwt add -b feature/new-ui

# Create without launching tmux
kwt add --no-launch -b feature/new-ui

# Create a tracking worktree from an existing remote branch
kwt add --from origin/feature/review feature/review

# After reviewing that checkout, explicitly start its workspace
kwt open "$(kwt get feature/review)"

# Open a worktree workspace, creating its session when needed
kwt open

# Open an exact worktree, even when it is outside the global worktree base
kwt open /path/to/worktree

# Establish that exact workspace session for another tmux client
kwt open /path/to/worktree --start-session

# Register, inventory, and open a plain directory workspace
kwt workspace add ~/notes
kwt workspace list --json
kwt open ~/notes --start-session

# Inspect worktrees and git status
kwt list
kwt status
kwt changes
kwt changes /path/to/worktree --json

# Discover and import a GitHub PR without launching tmux
kwt pr list --project github.com/acme/widget --json
kwt pr import 17 --project github.com/acme/widget --json

# Import and establish the canonical tmux workspace for another client
kwt pr import 17 --project github.com/acme/widget \
  --start-session --json

# Use a worktree path in scripts
cd "$(kwt get feature/new-ui)"
kwt exec feature/new-ui -- npm test

# Delete a worktree
kwt remove feature/new-ui
kwt remove -b feature/new-ui

# Diagnose and repair structural worktree metadata
kwt doctor
kwt doctor --fix

# Preview clean worktrees whose exact GitHub pull request is merged
kwt prune --merged --dry-run
```

When `-b` creates a branch, `kwt` fetches `origin` and starts from its default
branch. If that remote base is unavailable, it falls back to local `main`, then
`master`, then the branch checked out in the primary worktree.

Existing-branch worktrees are inert on creation: branch mutation and checkout
run without repository-configured hooks or filters and without kwt credential
variables, and `kwt` does not copy configured files, run setup commands,
or launch a layout against contributor-controlled content. Existing branch
names are treated literally when building the destination path. The checkout
still participates in ordinary status and fleet observation. Review it before
using `kwt open` to explicitly create and attach its workspace.

### Dashboard Keys

| Key       | Action                                  |
| --------- | --------------------------------------- |
| `up/down` | Move selection                          |
| `enter`   | Attach to selected workspace            |
| `n`       | Create a new branch and worktree        |
| `b`       | Search existing branches for a worktree |
| `L`       | Select workspace layout                 |
| `P`       | Switch active project perspective       |
| `p`       | Filter visible projects                 |
| `/`       | Search rows                             |
| `d`       | Delete selected worktree                |
| `K`       | Kill selected live workspace            |
| `s`       | Sync a remote-only branch row locally   |
| `c`       | Open a shell in the selected worktree   |
| `r`       | Refresh                                 |
| `?`       | Toggle help                             |
| `q`       | Quit                                    |

## Configuration

Global config lives at `~/.config/kwt/config.toml`, or
`$KWT_HOME/config.toml` when `KWT_HOME` is set. Repository-local overrides live
in `.kwt.toml` and are trust-gated before use. When `KWT_HOME` is set, that same
directory also holds `registry.json` and `pull-requests.json`, isolating kwt's
persistent state as a unit. Without it, each store follows its documented
platform config-directory behavior.

`config.toml` is the source of truth for layouts and agent commands. Workspaces
launch as a blank single-pane session unless a layout is selected — via
`--layout`, `--select-layout`, the TUI `L` key, or `layouts.default`. The name
`none` is reserved and always means a blank session. When kwt creates the file
for the first time, it writes a starter set of agents and layout presets to opt
into; after that, kwt does not rely on hidden layout or agent defaults in the
binary.

```toml
[worktree]
basedir = "~/.kwt/worktrees"
auto_mkdir = true

[naming]
template = "{{.FullPath}}/{{.Branch}}"

[naming.sanitize_chars]
"/" = "-"
":" = "-"

[agents]
codex = "codex"
claude = "claude"
roborev = "roborev tui"

[layouts]
# default = "quad"  # unset or "none" = blank single-pane session
auto_launch_on_add = true

[[layouts.presets]]
name = "quad"
arrange = "even-horizontal"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]

[[layouts.presets]]
name = "stack"
arrange = "even-vertical"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]

# Optional, opt-in multi-machine sync.
[fleet]
enabled = false
host_id = "laptop"
hub_url = "https://kwt-hub.example.net"
token_env = "KWT_FLEET_TOKEN"
# token_file = "~/.config/kwt/fleet.token"

[fleet.hub]
listen_addr = "127.0.0.1:8787"
store_path = "~/.config/kwt/fleet/state.json"
```

Pane entries are shell commands. `agent:<name>` expands through the `[agents]`
table before tmux starts, so command flags are configured once. Approval or
sandbox bypass flags are an explicit opt-in in your local config.

Useful config commands:

```bash
kwt config list
kwt config get layouts.default
kwt config set worktree.basedir ~/.kwt/worktrees
kwt config set --local layouts.default stack
```

### Multi-machine Sync

Multi-machine sync is opt-in and uses static config. Set `[fleet].enabled =
true`, configure a hub URL, and provide a bearer token through `token_env` or
`token_file`. Plain HTTP is allowed only for loopback hub URLs. Every
multi-machine hub URL must use HTTPS, typically through a private TLS endpoint
that forwards to the loopback listener.

The hub is a dumb store for signed-in hosts' latest worktree manifests.
Multi-machine status is advisory: it helps compare branch, commit, dirty-state,
and freshness across hosts, but it does not lock worktrees or enforce ownership.
When enabled, the dashboard shows remote-only rows with `WORKSPACE` set to
`remote`; selected-row details show the source machine and path. Wide terminals
may also show a `MACHINES` column, but the table keeps the worktree status
visible at roughly 100 columns. Select a remote-only branch row and press `s` to
sync that branch locally. Press `c` on a local row to open a shell there.
Remote-only sync verifies the created worktree against the hub-reported commit
when one is available, and skips repository setup (`copy_files` and
`setup_commands`). Existing local and remote branches use the same inert
creation boundary; setup hooks run only for newly created branches.

Useful commands:

```bash
kwt sync serve
kwt sync publish
kwt sync status
kwt sync forget <host-id>
```

When multi-machine sync is enabled, `kwt sync status` publishes this host before
reading the hub. Successful mutations also publish best-effort: `kwt add`, local
and global `kwt remove`, and explicit `kwt prune --expired` or `--merged`
policies when they actually remove a worktree. Doctor inspection and repair,
dry-runs, prune no-ops, and failed removals do not publish. Missing hub config,
disabled multi-machine sync, or publish failures never make the mutation command
fail; publish warnings may be written to stderr.

### Project Discovery

The dashboard lists worktrees from the global base directory and from projects
recorded in `config.toml`. Running `kwt` inside a repository registers or
refreshes that project entry, so future dashboard launches can see its
worktrees even when they are outside `worktree.basedir`.
Noninteractive clients can register an existing checkout explicitly with
`kwt projects add /absolute/repository/path --json`.
They can unregister it without deleting repository or worktree data with
`kwt projects remove /absolute/repository/path --json`.

### Repository Setup

Optional `repository_settings` copy files or run commands when new worktrees are
created by `kwt add`:

```toml
[[repository_settings]]
repository = "~/code/myapp"
basedir = "./worktrees"
copy_files = ["templates/.env.example"]
setup_commands = [
  "npm install",
  'printf "branch=%s\npath=%s\n" "{{.Branch}}" "{{.Path}}" > .worktree-info',
]
```

Template variables include `Host`, `Owner`, `Repository`, `FullPath`, `Branch`,
`Hash`, and `Path`. Quote variables in shell commands when values may contain
spaces.

## Commands

| Command          | Purpose                                     |
| ---------------- | ------------------------------------------- |
| `kwt`, `kwt tui` | Cross-project dashboard                     |
| `kwt add`        | Create a worktree                           |
| `kwt open`       | Open or establish a workspace session       |
| `kwt list`       | List worktrees                              |
| `kwt status`     | Show git status, sync state, and activity   |
| `kwt projects`   | List, register, or unregister projects      |
| `kwt pr`         | Discover and import pull requests as JSON   |
| `kwt get`        | Print a matching worktree path              |
| `kwt cd`         | Open a shell in a matching worktree         |
| `kwt exec`       | Run a command in a matching worktree        |
| `kwt remove`     | Delete a worktree, optionally its branch    |
| `kwt doctor`     | Inspect or repair structural worktree state |
| `kwt prune`      | Remove live worktrees by an explicit policy |
| `kwt sync`       | Publish and inspect multi-machine status    |
| `kwt tmux`       | Manage standalone tmux sessions             |
| `kwt workspace`  | Manage directory workspaces                 |
| `kwt config`     | Read and write config values                |
| `kwt completion` | Generate shell completion and integration   |

Run `kwt <command> --help` for flags and examples.

## Requirements

- Git 2.20+
  - Git 2.31+ for `kwt doctor` and `kwt prune --expired` or `--merged`
- Go 1.27+ to build from source
- tmux for workspace launch and `kwt tmux`

## Documentation

The maintained guide and reference live at [kwt.sh](https://kwt.sh/). To build
the site locally:

```bash
make docs-install
make docs-build
make docs-serve
```

## Releases

kwt uses semantic-version tags. Pushing a `vMAJOR.MINOR.PATCH` tag runs the test
suite and publishes macOS, Linux, and Windows archives plus checksums. The
[release checklist](docs/development/releasing.md) documents the maintainer
workflow.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).[^fork]

[^fork]:
    `kwt` began as a personalized fork of [`gwq`](https://github.com/d-kuro/gwq);
    the original project is Apache-2.0 licensed.
