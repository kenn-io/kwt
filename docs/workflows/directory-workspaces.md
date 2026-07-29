# Directory Workspaces

Not everything is a Git worktree. kwt can register plain directories — notes,
scratch space, non-Git projects — as workspaces, open them in tmux with your
configured layouts, and show them in the dashboard next to worktrees.

## Register a directory

```sh
kwt workspace add ~/notes
kwt workspace add ~/scratch/protos --name protos
kwt workspace list
kwt workspace remove protos
```

Names default to the directory base name and must be unique. Re-adding the same
directory updates its name. `remove` only unregisters: it never deletes the
directory, and a live tmux session is left running with a hint on how to kill
it. Workspace paths cannot contain `#`, which tmux reserves for format
expansion.

## Open from the dashboard

Launching `kwt` from a directory that is not inside a Git repository registers
that directory automatically and pre-selects its row, so `enter` immediately
creates and attaches its tmux session. The home directory is never
auto-registered.

Workspace rows show the workspace name under REPO, the path under BRANCH, and
session liveness under WORKSPACE; Git-specific columns show `-`. Rows match `/`
search by name and path. `K` kills a live session, and `d` unregisters the
workspace after confirmation. Git actions such as `n` (new branch), `b`
(existing branch), and `s` (sync) do not apply and say so in the status line.

## Sessions and layouts

Workspace sessions are named `kwt-workspace-dir-{name}-{hash}`, where the hash
covers the directory path. Renaming a workspace re-attaches to its existing
live session instead of orphaning it; the session adopts the new name the next
time it is created from scratch.

Layouts behave exactly as for worktrees: blank single-pane sessions by
default, with the same opt-ins, including the trust-gated per-directory
default in the directory's `.kwt.toml`:

```toml
[layouts]
default = "stack"
```

## Scope

The registry is machine-level configuration. Repository-local `.kwt.toml`
files cannot add workspaces, and workspaces are never published over
[multi-machine sync](../multi-machine-sync.md). See the
[configuration reference](../reference/configuration.md#directory-workspaces)
for the `[[workspaces]]` format.
