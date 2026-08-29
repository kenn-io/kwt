# Agent workspaces

Give each agent its own branch, worktree, and tmux workspace so parallel tasks
do not compete for one checkout. kwt exposes the same lifecycle to a person in
the dashboard and to an agent through commands and JSON.

## Configure a layout

A layout defines the tmux panes that kwt creates for a workspace. Add reusable
agent commands and a layout to your global configuration:

```toml
[agents]
codex = "codex"
roborev = "roborev tui"

[[layouts.presets]]
name = "review"
arrange = "even-horizontal"
panes = ["agent:codex", "agent:roborev", ""]
```

The empty string creates a plain shell. Agent commands can contain flags, but
approval or sandbox bypass settings should remain deliberate local choices.

Layouts are opt-in. Select one with `--layout`, `--select-layout`, the dashboard
`L` key, or `layouts.default`. Without a selection, kwt creates a blank
single-pane workspace. The reserved layout name `none` requests that blank
workspace explicitly.

## Create an isolated worktree

Create a branch, checkout, and workspace together:

```sh
kwt add -b fix/flaky-status --layout review
```

Use `--no-launch` when the agent needs only the checkout. A worktree created
from an existing local or remote branch always starts inert, even when a default
layout exists:

```sh
kwt add --from origin/fix/flaky-status fix/flaky-status
```

Inspect contributor-controlled files before opening that workspace. kwt does
not run configured setup, copy files, or start tmux during existing-branch
creation.

## Run and inspect work

Run a command without attaching to tmux:

```sh
kwt exec fix/flaky-status -- go test ./internal/status
```

Use the human summaries while steering several tasks:

```sh
kwt status
kwt changes "$(kwt get fix/flaky-status)"
```

Use JSON when another tool needs current inventory or an exact changed-file
snapshot:

```sh
kwt list --json
kwt changes "$(kwt get fix/flaky-status)" --json
```

The change result is bound to the worktree generation. If that checkout is
removed or replaced during inspection, kwt discards the stale result.

## Attach to the workspace

Open the workspace as a person:

```sh
kwt open fix/flaky-status
```

Establish it for another terminal client without attaching kwt's process:

```sh
kwt open "$(kwt get fix/flaky-status)" --start-session --json
```

New ordinary workspaces run on kwt's dedicated tmux server, `tmux -L kwt`,
instead of sharing the default server with personal sessions. During an upgrade,
kwt can reuse a verified matching workspace already running on the default
server. Upgrade cooperating clients together so an older client does not create
a parallel session there.

The JSON result names the selected endpoint with `session_name`,
`tmux_socket_name`, and `tmux_attach_mode`. Use those fields together; do not
infer the attachment policy from a socket name. The
[CLI reference](../reference/cli.md#kwt-open) defines the complete contract.

## Clean up

Inspect the final state, then remove the worktree and branch:

```sh
kwt changes "$(kwt get fix/flaky-status)"
kwt remove -b fix/flaky-status
```

Removal refuses dirty work and reports live processes whose current directory
is inside the checkout. Treat `--force` as an explicit decision after reviewing
those conflicts. See [Worktree lifecycle and maintenance](worktree-maintenance.md)
for pruning, doctor, and partial-cleanup outcomes.

The dashboard refreshes the active project first and processes confirmed
removals in order without blocking navigation. For its cache, concurrency, and
stale-row rules, see [TUI and project registry](../design/tui-projects.md).
