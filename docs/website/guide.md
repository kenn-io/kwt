# The kwt guide

How to use kwt, from registering a project to embedding it in your own tools.
Each section links to the [documentation](https://kwt.sh/docs/) for exact
commands, JSON fields, and configuration.

## 1. Register a project

There is no init step. Run `kwt` inside a repository and it becomes a
registered project. The dashboard opens with its primary checkout and any
linked worktrees, next to the other registered projects. A directory that is
not a repository becomes a directory workspace: a tmux session with no Git
behavior. Registration is stored in `registry.json` under `KWT_HOME`. kwt
writes nothing into the repository.

See [Quickstart](https://kwt.sh/docs/get-started/quickstart/),
[Install](https://kwt.sh/docs/get-started/install/), and
[Directory workspaces](https://kwt.sh/docs/workflows/directory-workspaces/).

## 2. Create a worktree

`n` in the dashboard, or `kwt add -b <branch>`, creates a branch, its worktree,
and its tmux workspace. `b`, or `kwt add --from <ref> <branch>`, checks out a
branch that already exists. Those worktrees start inert. kwt runs no hooks,
setup commands, or file copies and starts no workspace until you review the
worktree with `kwt changes` and open it with `kwt open`.

See [Create or import a worktree](https://kwt.sh/docs/workflows/worktree-maintenance/#create-or-import-a-worktree)
and [Configuration](https://kwt.sh/docs/reference/configuration/).

## 3. Configure a layout

Name agent commands under `[agents]` and combine them into a preset under
`[[layouts.presets]]` with `panes = ["agent:codex", "agent:roborev", ""]`.
Every workspace that uses the preset opens with those panes running. Select a
layout with `--layout`, the `L` key, or `layouts.default`. The reserved name
`none` gives a single blank pane. A trusted repository `.kwt.toml` can set a
local default.

See [Configure a layout](https://kwt.sh/docs/workflows/agent-workspaces/#configure-a-layout).

## 4. Run commands in a worktree

`enter` or `kwt open` attaches. `kwt exec <worktree> -- <command>` runs a
command with the worktree as its working directory, and `kwt get` prints the
path. Workspaces run on kwt's own tmux server, `tmux -L kwt`. A terminal
client that manages its own attachment runs
`kwt open <path> --start-session --json` first and attaches to the reported
`session_name`, `tmux_socket_name`, and `tmux_attach_mode`.

See [Run and inspect work](https://kwt.sh/docs/workflows/agent-workspaces/#run-and-inspect-work)
and [kwt open](https://kwt.sh/docs/reference/cli/#kwt-open).

## 5. Inspect changes

`kwt status` summarizes every worktree across projects. `kwt changes` lists
the changed files in one worktree, staged and working-tree sides separately,
including untracked files, renames, deletions, and conflicts. Both are
read-only. With `--json` the result carries the worktree `generation`.
`--expected-generation` makes kwt fail if the checkout was replaced.
`kwt list --json` is the inventory for the rest of your tooling.

See [Inspect current state](https://kwt.sh/docs/workflows/worktree-maintenance/#inspect-current-state),
[kwt changes](https://kwt.sh/docs/reference/cli/#kwt-changes), and
[kwt list](https://kwt.sh/docs/reference/cli/#kwt-list).

## 6. Import a pull request

`kwt pr list --project <slug> --json` shows pull requests.
`kwt pr import <id> --project <slug> --json` creates an inert worktree that
keeps the contributor branch's push destination. Attach only with
`kwt pr attach <path>`. `kwt open` and the dashboard refuse imported
pull-request worktrees.

See [Pull-request automation](https://kwt.sh/docs/reference/pull-requests/)
and [kwt pr](https://kwt.sh/docs/reference/cli/#kwt-pr).

## 7. Remove and prune worktrees

`kwt remove -b <branch>` refuses a dirty checkout or one a running process is
inside, and lists the process IDs. `kwt prune --expired` or `--merged` with
`--dry-run` prints every decision first. Merged pruning requires a merged pull
request and keeps the local branch. `kwt doctor` reports without changing
anything. `kwt doctor --fix` repairs only findings with one correct repair and
rescans.

See [Worktree lifecycle and maintenance](https://kwt.sh/docs/workflows/worktree-maintenance/),
[kwt doctor](https://kwt.sh/docs/reference/cli/#kwt-doctor), and
[kwt prune](https://kwt.sh/docs/reference/cli/#kwt-prune).

## 8. Sync across machines

Enable `[fleet]`, point each machine at a hub you run with `kwt sync serve`,
and publish with `kwt sync publish`. The dashboard marks remote-only rows,
differing heads, and where dirty files were last seen. `s` creates a
remote-only branch locally. Sync is advisory. kwt never transfers files or
commits, clones repositories, deletes remote worktrees, or locks a branch.

See [Multi-machine sync](https://kwt.sh/docs/multi-machine-sync/) and
[Sync architecture](https://kwt.sh/docs/design/multi-machine-sync/).

## 9. Embed kwt

Start with the CLI and its JSON output. A Go application can build the
inventory, removal, inspection, and SSH services in process from
`go.kenn.io/kwt`. A terminal client that manages its own attachment uses the
tmux session endpoint. Ghosthub embeds these services on local and SSH-hosted
machines. Any client can use the same interfaces.

See [Embed and connect kwt](https://kwt.sh/docs/integrations/embedding/),
[Design notes](https://kwt.sh/docs/design/), and
[Go package docs](https://pkg.go.dev/go.kenn.io/kwt).

## Documentation

The [documentation](https://kwt.sh/docs/) has the commands, JSON fields, exit
statuses, configuration keys, the threat model, and the design notes. Every
page has a Markdown version at the same path with a `.md` suffix, and
[llms.txt](https://kwt.sh/llms.txt) indexes them.
