# Ghosthub SSH Lifecycle Cutover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Atomically replace Ghosthub's Swift SSH pool, host-trust manager, and authentication session with daemon-owned kwt leases while preserving native prompts, terminal attachment, reconnect, and session safety.

**Architecture:** One long-lived pinned kwt CLI subprocess represents each Ghosthub lease. It bridges the loopback daemon without exposing daemon HTTP: stdout carries ordered JSON events, stdin carries prompt-bound responses and release/cancel control, and stderr carries bounded progress diagnostics. Once ready, kwt supplies immutable generation-bound OpenSSH arguments to existing tmux/Zellij clients. Swift owns presentation only; kwt owns every SSH process and connection lifecycle decision.

**Tech Stack:** Swift 6 actors and Swift Testing, pinned kwt `operation.stream.v1` and `ssh.lifecycle.v1`, Foundation `Process`, existing native SSH prompt views and terminal coordinators.

---

## Atomicity rule

The production switch and deletion of `SSHConnectionPool.swift`, `SSHHostTrustManager.swift`, and `SSHAuthenticationSession.swift` happen in the same mergeable change. Tests may introduce seams earlier on the branch, but no shipped operation may choose between Swift and kwt lifecycle owners.

### Task 1: Define the long-lived kwt lease process protocol

**Files:**

- Create: `Sources/App/KwtSSHLeaseClient.swift`
- Create: `Tests/App/KwtSSHLeaseClientTests.swift`
- Modify: `Sources/App/KwtSSHResolutionClient.swift`

- [ ] Write failing tests for ordered progress, warning, host-key prompt, password prompt, repeated keyboard-interactive prompts, empty responses, ready, connection change, terminal failure, release, and malformed/out-of-order events.
- [ ] Model one process as an actor. Its invocation carries structured target fields and expected route identity; the process remains alive after readiness until the actor sends release or cancellation.
- [ ] Decode one bounded JSON event per stdout line. Require matching operation ID, strictly increasing sequence, exact prompt ID, known projection policy, and one terminal outcome.
- [ ] Encode prompt responses and control frames to stdin. Never place secrets in arguments, environment overrides, logs, errors, or retained actor state after the round completes.
- [ ] Drain stderr concurrently so the child cannot block. Surface bounded progress to diagnostics without mixing it into JSON events.
- [ ] Treat EOF before a terminal event as `operation_outcome_unknown`; discard all connection arguments and force a fresh resolution rather than guessing whether a lease survived.
- [ ] Run `swift test --filter KwtSSHLeaseClientTests` and `make format`.
- [ ] Commit: `Add the kwt SSH lease process client`.

### Task 2: Bind operation prompts to existing native UI

**Files:**

- Modify: `Sources/App/SSHAuthenticationView.swift`
- Create: `Sources/App/SSHInteractionCoordinator.swift`
- Create: `Tests/App/SSHInteractionCoordinatorTests.swift`
- Modify: relevant host settings connection-test views/tests.

- [ ] Write failing tests for exact host/algorithm/fingerprint approval, mismatched prompt rejection, password/passphrase secure fields, multi-round keyboard-interactive, deliberate empty response, user rejection, window closure, timeout, and two windows observing one operation.
- [ ] Keep native sheet presentation and secure input in Swift. Replace direct Swift askpass/FIFO callbacks with prompt events from `KwtSSHLeaseClient` and prompt-ID-bound responses.
- [ ] Display only kwt's credential-free route target plus typed fingerprint fields. Never parse a host-key prompt string to make an authorization decision in Swift.
- [ ] Releasing one presentation owner must not cancel while another owner remains. The final owner may cancel an unready operation; a ready lease follows normal lease ownership.
- [ ] Run `swift test --filter SSHInteractionCoordinatorTests` and the focused Host Settings tests.
- [ ] Run `make format`.
- [ ] Commit: `Route kwt SSH prompts through native UI`.

### Task 3: Replace connection preparation with lease capabilities

**Files:**

- Modify: `Sources/App/SSHCommandArguments.swift`
- Modify: `Sources/App/NativeTmuxSessionCoordinator.swift`
- Modify: `Sources/App/WorkspaceSceneModel.swift`
- Modify: `Sources/App/WorkspaceSceneBootstrap.swift`
- Modify: `Tests/App/SSHCommandArgumentsTests.swift`
- Modify: `Tests/App/NativeTmuxSessionCoordinatorTests.swift`
- Modify: relevant scene tests.

- [ ] Write failing tests proving no remote command starts before a ready lease, all commands use the exact generation-bound arguments, activity is touched before use, cancellation releases only the caller's lease, and a stale generation cannot launch.
- [ ] Replace control-path and pool lookups with `KwtSSHLeaseClient` ready output. `SSHCommandArguments` becomes a thin composer of the kwt-issued connection arguments plus the owning command's remote payload.
- [ ] Keep tmux/Zellij discovery, PTY presentation, and remote command construction in their current owners. Kwt supplies transport arguments only.
- [ ] On `ssh_configuration_changed`, discard the pending attempt and require current policy resolution. On `ssh_connection_changed`, discard the lease and follow existing attach-only reconnect; never create or kill a tmux/Zellij session as transport recovery.
- [ ] Maintain one lease per presentation owner even when kwt shares the underlying master. Closing a surface writes release and waits boundedly for acknowledgment.
- [ ] Run the focused SSH command, tmux coordinator, and scene suites, then `make format`.
- [ ] Commit: `Use kwt lease capabilities for remote commands`.

### Task 4: Atomically remove Swift SSH lifecycle implementations

**Files:**

- Delete: `Sources/App/SSHConnectionPool.swift`
- Delete: `Sources/App/SSHHostTrustManager.swift`
- Delete: `Sources/App/SSHAuthenticationSession.swift`
- Delete or migrate: `Tests/App/SSHConnectionPoolTests.swift`
- Delete or migrate: `Tests/App/SSHHostTrustManagerTests.swift`
- Delete or migrate: `Tests/App/SSHAuthenticationSessionTests.swift`
- Modify: every remaining caller found by the searches below.

- [ ] Before deletion, search the full surface:

```sh
rg -n 'SSHConnectionPool|SSHHostTrustManager|SSHAuthenticationSession|ControlMaster|ControlPath|SSH_ASKPASS|known_hosts' Sources Tests
```

- [ ] Move observable behavior cases into pinned kwt contracts, lease-client tests, interaction-coordinator tests, or actual workflow tests. Do not preserve private Swift implementation tests.
- [ ] Delete all three production owners in one commit. Do not add forwarding wrappers, feature flags, or a fallback direct SSH lifecycle path.
- [ ] Confirm remaining `ControlMaster`, askpass, and known-hosts references are documentation/test fixtures or kwt-issued argument handling—not Swift ownership.
- [ ] Run `swift test` and `make format`.
- [ ] Commit: `Remove Swift SSH lifecycle ownership`.

### Task 5: Migrate residual host and discovery workflows

**Files:**

- Modify: `Sources/App/TailscaleDiscovery.swift`
- Modify: local/remote inventory and helper-upload callers using SSH arguments.
- Modify: transfer and connection-test workflows.
- Modify: corresponding suites including `Tests/App/TailscaleDiscoveryTests.swift` and `Tests/App/KwtRemoteInstallerTests.swift`.

- [ ] Enumerate every SSH consumer with:

```sh
rg -n '/usr/bin/ssh|\bssh\b|SSHCommandArguments|connectionArguments|proxyArguments' Sources Tests
```

- [ ] Add a lease requirement to Test Connection, Tailscale/exe.dev discovery targets, inventory, tmux/Zellij discovery and attachment, helper upload, remote kwt inventory/mutations, transfers, reconnect, cancellation, and scene restoration.
- [ ] Preserve per-project failure isolation and existing remote kwt CLI JSON contracts. A transport failure affects only its host/operation and never destroys a remote session.
- [ ] Ensure remote hosts still invoke Ghosthub's revision-pinned managed kwt helper through the local lease; the remote daemon is not an SSH broker.
- [ ] Run focused consumer suites and `make test-essential-workflows`.
- [ ] Run `make format`.
- [ ] Commit: `Route every Ghosthub SSH workflow through kwt leases`.

### Task 6: Prove replacement, idle, reconnect, and upgrade behavior

**Files:**

- Modify: `Tests/App/PinnedKwtContractTests.swift`
- Add focused integration helpers under `Tests/App/` only where subprocess orchestration requires them.
- Modify: `Makefile` if one reusable acceptance target is needed.

- [ ] Add revision-pinned cases for simultaneous windows, independent release, one-hour warm default, zero/extended idle timeout, configuration changes, daemon graceful replacement, deadline invalidation, abnormal death, cleanup failure, and reconnect to a surviving tmux session.
- [ ] Prove replacement drains and terminates rather than transferring a master. A newer daemon must create a fresh connection after the old process exits.
- [ ] Prove secrets are absent from process arguments, environment snapshots, stdout/stderr, daemon logs, runtime records, and retained files after completion.
- [ ] Exercise Test Connection, inventory, attach, helper upload, remote mutation, transfer, cancellation, and scene restoration through the exact pinned CLI.
- [ ] Run `make test-kwt-contract` and `make test-essential-workflows`.
- [ ] Commit: `Prove daemon-owned SSH lifecycle parity`.

### Task 7: Advance the pin and update trusted-boundary documentation

**Files:**

- Modify: `KWT_REVISION`
- Modify: `docs/architecture.md`
- Modify: `docs/threat-model.md`
- Modify: `docs/terminal-sessions.md`
- Modify: `docs/release.md`
- Modify: `CHANGELOG.md` when preparing the accepted release.

- [ ] Pin the merged kwt lifecycle revision and rerun the complete pinned contract suite from a clean daemon/runtime state.
- [ ] Document kwt daemon, CLI bridge, askpass helper, kit manager, and system OpenSSH as co-equal trusted local code. Document that Ghosthub owns UI/PTY/tmux presentation but no SSH policy or lifecycle.
- [ ] Document replacement drain, idle leases, reconnect, version minimum, projection review, and signed helper packaging. If helper contents or signing layout changed, update release packaging and `docs/release.md` in this same task.
- [ ] Run all required remote/terminal gates:

```sh
make test-libghostty-bootstrap
make python-test
swift test
make test-essential-workflows
make build
```

- [ ] Run sustained manual acceptance for host-key approval, password and keyboard-interactive, two-window sharing, replacement, and reconnect.
- [ ] Confirm no superpowers spec/plan artifacts are present in the PR and run `git diff --check`.
- [ ] Commit: `Complete the Ghosthub SSH lifecycle cutover`.

## Acceptance gate

Close kata `9565` only after the exact pinned revision passes the entire resolution, prompt, trust, lifecycle, replacement, upgrade, and workflow matrix. A passing unit suite without sustained daemon replacement and reconnect evidence is not sufficient.
