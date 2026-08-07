# Local service daemon

Kwt runs at most one writable local service daemon for each canonical kwt
home. The daemon is a host for kwt domain services; it is not the multi-machine
sync hub. The foundation release does not route existing worktree, TUI, or SSH
operations through it.

The process binds an automatically selected IPv4 loopback port and publishes
an owner-only runtime record under `<kwt-home>/runtime`. Clients verify the
recorded PID, process creation identity, service identity, and a challenge
proof before they send the record's bearer credential. A matching live but
unresponsive owner is preserved; kwt never starts a competing writer.

`kwt daemon start|stop|restart|status` manages the background process. `kwt
serve` runs the same host in the foreground, disables idle exit, and refuses to
replace an existing owner. Background logs are written to
`<kwt-home>/daemon.log`, rotate at 10 MiB, and retain three owner-only backups.

Compatible clients share the newest running daemon. A newer client asks an
older daemon to drain before replacement; an older client never downgrades a
newer daemon. Draining rejects new operations with a retryable typed response
that carries the deadline. Active work and leases may finish until
`daemon.replacement_grace`; the default is five minutes.

The foundation API exposes authenticated status and graceful shutdown under
`/api/v1`, proof-capable liveness at `/api/ping`, and credential-free OpenAPI at
`/openapi.json`. Domain APIs and automatic daemon startup arrive with their
individual migrations; an operation never has simultaneous direct and HTTP
execution paths.
