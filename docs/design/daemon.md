# Local service daemon

Kwt runs at most one writable local service daemon for each canonical kwt
home. The daemon is a host for kwt domain services; it is not the multi-machine
sync hub. It owns worktree inventory reads; worktree mutations, status
collection, tmux attachment, and SSH lifecycle move in later slices.

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

The API exposes authenticated status, graceful shutdown, worktree inventory,
and repository-config approval under `/api/v1`, proof-capable liveness at
`/api/ping`, and credential-free OpenAPI at `/openapi.json`. Inventory clients
require the `worktree.inventory.v1` capability. An operation never has
simultaneous direct and HTTP execution paths.

`kwt projects` and `kwt list` auto-start or reuse the daemon and require a
current inventory result. They fail instead of falling back to cached or direct
filesystem data. The TUI may paint immediately from the derived last-known-good
cache at `<kwt-home>/cache/inventory-v1.json`, then requests one current
snapshot. Failure to initialize or publish the disposable cache is diagnostic;
current inventory remains available without it. The cache is never mutation
authority. Git status and fetch remain in the foreground client so their
credential environment is unchanged.

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

For remote use, including Ghosthub, the remote shell invokes the remote `kwt`
CLI and that CLI talks only to its same-machine loopback daemon. The daemon is
never exposed as a remote service. A monitoring read may start or replace a
daemon after a kwt upgrade; the later SSH lease migration must retain the
documented replacement-grace behavior for active sessions.
