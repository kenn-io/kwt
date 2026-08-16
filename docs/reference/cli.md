# CLI Reference

Run `kwt <command> --help` for command-specific flags. This page summarizes the
stable command surface.

Kwt requires Git 2.20 or newer. `kwt doctor` and the `kwt prune --expired` or
`--merged` policies require Git 2.31 or newer: maintenance inventory relies on
`git worktree list --expire`, and structural repair uses `git worktree repair`.

| Command          | Purpose                                                |
| ---------------- | ------------------------------------------------------ |
| `kwt`, `kwt tui` | Open the cross-project and multi-machine dashboard.    |
| `kwt add`        | Create a worktree and optionally launch its workspace. |
| `kwt branches`   | List branches available for a new worktree.            |
| `kwt open`       | Open or establish a workspace session.                 |
| `kwt list`       | List worktrees.                                        |
| `kwt status`     | Show Git status, sync state, and activity.             |
| `kwt projects`   | List registered project repositories.                  |
| `kwt pr`         | Discover and import pull requests through JSON.        |
| `kwt get`        | Print a matching worktree path.                        |
| `kwt cd`         | Open a shell in a matching worktree.                   |
| `kwt exec`       | Run a command in a matching worktree.                  |
| `kwt remove`     | Delete a worktree, optionally its branch.              |
| `kwt doctor`     | Inspect or repair structural worktree consistency.     |
| `kwt prune`      | Remove live worktrees by an explicit policy.           |
| `kwt sync`       | Publish and inspect multi-machine sync state.          |
| `kwt tmux`       | Manage standalone tmux sessions.                       |
| `kwt workspace`  | Manage directory workspaces.                           |
| `kwt config`     | Read and write config values.                          |
| `kwt daemon`     | Manage the background local service daemon.            |
| `kwt serve`      | Run the local service daemon in the foreground.        |
| `kwt completion` | Generate shell completion and integration.             |
| `kwt version`    | Show version and build information.                    |

## Examples

```sh
kwt add -b fix/parser-race
kwt add --from origin/fix/review fix/review
kwt branches --json
kwt open parser
kwt open /path/to/worktree --start-session
kwt status
kwt pr list --project github.com/acme/widget --json
kwt pr import 17 --project github.com/acme/widget \
  --expected-repository github.com/acme/widget \
  --expected-registration <project.registration_fingerprint> \
  --start-session --json
kwt sync status
kwt exec fix/parser-race -- go test ./internal/parser
kwt doctor
kwt prune --merged --dry-run
kwt workspace add ~/notes
kwt workspace list
kwt workspace list --json
kwt open ~/notes --start-session
kwt config get layouts.default
kwt config set --local layouts.default stack
kwt daemon start
kwt daemon status
kwt daemon status --json
kwt daemon restart
kwt daemon stop
kwt serve
```

`kwt daemon start` replaces a compatible running daemon only when the invoking
build is provably newer and `daemon.auto_restart` is `"newer"`. `kwt daemon
restart` refuses a provable downgrade and also refuses different builds whose
order is unknown, including distinct revisions with the same source commit
time. Use `kwt daemon stop` followed by `kwt daemon start` as the explicit
override when you intend to install an otherwise unordered build. Replacement
and restart print drain progress to stderr before waiting; JSON stdout and
ordinary command exit behavior remain unchanged.

`kwt projects`, `kwt list`, `kwt remove`, and TUI inventory/removal auto-start
or reuse a compatible local daemon. CLI inventory requires a current refresh
and fails if one cannot complete; it never prints cached data. The TUI may
paint from the daemon's last-known-good cache while requesting one current
snapshot. Other worktree mutations, Git status collection, tmux attachment,
and SSH connection lifecycle remain on their existing paths until their
complete service migrations. `kwt ssh resolve` is the non-connecting Stage 1
exception described below.

Successful `kwt list --json` and `kwt projects --json` output remains a bare
top-level array. A daemon or inventory failure instead writes this shared
shape to stdout:

```json
{
  "error": {
    "code": "daemon_draining",
    "message": "the kwt daemon is draining",
    "retryable": true,
    "details": { "drain_deadline": "2026-08-10T01:02:03Z" }
  }
}
```

`code`, `retryable`, and documented detail types are the machine contract;
`message` is explanatory prose and may change. A corresponding single human
line remains on stderr. `list` daemon/inventory failures exit `1`.
`projects` preserves its existing command contract: argument and not-found
failures exit `2`, operational failures exit `1`, and existing mutation codes
such as `registration_changed` retain the same envelope. Kwt does not
normalize unrelated commands' exit behavior. Exit `255` remains unused so an
SSH caller can distinguish a remote-shell transport failure.

Daemon ownership must not buffer human-facing operation progress. Removal
continues to print each selected worktree's result as soon as that target
finishes. Before longer mutations such as add, pull-request import, doctor,
prune, or SSH lifecycle move behind the daemon, their ordered status events
must stream to CLI stderr as they occur; machine-readable stdout and established
exit codes remain unchanged. The daemon now provides the bounded
`operation.stream.v1` transport for that migration: progress and warnings are
flushed to stderr, prompt responses are bound to the prompt that requested
them, and only the owning command writes its final stdout result. This
capability alone does not change any command's execution owner or user-facing
behavior.

An interrupted client may resume one retained stream from its last accepted
event sequence on the same daemon. If the daemon or retained result is gone,
kwt reports `operation_outcome_unknown`; it does not repeat the domain command
and risk duplicating a mutation. This outcome is always non-retryable because
the mutation may already have completed.

## SSH route resolution

```sh
kwt ssh resolve build.example.com --json
kwt ssh resolve 2001:db8::42 --user deploy --port 2200 --json
```

`kwt ssh resolve <hostname> [--user USER] [--port PORT]` asks the same-machine
daemon for the effective OpenSSH route. JSON is the native-consumer contract;
human output prints only credential-free targets in connection order. Stable
kwt domain failures use the shared error envelope and exits `1` or `2`, never
SSH's reserved exit `255`.

Resolution preserves explicit user and port precedence, aliases, raw IPv6
command grammar, and direct ProxyJump order. POSIX hosts evaluate `ssh -G`
inside the account login shell with nonce-framed stdout; Windows invokes
system OpenSSH directly. Opaque ProxyCommand routes and a jump host that adds
another proxy route return `ssh_route_unreviewable`. The command only observes
configuration: it does not connect, approve trust, prompt for credentials, or
create a ControlMaster.

Execution policy `kwt.openssh.projection.v1` emits
`CanonicalizeHostname=no` and resolved `HostName`, `User`, `Port`,
and `HostKeyAlias` in fixed order for each route target. Snapshot targets are
ordered in connection order. A downstream projection is not a standalone
direct-connect command: the current connection-lifecycle owner must combine it
with master-backed proxy transport through the preceding prepared target. Its
positive directive set is:

- trust and crypto: `UserKnownHostsFile`, `GlobalKnownHostsFile`,
  `KnownHostsCommand`, `RevokedHostKeys`, `HostKeyAlgorithms`,
  `KexAlgorithms`, `Ciphers`, `MACs`, `RequiredRSASize`,
  `CASignatureAlgorithms`, `CheckHostIP`, `HashKnownHosts`,
  `VerifyHostKeyDNS`, `VisualHostKey`, and `FingerprintHash`;
- network selection: `AddressFamily`, `BindAddress`, and `BindInterface`;
- authentication: `AddKeysToAgent`, `CertificateFile`, `EnableSSHKeysign`,
  `ForwardAgent`, `GSSAPIAuthentication`, `GSSAPIDelegateCredentials`,
  `HostbasedAcceptedAlgorithms`, `HostbasedAuthentication`, `IdentitiesOnly`,
  `IdentityAgent`, `IdentityFile`, `KbdInteractiveAuthentication`,
  `PasswordAuthentication`, `PKCS11Provider`, `PreferredAuthentications`,
  `PubkeyAcceptedAlgorithms`, `PubkeyAuthentication`, `SecurityKeyProvider`,
  and `UseKeychain`;
- client environment and behavior: `EscapeChar`, `SendEnv`, and `SetEnv`.

Path-bearing authentication values and `SetEnv` are represented only as
owner-private ephemeral configuration lines, not argv or diagnostics. Every
unlisted directive—including forwards, commands, and user ControlMaster
settings—still changes route identity but is never replayed for execution.

## SSH connection leases

```sh
kwt ssh lease build.example.com \
  --route-identity <identity-from-ssh-resolve> \
  --projection-policy kwt.openssh.projection.v1 \
  --json
```

`kwt ssh lease` is the long-lived native-client bridge to the same-machine
daemon. It re-resolves the logical target, refuses a changed route, prepares
every ProxyJump hop in order, and streams operation events as NDJSON. Prompt
events may occur repeatedly; the client answers each with one
`{"prompt_id":"...","value":"..."}` line on stdin. The completion event
contains generation-bound OpenSSH arguments. Each prompt carries the daemon's
deadline, so blocked input ends when the prompt expires and the command reports
the daemon's terminal timeout. OpenSSH confirmation requests use the
`ssh_host_key` kind and are not sensitive; credential and keyboard-interactive
requests use `ssh_authentication` and are sensitive. Every prompt includes its
credential-free logical target, effective target, display target, zero-based
hop index, and route hop count in `details`, so a native client can identify
which direct or ProxyJump target controls the prompt. Host-key prompts also
include a `host_key` object containing the reviewed `host`, `algorithm`, and
`fingerprint`, so native clients do not parse OpenSSH prose. The prompt message
remains OpenSSH's original text. Other OpenSSH confirmation shapes are
preserved as sensitive `ssh_authentication` prompts without a claimed host-key
identity. Unhinted prompts are always handled this way, even when their prose
resembles a host-key question. The process keeps the lease alive
while stdin remains open, touches it every ten seconds, and releases it when
stdin reaches EOF or the command is canceled. Progress and warnings are written
as they occur rather than buffered.

Multiplexed-client arguments reapply the final target's reviewed
`ForwardAgent`, `SendEnv`, `SetEnv`, and `EscapeChar` settings. Private
`SetEnv` values remain in daemon-owned ephemeral configuration rather than
appearing in argv.

The daemon lease bridge requires a persistent OpenSSH master. A platform that
cannot provide multiplexing returns `ssh_route_unreviewable` instead of
publishing direct-connect arguments outside the daemon's prompt and trust
boundary. The embeddable Go API may still return a masterless lease to an
in-process owner that provides that direct execution boundary itself.

The daemon expires a lease after thirty seconds without a successful touch.
Release and expiry cleanup are bounded; a timed-out cleanup retains the lease
for a later retry instead of releasing its daemon reservation.
After final release, `ssh.idle_timeout` controls how long a non-agent-forwarding
master remains warm. Agent-forwarding masters disconnect immediately. Daemon
replacement reports its active lease count, waits through
`daemon.replacement_grace`, then invalidates remaining leases and terminates
their verified masters; a successor never adopts them as live connections.

When `kwt add -b` creates a branch, it fetches `origin` and starts from its
default branch. If that remote base is unavailable, it falls back to local
`main`, then `master`, then the branch checked out in the primary worktree.

`kwt add <branch>` checks out an existing local branch. Use
`kwt add --from <remote-ref> <branch>` when a remote candidate must become a
local tracking branch. Shorthand such as `origin/topic` is accepted, but kwt
verifies it against fetched refs and passes the full `refs/remotes/...` identity
to Git. Neither existing-branch path copies files, runs setup commands, or
launches a workspace; `--layout` and `--select-layout` are rejected.
Git branch mutation and checkout run with an empty hooks directory, configured
smudge and process filters disabled, and kwt credential variables removed.
Environment references in the source-derived branch name remain literal when
kwt builds the destination path. Submodules are not recursively updated during
creation; inspect the superproject first, then update submodules explicitly
after acknowledgement.

The created worktree participates in ordinary status and fleet observation.
Review the checkout and run `kwt open <worktree>` as the explicit
acknowledgement that opts in to its layout and pane commands.

`kwt branches --json` emits only candidates not already checked out, with
`name`, a source-qualified display `label`, the full source ref, and
`is_remote` fields for interactive clients.

## `kwt open`

With no argument, `kwt open` fuzzy-picks a worktree. A pattern narrows the
cross-project list and opens the sole match directly. An exact registered
directory workspace path resolves before Git worktree discovery. Kwt creates
or repairs the canonical tmux workspace with its resolved layout before
attaching.

An exact worktree-root path is resolved directly from Git before pattern
matching, including registered primary checkouts and linked worktrees outside
the configured global worktree base.

On Unix-like systems, an external ordinary or protected attachment replaces
the `kwt` process with the tmux client. tmux therefore owns signal handling and
the final exit status, and no waiting `kwt` parent remains. Windows retains a
waiting parent because it has no Unix process-replacement primitive.
An ordinary open from inside tmux switches the current client instead.
Protected attachment always remains external because it targets a separate
workspace-specific socket.

`kwt open <exact-workspace-path> --start-session` performs the same layout and
session bootstrap without attaching a client. Use it before an external
ordinary tmux client attaches to a session that may not exist yet. Registered
directory paths resolve from the workspace registry; worktree paths resolve
directly from Git rather than the global worktree base. This keeps automation
noninteractive and supports both plain directories and linked worktrees stored
outside that base.
Protected pull-request imports remain restricted to `kwt pr attach`.

## `kwt workspace`

`workspace add [path]` registers a plain directory. The `workspace remove`
command accepts its name and unregisters it without deleting the directory or
killing a live tmux session. `workspace list` reports registered directories
and session state.

`workspace list --json` emits an array of objects with `name`, canonical
absolute `path`, effective `session_name`, and boolean `session_live`. An empty
registry emits `[]` with no table prose. When a workspace was renamed while its
session remained live, `session_name` reports that matching live session so
clients can attach to it; otherwise it reports the canonical name kwt will use
when establishing the session.

## `kwt list`

The command obtains inventory from the same-machine daemon, including when a
remote shell invokes kwt. Its human output and machine-readable schema are
unchanged; freshness metadata stays in the daemon API envelope and is not
added to the top-level JSON array.

`--json` emits an array of objects with `path`, `branch`, `commit_hash`, `is_main`,
`created_at` (worktree directory mtime), `generation` (the durable identity for
conditional removal), `repository` (the `host/owner/name` slug, or a
`local/<path>` fallback for a repository without a usable remote — see below),
and `session_name` (the tmux workspace session name kwt attaches to).
An imported pull-request worktree additionally includes `tmux_socket_name` for
its protected workspace-specific server. To converge on the same session, run
`kwt open <path> --start-session` before an ordinary attach-only client, or
`kwt pr attach <path>` when `tmux_socket_name` is present. This lets kwt create
the session when needed without a client creating it bare or bypassing its
protected attach policy. See [Attaching from other
tools](#attaching-from-other-tools) before using `new-session`.
`kwt open` and dashboard open actions refuse protected pull-request imports
and direct the user through `kwt pr attach`.
`created_at` and `generation` are populated in both local and `-g` mode.

## `kwt remove`

`--if-generation <id>` makes a single-worktree removal conditional on the
`generation` value returned by `kwt list --json`. Kwt stores this random
identity in the worktree's Git administrative directory and compares it while
holding the repository's worktree-mutation lock, so automation cannot delete a
replacement checkout created at the same path. Ordinary directory changes do
not alter the generation.

Removal is executed by the same-machine daemon through kwt's public Go service.
The daemon reopens the named repository and revalidates the exact path,
generation, main-worktree status, and lock state while holding the repository's
cross-process mutation lock. It also performs generation-safe registry cleanup.
The CLI and TUI retain selection, output, fleet publication, and tmux-session
cleanup; stale inventory alone never authorizes deletion.
Automation that has confirmed terminal-session state can add
`--if-session-name` with `--if-session-absent`.
`--if-session-socket-directory` selects an explicit `TMUX_TMPDIR`.
`--if-session-socket-name` selects an exact named socket such as the
workspace-specific server used by an imported pull request. Supply both
selectors for a legacy named socket located under an explicit `TMUX_TMPDIR`;
otherwise omit the directory to use the canonical named-socket location.
These flags require `--if-generation`. Kwt revalidates absence while holding
the same project lifecycle fences used by guarded `kwt open`. A live-session
identity is rejected with a retryable conflict: callers must explicitly stop
the exact session, freshly confirm absence, and retry. Kwt does not claim an
atomic live-session freeze on a shared tmux server because tmux command queues
can continue changing topology while an external signal helper starts.
Known Git removal failures use the stable `removal_failed` code and preserve
their credential-sanitized message and partial-result fields across the daemon
boundary. Unexpected failures use `internal` and withhold their cause.
If a removal response is lost, the client requests a fresh bounded repository
inventory. A missing original generation is reported as an irreversible partial
result so fleet publication, TUI refresh, and session cleanup still run. If the
postcondition cannot be observed, the client reports an indeterminate transport
failure and requests fleet/UI refresh without claiming removal or killing tmux.

## `kwt doctor`

Requires Git 2.31 or newer.

```sh
kwt doctor          # inspect without changing anything
kwt doctor --fix    # apply only confirmed, unambiguous repairs
kwt doctor --json   # emit exact findings for automation
kwt doctor --quiet  # print nothing; use the exit status
```

Doctor reads project registrations, Git worktree metadata, filesystem
backlinks, the global worktree directory, and live registry paths. The human
report starts with a summary, lists **Ready to fix** findings before **Needs
review**, and omits healthy repositories. Long home and temporary paths are
shortened for display; `--json` keeps exact paths and versioned evidence.
After `--fix`, the report first lists each completed repair under **Fixed**,
followed by findings that remain after the rescan. JSON exposes the same
repairs in `fixed` and counts them in `summary.fixed_findings`. Use `--quiet`
when only the exit status matters; it cannot be combined with `--json`.

If a registered project path is confirmed absent, doctor searches only its
bounded local inventory for the same credential-free repository identity:

- one matching main repository produces `project_path_moved` and `--fix`
  updates the registration to that verified absolute path;
- one matching main repository that is already registered produces
  `stale_project_registration` and `--fix` removes only the unchanged duplicate;
- multiple missing registrations that claim the same matching repository
  produce `ambiguous_project_relocation` and require you to choose which
  registration should remain;
- no match produces `stale_project_registration` and `--fix` removes the
  unchanged registration;
- multiple matching clones produce `ambiguous_project_relocation` and require
  a manual choice; and
- path-derived, malformed, or otherwise unsafe identities remain
  `project_unreachable` and are never changed automatically.

Project changes use full-entry compare-and-swap, preserve the name and last
touched time, and become safe no-ops if the path, target, or global config
changes concurrently. A relocation is also a no-op if another project becomes
registered at the target before the config transaction commits. Doctor reloads
both global config and the registry before its final report, so a successful
fix can exit healthy while a no-op remains an
exit-`1` finding. Generation-less registry adoption also rechecks the live Git
generation while holding the repository mutation lock, so a replacement at the
same path is never assigned the inspected worktree's generation.

`kwt doctor --fix` repairs only uniquely owned backlinks and confirmed-absent
Git or registry records. It can adopt a generation-less registry record with
compare-and-swap after clearing inherited expiration metadata, including when
the registry path is a symlink alias for the live Git worktree. A nonempty
registry/Git generation mismatch is a possible replacement conflict and stays manual.
Because Git can repair multiple backlinks in one operation, doctor
skips backlink repair for a repository unless every repairable backlink is in
the validated fix scope. Doctor repairs backlinks before native metadata
pruning, prunes only when every removable record is in the validated fix scope,
revalidates under each repository's mutation lock, and rescans before reporting
the final state. A linked worktree's administrative directory is accepted only
when a full scan of the repository's worktree metadata finds exactly one
reverse backlink to that path. Registry entries are not reported or changed
while their creator still holds the path lock. Tokens abandoned after a creator
exits are inspected and cleaned only after doctor acquires that same lock.
Doctor groups registry paths by canonical filesystem identity. Equivalent
aliases for one live worktree appear under **Duplicate registry paths**, and `--fix`
atomically collapses the unchanged group to the path spelling recorded by Git.
Equivalent aliases for a worktree path confirmed absent are removed as one
unchanged group during the same fix run after absence is rechecked. Aliases with
different generations, expiration, source-review state, or other policy
metadata, incompletely inspectable groups, and aliases for an existing path
that is not a verified Git worktree remain under **Needs review**. It
normalizes legacy registry and configured project URLs before comparison and
output. Values that cannot be converted to credential-free identities are
redacted or omitted; reachable projects fall back to the canonical live origin
or local identity. Origin-based expiration records are valid when they match
the verified origin for that worktree, even when the configured project names
an upstream repository. It never removes a live worktree. A live worktree
without a valid Git generation is manual until a normal listing from the
verified repository (`kwt list`) adopts it; rerun `kwt doctor --fix` afterward
to reconcile a generation-less legacy registry record. Project registration
repair runs afterward, so Git, registry, and global-config locks are never
nested.

Exit status is `0` when healthy, `1` when findings remain, and `2` when
inspection or requested fixes cannot complete.

## `kwt prune`

Prune requires Git 2.31 or newer and exactly one live-removal policy:

```sh
kwt prune --expired --dry-run
kwt prune --expired
kwt prune --expired --force
kwt prune --merged --dry-run
kwt prune --merged
```

`--expired` evaluates registered expiration times. An expired entry whose path
is already absent reports `doctor_required` and points to `kwt doctor --fix`;
expiration pruning no longer silently unregisters it. `--force` is available
only for expiration policy and never bypasses the generation requirement.
Ignored build artifacts do not count as dirty for expiration policy and are
removed with the worktree; use `--dry-run` to preview the decision. Kwt
rechecks the complete expiration record before removal. If another command
extends or clears the expiration after inspection, the worktree is preserved
with `expiration_policy_changed`.

`--merged` inventories linked worktrees from configured projects, the global
worktree directory, and finalized registry entries, then confirms GitHub
pull-request evidence before any removal begins. Registry entries still owned
by a creation token remain doctor-only. Imported worktrees must match their
complete provenance, including canonical live workspace and main-repository
paths; an existing
symlink alias is equivalent to its resolved path. New imported-worktree
provenance also carries the durable generation, preventing an old record from
matching a replacement at the same path. A provenance-backed candidate also
requires its current alias-connected project registration. Kwt holds that
project authority while it probes the exact protected tmux endpoint, removes
the worktree, and compare-and-swap deletes provenance. A live endpoint reports
`protected_session_live`; an unavailable or indeterminate endpoint reports
`protected_endpoint_inventory_incomplete`. Both preserve the worktree and
provenance. Protected sessions created by released kwt versions under an
explicit `TMUX_TMPDIR` remain attachable and continue to block pruning when the
invoking client supplies that same directory; new sessions use kwt's canonical
owner-specific socket location. Ordinary worktrees must have
an exact configured upstream repository and branch read from that linked worktree,
and their current HEAD must exactly match one associated PR head SHA
with a non-null merged timestamp. GitHub repository redirects are resolved
before PR lookup and checked against imported provenance aliases. Redirects for
the upstream source repository are also resolved for PR matching and diagnostic
head lookup, while the observed upstream remains the lock-time removal
condition. This recognizes squash and rebase merges without using local
ancestry. A configured project remains the PR base when its local `origin` is a
fork; kwt reads the observed origin from each linked worktree separately and
revalidates it, the local branch, and the upstream under the repository lock.
If a configured project's repository identity is missing or invalid, the
candidate reports `doctor_required`; only unconfigured discovery roots may use
their live origin as the PR base. That fallback also requires every configured
project root to be inspectable. If one is stale or temporarily unavailable,
globally and registry-discovered candidates report `doctor_required` before
GitHub lookup, while candidates from healthy configured projects remain
eligible. Run `kwt doctor --fix` to repair or remove the stale registration and
restore unconfigured discovery.
When global discovery finds a repository that is not configured, each
worktree's own observed origin becomes its PR base; one worktree's origin is
never reused for a sibling. An expiration registry record may identify either
that verified origin or the configured project. A missing or noncanonical
origin preserves the worktree before provider lookup. Advanced heads, ambiguous matches,
closed-but-unmerged PRs, dirty worktrees, and missing generations are preserved
with explicit reasons. There is no merged-policy force mode, and removal
preserves the local branch. Ignored untracked files count as local changes and
also preserve the worktree. Removals execute from the resolved main repository,
so pruning one globally discovered worktree does not strand later candidates.
Multiple registry paths that resolve to the same worktree are ambiguous and
report `doctor_required` instead of allowing path-order-dependent removal.
Git commands run against candidate worktrees without kwt's GitHub or fleet
credentials in their environment, including during lock-scoped validation and
removal. This prevents worktree-selected filters, hooks, or filesystem monitors
from receiving those tokens.

An individual path under the global worktree directory that is no longer a
usable Git worktree reports `doctor_required`; it does not prevent other
repositories from being evaluated. An incomplete directory traversal still
stops inspection with exit status `2`, because kwt cannot know what was missed.
If Git deregisters a worktree but cannot remove every file, prune completes its
guarded registry and provenance cleanup, publishes the new fleet state, and
reports `cleanup_incomplete` with the residual path for manual inspection.

Both policies support `--dry-run` and `--json`. Exit status is `0` when all
confirmed candidates were handled or no candidate exists, `1` for safety skips,
provider failures, or incomplete cleanup, and `2` when inspection or command
usage cannot complete. Bare `kwt prune` is an exit-`2` usage error: run
`kwt doctor --fix` for the stale-metadata behavior that the bare command used to
perform. JSON result reports use stdout without duplicating human result
summaries on stderr. Dry-runs revalidate Git's current inventory under the same
repository lock as removal and report `locked_worktree` or `main_worktree`
instead of claiming those paths would be removed.

## `kwt projects`

The list action obtains a current inventory snapshot from the same-machine
daemon. `projects add` remains a direct foreground mutation. `projects remove`
is an authenticated daemon-owned transaction so it can serialize registry
changes with worktree creation, pull-request import, and protected-session
establishment.

`--json` emits an array of the registered project repositories (`{repository,
name, path, last_touched, registration_fingerprint}`), so external automation
can discover main-repo paths that live outside the configured worktree base
directory without parsing the config file. `repository` uses the same
`host/owner/name` slug as `kwt list --json`'s `repository` field, so the two
surfaces can be joined. `registration_fingerprint` is an opaque observation
token for the complete persisted entry; callers must not parse or synthesize
it.

Entries whose configured paths are missing, inaccessible, or no longer Git
repositories remain visible. Their `path` is the exact persisted value and
their `repository` is a stable credential-free identity, allowing automation
to authorize metadata removal without inspecting the checkout. Pathless
entries are omitted. Kwt does not scan for moved checkouts; running kwt in the
checkout's new location, or using `kwt projects add <new-path>`, updates the
existing record by repository identity.

`kwt projects add <path>` registers an existing Git checkout without opening
the dashboard. A linked-worktree path resolves to its main repository before
registration, and repeating the command updates the existing entry rather than
adding a duplicate. Registration changes serialize with protected project
operations across both the old and new repository identities.

Machine callers use `kwt projects remove <exact-registered-path>
--expected-repository <identity> --expected-registration <fingerprint> --json`
to unregister exactly one observed project. Supply all three values from the
same current `kwt projects --json` entry; paths are matched byte-for-byte,
including trailing whitespace. A human invocation without `--json` may omit
both expected flags, in which case kwt performs one current exact-path lookup
before submitting the removal. Supplying only one flag is invalid. Kwt never
automatically refreshes and retries a `registration_changed` response.

Canonical stored identities are authoritative; legacy registrations use an
authoritative live Git identity when available and an exact-path local identity
otherwise. The checkout may no longer exist. The daemon validates the opaque
fingerprint before resolving identity, then verifies the project's durable
protected endpoints, including connected repository-transfer aliases, under
the shared project fence before performing a final raw registry
compare-and-swap. Any persisted-field change, including `last_touched`,
invalidates the fingerprint. A live protected tmux session or incomplete
endpoint authority fails closed. Ordinary/default-server tmux sessions do not
block removal and are never killed.

Unregistration is metadata-only: it never deletes repositories or worktrees
and never kills a tmux session. Concurrent registration changes are preserved
and reported as retryable. Worktree creation and pull-request import keep their
existing foreground status output; the daemon fence does not make these CLI
operations asynchronous or hide progress.

With `--json`, `projects add` returns `status: "registered"`; removal uses the
same project object with `status: "unregistered"`:

```json
{
  "status": "registered",
  "project": {
    "repository": "github.com/kenn-io/kwt",
    "name": "kwt",
    "path": "/code/kwt",
    "last_touched": "2026-07-27T11:16:16Z"
  }
}
```

Failures return a stable error envelope on stdout. `invalid_repository` exits
2; `registration_failed` exits 1. Project removal additionally reports
`project_not_found` with exit 2 and `unregistration_failed`,
`protected_session_live`, or `protected_endpoint_inventory_incomplete` with
exit 1. The retryable `registration_changed` also exits 1. Protected-session
errors expose only the sanitized session and protected-socket identity needed
to resolve the block:

```json
{
  "error": {
    "code": "invalid_repository",
    "message": "/missing is not an accessible Git repository",
    "retryable": false
  }
}
```

## `kwt pr`

`kwt pr list` and `kwt pr import` are the noninteractive, structured boundary
for pull-request clients. kwt owns provider calls, ref handling, branch and
workspace naming, normal worktree creation and setup, push configuration,
provenance, and tmux session naming. See [Pull-request
automation](pull-requests.md) for the JSON and exit-status contract.
Every imported workspace record includes `tmux_socket_name`.
`pr import --start-session` additionally establishes a blank shell-only
session without attaching, for clients that provide their own ordinary tmux
presentation. It does not execute configured layouts or agent commands.
Automation clients may pass `--expected-repository` and
`--expected-registration` together to bind an import to the selected
`kwt projects --json` record. Kwt revalidates that authority under the project
lifecycle fence before creating the worktree; a mismatch fails with retryable
`registration_changed` and does not import anything.
Attach with `kwt pr attach <workspace.path>`, which verifies the persisted
identity, creates or repairs that protected blank session when needed, and
uses `attach-session -E`. This is an interactive exception to the PR JSON
contract: failures before attachment remain structured, but after a successful
Unix process replacement tmux owns terminal output and the final exit status.
Automation clients may additionally pass `--expected-repository`,
`--expected-registration`, `--expected-generation`, `--expected-session`, and
`--expected-socket` together. Kwt revalidates that exact inventory authority
under the project lifecycle fence before creating or repairing the protected
session; a mismatch fails with retryable `registration_changed` and does not
touch tmux. Omit all five flags for ordinary interactive attachment.
Provenance with a durable generation applies only to that exact worktree
incarnation. If the path is later reused, stale provenance neither protects the
replacement from ordinary `kwt open` nor authorizes `kwt pr attach` or merged
pruning. Generation-less legacy provenance continues to match by its verified
path, branch, and repository identity.

### Repository identity fallback

A repository's `repository` slug is derived from its origin remote
(`host/owner/name`). A repository with no usable remote instead gets a
deterministic path-based fallback of the form `local/<absolute-path>` (path
separators normalized to `/`). Every surface that reports repository identity —
`kwt list --json`, `kwt list -g --json` discovery, and `kwt projects --json` —
resolves it through the same code, so the fallback is identical across all
three and the surfaces remain joinable even for local-only repositories.

## Workspace session bootstrap

Every workspace session kwt creates (`add`, `open`, and the TUI) applies the
same bootstrap so panes are indistinguishable regardless of which client
attaches. kwt sets `default-command` to the empty string, which tells tmux to
start its configured `default-shell` natively instead of passing a command
string through `$SHELL -c`. This supports valid non-POSIX shells such as fish
and tcsh and avoids running startup hooks in an extra non-login shell. For the
first pane, kwt queries the session's resolved `default-shell` and executes it
directly with `-l` after applying the session environment bootstrap. This
login-shell behavior is the parity mechanism across launchers; panes otherwise
see tmux's own `TERM_PROGRAM`/`TERM_PROGRAM_VERSION`, which tmux sets in every
pane regardless of what kwt does.

Session creation uses an inert first-pane placeholder to make that environment
ordering safe. `new-session` starts `sleep 2147483647` as separate argv words,
so no user shell or profile runs before the session exists. kwt then installs
the session remove-markers, resolves `default-shell`, and replaces the
placeholder with `<resolved-shell> -l`. Only after that respawn does it create
the remaining panes. Thus every real shell starts after launcher state has been
masked, and the first pane's startup files run exactly once.

kwt treats a canonical set of variables as launcher state — scoped to the
terminal, shell, or tool that launched kwt, not to the workspace session tmux
hosts — and applies it in two places that share a single definition, with one
explicit, documented exception, so they cannot silently drift apart:

- **Exec-time sanitization.** Every tmux invocation kwt makes execs tmux with
  these variables removed from its own environment, EXCEPT `EDITOR` and
  `VISUAL`. If no tmux server is running yet, the invocation that starts one
  is what seeds that server's GLOBAL environment table; sanitizing at exec
  time keeps launcher state out of that table in the first place, rather than
  only masking it later per session. (`PWD`/`OLDPWD`/`SHLVL`/`_` are included
  even though they aren't terminal-integration variables: worktree
  directories are passed to tmux via `-c`, and shells re-derive these on
  their own.) `EDITOR`/`VISUAL` are kept here because tmux itself reads them
  at server start to choose its default key mode (`status-keys`/`mode-keys`:
  vi vs. emacs), and a user's `tmux.conf` may consult them too; stripping
  them from the server's own exec environment would silently flip that
  behavior for every kwt-started server.
- **Session remove-markers.** The same variables — including `EDITOR` and
  `VISUAL`, with no exception — are also removed from each session with a
  session-scoped remove-marker (`set-environment -r`), which masks the
  global/server value for that session only without touching other sessions
  or the server-wide environment. This covers sessions created against an
  already-running server whose global table predates kwt's exec-time
  sanitization (e.g. a server another tool started), and it is what keeps
  `EDITOR`/`VISUAL` out of every pane's shell even though the server process
  itself now keeps them.

The full list: exact names `__CFBundleIdentifier`, `EDITOR`,
`KWT_FLEET_TOKEN`, `KWT_GITHUB_TOKEN`, `OLDPWD`, `PROMPT`, `PROMPT_COMMAND`,
`PWD`, `RPROMPT`, `SHLVL`, `TERM_PROGRAM`, `TERM_PROGRAM_VERSION`, `VISUAL`,
`WINDOWID`, `_`; and prefixes `ALACRITTY_`, `CONDA_`, `FZF_`, `ITERM`,
`KITTY_`, `NVM_`, `PYENV_`, `STARSHIP_`, `VIRTUAL_ENV`, `WEZTERM_`, `WT_`,
`VSCODE_`. Every kwt workspace also removes the exact variable named by
`fleet.token_env`, case-insensitively, both from tmux subprocesses and from
the session environment. Operational kwt variables such as `KWT_HOME` are
preserved. `EDITOR` and `VISUAL` are excluded from exec-time sanitization
only, per above; every other name in this list is treated identically by both
mechanisms.

`TERMINFO` is deliberately excluded from the whole list: it is functional
terminal configuration (a custom terminfo database path), not transient
launcher-integration state, and is needed for tmux attach rendering and for
pane applications resolving tmux's own `TERM`.

### Attaching from other tools

kwt applies this bootstrap when it _creates_ a session. A session that some
other tool created — for example with `tmux new-session -A -s <session_name>`,
which attaches if the session exists but otherwise creates it bare — starts
without the `default-command` and remove-markers, so its windows would not
match kwt's until repaired.

Two rules keep external tools consistent with kwt:

- **When the session already exists, attach only:** use `tmux attach-session -t
<session_name>` (or `switch-client -t` from inside tmux). Attach-only commands
  never create a bare session, so there is nothing to repair.
- **If your tool creates the session itself, apply the equivalent bootstrap:**
  set `default-command` to `""` and add a session-scoped remove-marker
  (`set-environment -r <name>`) for each launcher variable listed above
  (including `EDITOR`/`VISUAL` — the exec-time exception above applies only to
  how the tmux server process itself was started, not to the session
  remove-markers). To keep the first pane clean as well, create it with an
  inert direct-argv placeholder, install the markers, resolve the session's
  `default-shell`, and respawn that pane with the resolved shell and `-l`.

kwt is also self-healing here: the next time it attaches to a session it finds
already running, it re-applies the safe bootstrap subset (`default-command`
plus the remove-markers — never construction or pane commands), so a session
another tool created bare converges on consistent behavior for windows opened
after that attach.

PR imports use a stricter reuse boundary. Every import reports a deterministic,
workspace-specific socket. `pr import --start-session` and `pr attach` create
one blank shell session, record the canonical workspace path, and reuse only a
same-named session with that exact marker. They never execute configured layout
or agent commands. The isolated server starts without the provider or
configured fleet credential. The protected session also masks those names and
filters them from `update-environment`. Because tmux options are mutable,
`pr attach` enforces `attach-session -E` rather than trusting the current
option value.

The repair path deliberately does not rewrite panes in an externally created
session that is already running; it only makes future windows consistent. In a
session kwt creates itself, the inert-placeholder/respawn sequence also covers
the first pane, including when the tmux server was already running with
launcher variables in its global environment table.

## Exit behavior

Commands intended to launch the dashboard or attach to tmux require an
interactive terminal. In non-interactive contexts, use data-oriented commands
such as `list`, `status`, `get`, and `exec`.
