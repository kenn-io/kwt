# Agent Workspaces

`kwt` is useful when each branch deserves its own terminal workspace and agents
should not fight over one checkout.

## Layouts

Layouts define tmux panes. Keep them explicit and boring:

```toml
[agents]
codex = "codex"
claude = "claude"
roborev = "roborev tui"

[[layouts.presets]]
name = "review"
arrange = "even-horizontal"
panes = ["agent:codex", "agent:roborev", ""]
```

The empty string creates a plain shell. Agent commands can include flags, but
approval and sandbox bypass flags should be deliberate local choices.

Layouts are opt-in: without `--layout`, `--select-layout`, or a
`layouts.default`, a workspace is a blank single-pane session. The reserved
name `none` selects the blank session explicitly.

## Working loop

```sh
kwt add -b fix/flaky-status
kwt exec fix/flaky-status -- go test ./internal/status
kwt open fix/flaky-status
```

For long-running agent work, keep one branch per worktree, one tmux workspace per
branch, and let `kwt status` show which branches are dirty, ahead, behind, or
quiet.

Kwt keeps workspaces and standalone `kwt tmux` jobs on a dedicated
tmux server named `kwt`. On POSIX systems, inspect it manually with:

```sh
env -u TMUX_TMPDIR tmux -L kwt list-sessions
```

Kwt temporarily adopts a verified matching session on the default
server during rollout; if both endpoints contain a valid match, the dedicated
server wins. `kwt tmux list` shows `[kwt]` or `[default]` for this reason.
Mixed old and new Kwt binaries may still create parallel same-name sessions,
so cooperating automation should be upgraded together.

Opening a workspace from a client on another tmux server creates a nested
client after removing the outer `TMUX` and `TMUX_PANE` selectors. Detach to
return to the outer server. If both clients share the default prefix, send the
prefix twice to address the inner client.

## Cross-project steering

When a cached catalog is available, the dashboard paints it first, then
refreshes only the project you are viewing. A cold start waits for the initial
current inventory. You can search, move through rows, open a shell in an existing
directory, or attach to a session that Kwt verifies is already live while that
refresh runs. Creating, deleting, syncing, killing, or starting a session waits
for current inventory.

Kwt refreshes the global catalog once in the background. Choosing all projects
runs a current global refresh and collects status for the displayed rows. Git
status collection uses a bounded worker pool. Activity is the newest of the
HEAD commit, worktree directory, and changed or untracked file times; Kwt does
not scan every tracked file to order the dashboard.

Confirmed deletions enter one background queue. The row immediately shows
`removing…` while navigation and safe actions on other rows remain available.
Kwt processes removals in confirmation order. A failed removal restores the
row and reports the error; later queued removals still run.

The dashboard is project-aware. Use `P` to set the active project perspective
before creating a worktree. Press `n` for a new branch or `b` to search local
and remote branches that are not already checked out. A selected remote branch
is checked out without repository hooks, configured filters, setup commands, or
workspace commands. It still participates in ordinary status and fleet
observation. Review it before pressing `enter`; that explicit attach
acknowledges the checkout and creates its session. Use lowercase `p` for a
temporary project-name filter and `/` for row search.
