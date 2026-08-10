# Guarded Project Unregistration

## Status

Approved for implementation on 2026-08-10.

## Purpose

Ghosthub must be able to unregister a kwt project whose checkout is missing or
inaccessible without relying on stale worktree inventory. The operation must
bind to the exact persisted project registration and must not orphan a live
protected tmux session by removing its inventory owner.

This change combines two requirements in one delivery:

1. exact path plus credential-free repository identity selects the registration;
2. one daemon-owned transaction verifies protected endpoint authority and
   removes the registration only when no protected session is live.

The daemon implementation has not shipped. The change evolves it directly and
does not retain a legacy route, path-only fallback, or direct project-removal
execution path.

## User Contract

The removal command is:

```text
kwt projects remove <exact-registered-path> \
  --expected-repository <credential-free-identity> \
  --json
```

Both the exact path and expected repository are required. The path is the raw
persisted registry identity. The CLI, daemon client, HTTP request, service, and
configuration layer must not trim, clean, expand, canonicalize, or otherwise
rewrite it. A registration at `/repo ` is distinct from `/repo`.

The daemon client separately captures the invoking client's sanitized path
expansion context. The service uses that context only to derive the selected
registration's effective clone path for provenance association; it never
changes the exact persisted path used for selection or CAS.

The expected repository uses the same canonical, credential-free identity
emitted by `kwt projects --json`. Removal never accepts or returns a
credential-bearing repository URL.

Successful JSON retains the existing shape:

```json
{
  "status": "unregistered",
  "project": {
    "repository": "github.com/acme/widget",
    "name": "widget",
    "path": "/exact/persisted/path",
    "last_touched": "2026-08-10T00:00:00Z"
  }
}
```

Removal remains metadata-only. It never deletes a checkout, linked worktree,
branch, sentinel file, provenance record, tmux server, or tmux session.

## Architecture

### Daemon-owned transaction

`projects remove` becomes a client of a project-removal service hosted at
`POST /api/v1/projects/remove`. The service owns the full transaction:

1. validate the request structure and canonical expected repository;
2. acquire the project lifecycle fence;
3. reload the global project snapshot;
4. find exactly one entry whose persisted path equals the request path byte for
   byte;
5. compare its publishable canonical repository identity with the expected
   identity;
6. load and validate the complete durable protected-endpoint authority for that
   project clone;
7. probe every protected socket while the fence remains held;
8. reject live or indeterminate state;
9. remove the exact raw project entry through the existing configuration CAS.

The checkout and Git repository are never inspected during this transaction.
Consequently, deleted or inaccessible checkouts remain removable when durable
endpoint authority is complete and no protected session is live.

The daemon advertises the truthful `project.removal.v1` capability. The
capability is runtime introspection, not a compatibility bridge. The old direct
command path and its path-only config helper are removed in the same change.

### Project lifecycle fence

A cross-process project fence lives under KWT_HOME and is keyed by a SHA-256
digest of the canonical credential-free repository identity. The digest avoids
putting repository names or credentials in filenames. Lock ordering is:

```text
project lifecycle fence
  -> repository worktree mutation or creation lock
  -> pull-request provenance lock
  -> global configuration lock
```

Code must not acquire the project fence while holding a downstream lock.

The fence is shared by:

- guarded project removal;
- pull-request import, including optional protected-session startup;
- protected `kwt pr attach` session establishment;
- worktree creation/import that began from a registered project snapshot.

An in-flight worktree creation records the matching registration before it
waits for the fence and revalidates that registration after acquiring it. If
removal won, the creation fails with `registration_changed`. A new `kwt add`
started after unregistration remains valid and follows the existing local
repository workflow without requiring project registration.

If import or creation wins the fence, removal observes the completed durable
state. If removal wins, a waiting import, creation, or protected-session
establishment cannot proceed from its stale registration.

Protected attach holds the fence only through provenance revalidation and
session establishment. It releases the fence before invoking the blocking tmux
client attachment. Once a protected session exists, removal discovers it by
probing the protected socket and rejects independently of whether a client is
attached.

### Durable endpoint authority

The owner-only pull-request provenance store is the durable authority for
protected endpoints. It remains usable when the project checkout and worktree
paths are missing. Records belong to the selected project clone only when both
their canonical repository identity and recorded project path match the
selected registration's identity and effective path.

Every selected record must provide and validate:

- project identity and project path;
- worktree path;
- durable worktree generation;
- exact tmux session name;
- deterministic protected socket name derived from session name and worktree
  path.

The service reads the complete provenance snapshot under its existing lock.
Unreadable, undecodable, unsupported, ambiguous, or incomplete authority fails
closed. A record with the matching repository identity whose clone ownership
cannot be proved also fails closed rather than being ignored.

Ordinary/default-server tmux sessions are outside this authority. They neither
block project removal nor get inspected or killed.

### Tmux probing

Protected liveness uses a context-aware named-socket probe that distinguishes:

- **live**: the expected exact session is present on the protected server;
- **absent**: no protected server/session exists for the endpoint;
- **indeterminate**: tmux could not provide authoritative state, or the named
  server contains unexpected state.

Only absent permits the transaction to continue. Live returns
`protected_session_live`. Indeterminate returns
`protected_endpoint_inventory_incomplete`. Attached and detached live sessions
are equivalent.

No generic boolean `HasSession` result is sufficient because command failure
must not be collapsed into absence.

## Stable Outcomes

The project command continues to use the shared machine-readable error
envelope established by kwt #72.

| Code                                      | Retryable      | Meaning                                                                                         |
| ----------------------------------------- | -------------- | ----------------------------------------------------------------------------------------------- |
| `project_not_found`                       | no             | No persisted path matches exactly.                                                              |
| `registration_changed`                    | yes            | Repository identity differs, CAS lost, or an in-flight mutation lost registration revalidation. |
| `protected_session_live`                  | no             | The exact protected endpoint has a live tmux session.                                           |
| `protected_endpoint_inventory_incomplete` | cause-specific | Endpoint authority or liveness cannot be proved complete.                                       |
| `unregistration_failed`                   | no             | A known project-unregistration failure occurred outside the states above.                       |
| `internal`                                | no             | An unexpected private cause was withheld.                                                       |

`protected_session_live` details contain only sanitized `session_name`,
`socket_name`, and opaque `generation`. The incomplete code is retryable for a
transient provenance-lock or tmux-probe failure and non-retryable for malformed
durable records. Public messages and details never contain remote URLs, raw
repository configuration, environment values, or subprocess output.

## Internal Go Surface

The root `go.kenn.io/kwt` package exposes only the small contracts needed by the
daemon and embedders:

- `ProjectRemovalRequest` with exact path and expected repository;
- `ProjectRemovalResult` with the credential-free removed project;
- `ProjectRemover`;
- `NewProjectRemovalService` and its focused options.

Implementation types, fence mechanics, provenance inspection, and tmux probes
remain internal.

## Verification

Behavioral coverage must prove:

1. `/repo ` is removable only with the exact `/repo ` argument; `/repo` is
   distinct, and the CLI preserves trailing whitespace.
2. Matching path and repository identity succeeds; mismatched identity and CAS
   replacement return `registration_changed` and preserve the entry.
3. A missing checkout with an attached protected session rejects removal.
4. A missing checkout with a detached-but-live protected session rejects
   removal.
5. A missing checkout with no protected session unregisters successfully.
6. Checkout directories, linked worktrees, sentinels, provenance, and ordinary
   live tmux sessions remain untouched.
7. Removal racing protected session establishment cannot orphan a session:
   establishment wins and removal rejects, or removal wins and establishment
   returns `registration_changed` before creating the session.
8. Removal racing pull-request import and registered-project worktree creation
   follows the same fence and revalidation rule.
9. Unreadable, corrupt, ambiguous, incomplete, and indeterminate endpoint
   authority rejects without registry mutation.
10. Daemon HTTP and CLI JSON preserve codes, messages, retryability, safe
    details, and the existing success shape without exposing repository
    credentials.

Tests exercise owned behavior at configuration, fence, service, daemon HTTP,
and CLI subprocess boundaries. They do not duplicate filesystem-lock or tmux
library behavior already covered by those packages.

## Delivery

The implementation lands as one PR after kwt #72. Exact identity selection is
built first, followed by the shared fence and guarded endpoint transaction.
Documentation moves with the CLI and daemon behavior. The local superpowers
spec and implementation plan are removed before the branch is pushed so they
do not become shipped product documentation.
