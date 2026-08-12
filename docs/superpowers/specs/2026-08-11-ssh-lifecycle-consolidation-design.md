# Shared OpenSSH Lifecycle and Ghosthub Migration

Status: draft for maintainer review

Date: 2026-08-11

## Summary

Kwt will become the shared owner of OpenSSH configuration resolution,
connection lifecycle, host-key review, and interactive authentication for Kenn
applications. System OpenSSH remains the transport, configuration, agent,
known-hosts, and authentication authority. `go.kenn.io/kit/openssh` remains the
application-neutral mechanics layer. Kwt adds the product lifecycle, daemon,
public Go, CLI, and operation-stream contracts used by Forge and by non-Go
clients.

Ghosthub's existing Swift implementation is the executable reference contract.
The migration proceeds capability by capability. A capability moves only after
revision-pinned parity evidence exists; the same cutover removes its Swift
owner. There is no production shadow mode, fallback implementation, or
indefinite dual ownership.

The first capability moves configuration and route resolution. The second
moves connection pooling, host trust, and authentication as one unit because
those responsibilities are tightly coupled. The final capability removes
residual Swift SSH argument and lifecycle construction from every consumer.
There is no Rust Ghosthub SSH client in the current scope, but the kwt contract
must remain portable so the planned Windows and Linux client can adopt it
without another implementation of SSH policy.

## Current grounding

Kit v0.19.1 already provides the reusable mechanics assigned to it:

- structured target parsing and conservative validation;
- `ssh -G` output parsing and normalized effective configuration;
- direct ProxyJump route expansion and rejection of opaque route forms;
- route identity and bounded control-socket naming;
- explicit master and masterless client argument construction;
- secure ControlMaster creation, readiness, probes, teardown, idle cleanup,
  generation binding, and event delivery;
- Unix persistent connections and detectable masterless behavior elsewhere.

Kwt already has one per-home loopback daemon, authenticated runtime discovery,
capability negotiation, replacement draining, stable service errors, and
revision-pinned CLI consumption from Ghosthub. The SSH capability will extend
that daemon. It will not create a second SSH-specific daemon.

Ghosthub currently owns the behavior in these primary Swift implementations:

- `SSHConfigurationResolver.swift`;
- `SSHConnectionPool.swift`;
- `SSHHostTrustManager.swift`;
- `SSHAuthenticationSession.swift`.

The migration must also cover consumers and presentation code in:

- `SSHCommandArguments.swift`;
- `NativeTmuxSessionCoordinator.swift`;
- `WorkspaceSceneModel.swift`;
- `SSHAuthenticationView.swift`;
- `TailscaleDiscovery.swift`.

Discovery-provided destinations are first-class inputs. Resolution and
connection APIs cannot assume every target originated as a static Host stanza
in `ssh_config`.

## Goals

1. Give Forge and Ghosthub one implementation of OpenSSH route, trust,
   authentication, and connection lifecycle policy.
2. Preserve the complete observable behavior and security invariants of
   Ghosthub's Swift implementation before deleting it.
3. Keep system OpenSSH authoritative instead of replacing it with a Go SSH
   protocol stack.
4. Support native macOS, Linux, and Windows callers. Windows is a first-class
   masterless mode, not an error-shaped afterthought.
5. Preserve interactive CLI ergonomics with ordered progress and prompts while
   retaining stable machine-readable results and exit behavior.
6. Keep the public Go surface importable from `go.kenn.io/kwt` without a
   ceremonial `kwt/ssh` package.
7. Make daemon replacement, cancellation, config changes, crashes, and idle
   cleanup explicit parts of the connection contract.

## Non-goals

- Reimplementing SSH transport, key exchange, known-hosts storage, agent
  protocols, or `ssh_config` evaluation in Go.
- Making the kwt daemon remotely reachable. A remote shell invokes the remote
  kwt CLI, and that CLI talks only to its same-machine loopback daemon.
- Moving terminal rendering, PTY ownership, tmux lifecycle, scene management,
  or native prompt presentation into kwt.
- Building the future Rust Ghosthub SSH client in this program.
- Transferring live leases or authenticated masters between daemon processes.
- Supporting arbitrary ProxyCommand routes or nested dynamic proxy routes that
  cannot be reviewed independently.
- Keeping compatibility paths for unreleased kwt SSH daemon contracts.

## Ownership and dependency direction

```text
system OpenSSH
  ssh_config, agent, known_hosts, askpass, authentication, transport
                               ^
                               |
                    go.kenn.io/kit/openssh
           validation, arguments, route identity, ControlMaster
                               ^
                               |
                        go.kenn.io/kwt
        public Go service, policy, leases, prompts, daemon, CLI
                 ^                              ^
                 |                              |
              Forge                         Ghosthub
       in-process Go service             pinned kwt CLI
```

Kit owns reusable mechanisms. Kwt owns product policy and lifecycle. Forge
constructs the public kwt service in process. Ghosthub invokes its exact pinned
local kwt executable and never connects to the loopback daemon HTTP API
directly.

The local kwt daemon owns outbound SSH state for Ghosthub. A kwt daemon on a
remote host continues to own that host's worktree services; it is not exposed
as an SSH broker. Ghosthub first obtains a local connection capability, then
uses that connection to invoke the exact managed kwt CLI on the remote host.

Kwt expresses connection, activity, idle, and teardown policy exclusively
through `kit/openssh.PersistentManager` and kit argument builders. Kwt never
probes, adopts, unlinks, or otherwise manipulates a control socket directly.
If a missing kit API prevents a safe lifecycle operation, kit is extended
first rather than bypassed in kwt.

## Public Go service

Kwt adds the minimum SSH aliases, contracts, and constructors to its root
package. The implementation remains in focused internal packages so the
repository root gains one public-surface file rather than a parallel package
hierarchy. The public names are:

```go
type SSHService
type SSHServiceOptions
type SSHTarget
type SSHResolveRequest
type SSHRouteSnapshot
type SSHLeaseRequest
type SSHLease
type SSHEvent

func NewSSHService(SSHServiceOptions) *SSHService
```

Callers import `go.kenn.io/kwt`, not `go.kenn.io/kwt/ssh`, and do not construct
kit's manager themselves.

The service core does not import Cobra, Huma, daemon clients, terminal UI, or
Ghosthub types. Runners, login-shell selection, state roots, clocks, and event
sinks are injected. The in-process and daemon adapters return the same typed
results and stable service errors.

Forge constructs this service directly and selects noninteractive prompt
policy. It receives the same route snapshots, generations, masterless mode,
and lifecycle events without starting or calling the kwt daemon. Once the
service is proven, a separate Forge PR replaces its thin direct
`kit/openssh` adapter with the kwt service; Forge's terminal and persistent
fleet state remain Forge-owned.

## Stage 1: observed route resolution

### Resolution boundary

`kwt ssh resolve <target> --json` accepts one structured logical destination
plus explicit user and port fields when supplied. Targets may originate from a
manual host, an OpenSSH alias, or discovery such as a Tailscale/exe.dev result.
They pass kit's conservative target validation before every OpenSSH invocation.
Account names or destinations outside that allowlist belong behind a trusted
OpenSSH alias; kwt does not weaken validation to accept version-dependent shell
syntax.

On POSIX clients, kwt evaluates configuration with `ssh -G` inside the current
account's login shell. It emits unpredictable begin/end markers around the
command output and parses only the framed stdout between them. Login-shell
banners, diagnostics, and marker-like stderr cannot become replayed OpenSSH
options. Windows has no POSIX account-login-shell boundary; its masterless
resolver invokes system OpenSSH directly with the request-scoped client
environment and the same target validation and output parsing.

The resolver preserves explicit target user and port precedence, IPv6 command
grammar, all normalized `ssh -G` options, known-hosts policy, cryptographic
algorithm constraints, ProxyJump, ProxyCommand, and host-key alias. Direct
ProxyJump hops are resolved independently in connection order. Opaque
ProxyCommand routes and jump hosts that introduce another proxy route fail
closed because kwt cannot independently bind trust policy to every hop.

### Route snapshot

A successful result is an immutable observed snapshot containing:

- the logical endpoint;
- each route target in connection order;
- the effective user, hostname, port, host-key alias, host-key policy, proxy
  policy, and complete canonical option stream for each target;
- a cryptographic route identity derived through kit;
- `observed_at` in UTC;
- a credential-free display projection for native UI.

The complete option stream may include local configuration paths required for
later exact replay. It never includes prompt responses or daemon credentials,
and only the credential-free display projection is copied into human-facing
diagnostics.

The route identity is a conditional token, not authorization. It binds the
logical destination, every resolved target, and the complete normalized
effective configuration. The client treats it as opaque.

Before acquiring a lease or beginning trust/authentication, kwt resolves the
route again through the same platform-specific execution boundary and compares
it to the caller's expected route identity. A mismatch returns retryable
`ssh_configuration_changed` and no SSH process is launched. Kwt may return the
new safe snapshot for display, but neither the CLI nor Ghosthub silently
authorizes or retries against it.

Kwt resolves once more before reporting a connection ready. If that identity
changed while the connection was prepared, kwt terminates the stale attempt
through kit and returns `ssh_configuration_changed`.

### Ghosthub cutover

Ghosthub replaces `SSHConfigurationResolver.swift` with a thin model and pinned
kwt CLI adapter. The adapter passes structured discovery or configured targets,
decodes the snapshot, retains the opaque identity, and maps stable errors.
Ghosthub's trust and connection code temporarily consumes this kwt-owned
snapshot, but it no longer runs or parses `ssh -G` itself.

The Stage 1 Ghosthub change deletes the Swift resolver and its implementation-
specific tests. Equivalent cases move into kwt and the pinned cross-process
contract suite. There is no runtime fallback to Swift resolution.

The first daemon contract advertises `ssh.resolve.v1` and serves
`POST /api/v1/ssh/resolve`. Its request, result, and service-error schemas come
from the same contracts as the public Go service. There is no pre-v1 SSH route
or compatibility advertisement. The additive API schema version advances when
the route lands.

## Ordered operation stream prerequisite

Stage 2 is blocked on kata `jkzd`, the ordered daemon-operation streaming
contract. A long-running kwt operation must publish progress as it happens,
not buffer it until completion. Human CLI clients render progress to stderr;
machine-readable final stdout and established command exit behavior remain
stable.

Interactive SSH adds a versioned, ordered, bidirectional control channel
between the kwt CLI and its caller. This channel is distinct from ordinary
stdout and stderr so terminal diagnostics cannot spoof operation events and
prompt events cannot corrupt the final JSON result. The platform adapter may
use inherited private pipes or handles, but its observable frames are the same
on every platform.

Every frame carries an operation ID and monotonically increasing sequence.
Prompt events additionally carry a unique prompt ID, kind, bounded exact
message, route target identity, and safe target display. Responses bind to the
operation ID and exact prompt ID. Late, duplicate, out-of-order, or wrong-route
responses are rejected.

One operation may emit multiple sequential prompt/response rounds. This covers
keyboard-interactive challenges, multiple questions in one authentication
exchange, and OpenSSH re-prompting after a rejected answer. An empty response
is representable because some keyboard-interactive prompts deliberately
require it. Cancellation is an ordered control event and causes bounded child
process cleanup.

The stream defines bounded buffering, slow-consumer behavior, daemon drain
events, loss-of-stream behavior, and one terminal completion event. A lost
stream never becomes a success-shaped result. Connection readiness and lease
ownership are reconciled by generation rather than inferred from the last line
seen by a client.

The first released stream and lifecycle capabilities are
`operation.stream.v1` and `ssh.lifecycle.v1`. Clients require both before
starting an interactive SSH operation. Kwt changes these unreleased contracts
in place during development rather than advertising parallel compatibility
capabilities.

## Stage 2: connection, trust, and authentication

### Connection preparation

A lease request includes the expected route identity, caller scope, requested
connection options, and an operation event sink. Kwt revalidates the route,
then asks kit's manager to establish or reuse the route-specific master owned
by the current daemon.

Each daemon process creates one owner-specific private manager namespace. The
route identity and validated target determine the manager identity within that
namespace, so a replacement daemon cannot implicitly discover or adopt its
predecessor's socket. Kwt selects the trusted namespace root but does not
construct or predict socket paths. A lease is bound to the kit connection
generation returned by `Connect`; every client-argument request, activity
touch, liveness probe, and release validates that generation.

A successful result is explicit about mode:

- `multiplexed`: an authenticated Unix master exists and the result includes
  kit-produced client arguments for that exact generation;
- `masterless`: persistent multiplexing is unsupported and the result includes
  kit-produced direct client arguments with implicit sharing disabled.

Masterless is not described as an authenticated reusable connection. Each
direct SSH process owns its own transport and may require interaction. The
future Rust client can add a kwt-managed direct-run adapter without changing
route, prompt, or error contracts. The macOS Swift cutover exercises the
multiplexed path while cross-platform Go tests keep masterless construction
first-class.

### Host-key review

Kwt never obtains a key from a separate scanner and writes it as trusted. It
allows system OpenSSH to produce its ordinary host-key prompt. A private
askpass helper forwards the exact bounded prompt to the daemon, which publishes
a typed host-key event over the operation stream.

The event identifies the exact logical and route target and parses the
algorithm and fingerprint for stable native presentation. Approval is bound to
that prompt ID, route target, algorithm, and fingerprint. Kwt returns `yes` to
the same waiting OpenSSH prompt only after the client explicitly approves.
OpenSSH then writes the key to the configured `UserKnownHostsFile`.

The child runs without a TTY and sets `SSH_ASKPASS_REQUIRE=force`. Kwt therefore
requires OpenSSH 8.4 or newer for daemon-owned interactive preparation. An
older client returns `ssh_unsupported_version` before beginning interaction.
Kwt does not depend on OpenSSH's platform-dependent fallback choice between a
TTY and askpass.

Configured `StrictHostKeyChecking=yes` remains strict and is never weakened.
Configured `no` or `off` remains the user's OpenSSH policy. Review-managed
`ask` and `accept-new` are tightened to explicit approval and disable
`UpdateHostKeys` for that reviewed connection so unreviewed additional keys
cannot be persisted.

Host-key replay uses the route snapshot's known-hosts and cryptographic
constraints under an empty base SSH configuration. ProxyJump route keys are
reviewed sequentially. When a preceding hop requires authentication, kwt
prepares that reviewed hop first and uses its same-daemon control connection to
reach the next route target.

### Authentication prompts

After host trust is established, the same daemon operation lets OpenSSH produce
password, passphrase, and keyboard-interactive prompts through forced askpass.
Kwt publishes the exact bounded challenge, prompt kind, and controlling route
host. Ghosthub renders it in native secure UI and returns the response on the
private operation channel.

The response exists in the caller, CLI, and daemon memory only for that prompt
round. It is delivered to the private askpass process through an owner-only
FIFO or equivalent private platform pipe. It never appears in arguments,
environment values, stdout, diagnostics, daemon logs, runtime records, or
persistent kwt state. Buffers and temporary paths are bounded and removed on
completion, cancellation, timeout, or daemon shutdown.

Multiple Ghosthub windows may hold presentation ownership of one in-flight
attempt. Releasing one owner does not cancel the attempt while another owner
remains. The daemon coalesces attempts only when their complete expected route
identity matches. A route change creates a new attempt rather than attaching a
prompt response to stale policy.

### Lease lifetime and activity

The operation stream remains the lease owner after readiness. Closing or
canceling it releases that client lease. Multiple clients may independently
lease one connection generation. A client must touch its lease immediately
before launching work through the returned arguments. Touch detects a replaced
generation; it is not a guarantee against all later network failure.

Active leases prevent ordinary idle teardown. When the final lease ends, the
master remains warm until `ssh.idle_timeout`, one hour by default. The timeout
is configurable; zero requests immediate teardown. Health probes and prompt
state changes do not count as user activity and cannot keep an unused master
alive indefinitely.

The daemon's own idle timeout may terminate unleased masters sooner. Every
teardown uses kit's bounded manager operation. Failures remain observable and
retain kit's authoritative target/quarantine state; kwt never unlinks a path to
make an error disappear.

### Daemon replacement and crash recovery

Replacement follows the existing daemon drain contract:

1. Stop accepting new SSH operations and leases.
2. Publish the drain deadline and active lease count.
3. Allow existing leases to continue until `daemon.replacement_grace`.
4. At the deadline, emit `ssh_connection_changed` for remaining generations.
5. Terminate every master through kit before releasing daemon ownership.
6. Start the replacement only after the old owner exits.

Live leases and masters are never transferred to the replacement daemon.
Ghosthub reconnects its disposable SSH client to the surviving remote tmux
session after it acquires a new route snapshot and lease.

Kwt keeps the minimum owner-only durable metadata required to identify manager
namespaces and targets left by an abnormal daemon death. A later daemon may use
a verified, teardown-only kit API to terminate such an orphan. It never opens
the prior namespace for normal lease acquisition and never returns an adopted
orphan to a client as a ready connection. If kit lacks the required
teardown-only operation, kit is extended first. If identity, ownership, route,
or socket state cannot be proved, kwt preserves/quarantines the entry and
reports an actionable error rather than unlinking it.

### Ghosthub cutover

After the kwt service, daemon adapter, operation stream, and pinned contract
suite pass, Ghosthub replaces these three owners together:

- `SSHConnectionPool.swift`;
- `SSHHostTrustManager.swift`;
- `SSHAuthenticationSession.swift`.

The replacement Swift layer contains only pinned kwt process control, typed
event decoding, prompt response encoding, native view state, and terminal
client launch from kit-produced arguments. `SSHAuthenticationView.swift`
continues to render native UI but no longer parses OpenSSH prompts or owns
secret transport.

No connection falls back to the removed Swift path. Missing capabilities,
unsupported OpenSSH, route changes, daemon failures, and malformed events are
explicit failures with recovery actions.

## Stage 3: complete consumer migration

Every SSH consumer is audited and moved to route snapshots and generation-
bound lease results. This includes tmux and Zellij inventory and attachment,
managed kwt provisioning, project/worktree operations, transfers, connection
tests, Tailscale/exe.dev discovery targets, reconnect supervisors, and scene
restoration.

`SSHCommandArguments.swift` no longer reconstructs route, known-hosts,
ControlMaster, ProxyJump, keepalive, or control-path policy. It may combine an
opaque kwt-issued client argument vector with the remote command and PTY mode
needed by a presentation. It cannot remove, override, or reorder security-
relevant kwt arguments.

`NativeTmuxSessionCoordinator.swift` and `WorkspaceSceneModel.swift` retain UI
and reconnect ownership. A transport failure discards the affected lease,
resolves current policy again, and follows the existing attach-only recovery
contract. Exit status 255 remains OpenSSH's transport/setup signal; kwt CLI
domain failures never use 255.

The final Ghosthub change removes residual Swift SSH lifecycle helpers and
updates architecture, threat-model, terminal-session, and release documents to
make kwt the owner.

## Stable service and CLI errors

SSH failures use the same `code`, `message`, `retryable`, and allowlisted typed
`details` descriptor across the public Go, HTTP, CLI, and Ghosthub boundaries.
The initial registry is:

| Code | Meaning |
| --- | --- |
| `ssh_invalid_target` | The structured logical destination is invalid. |
| `ssh_unsupported_version` | System OpenSSH is too old for the required interaction contract. |
| `ssh_resolution_failed` | `ssh -G` or framed login-shell evaluation failed. |
| `ssh_route_unreviewable` | ProxyCommand or nested proxy policy cannot be reviewed safely. |
| `ssh_configuration_changed` | Current resolved route differs from the expected identity. |
| `ssh_interaction_required` | A noninteractive caller encountered a prompt. |
| `ssh_prompt_rejected` | The user rejected a host key or authentication interaction. |
| `ssh_prompt_timed_out` | The bound prompt received no response before its deadline. |
| `ssh_connection_failed` | OpenSSH could not establish or validate the connection. |
| `ssh_connection_changed` | The expected lease generation is no longer authoritative. |
| `ssh_control_path_occupied` | A verified control path is occupied by an unrecognized listener. |
| `ssh_cleanup_failed` | Bounded teardown failed and the connection remains unavailable. |

Cancellation preserves `context.Canceled` or deadline identity in Go and maps
to the established CLI signal/exit behavior where that command has adopted the
graceful-signal wrapper. Unknown errors become generic `internal` responses;
their private causes are redacted and retained only in bounded owner-only logs.

Messages and details never expose prompt responses, bearer credentials,
credential-bearing URLs, private FIFO paths, runtime tokens, raw environment
values, or unbounded OpenSSH diagnostics. Host names, safe aliases,
fingerprints, algorithms, and bounded exact prompts are exposed only where the
native client needs them for an informed interaction.

## Security invariants

- Every OpenSSH invocation validates its structured target first.
- Resolution, trust, authentication, readiness, and returned client arguments
  remain bound to one complete route identity and kit connection generation.
- Account-login-shell output is nonce framed; stderr is always diagnostic.
- OpenSSH 8.4+ is required and `SSH_ASKPASS_REQUIRE=force` is set for daemon-
  owned interactive children.
- Host-key approval is replayed to OpenSSH's own waiting prompt. Kwt never
  writes separately scanned keys.
- Prompt responses use exact operation/prompt binding and private ephemeral
  pipes. They are never persisted or logged.
- Routine clients explicitly disable implicit ControlMaster and ControlPersist
  behavior and use only kit-issued connection arguments.
- Kwt never operates on control sockets outside kit's manager API.
- Daemon replacement drains and terminates; it does not transfer authenticated
  masters or active leases.
- Unknown or indeterminate socket, process, route, or cleanup state fails
  closed and preserves authority for recovery.
- The daemon remains same-user, loopback-only, bearer authenticated, and never
  remotely reachable.

The kwt development threat model is updated in the Stage 2 service PR. The
Ghosthub threat model is updated before its Stage 2 pin advances, making the
daemon, pinned CLI, operation channel, and askpass helper co-equal trusted
local code inside the signed release boundary.

## Parity evidence and acceptance gate

Kata `9565` remains open until the final Ghosthub cutover. Acceptance requires
a revision-pinned contract suite that runs Ghosthub's exact kwt revision and
proves the following observable cases.

### Resolution and route identity

- account-login-shell invocation and nonce-framed banner rejection;
- aliases, explicit user/port precedence, IPv4, raw command IPv6, and safe
  display formatting;
- complete normalized option retention and deterministic identity;
- direct and multi-hop ProxyJump expansion;
- rejection of opaque ProxyCommand and nested proxy routes;
- host-key alias and strict-host-key policy preservation;
- cryptographic algorithm and known-hosts option preservation;
- discovery-provided targets, including Tailscale/exe.dev inputs;
- cancellation, timeout, malformed output, and configuration changes between
  observation, preparation, and readiness.

### Trust and authentication

- known keys, new keys, changed keys, configured `yes`, `ask`, `accept-new`,
  `no`, and `off` policy;
- exact algorithm/fingerprint binding and rejection of a mismatched approval;
- sequential unseen ProxyJump hosts and authentication of a preceding hop;
- password, passphrase, keyboard-interactive, multiple prompt rounds,
  deliberate empty responses, wrong-answer re-prompts, cancellation, and
  timeout;
- forced askpass behavior and refusal of OpenSSH older than 8.4;
- owner-only ephemeral response transport and absence of secrets from argv,
  environment, stdout, logs, runtime records, and retained files;
- bounded diagnostics and hostile prompt/banner content.

### Connection lifecycle

- simultaneous windows sharing one route while holding independent leases;
- final lease release followed by one-hour default warm persistence;
- configured immediate and extended idle cleanup;
- touch-before-use and generation mismatch behavior;
- master establishment failure, liveness failure, teardown failure, and
  occupied/indeterminate socket state;
- daemon idle shutdown, graceful replacement with active leases, deadline
  invalidation, and reconnect to a surviving tmux session;
- abnormal daemon death followed by verified teardown-only orphan adoption;
- control-path length fallback and rejection;
- concurrent Ghosthub/CLI/Forge clients under the same kwt home;
- bounded event backpressure and prompt-stream loss;
- masterless Windows construction that never assumes a control socket.

### Ghosthub workflows

- Test Connection, inventory, tmux/Zellij discovery and attachment, helper
  upload, remote kwt inventory/mutations, transfers, reconnect, cancellation,
  and scene restoration use the same leased route capability;
- host-key and authentication sheets preserve their current native behavior;
- closing one presentation releases only its lease and never destroys a tmux
  or Zellij session;
- no removed Swift resolver, pool, trust, or authentication implementation is
  reachable in production;
- a representative existing Ghosthub installation upgrades to the pinned kwt
  daemon without losing host settings, known-hosts state, or remote sessions.

Tests compare observable structured results, events, process invocations, files,
and lifecycle outcomes. They do not compare private Swift and Go implementation
details. "Looks equivalent" is not acceptance evidence.

## Delivery sequence

1. Complete kata `jkzd`: ordered daemon progress, prompt-capable duplex control,
   bounded backpressure, cancellation, and loss-of-stream semantics.
2. Prove that the complete resolution parity matrix can replay normalized
   `ssh -G` output under an empty base configuration with equivalent behavior,
   including repeated values, quoting-sensitive values, forwards, identity
   files, and token-expanded paths. If equivalence cannot be proved, revise the
   snapshot binding model before connection, lease, trust, or authentication
   implementation begins.
3. Add the public kwt resolution service, daemon route, CLI command, stable
   errors, and Go/subprocess tests.
4. Cut Ghosthub Stage 1 to the pinned resolver contract and delete
   `SSHConfigurationResolver.swift`.
5. Add kwt connection leases, forced askpass trust/authentication, multi-round
   prompts, replacement/crash behavior, and the initial parity corpus.
6. Cut Ghosthub Stage 2 atomically, deleting its pool, trust manager, and
   authentication session implementations while retaining thin native UI.
7. Migrate Forge's direct kit adapter to the public kwt service without moving
   Forge terminal or persistent fleet ownership.
8. Audit and migrate every residual Ghosthub consumer and argument builder in
   Stage 3.
9. Run sustained replacement, idle, reconnect, failure, and upgrade evidence;
   update both products' architecture/threat/release documentation; close kata
   `9565` only with the final evidence.

Each stage may span several implementation PRs, but no individual operation
has simultaneous Swift and kwt production owners. Superpowers planning files
are removed before any implementation PR is opened.
