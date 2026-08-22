# Local service daemon

Kwt runs at most one writable local service daemon for each canonical kwt
home. The daemon is a host for kwt domain services; it is not the multi-machine
sync hub. It owns worktree inventory reads and guarded project unregistration;
other worktree mutations, status collection, tmux attachment, and SSH lifecycle
move in later slices.

The process binds an automatically selected IPv4 loopback port and publishes
an owner-only runtime record under `<kwt-home>/runtime`. Clients verify the
recorded PID, process creation identity, service identity, and a challenge
proof before they send the record's bearer credential. A matching live but
unresponsive owner is preserved; kwt never starts a competing writer.

`kwt daemon start|stop|restart|status` manages the background process. `kwt
serve` runs the same host in the foreground, disables idle exit, and refuses to
replace an existing owner. Background logs are written to
`<kwt-home>/daemon.log`, rotate at 10 MiB, and retain three owner-only backups.

Compatible clients share the newest running daemon. Build order uses an exact
full source revision first, then differing semantic versions, then the source
commit time. Source times are canonical RFC3339 UTC values authenticated in
both the private runtime record and status response. Hashes are never compared
lexically. Matching semantic module versions count as the same build only when
both installations explicitly lack VCS revision and time identity, as with
`go install ...@version`. Different revisions with equal source times, missing
contemporary metadata, or invalid values have unknown order. A build from a
dirty worktree marks its revision dirty and omits revision time, so it cannot
claim the clean checkout's identity or source order.

With `daemon.auto_restart = "newer"`, a provably newer client asks an older
daemon to drain before replacement. Automatic start reuses a ready daemon when
order is unknown; it never guesses. Explicit restart accepts the same or a
newer invoking build, returns `daemon_downgrade_refused` for an older build,
and returns `daemon_build_order_unknown` when order cannot be proved. A
draining daemon applies the same refusal before another binary waits and
launches. `kwt daemon stop` followed by `kwt daemon start` is the deliberate
operator override for unknown order and may install either build. Ghosthub may
use that override only for an explicit helper-update action, never routine
polling.

Draining rejects new operations with a retryable typed response that carries
the deadline. The command requesting shutdown prints the returned drain state
immediately, then continues reporting observed drain state while it waits.
Active work and leases may finish until `daemon.replacement_grace`; the default
is five minutes.

The API schema is `1.11.0`. It exposes authenticated status, graceful shutdown,
worktree inventory, guarded project unregistration, and repository-config
approval under `/api/v1`, proof-capable liveness at `/api/ping`, and
credential-free OpenAPI at `/openapi.json`. Inventory clients require the
`worktree.inventory.v2` capability; guarded unregistration requires
`project.removal.v1`, SSH route resolution requires `ssh.resolve.v1`, and
daemon-owned connection leases require `ssh.lifecycle.v1`. Worktree removal
uses `worktree.removal.v2` for session, branch, and HEAD guards. Clients that bind
short commands to a connection-owned hold additionally require
`ssh.lease.hold.v1`; stale development daemons fail capability negotiation
before acquiring a lease rather than falling back to periodic touches.
Daemons advertise `operation.stream.v1` when they can
carry ordered domain-operation events and bound prompt responses. Advertising
that transport capability does not start a domain operation or move any
existing command behind the daemon. An operation never has simultaneous direct
and HTTP execution paths.

Service failures cross the in-process, HTTP, and machine-readable CLI
boundaries as one descriptor with `code`, human-facing `message`, `retryable`,
and optional typed `details`. Adapters preserve the descriptor; they do not
infer a failure from prose or HTTP status. HTTP uses the same message as its
RFC problem `detail`. Unknown HTTP codes become `daemon_transport_failed`
instead of being guessed. Detail keys are allowlisted per code: draining may
carry an RFC3339 `drain_deadline`, and repository trust interaction carries
its typed digest-bound prompt fields. Within API major 1, draining responses
also mirror the deadline in the legacy top-level `drain_deadline` field, and
clients recognize a legacy `busy` response only when it carries a valid drain
deadline.

The daemon and inventory paths currently emit these stable codes:

| Code                                      | Meaning                                                           |
| ----------------------------------------- | ----------------------------------------------------------------- |
| `invalid_request`                         | The request is structurally invalid.                              |
| `daemon_start_failed`                     | The daemon could not launch or become ready.                      |
| `daemon_unresponsive`                     | A verified owner exists but cannot safely be reused or replaced.  |
| `daemon_incompatible`                     | The owner lacks the required API major or capability.             |
| `daemon_downgrade_refused`                | An older client attempted replacement.                            |
| `daemon_build_order_unknown`              | Replacement order cannot be proved.                               |
| `daemon_draining`                         | The owner is draining; retry according to its deadline.           |
| `daemon_transport_failed`                 | The verified daemon exchange failed or was not understood.        |
| `inventory_timeout`                       | A current inventory refresh exceeded its bound.                   |
| `inventory_failed`                        | Inventory discovery failed for another known source cause.        |
| `removal_failed`                          | A known worktree removal failure retained safe actionable detail. |
| `project_not_found`                       | No exact persisted project path matched the request.              |
| `registration_changed`                    | Project identity or registry state changed; retry from inventory. |
| `unregistration_failed`                   | Project metadata could not be removed safely.                     |
| `protected_session_live`                  | A live protected tmux endpoint still belongs to the project.      |
| `protected_endpoint_inventory_incomplete` | Durable endpoint authority could not be verified.                 |
| `interaction_required`                    | Repository configuration needs digest-bound approval.             |
| `operation_id_conflict`                   | An operation identifier was reused for a different request.       |
| `operation_capacity_exhausted`            | Bounded operation capacity was exhausted.                         |
| `operation_outcome_unknown`               | The operation's terminal outcome can no longer be proved.         |
| `ssh_invalid_target`                      | Structured SSH target validation failed.                          |
| `ssh_resolution_failed`                   | Effective OpenSSH configuration could not be observed.            |
| `ssh_route_unreviewable`                  | A proxy route cannot be bound to independently reviewed hops.     |
| `ssh_configuration_changed`               | A later lifecycle request observed a different route identity.    |
| `ssh_unsupported_version`                 | The installed OpenSSH cannot support the required prompt policy.  |
| `ssh_interaction_required`                | Connection preparation needs a prompt-capable client.             |
| `ssh_prompt_rejected`                     | The client rejected an OpenSSH prompt.                            |
| `ssh_prompt_timed_out`                    | A bound OpenSSH prompt exceeded its response deadline.            |
| `ssh_connection_failed`                   | OpenSSH could not establish the reviewed route.                   |
| `ssh_connection_changed`                  | A generation-bound lease is no longer usable.                     |
| `ssh_control_path_occupied`               | A verified private control path is occupied unexpectedly.         |
| `ssh_cleanup_failed`                      | Verified SSH connection cleanup did not complete.                 |
| `internal`                                | An unexpected failure was withheld from the public response.      |

`operation_journal_unavailable` remains reserved until kwt has a durable
operation journal. The current operation stream is deliberately in-memory and
same-daemon only.

Operation events carry an opaque operation ID and a strictly increasing
sequence number. A reconnect sends its last accepted sequence and receives
retained later events before live events. One operation may carry multiple
prompt rounds; each response is accepted only for the exact current prompt ID,
including an intentionally empty response. Stale, duplicate, and
cross-operation responses fail closed. A client acknowledges a prompt sequence
only after its bound response succeeds, so reconnect replays an unanswered
prompt. Prompt events carry the daemon's response deadline. When that deadline
expires, the client stops waiting for input, consumes the prompt sequence
without a response, and continues reading until the daemon publishes the
authoritative terminal failure.

Each operation retains at most 256 events and 1 MiB of event payload. Public
failure sanitization happens before admission, and each admitted event is
retained and replayed from one immutable encoded representation so later
caller mutation cannot change the stream or its byte accounting. The daemon
admits at most 128 active operations and retains at most 128 completed
operations for five minutes. An operation admits at most eight subscribers,
with at most 128 live subscribers across the daemon. Retained terminal replays
and streams detached after queue overflow count against both limits until the
HTTP subscriber closes. Publishing a terminal event does not release an active
operation slot until its worker cleanup returns. Each response write has a
five-second deadline. Clients wait at most two seconds for event-stream headers
and for a prompt-response exchange; after event headers arrive, the stream body
lifetime follows the caller's operation context. Replay queues reserve a slot
that progress and warning events cannot consume; prompts and terminal
completion remain replayable when noncritical delivery saturates. The daemon
rejects excess work or subscriptions instead of dropping a prompt or terminal
result. If retained-event capacity terminates an admitted worker, the terminal
result is `operation_outcome_unknown`, never a safely retryable capacity
rejection. A new operation has five seconds to gain its first subscriber.
Afterward, losing the final subscriber starts the same five-second reconnect
grace before the worker is canceled. A client retries one interrupted stream
against the same proof-verified daemon. Daemon loss, retention loss, or
replacement of the runtime owner returns
`operation_outcome_unknown`; the client never repeats the domain mutation to
guess its result. Every unknown-outcome descriptor is non-retryable because the
original mutation may already have completed. If a client cannot render or
otherwise handle a preterminal event, it requests cancellation on a best-effort
basis and reports `operation_outcome_unknown`. A terminal result remains
authoritative even when the client cannot render its terminal event.

Operations reserve daemon work until their workers return, including cleanup
after a terminal outcome is published. Draining refuses new operations and lets
admitted work run until the published replacement deadline. At the deadline it
publishes an unknown terminal outcome, cancels remaining workers and HTTP
handlers, then waits up to five seconds for their reservations to release.

`POST /api/v1/ssh/resolve` evaluates system OpenSSH configuration through one
daemon-owned service and returns an immutable route snapshot. On POSIX, the
service runs nonce-framed `ssh -G` inside the account's configured login shell
so shell startup banners cannot become configuration. Windows invokes system
OpenSSH directly. Each request reloads the global fleet-token environment name
and uses the invoking CLI's fresh environment and working directory rather than
daemon startup state. The client strips configured credential variables before
transport, and the daemon repeats that stripping after reloading its
authoritative configuration. OpenSSH's executable path is bound from this
invocation context before a login shell can change `PATH`. Requests without
invocation-context authority fail closed.
Direct ProxyJump hops are resolved in connection order; opaque ProxyCommand and
nested proxy routes fail closed. The complete normalized option stream
contributes to route identity but never crosses the HTTP boundary. Each target
projection is target-local; a downstream projection requires master-backed
proxy transport through its preceding prepared target and is never a
standalone direct-connect command. This resolution route caps stdout and
stderr at 1 MiB each and cancels the complete resolver process tree when either
bound is exceeded. The daemon also caps the encoded public route snapshot at
8 MiB; the client reserves 64 KiB above that bound for response framing, and an
oversized snapshot fails as `ssh_resolution_failed`. Resolution does not create
a connection, control socket, trust decision, credential prompt, or lease.

`kwt ssh lease` may resolve and acquire in one CLI process when the caller has
no prior snapshot. Clients performing a conditional launch continue supplying
the reviewed route identity and projection policy. `kwt ssh exec` and
`kwt ssh copy` use the combined form for one bounded command or transfer: the
daemon owns route resolution, prompt handling, ProxyJump masters, and lease
lifecycle, while the foreground kwt process owns the system SSH or SFTP child
and streams its output directly. Copy uses SFTP rather than a remote shell,
resolves the local source to an absolute literal path, and escapes SFTP batch
metacharacters; the child receives only the daemon's
generation-bound, fail-closed arguments. Consumers do not reconstruct those
arguments or load SSH configuration again. Cancellation terminates the entire
client process tree. An authenticated HTTP stream binds the lease to the
foreground owner without depending on that process to run heartbeat code while
job-control suspended; disconnecting the stream restores ordinary bounded
expiry. The client releases the daemon lease under a separate cleanup deadline.

`kwt projects` and `kwt list` auto-start or reuse the daemon and require a
current inventory result. They fail instead of falling back to cached or direct
filesystem data. The TUI may paint immediately from the derived last-known-good
cache at `<kwt-home>/cache/inventory-v2.json`, then requests one current
snapshot. Failure to initialize or publish the disposable cache is diagnostic;
current inventory remains available without it. Only dashboard snapshots with
protected tmux sockets resolved may replace the shared cache. The cache is never
mutation authority. Git status and fetch remain in the foreground client so
their credential environment is unchanged.

A current snapshot carries the effective global configuration used for its
discovery. Before enabling actions, the TUI installs that configuration,
collects status with its worktree base directory, and rebuilds config-derived
tmux and credential handling. Cached first paint never changes client
configuration.

Each inventory request carries the invoking client's working directory, home
directory, and sanitized environment map for path expansion. The daemon does
not use its startup environment or working directory to interpret global path
configuration, so a reused daemon preserves foreground CLI semantics.

Repository-local configuration is resolved per request. Unknown content
produces a digest-bound interaction requirement. Approval reopens and hashes
the file before persisting trust; rejection and noninteractive ignore are
request-local. Noninteractive commands preserve the historical global-only
fallback and warning.

Project unregistration takes an exact persisted path, the credential-free
repository identity, and the opaque registration fingerprint from current
project inventory. The fingerprint covers the complete decoded raw registry
entry, including unknown fields and `last_touched`; it is a concurrency token,
not authorization. A home-scoped transition lock hands the exact raw
registration from registry writers to its
identity-keyed project fence: the registration and identity are revalidated
after that fence is acquired, before the transition lock is released.
Registration changes acquire both old and new identity fences in deterministic
order. Worktree creation may remain unregistered, but pull-request import and
protected attach require a current registration. Under the identity fence,
removal reads durable pull-request provenance, follows repository-transfer
alias history across clone-path drift, derives each protected socket, performs
a fail-closed three-state tmux probe, and finally compare-and-swaps the raw
registration. Live sessions block removal; indeterminate probes, disconnected
same-path provenance, or incomplete authority reject it. The transaction
mutates only project metadata and never sends a tmux kill command.

The daemon compares the required fingerprint against its own freshly loaded
exact registration before identity resolution or endpoint inspection. The
raw-entry revalidation and final compare-and-swap remain mutation authority.
A mismatch returns retryable `registration_changed`; clients must refresh and
ask the user to authorize the newly observed entry rather than retrying
automatically. If a transport response is lost, current inventory distinguishes
a completed removal, an unchanged registration, and a same-path replacement.

For remote use, including Ghosthub, the remote shell invokes the remote `kwt`
CLI and that CLI talks only to its same-machine loopback daemon. The daemon is
never exposed as a remote service. A monitoring read may start or replace a
daemon after a kwt upgrade; the later SSH lease migration must retain the
documented replacement-grace behavior for active sessions.
