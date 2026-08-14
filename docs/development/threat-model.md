# Threat model

`kwt` is a local developer tool, not a sandbox for Git repositories. Security
reviews should distinguish data controlled by a branch from execution policy
already controlled by the machine's owner.

## Trusted local state

The following are trusted because changing any of them already gives the local
user or an attacker equivalent code-execution capability outside `kwt`:

- the operating-system account and its environment;
- executables on `PATH`, shell and tmux configuration, and agent commands;
- global `kwt` configuration;
- an existing repository's Git configuration, hooks, remotes, URL rewrites,
  credential helpers, filters, and fsmonitor configuration; and
- authenticated fleet peers belonging to the same user.

Consequently, ordinary Git inspection after a successful checkout—including
status, log, diff, ref discovery, and fleet metadata collection—is within the
accepted execution boundary. If trusted Git configuration deliberately invokes
a helper while performing those operations, `kwt` does not try to provide a
stronger sandbox than Git. Registry staleness that changes when inspection
occurs is a correctness concern, not a security boundary bypass.

## Untrusted inputs

Branch and ref names, pull-request metadata, remote repository contents, and
repository file contents are untrusted data. They must not be interpolated into
shell commands, environment-variable expansion, credentials, remote
destinations, or paths outside the user-selected or configured destination.

Repository-local `.kwt.toml` is separately trust-gated. Even a trusted local
file cannot set machine-level fleet credentials or endpoints. Trusting that
file authorizes its repository-scoped policy; it does not authorize branch
names or remote metadata to become shell syntax.

## Worktree creation and repository automation

Checking out a local or remote branch is an explicit user action and establishes
the same boundary as checking it out with Git directly. `kwt` still makes
creation unsurprising:

- ref and branch values are passed as arguments, never shell source;
- automated checkout disables configured hooks, filters, and recursive
  submodule updates;
- imported existing branches do not automatically run `copy_files`,
  `setup_commands`, layouts, or pane commands; and
- `kwt`-managed tokens are removed from checkout and workspace processes.

After checkout, the worktree participates in normal status, discovery, and
fleet observation. Opening it is the explicit opt-in to create its configured
workspace and run any trusted layout or pane commands.

## Secrets and machine-level policy

`kwt` bearer tokens, token-file locations, and credential-bearing remote URLs
must not be exposed to repository-controlled processes, printed, persisted in
shared state, or published to fleet. Fleet configuration is global-only, and
non-loopback fleet transport requires HTTPS.

Ordinary developer credentials and environment variables outside `kwt`'s own
credential surfaces remain the user's responsibility.

## Local daemon authority

The operating-system account is the local daemon trust boundary. The daemon
runtime directory and bearer record are owner-only because the credential can
authorize future worktree mutations and authenticated SSH use. Clients prove
the recorded endpoint before sending the bearer. The server accepts only its
exact loopback Host, rejects browser Origin requests, bounds bodies and
diagnostics, and never logs credentials or request bodies.

Inventory repository paths are carried only in authenticated request bodies,
not URL paths. The owner-only inventory cache contains derived discovery data;
it is disposable and never authorizes a mutation. Repository-local config is
never implicitly trusted: interactive approval is bound to a content digest
that the daemon revalidates, while rejection and noninteractive skipping apply
only to that request. Authenticated inventory requests also carry the client's
path-expansion context, including its environment. This transient context is
used only to interpret trusted global paths and is neither logged nor persisted.
A remote client reaches inventory by invoking the remote machine's kwt CLI over
its existing shell boundary; the loopback daemon itself is not remotely
reachable.

An operation ID is a routing handle, not an authorization credential. Every
event, response, and cancellation request still requires the owner-only bearer
credential and the daemon's loopback host checks. Prompt responses are accepted
only on the authenticated response endpoint and only for the operation's exact
current prompt ID. They are held only for the active prompt round and are not
rendered, logged, or persisted. Event and response payloads, replay and live
subscriber queues, every live subscription including terminal replay, active
operations, and retained completed operations are bounded so an authenticated
or same-user local client cannot create unbounded daemon memory growth. The
live queue reserves critical-event capacity that noncritical progress cannot
consume. The same operating-system user remains the trust boundary.

The operation stream is reconnectable only while the same verified daemon
retains it in memory. Neither an operation ID nor a request digest authorizes a
new daemon to adopt or replay work. Loss of that authority is reported as an
unknown outcome rather than causing an automatic retry of a mutation.

An SSH lease ID is likewise only a same-daemon routing handle. The daemon keeps
the generation-bound lease and private projection state; clients receive only
the arguments needed to reach that verified master. Lease touch and release
require the owner-only bearer, and leases expire after thirty seconds without a
heartbeat so a crashed CLI cannot pin authentication state indefinitely. During
replacement, new leases are refused, existing leases may finish within the
advertised grace period, and remaining lease handles are invalidated before the
SSH owner is closed. Live masters are never transferred to the successor.
Masterless direct arguments are never published through the daemon API. They
remain available only to an in-process Go owner that assumes responsibility for
the direct authentication, prompt, and trust boundary.

Interactive SSH preparation uses the running kwt executable as a forced
OpenSSH askpass helper only when its process environment contains a fresh,
one-time channel handle. Helper dispatch occurs before configuration or Cobra
startup. On POSIX the channel is an owner-private Unix socket; on Windows it is
a named pipe with a protected DACL granting the current user. The opaque handle
also carries a random 256-bit secret, and every request must prove it before a
prompt is forwarded. Prompt and response frames, concurrent connections,
rounds, and deadlines are bounded. Responses cross only this local channel and
stdout back to the waiting OpenSSH process; they are not placed in arguments,
environment values, daemon events, logs, or persistent state. Deliberate empty
responses remain distinct from rejection. Kwt creates no channel state unless
the selected system OpenSSH satisfies the 8.4 forced-askpass floor.

SSH route resolution validates user, hostname, and port before invoking
OpenSSH. POSIX resolution quotes the validated argv into the account login
shell and accepts stdout only between unpredictable, exact nonce markers;
startup output and marker-like stderr are not configuration. Windows avoids a
shell boundary and invokes OpenSSH directly. Kwt's built-in credential
environment variables and the configured fleet-token variable are removed
before either process boundary. The daemon reloads the fleet-token variable
name and sanitizes a fresh process environment for every resolution, so a
compatible daemon cannot retain stale credential policy after configuration
changes. Cancellation terminates the complete resolver process tree through a
POSIX process group or a Windows Job Object, so an OpenSSH configuration
command cannot survive daemon drain while retaining its output handles. Stdout
and stderr are each capped at 1 MiB; exceeding either cap cancels that same
owned process tree.

The daemon retains the complete normalized `ssh -G` stream only as private
route-identity input. Authenticated responses expose semantic targets and the
reviewed execution projection, never arbitrary directives. Opaque
ProxyCommand and nested proxy routes are rejected because kwt cannot bind
independent trust policy to every hop. Projections are target-local; route
consumers must use the ordered preceding target's prepared master as proxy
transport rather than executing a downstream projection directly. Stage 1
resolution does not authorize a host key, authenticate, create a master, or
make the returned route identity an authorization credential.

Guarded project unregistration requires the exact persisted path, its
credential-free repository identity, and an opaque fingerprint of the complete
decoded registry entry the client observed. The daemon derives and compares
that token from its own fresh snapshot before identity resolution or endpoint
inspection; it never treats the client token as a description of trusted state.
The raw registration comparison and final compare-and-swap remain mutation
authority. An identity-keyed, owner-private project fence serializes removal
with operations that can create worktrees, provenance, or protected sessions.
Pull-request provenance is owner-private durable authority: kwt writes it
before establishing a protected session and rolls an import back if that write
fails. Manual deletion of this owner-private state is outside the supported
integrity model.

The daemon derives protected socket names from durable session and worktree
identity and probes each endpoint without collapsing operational failures into
absence. New protected named-socket commands discard ambient `TMUX_TMPDIR`, so
a foreground client and the daemon resolve the same owner-specific endpoint.
Released kwt versions inherited that variable; migration-aware operations
check the canonical endpoint first, then the invoking client's explicit legacy
directory without inheriting any other environment state. Probes strip the
configured fleet-token environment name in addition to kwt's canonical
credential variables. A live endpoint blocks metadata removal and
provenance-backed pruning; permission, connection, or
other indeterminate failures fail closed. Public errors expose only the exact
protected session and socket identity needed to resolve the block. Ordinary
tmux-server sessions are outside this authority and are neither probed as
project endpoints nor killed.

Public service descriptors never contain an unexpected error's private cause.
HTTP detail keys are selected by stable code and type-checked before
serialization; arbitrary cause metadata is not forwarded. The owner-private
daemon log records the route, stable code, and a diagnostic capped at 1024
bytes. The bearer, values from credential-named environment variables,
authorization headers, and URL user information are redacted before that
diagnostic is written. Request bodies are never logged.

PID liveness alone does not authorize cleanup or replacement. Kwt also checks
the recorded process creation identity. A dead PID or exact creation mismatch
makes a runtime record stale; an identity that cannot be read, a timed-out
probe, or failed proof preserves the record and blocks a second writer.

## Out of scope

Do not report an issue as a `kwt` vulnerability when exploitation requires:

- a malicious binary already on the user's `PATH`;
- hostile shell, tmux, Git, hook, filter, fsmonitor, or credential-helper
  configuration already installed by the local user;
- another malicious process running as the same operating-system user;
- a compromised authenticated fleet machine; or
- treating a normal Git command after checkout as crossing an untrusted-code
  execution boundary.

These assumptions may still expose usability or correctness bugs. Severity
should follow the concrete consequence within the boundaries above: credential
disclosure, command injection by branch-controlled data, trust bypass, writes
outside configured roots, or pushes to the wrong repository are security
issues; stale UI state, compatibility, and ordinary Git behavior are not.
