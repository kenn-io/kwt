# Directory workspaces

Use a directory workspace when you want a managed tmux environment for notes,
scratch space, or a non-Git project. Directory workspaces appear beside Git
worktrees in the dashboard but never gain Git-specific behavior.

## Register a directory

```sh
kwt workspace add ~/notes
kwt workspace add ~/scratch/protos --name protos
```

Names default to the directory's base name and must be unique. Re-adding the
same directory updates its name. Paths cannot contain `#`, which tmux reserves
for format expansion.

Running `kwt` from a plain directory also registers and selects it automatically,
except for your home directory.

## Open it

Open a registered directory by its exact path:

```sh
kwt open ~/notes
```

In the dashboard, select the directory row and press `enter`. Directory rows
show the workspace name, path, and session state; Git-specific columns show
`-`. Use `K` to stop its live session.

## Choose a layout

Directory workspaces use the same layouts as worktrees. They start with one
blank pane unless you select a layout or configure a default. A trusted
`.kwt.toml` inside the directory can choose its local default:

```toml
[layouts]
default = "stack"
```

See [Agent workspaces](agent-workspaces.md#configure-a-layout) for global agent
and layout configuration.

## Use structured inventory

List directory workspaces for a person or another tool:

```sh
kwt workspace list
kwt workspace list --json
```

JSON returns each workspace's `name`, canonical absolute `path`, effective
`session_name`, `session_live`, `tmux_socket_name`, and `tmux_attach_mode`. An
empty registry returns `[]`.

Another tmux client can ask kwt to establish the workspace without attaching:

```sh
kwt open ~/notes --start-session --json
```

New sessions use kwt's dedicated tmux server. A verified matching session on
the default server can be reused during an upgrade. Treat the endpoint in this
result as one observation for the immediate attachment; use all three session
endpoint fields together. The [embedding guide](../integrations/embedding.md)
explains when to use this boundary.

## Remove the registration

```sh
kwt workspace remove protos
```

Removal unregisters the workspace. It never deletes the directory. A live tmux
session is left running, and kwt reports how to stop it. In the dashboard, `d`
performs the same unregister action after confirmation.

Directory workspaces are machine-level configuration and are not published by
[multi-machine sync](../multi-machine-sync.md). Repository-local configuration
cannot add them. See the
[configuration reference](../reference/configuration.md#directory-workspaces)
for the `[[workspaces]]` format.
