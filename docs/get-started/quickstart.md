# Quickstart

## Before you start

[Install kwt](install.md), then make sure `kwt`, Git, and tmux are available in
your terminal. You do not need to edit configuration before the first run.

## Open the dashboard

From any directory:

```sh
kwt
```

Bare `kwt` opens the full-screen dashboard when stdin and stdout are interactive.
Use `kwt tui` when you want the explicit command form. Launching from a Git
repository registers it as a project; launching from a plain directory registers
it as a [directory workspace](../workflows/directory-workspaces.md) and
pre-selects its row, so `enter` opens a tmux session right there.

Useful keys:

| Key     | Action                                                  |
| ------- | ------------------------------------------------------- |
| `enter` | Attach to the selected workspace.                       |
| `n`     | Create a new branch and worktree.                       |
| `b`     | Search local and remote branches for a worktree.        |
| `P`     | Switch the active project perspective.                  |
| `p`     | Filter visible projects by name.                        |
| `/`     | Search rows within the active perspective/filter.       |
| `L`     | Select a workspace layout.                              |
| `d`     | Delete the selected worktree or unregister a workspace. |
| `K`     | Kill the selected live tmux workspace.                  |
| `s`     | Sync a remote-only branch row locally.                  |
| `c`     | Open a shell in the selected worktree.                  |
| `r`     | Refresh.                                                |
| `?`     | Toggle help.                                            |

### Understand checkout labels

The branch column distinguishes checkout state from branch identity:

- A primary checkout adds `[primary]`, for example `main [primary]`.
- A detached checkout shows `detached@<commit>`, using the first eight
  characters of the commit hash.
- A detached primary checkout combines both labels, for example
  `detached@be094b1b [primary]`.

For rows observed on multiple machines, `[primary]` appears only when every
observation agrees that the checkout is primary. Primary checkouts are
protected from dashboard deletion; pressing `d` reports the protected
checkout's abbreviated path.

## Create a worktree

```sh
kwt add -b feature/new-ui
```

When `-b` creates a branch, `kwt` fetches `origin` and starts from its default
branch. If that remote base is unavailable, it falls back to local `main`, then
`master`, then the branch checked out in the primary worktree.

To use an existing branch, press `b` in the dashboard and search the available
local and remote branches. From the CLI, pass the local branch directly or
retain an exact remote source with `--from`:

```sh
kwt add feature/local
kwt add --from origin/feature/review feature/review
```

Existing-branch creation disables repository-configured Git hooks and checkout
filters, removes kwt credential variables from Git, treats branch names
literally in destination paths, and skips `copy_files`, `setup_commands`, and
workspace launch. Ordinary status and fleet observation remain available.
After reviewing the checkout, explicitly acknowledge it and create its
workspace:

```sh
kwt open "$(kwt get feature/review)"
```

For local and newly created branches, `kwt add` launches a tmux workspace by
default — a blank single-pane session unless a
[layout](../reference/configuration.md) is selected. To create without
launching:

```sh
kwt add --no-launch -b feature/new-ui
```

## Use worktrees from scripts

```sh
cd "$(kwt get feature/new-ui)"
kwt exec feature/new-ui -- npm test
```

Machine-readable surfaces are available when another tool needs to coordinate
with kwt:

```sh
kwt branches --json
kwt list --json
kwt projects --json
kwt pr list --project github.com/acme/widget --json
```

## Clean up

```sh
kwt remove feature/new-ui
kwt remove -b feature/new-ui
kwt prune
```

`remove -b` removes both the worktree and the matching branch. `prune` cleans up
stale Git worktree metadata.

Next, see [Agent workspaces](../workflows/agent-workspaces.md) to configure tmux
layouts or the [CLI reference](../reference/cli.md) for the complete command
surface.
