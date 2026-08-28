# Quickstart

## Before you start

[Install kwt](install.md), then confirm that `kwt`, Git, and tmux are available
in your terminal. You do not need to edit configuration before the first run.

Start from a Git repository that you want kwt to manage:

```sh
cd /path/to/repository
```

## Work interactively

Open the terminal dashboard:

```sh
kwt
```

Running kwt inside the repository registers it as a project. The dashboard now
shows its primary checkout and linked worktrees alongside your other registered
projects.

Create a worktree with `n`, enter a branch name such as `feature/new-ui`, and
confirm. kwt creates the isolated checkout and starts its tmux workspace with
the current layout choice. Press `enter` on the row to attach again later.

These keys cover the first workflow:

| Key     | Action                                     |
| ------- | ------------------------------------------ |
| `n`     | Create a new branch and worktree.          |
| `b`     | Create a worktree from an existing branch. |
| `enter` | Open or attach to the selected workspace.  |
| `L`     | Choose the workspace's tmux layout.        |
| `d`     | Remove the selected worktree.              |
| `?`     | Show the complete dashboard key reference. |
| `q`     | Quit the dashboard.                        |

When you finish, select the new worktree and press `d`. kwt confirms the exact
checkout before removing it. Primary checkouts cannot be removed from the
dashboard.

## Automate a worktree

The CLI exposes the same lifecycle without opening the dashboard:

```sh
kwt add -b feature/new-ui
kwt get feature/new-ui
kwt exec feature/new-ui -- make test
kwt changes --json
kwt list --json
kwt remove -b feature/new-ui
```

`kwt add -b` creates a new branch and starts its workspace. Use `--no-launch`
when an agent needs the checkout but not tmux. `kwt get` prints the matching
path, and `kwt exec` runs a command with that worktree as its current directory.
The two JSON commands return current inventory and an exact changed-file
snapshot without requiring screen parsing.

`kwt remove -b` removes the worktree and its branch. Removal stops when the
worktree is dirty or a live process is using its directory. Inspect the
reported conflict before deciding whether an explicit `--force` is appropriate.

Worktrees created from an existing local or remote branch start inert. kwt does
not run repository setup or launch a workspace until you review the checkout
and open it explicitly. See [Agent workspaces](../workflows/agent-workspaces.md)
for that loop and for tmux layouts.

## What to read next

- [Worktree lifecycle and maintenance](../workflows/worktree-maintenance.md)
  explains status, change inspection, doctor, pruning, and removal.
- [Agent workspaces](../workflows/agent-workspaces.md) covers repeatable tmux
  layouts and noninteractive work.
- [Directory workspaces](../workflows/directory-workspaces.md) manages tmux
  workspaces that are not Git repositories.
- [Pull-request automation](../reference/pull-requests.md) covers inert imports
  and protected attachment.
- [CLI reference](../reference/cli.md) defines commands, JSON, exit status, and
  guarded-operation contracts.
