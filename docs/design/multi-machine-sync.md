# Multi-machine Sync Architecture

Multi-machine sync is the opt-in layer for coordinating active Git worktrees
across a small trusted set of machines. It shows the union of observed worktrees
and identifies which worktrees are missing or different on the current host.

It is not a file synchronizer. It must not sync dirty files, delete worktrees,
or require background daemons for single-machine users.

## Principles

- Multi-machine sync is disabled by default and inert when disabled.
- The hub is a dumb store: authenticate, validate, store latest manifests by
  host, and serve combined worktree state.
- Every enabled node publishes a manifest. A hub node publishes through the same
  HTTP API as any other node; only the hub daemon writes the store file.
- Spokes do not need a daemon in v1. Publish on successful `add`, `remove`, and
  `prune`, and publish before multi-machine reads with a short best-effort
  timeout.
- The UI language is advisory: "missing on this host" and "different head on
  host X" are honest; "behind host X" is only valid when local Git data proves
  ancestry.

## Configuration shape

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

There is no `role` field. A node with `[fleet.hub]` configured is the hub. A
node without it is a publisher/client.

The hub machine's CLI still publishes through the same HTTP API as any other
node. For a loopback-only hub it may default an empty `hub_url` from
`[fleet.hub].listen_addr`; multi-machine clients should use an HTTPS `hub_url`
that reaches the hub through a private TLS endpoint.

Tokens must come from `token_file` or `token_env`, not inline TOML. The daemon
listener stays on loopback. Plain HTTP bearer-token requests are valid only for
loopback hub URLs, and the client bypasses environment proxies for those
requests. Every multi-machine hub needs an HTTPS endpoint, commonly Tailscale
Serve, Caddy, or another private TLS proxy forwarding to the loopback listener.
The hub rejects every non-loopback listen address.

If `host_id` is omitted, default from `os.Hostname()` after trimming whitespace
and normalizing to lowercase `[a-z0-9._-]`. An empty or invalid host ID must fail
before publish. The hub must reject invalid host IDs in URL path segments.

## Project identity

Rows are keyed by logical project identity, not local paths. The same
repository can live at `~/code/kwt` on one host and `/src/kwt` on another.

Remote URL normalization must handle these common forms as the same project:

- `git@github.com:kenn-io/kwt.git`
- `https://github.com/kenn-io/kwt`
- `https://github.com/kenn-io/kwt.git`

They normalize to `github.com/kenn-io/kwt`. Owner/name differences remain
different projects, so forks are distinct unless the user explicitly configures
a canonical identity. SSH host aliases are not safely inferable from Git URLs,
so explicit identity override is required when automatic normalization would
split the multi-machine view incorrectly.

## Manifest

Each publish sends a versioned manifest:

```json
{
  "schema_version": 1,
  "host_id": "host-a",
  "host": {
    "hostname": "Host-A",
    "platform": "darwin/arm64"
  },
  "observed_at": "2026-07-04T12:00:00Z",
  "projects": [
    {
      "identity": "github.com/kenn-io/kwt",
      "name": "kwt",
      "local_root": "/workspace/user-a/code/kwt",
      "remote_url": "git@github.com:kenn-io/kwt.git"
    }
  ],
  "worktrees": [
    {
      "project_identity": "github.com/kenn-io/kwt",
      "kind": "branch",
      "ref": "feature/machine-view",
      "branch": "feature/machine-view",
      "path": "/workspace/user-a/worktrees/github.com/kenn-io/kwt/feature-machine-view",
      "head": "abcdef123456",
      "head_time": "2026-07-04T11:30:00Z",
      "upstream": "origin/feature/machine-view",
      "ahead": 1,
      "behind": 0,
      "status": {
        "modified": 0,
        "added": 0,
        "deleted": 0,
        "untracked": 0,
        "staged": 0,
        "conflicts": 0
      },
      "last_activity": "2026-07-04T11:45:00Z",
      "is_main": false
    }
  ]
}
```

Worktree identity is `(project_identity, kind, ref)`. Branch worktrees use
`kind = "branch"` and `ref = branch`; detached worktrees use
`kind = "detached"` and `ref = head`.

## CLI surface

The v1 commands are:

| Command                     | Purpose                                                                   |
| --------------------------- | ------------------------------------------------------------------------- |
| `kwt sync serve`            | Run the hub HTTP server in the foreground.                                |
| `kwt sync publish`          | Build and publish the local manifest.                                     |
| `kwt sync status`           | Publish best-effort, fetch hub state, and render the multi-machine table. |
| `kwt sync forget <host_id>` | Ask the hub to delete a retired host.                                     |

Existing worktree mutation commands publish after successful local
mutations when multi-machine sync is enabled. Publish failures must not fail the
mutation.

The TUI consumes the same hub state as `kwt sync status`. It includes
remote-only branch rows, shows machine presence in selected-row details, and may
show a `MACHINES` column on wide terminals. At roughly 100 columns the table
prioritizes worktree status over host lists so `WORKSPACE` remains visible. The
user can sync a remote-only branch onto the current host. The sync action is
local: it uses the configured project root and normal worktree naming rules,
verifies the created worktree against the hub-reported head when present, and
then publishes best-effort. It can only check out branches whose commits are
already available locally or through a fetched remote; machines with unpushed
commits must push or otherwise transfer those commits first. Detached-head rows
remain visible but are not synced in v1.

## Hub API

The v1 API is intentionally small:

| Route                                         | Purpose                             |
| --------------------------------------------- | ----------------------------------- |
| `GET /api/v1/ping`                            | Health and runtime metadata.        |
| `POST /api/v1/fleet/hosts/{host_id}/manifest` | Store one host manifest.            |
| `GET /api/v1/fleet/state`                     | Return grouped multi-machine state. |
| `DELETE /api/v1/fleet/hosts/{host_id}`        | Forget a retired host.              |

Manifest requests are capped at 1 MiB. The hub rejects missing bearer auth,
unknown schema versions, host ID mismatches, invalid host IDs, invalid project
identities, oversized bodies, and public or unspecified listen addresses.

`GET /api/v1/fleet/state` returns grouped rows and an `ETag` equal to the
`state_version`, where `state_version` is derived from canonical stored manifests
and warnings. Clients own interpretation such as freshness thresholds and
whether a row is present locally.

The hub store write must be atomic: write a temporary file, fsync as practical,
then rename over `state.json`.

## Multi-machine view

For each row, `kwt sync status` and the TUI can show:

- project identity and display name;
- branch or detached head;
- hosts where the worktree exists;
- whether this host has it;
- whether this host has a different head from another observed host;
- whether any observed copy has uncommitted changes;
- host freshness derived from `observed_at`.

Stale host reports remain visible as stale. Retired hosts are removed with the
forget endpoint rather than silently disappearing.

## Error handling

When multi-machine sync is disabled, its errors cannot affect existing commands.

When multi-machine sync is enabled but the hub is unreachable, local commands
continue with local state and report a concise warning. Mutation commands such as
`kwt add` must not fail solely because manifest publication failed.

Project paths that do not exist on a host should not poison multi-machine state.
The local manifest builder reports only projects and worktrees it can observe.

## Testing expectations

Multi-machine sync tests should use local HTTP handlers and temporary stores.
They should not require Tailscale, public network access, elevated privileges,
launchd, systemd, or an external daemon.

The tests should protect observable contracts: the disabled subsystem is inert,
token loading works, listen validation rejects public binds, URL normalization is
stable, manifest validation rejects bad payloads, the hub groups state correctly,
ETags change when state changes, and local reconciliation reports present,
missing, and different rows without inventing cross-host ancestry.
