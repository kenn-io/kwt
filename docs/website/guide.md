# The kwt guide: nine stops from register to embed

The loop people and coding agents run against isolated worktrees. Every stop
links into the [documentation](https://kwt.sh/docs/) for exact commands, JSON
fields, and configuration.

## 01 / Register a project by running kwt in it

There is no init step. Run `kwt` inside a repository and it becomes a
registered project; the dashboard opens with its primary checkout and linked
worktrees beside every other project you have registered. A plain directory
becomes a directory workspace with a tmux session and no Git behavior.
Registration lives in `registry.json` under `KWT_HOME`; nothing is written
into the repository.

→ [Quickstart](https://kwt.sh/docs/get-started/quickstart/),
[Install](https://kwt.sh/docs/get-started/install/),
[Directory workspaces](https://kwt.sh/docs/workflows/directory-workspaces/)

## 02 / Branch into a worktree of its own

`n` in the dashboard, or `kwt add -b <branch>`, creates a branch, its isolated
checkout, and its tmux workspace in one move. `b`, or `kwt add --from
<ref> <branch>`, checks out a branch that already exists. Those worktrees start
inert: no hooks, no setup commands, no copied files, no workspace, until you
review them with `kwt changes` and open them with `kwt open`.

→ [Create or import a worktree](https://kwt.sh/docs/workflows/worktree-maintenance/#create-or-import-a-worktree),
[Configuration](https://kwt.sh/docs/reference/configuration/)

## 03 / Lay out the workspace once

Name agent commands under `[agents]`, compose them into a preset under
`[[layouts.presets]]` with `panes = ["agent:codex", "agent:roborev", ""]`, and
every workspace that selects the preset opens with those panes running. Select
a layout with `--layout`, the `L` key, or `layouts.default`; `none` asks for a
blank pane. A trusted repository `.kwt.toml` can set a local default.

→ [Configure a layout](https://kwt.sh/docs/workflows/agent-workspaces/#configure-a-layout)

## 04 / Work in it, attached or not

`enter` or `kwt open` attaches. `kwt exec <worktree> -- <command>` runs a
command with the worktree as its working directory, and `kwt get` resolves
the path. Workspaces run on kwt's dedicated tmux server, `tmux -L kwt`. A
terminal client that owns its attachment runs
`kwt open <path> --start-session --json` first and attaches to the reported
`session_name`, `tmux_socket_name`, and `tmux_attach_mode`.

→ [Run and inspect work](https://kwt.sh/docs/workflows/agent-workspaces/#run-and-inspect-work),
[kwt open](https://kwt.sh/docs/reference/cli/#kwt-open)

## 05 / Read back exactly what changed

`kwt status` summarizes every worktree across projects. `kwt changes` lists
the exact changed files in one worktree, staged and working-tree sides
separately, with untracked files, renames, deletions, and conflicts. Both are
read-only. With `--json` the result carries the worktree `generation`;
`--expected-generation` makes kwt fail closed if the checkout was replaced.
`kwt list --json` is the inventory the rest of your tooling reads.

→ [Inspect current state](https://kwt.sh/docs/workflows/worktree-maintenance/#inspect-current-state),
[kwt changes](https://kwt.sh/docs/reference/cli/#kwt-changes),
[kwt list](https://kwt.sh/docs/reference/cli/#kwt-list)

## 06 / Import a pull request behind a protected boundary

`kwt pr list --project <slug> --json` discovers pull requests;
`kwt pr import <id> --project <slug> --json` creates an inert worktree that
preserves the contributor branch's exact push destination. Attach only through
`kwt pr attach <path>`; direct `kwt open` and the dashboard refuse imported
pull-request workspaces.

→ [Pull-request automation](https://kwt.sh/docs/reference/pull-requests/),
[kwt pr](https://kwt.sh/docs/reference/cli/#kwt-pr)

## 07 / Clean up with previews and named stops

`kwt remove -b <branch>` refuses a dirty checkout or one a live process is
inside, naming the process IDs. `kwt prune --expired` or `--merged` with
`--dry-run` shows the complete decision set first; merged pruning requires a
confirmed merged pull request and keeps the local branch. `kwt doctor` reports
without changing anything; `--fix` repairs only findings with one provable
answer and rescans.

→ [Worktree lifecycle and maintenance](https://kwt.sh/docs/workflows/worktree-maintenance/),
[kwt doctor](https://kwt.sh/docs/reference/cli/#kwt-doctor),
[kwt prune](https://kwt.sh/docs/reference/cli/#kwt-prune)

## 08 / See your worktrees on every machine

Enable `[fleet]`, point each machine at a hub you run with `kwt sync serve`,
and publish with `kwt sync publish`. The dashboard marks remote-only rows,
differing heads, and where dirty files were last seen; `s` creates a
remote-only branch locally. Sync is advisory: kwt never transfers files or
commits, clones repositories, deletes remote worktrees, or locks a branch.

→ [Multi-machine sync](https://kwt.sh/docs/multi-machine-sync/),
[Sync architecture](https://kwt.sh/docs/design/multi-machine-sync/)

## 09 / Build your own tools on the same lifecycle

Start with the CLI and its JSON. A Go application can construct the inventory,
removal, inspection, and SSH services in process from `go.kenn.io/kwt`. A
terminal client that owns its attachment uses the tmux session endpoint.
Ghosthub embeds these services across local and SSH-hosted machines; it is one
consumer of interfaces available to any client.

→ [Embed and connect kwt](https://kwt.sh/docs/integrations/embedding/),
[Design notes](https://kwt.sh/docs/design/),
[Go package docs](https://pkg.go.dev/go.kenn.io/kwt)

## Next

Commands, JSON fields, exit statuses, configuration keys, the threat model, and
design notes live in the [documentation](https://kwt.sh/docs/). Every page has
a Markdown twin at the same path with `.md`, and
[llms.txt](https://kwt.sh/llms.txt) indexes them.
