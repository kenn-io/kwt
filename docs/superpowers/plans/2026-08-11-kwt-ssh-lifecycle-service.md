# Kwt SSH Lifecycle Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Extend the proven kwt route resolver into the sole owner of OpenSSH trust, authentication, ControlMaster lifecycle, leases, idle persistence, and daemon drain behavior.

**Architecture:** The public SSH service composes the Stage 1 resolver, the merged ordered-operation hub, and kit's `openssh.PersistentManager`. Every connection starts from an expected route identity, re-resolves before preparation and readiness, and returns a generation-bound lease. System OpenSSH remains authoritative for host-key and authentication prompts through forced askpass. Kwt owns policy and ephemeral prompt transport but never manipulates control sockets outside kit.

**Tech Stack:** Go 1.26, `go.kenn.io/kit/openssh`, kwt operation streams, Huma v2, Cobra, system OpenSSH 8.4+.

---

## Merge gates

- The ordered daemon operation streaming plan is merged with `operation.stream.v1`.
- The Stage 1 resolution service is merged with `ssh.resolve.v1` and `kwt.openssh.projection.v1` parity evidence.
- Any missing socket or lifecycle primitive is added to kit in a separate app-neutral PR before kwt uses it.

### Task 1: Add SSH lifecycle configuration and OpenSSH version policy

**Files:**

- Modify: `pkg/models/models.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `docs/reference/configuration.md`
- Create: `internal/ssh/version.go`
- Create: `internal/ssh/version_test.go`

- [ ] Write failing config tests for a one-hour default `ssh.idle_timeout`, zero meaning immediate teardown, negative rejection, and explicit longer values.
- [ ] Add:

```go
type SSHConfig struct {
    IdleTimeout time.Duration `mapstructure:"idle_timeout" toml:"idle_timeout"`
}
```

  to the global machine-level config. Repository-local config must not override SSH daemon policy.
- [ ] Write version-parser tests using vendor fixtures such as Apple and portable OpenSSH strings. Parse only `OpenSSH[_ ]MAJOR.MINOR`; accept suffixes and fail closed when no authoritative OpenSSH version is present.
- [ ] Execute `ssh -V` once per daemon process through an injected runner, preserve cancellation, and reject interactive lifecycle below 8.4 with `ssh_unsupported_version` before creating askpass state.
- [ ] Do not assert the developer or CI machine's incidental version string.
- [ ] Run `go test ./internal/config ./internal/ssh -run 'Test.*(SSHConfig|OpenSSHVersion)'`.
- [ ] Commit: `Define SSH lifecycle configuration and version policy`.

### Task 2: Add lease and event contracts to the public service

**Files:**

- Modify: `internal/ssh/types.go`
- Modify: `internal/ssh/service.go`
- Modify: `ssh.go`
- Modify: `service/error.go`
- Test: `internal/ssh/service_test.go`
- Test: `public_test.go`

- [ ] Write failing tests for expected-route enforcement, generation-bound arguments, independent leases sharing one route, touch-before-use, final release, masterless mode, and stable error mapping.
- [ ] Add the remaining approved root contracts:

```go
type LeaseRequest struct {
    Snapshot RouteSnapshot `json:"snapshot"`
}

type Lease interface {
    Generation() uint64
    Arguments(context.Context) ([]string, error)
    Touch() error
    Release() error
}

type Event struct {
    RouteIdentity string
    Generation    uint64
    State         string
    Failure       *service.Descriptor
}
```

- [ ] Add stable codes `ssh_unsupported_version`, `ssh_interaction_required`, `ssh_prompt_rejected`, `ssh_prompt_timed_out`, `ssh_connection_failed`, `ssh_connection_changed`, `ssh_control_path_occupied`, and `ssh_cleanup_failed`.
- [ ] Keep the root file as aliases and constructors only. Domain implementation remains under `internal/ssh`.
- [ ] Run `go test -race ./internal/ssh ./... -run 'Test.*SSHService'`.
- [ ] Commit: `Define kwt SSH leases and lifecycle events`.

### Task 3: Compose leases with kit's persistent manager

**Files:**

- Create: `internal/ssh/manager.go`
- Create: `internal/ssh/manager_test.go`
- Create: `internal/ssh/manager_windows_test.go`
- Modify: `internal/ssh/service.go`

- [ ] Write failing concurrency tests for same-route sharing, different-route refusal/replacement, generation mismatch, release-vs-touch, idle cleanup, failed teardown quarantine, occupied control path, and Windows masterless arguments.
- [ ] Construct exactly one `openssh.PersistentManager` per daemon owner using an owner-private directory beneath canonical `KWT_HOME`. Express connect, arguments, touch, probe failure, idle monitor, and disconnect exclusively through kit methods.
- [ ] Derive kit identity from the opaque route identity and projection policy; never recompute it from display fields.
- [ ] Before acquiring a lease, re-resolve and compare the caller's expected route identity. After OpenSSH preparation and before returning ready, resolve and compare again. On either mismatch, terminate only the stale attempt through kit and return `ssh_configuration_changed`.
- [ ] Keep reference counts and lease IDs in kwt, not kit. Active leases prevent ordinary and idle teardown; final release starts the configured idle window rather than disconnecting immediately unless the timeout is zero.
- [ ] On `ErrPersistentUnsupported`, return a masterless lease whose arguments contain the same projection and no control path. No caller may assume a socket exists.
- [ ] Run `go test -race ./internal/ssh -run 'Test.*(Lease|Manager|Idle|Masterless)'` and cross-compile Windows tests.
- [ ] Commit: `Manage generation-bound SSH leases through kit`.

### Task 4: Implement private forced-askpass prompt transport

**Files:**

- Create: `internal/ssh/askpass.go`
- Create: `internal/ssh/askpass_unix.go`
- Create: `internal/ssh/askpass_windows.go`
- Create: `internal/ssh/askpass_test.go`
- Create: `internal/ssh/testdata/askpass-helper/main.go`
- Modify: `internal/credentials/environment.go`

- [ ] Write failing subprocess tests for host-key, password, passphrase, multiple keyboard-interactive rounds, deliberate empty responses, wrong-answer re-prompt, rejection, timeout, cancellation, and hostile prompt text.
- [ ] Create an owner-private per-operation directory and FIFO or platform-equivalent pipe. Pass only a random prompt-channel handle to the askpass helper; never pass responses in argv or environment.
- [ ] Force `SSH_ASKPASS_REQUIRE=force`, set the helper path explicitly, detach stdin from a TTY, and strip bearer tokens plus configured credential environment names from both OpenSSH and helper environments.
- [ ] Bind every operation-stream response to operation ID, prompt ID, route target, and the parsed host-key algorithm/fingerprint when applicable.
- [ ] Bound prompts, response size, round count, temporary path length, and deadlines. Remove state on every terminal path and zero response buffers where practical.
- [ ] Keep the helper generic: it relays one prompt and one response; it does not decide trust or parse SSH policy.
- [ ] Run `go test -race ./internal/ssh -run 'Test.*Askpass'`.
- [ ] Commit: `Add private multi-round SSH askpass transport`.

### Task 5: Implement host-key review and authentication preparation

**Files:**

- Create: `internal/ssh/prepare.go`
- Create: `internal/ssh/prepare_test.go`
- Create: `internal/ssh/prepare_integration_test.go`
- Modify: `internal/ssh/service.go`

- [ ] Write failing behavior tests for known/new/changed keys; `yes`, `ask`, `accept-new`, `no`, and `off`; exact fingerprint binding; sequential ProxyJump keys; preceding-hop authentication; password/passphrase/keyboard-interactive; and prompt-stream loss.
- [ ] Run OpenSSH under `-F /dev/null` with only the v1 projection, kwt-owned reviewed host-key overrides, and the private askpass environment.
- [ ] Preserve configured `StrictHostKeyChecking=yes`. Tighten review-managed `ask` and `accept-new` to explicit approval and set `UpdateHostKeys=no` for the reviewed preparation. Preserve deliberate `no`/`off` policy without presenting a false review.
- [ ] Let the same waiting OpenSSH process write known_hosts only after its exact prompt is approved. Kwt must not edit known_hosts itself.
- [ ] Prepare direct ProxyJump hops in order using same-daemon leases; reject opaque or nested routes before interaction.
- [ ] Normalize no-prompt noninteractive callers to `ssh_interaction_required`; do not auto-approve or synthesize credentials.
- [ ] Run `go test -race ./internal/ssh -run 'Test.*Prepare'`. Run real-OpenSSH integration tests only behind an explicit environment gate and local fixture server.
- [ ] Commit: `Prepare reviewed SSH trust and authentication`.

### Task 6: Expose daemon lifecycle operations and CLI streaming

**Files:**

- Modify: `internal/daemon/types.go`
- Modify: `internal/daemon/server.go`
- Modify: `internal/daemon/host.go`
- Modify: `internal/daemon/runtime.go`
- Modify: `internal/daemon/client.go`
- Create: `internal/daemon/ssh_lifecycle_test.go`
- Modify: `internal/cmd/ssh.go`
- Create: `internal/cmd/ssh_lifecycle_test.go`

- [ ] Write failing HTTP/CLI tests for starting an operation, following progress, multiple prompts, acquiring a lease, fetching generation-bound arguments, touching, releasing, cancellation, drain refusal, and stable JSON failures.
- [ ] Advertise `ssh.lifecycle.v1` only when the resolver, operation hub, manager, and askpass helper are all available. Keep `ssh.resolve.v1` independently usable.
- [ ] Add domain routes under `/api/v1/ssh/...` that create/follow operations through the generic hub. Do not add a direct synchronous lifecycle route beside the streamed route.
- [ ] Add CLI commands required by Ghosthub's thin adapter, with human progress/warnings flushed to stderr as events arrive and machine final results emitted once to stdout.
- [ ] Wrap long-running SSH commands with `withGracefulSignals`; signal cancellation must propagate to OpenSSH and preserve 130/143 behavior.
- [ ] Run `go test -race ./internal/daemon ./internal/cmd -run 'Test.*SSH'`.
- [ ] Commit: `Serve streaming SSH lifecycle operations`.

### Task 7: Integrate replacement, crash cleanup, and observability

**Files:**

- Modify: `internal/daemon/host.go`
- Modify: `internal/daemon/controller.go`
- Modify: `internal/daemon/gate.go`
- Modify: `internal/daemon/log.go`
- Test: `internal/daemon/host_test.go`
- Test: `internal/daemon/controller_test.go`

- [ ] Write failing tests for replacement with active leases, published draining lease count/deadline, bounded invalidation, teardown errors, abnormal daemon death, verified teardown-only orphan adoption, and no live-master transfer.
- [ ] During replacement, stop new SSH operations and leases, wait through `daemon.replacement_grace`, emit `ssh_connection_changed` for remaining lease generations at the deadline, and ask kit to disconnect every verified manager entry.
- [ ] A successor may inspect an owner-private orphan only for bounded teardown. It must never adopt it as a usable connection or silently preserve a lease.
- [ ] Report cleanup failures through bounded owner-only logs and SSH events without control paths, raw environment, prompt values, or credentials.
- [ ] Do not remove the runtime record or release ownership until HTTP handlers are canceled and teardown has reached success or explicit quarantined failure.
- [ ] Run `go test -race ./internal/daemon -run 'Test.*(Replacement|Drain|Orphan|SSH)'`.
- [ ] Commit: `Drain SSH leases during daemon replacement`.

### Task 8: Complete parity evidence and documentation

**Files:**

- Modify: `docs/design/daemon.md`
- Modify: `docs/development/threat-model.md`
- Modify: `docs/reference/cli.md`
- Modify: `docs/reference/configuration.md`

- [ ] Run the resolution, trust/authentication, lifecycle, Windows masterless, and replacement cases listed in the approved spec. Compare structured results, invocations, files, events, and lifecycle outcomes—not Go implementation details.
- [ ] Document the daemon, operation channel, askpass helper, system OpenSSH, kit manager, and private state directories as the trusted local boundary.
- [ ] Run `gofmt` on all touched Go files.
- [ ] Run `go test -race ./...`, `make lint`, `make build`, and `git diff --check`.
- [ ] Remove superpowers spec/plan artifacts from the implementation branch before opening its PR.
- [ ] Commit: `Document daemon-owned SSH lifecycle`.

## Handoff gate

Ghosthub Stage 2 may start only after the exact merged kwt revision passes the pinned multi-round prompt, host-key policy, connection sharing, idle timeout, replacement, crash, and reconnect corpus. No Swift pool/trust/auth implementation is removed based on unit tests alone.
