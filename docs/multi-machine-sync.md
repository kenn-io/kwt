# Multi-machine sync

Use multi-machine sync to compare worktree state across your trusted development
machines. It shows where a branch exists, whether machines observed different
heads, and where dirty files were last seen. You can then create a local
worktree from a remote-only branch observation.

This state is advisory. kwt does not transfer files or commits, clone
repositories, delete remote worktrees, or lock a branch on another machine.

## Compare machines in the dashboard

When sync is enabled, the dashboard combines remote observations with local
worktrees:

- `WORKSPACE` shows `remote` when a row exists on another machine but not here.
- Selected-row details name the source machine and path.
- `HEADS` shows `local only`, `remote only`, or a differing source host. Local
  rows can also show Git push and pull counts such as `↑2` or `↓1`.
- `CHANGES` shows local counts such as `~3 ?2`, or prefixes them with `remote`
  when only another machine reported changes.
- Wide terminals can show a `MACHINES` column. Narrower views keep worktree
  status visible instead.

The command-line view publishes this machine best-effort, then reads the hub:

```sh
kwt sync status
```

## Create a local worktree from a remote observation

Select a remote-only branch row and press `s`. kwt uses the matching local
project and its normal worktree naming rules. The branch must already be
available in the local repository or on a fetched remote.

If the source machine has unpushed commits, push or fetch them first. When the
hub supplied a head commit, kwt verifies that the new worktree matches it and
removes the checkout if it does not. Dirty files are never copied.

Remote-only sync skips repository setup, including `copy_files` and
`setup_commands`. Review the checkout before pressing `enter` to start its
workspace. Detached-head observations are shown for awareness but cannot be
synced by the current protocol.

## Configure a hub and clients

Multi-machine sync is disabled by default. One machine runs the loopback hub;
every enabled machine publishes its latest local manifest to that hub.

```toml
[fleet]
enabled = true
host_id = "host-a"
hub_url = "https://host-a.example"
token_file = "~/.config/kwt/fleet.token"

[fleet.hub]
listen_addr = "127.0.0.1:8787"
store_path = "~/.local/share/kwt/fleet/state.json"
```

Provide the bearer token through `token_file` or `token_env`; do not put it
directly in `config.toml`. A hub URL must use HTTPS unless it names a loopback
host. The hub itself accepts only a loopback listen address, so expose it
through a private TLS endpoint that forwards to the listener.

Start the hub and publish from a client with:

```sh
kwt sync serve
kwt sync publish
```

Successful local worktree mutations also publish best-effort when sync is
enabled. A publication failure produces a warning but does not turn a completed
local mutation into a failure.

## Understand freshness and differences

Each machine reports the Git facts it can see locally: branch or detached head,
head commit, ahead and behind counts against its own remote-tracking refs, dirty
counts, path, and last activity.

The combined view does not claim ancestry that no machine proved. If two hosts
report different heads, kwt says they differ. It says one is behind only when
the local clone has the commits needed to establish that relationship.

The hub keeps the latest manifest for each host until you remove it. Forget a
retired or renamed machine so its old observations disappear:

```sh
kwt sync forget old-host
```

See [Multi-machine sync architecture](design/multi-machine-sync.md) for the
manifest identity, merge, freshness, and trust model.
