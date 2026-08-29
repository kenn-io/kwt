# Embed and connect kwt

kwt exposes the same worktree lifecycle used by its dashboard to scripts,
long-running local clients, and Go applications. Ghosthub is one consumer of
these interfaces, but none of them is specific to Ghosthub.

## Choose an integration boundary

| Boundary              | Use it when                                                                                                        |
| --------------------- | ------------------------------------------------------------------------------------------------------------------ |
| CLI and JSON          | An agent, script, or remote shell can run a kwt process for each action.                                           |
| Go services           | Your Go application wants inventory, removal, inspection, or SSH services in process.                              |
| Local daemon          | A separate same-machine process needs current shared inventory, one lifecycle owner, or streamed operation events. |
| Tmux session endpoint | Your application owns the terminal client and needs kwt to establish the correct workspace first.                  |

Start with the CLI unless process startup or in-process composition is a real
constraint. It preserves kwt's command behavior, stable JSON, and exit status
without requiring a client to reproduce local-daemon discovery.

## Use the Go services in process

The module root exports transport-neutral services and their request and result
types:

```go
import kwt "go.kenn.io/kwt"

inventory := kwt.NewInventoryService(kwt.InventoryServiceOptions{Source: source})
removals := kwt.NewRemovalService(kwt.RemovalServiceOptions{Home: kwtHome})
projects := kwt.NewProjectRemovalService(kwt.ProjectRemovalServiceOptions{
    Home: kwtHome,
})
inspections := kwt.NewInspectionService(kwt.InspectionServiceOptions{
    Inventory: inventory,
})
ssh := kwt.NewSSHService(kwt.SSHServiceOptions{
    Home:              kwtHome,
    AskpassExecutable: helperPath,
})
```

Use the inventory service to obtain current project or worktree snapshots. Pass
the returned generation and registration facts back to removal services so a
later mutation applies only to the checkout or project entry that the user
reviewed. The inspection service returns one generation-safe changed-file
snapshot.

The removal service checks the host process table by default and refuses to
remove a worktree when it is in use or the check is inconclusive. An embedder
that already owns an equivalent process-isolation boundary can supply
`RemovalServiceOptions.ProcessGuard`. That callback replaces kwt's default
check, so the embedder is responsible for preserving its own worktree-use
policy. Callers can still request the documented `Force` override explicitly.

An SSH lifecycle owner also supplies an executable that dispatches
`kwt.RunSSHAskpassHelper` before normal application startup. This keeps prompt
transport inside kwt's reviewed boundary.

The [`go.kenn.io/kwt/service`](https://pkg.go.dev/go.kenn.io/kwt/service)
package defines stable error and operation-event contracts. The
[`go.kenn.io/kwt/pkg/models`](https://pkg.go.dev/go.kenn.io/kwt/pkg/models)
package contains shared wire-facing models, including tmux attachment policy.
See the [root package documentation](https://pkg.go.dev/go.kenn.io/kwt) for
constructor options and complete types.

## Use the local daemon

kwt runs one writable daemon for each canonical kwt home. It binds an
automatically selected loopback port, publishes an owner-only runtime record,
and requires clients to prove the process identity before sending its bearer
credential. Do not assume a fixed port or expose the daemon on a network.

The versioned local API owns current worktree inventory, repository-config
trust, worktree and project removal, operation streams, and reviewed SSH route
and lease lifecycle. It also exposes status and graceful shutdown for daemon
management. Read the advertised capability set before using an operation; a
compatible API version alone does not promise every service.

Use the [local service daemon design](../design/daemon.md) for discovery,
authentication, replacement, capability, route, and error details. For a remote
machine, invoke that machine's `kwt` CLI over the remote shell. The remote kwt
process talks to its own loopback daemon; the daemon itself is not a remote API.

The daemon does not expose tmux routes. Workspace establishment remains a CLI
or in-process session-endpoint operation.

## Attach through a tmux session endpoint

Worktree and directory-workspace JSON describes a selected endpoint with three
fields:

| Field              | Meaning                                                                                  |
| ------------------ | ---------------------------------------------------------------------------------------- |
| `session_name`     | The exact tmux session to attach to.                                                     |
| `tmux_socket_name` | The selected named server or protected socket; an empty value has mode-specific meaning. |
| `tmux_attach_mode` | `direct` for an ordinary endpoint or `protected` for the pull-request attachment flow.   |

Never infer attachment policy from whether the socket name is empty. A direct
empty socket can name a verified adopted session on tmux's default server. A
protected empty socket is unresolved and cannot be attached.

Inventory is a snapshot. Immediately before a direct attachment, establish the
workspace and use the returned endpoint:

```sh
kwt open /exact/workspace/path --start-session --json
```

For `tmux_attach_mode: "protected"`, do not run tmux directly. Use
`kwt pr attach /exact/workspace/path`, with current guarded inventory fields
when the attachment follows a user selection. The
[CLI reference](../reference/cli.md#attaching-from-other-tools) defines direct,
adopted, nested, and protected attachment behavior.

## Resolve and hold SSH connections

The public SSH service resolves system OpenSSH configuration into a reviewed,
credential-free route snapshot and can acquire a generation-bound lease for
that route. It preserves direct and ProxyJump order and stops when a route
cannot be reviewed safely.

Choose host-key policy explicitly:

- `review` allows a prompt-capable client to present the reviewed host,
  algorithm, and fingerprint before answering OpenSSH.
- `strict` is for unattended work and accepts only keys OpenSSH already trusts.

Leases report ordered progress and can carry multiple authentication prompts.
They remain valid only for the route generation that was reviewed. Release a
lease when the client no longer needs it, and close an in-process SSH service
during application shutdown. The daemon also expires leases abandoned by its
clients and bounds connection cleanup.

Use `kwt ssh resolve` and `kwt ssh lease --json` for a subprocess boundary.
Use `kwt ssh exec` or `kwt ssh copy` when kwt should own one short command or
file transfer. Those commands construct and run the SSH or SFTP client; callers
must not rebuild private OpenSSH arguments from a route snapshot.

## Handle operations and errors

Across Go, HTTP, and machine-readable CLI boundaries, use the stable error
descriptor rather than matching prose:

- `code` identifies the failure category;
- `retryable` says whether the error category permits another attempt after its
  documented remediation;
- `details` carries documented typed evidence; and
- `message` explains the problem to a person and may change.

Operation events use one opaque operation ID and an increasing sequence. A
client reconnecting to the same daemon can ask for events after its last
accepted sequence. Bind every prompt response to the prompt ID that requested
it, and keep reading until the daemon publishes the terminal result.

If kwt reports `operation_outcome_unknown`, do not repeat the mutation. The
original operation may have completed even though its retained result can no
longer be proved. Refresh current inventory and reconcile the observed state
instead.
