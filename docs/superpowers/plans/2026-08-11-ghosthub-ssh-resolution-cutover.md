# Ghosthub SSH Resolution Cutover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Cut Ghosthub Stage 1 from its Swift `ssh -G` resolver to the exact revision-pinned kwt `ssh.resolve.v1` contract while retaining Swift connection pooling, trust, authentication, terminal, and reconnect ownership.

**Architecture:** Ghosthub invokes only its bundled local kwt binary with a structured target and decodes an immutable route snapshot. The snapshot's opaque identity and v1 execution projection replace Swift parsing and argument selection. Existing Swift pool/trust/auth actors consume the snapshot during the interim stage, but they never run `ssh -G` or reinterpret unlisted directives. A pinned subprocess contract proves the kwt projection against the former Swift matrix before the old resolver is removed.

**Tech Stack:** Swift 6, Swift Testing, Foundation `Process`, Ghosthub's `AccountCommandRunner`, revision-pinned kwt helper, system OpenSSH.

---

## Repository and merge prerequisites

- Execute this plan in a fresh Ghosthub worktree based on the then-current `origin/main`, including Ghosthub #94.
- First merge and pin the kwt Stage 1 resolution PR. Do not point `KWT_REVISION` at an unmerged branch.
- The kwt revision must advertise `ssh.resolve.v1` and return projection policy `kwt.openssh.projection.v1`.
- This is a nonvisual ownership cutover. Website screenshots are not required unless implementation changes a documented screen.

### Task 1: Add Swift route snapshot and error models

**Files:**

- Create: `Sources/App/KwtSSHResolutionClient.swift`
- Create: `Tests/App/KwtSSHResolutionClientTests.swift`

- [ ] Write failing Swift Testing cases for decoding direct and multi-hop routes, IPv6, repeated options, projection policy, opaque route identity, observed time, and each stable kwt SSH error descriptor.
- [ ] Add focused `Decodable`, `Equatable`, and `Sendable` models:

```swift
struct KwtSSHRouteSnapshot: Decodable, Equatable, Sendable {
    let logicalTarget: KwtSSHTarget
    let targets: [KwtSSHResolvedTarget]
    let routeIdentity: String
    let projectionPolicy: String
    let observedAt: Date
}

enum KwtSSHResolutionError: Error, Equatable, Sendable {
    case invalidTarget(String)
    case resolutionFailed(String, retryable: Bool)
    case routeUnreviewable(String)
    case configurationChanged
    case incompatibleHelper
    case malformedOutput
}
```

- [ ] Reject an empty route identity, an unknown projection policy, missing effective endpoint fields, raw canonical-option fields, and malformed stable descriptors.
- [ ] Keep the execution projection separate from credential-free display values so UI code never renders private paths or `SetEnv` values.
- [ ] Run `swift test --filter KwtSSHResolutionClientTests`; expect all model tests to pass.
- [ ] Run `make format`.
- [ ] Commit: `Decode kwt SSH route snapshots`.

### Task 2: Invoke the exact pinned kwt resolver

**Files:**

- Modify: `Sources/App/KwtSSHResolutionClient.swift`
- Modify: `Sources/App/KwtBinaryLocator.swift`
- Test: `Tests/App/KwtSSHResolutionClientTests.swift`
- Test: `Tests/App/KwtBinaryLocatorTests.swift`

- [ ] Write failing tests with an injected runner proving the client passes hostname, user, and port as separate arguments; preserves discovery-provided aliases; requests JSON; honors cancellation and timeout; and never constructs shell syntax from the target.
- [ ] Locate the bundled local kwt through `KwtBinaryLocator`. Invoke:

```text
kwt ssh resolve <hostname> --user <user> --port <port> --json
```

  omitting absent flags rather than synthesizing defaults.
- [ ] Run the process off the main actor with a bounded timeout. Kwt itself owns the account-login-shell `ssh -G` boundary, so Ghosthub must not wrap the request in another resolver shell or invoke system `ssh -G`.
- [ ] Decode stdout only on status 0. Decode the stable kwt JSON failure envelope for nonzero domain failures; retain status 255 exclusively as OpenSSH transport/setup meaning where existing callers already use it.
- [ ] Never log raw stdout/stderr from the resolver. Bound and sanitize user-facing failures through existing SSH failure mapping.
- [ ] Run `swift test --filter KwtSSHResolutionClientTests` and `swift test --filter KwtBinaryLocatorTests`; expect both suites to pass.
- [ ] Run `make format`.
- [ ] Commit: `Invoke the pinned kwt SSH resolver`.

### Task 3: Extend the pinned kwt contract suite with projection parity

**Files:**

- Modify: `Tests/App/PinnedKwtContractTests.swift`
- Modify: `Makefile`

- [ ] Write pinned subprocess cases covering account-login-shell banners, aliases, explicit user/port precedence, IPv4, raw IPv6, HostKeyAlias, ProxyJump ordering, opaque/nested proxy rejection, repeated identity files, crypto/known-hosts constraints, private `SetEnv`, token-expanded paths, and configuration change producing a new route identity.
- [ ] Port the former Swift selective matrix as observable kwt expectations:
  - the complete effective stream changes the opaque identity;
  - only `kwt.openssh.projection.v1` directives appear in execution projection;
  - forwards, arbitrary commands, user ControlMaster settings, and unknown directives never appear;
  - `SetEnv` does not appear in argv or diagnostics.
- [ ] Include discovery-driven Tailscale/exe.dev targets as structured input. Do not test Tailscale's implementation or OpenSSH library behavior.
- [ ] Add the SSH cases to `make test-kwt-contract` without creating a second contract target or alternate binary path.
- [ ] Run `make test-kwt-contract`; expect the exact `KWT_REVISION` helper to pass all existing project lifecycle and new SSH resolution cases.
- [ ] Commit: `Pin kwt SSH projection parity`.

### Task 4: Replace resolver ownership in connection preparation

**Files:**

- Modify: `Sources/App/SSHConnectionPool.swift`
- Modify: `Sources/App/SSHCommandArguments.swift`
- Modify: `Sources/App/NativeTmuxSessionCoordinator.swift`
- Modify: `Tests/App/SSHConnectionPoolTests.swift`
- Modify: `Tests/App/SSHCommandArgumentsTests.swift`
- Modify: `Tests/App/NativeTmuxSessionCoordinatorTests.swift`

- [ ] Write failing tests showing one observed kwt snapshot feeds pool identity, command arguments, and tmux attachment; a changed route identity prevents preparation; and discovery targets follow the same path as configured hosts.
- [ ] Introduce a small snapshot provider protocol implemented by `KwtSSHResolutionClient` and inject it into the pool/coordinator actors.
- [ ] Replace all calls to `SSHConfigurationResolver.configuration`, `effectiveHost`, `snapshotConnectionArguments`, and `connectionArgumentsSnapshot` with semantic fields and the ordered v1 projection from the same snapshot.
- [ ] Keep ControlMaster creation, generation, activity touch, and teardown in `SSHConnectionPool` for this stage. Include kwt's route identity and projection policy in its existing connection cache identity so a changed observation cannot reuse an old master.
- [ ] Revalidate through kwt before beginning a new pooled connection. Return the existing retryable connection-change UI path rather than silently fetching and using a changed route.
- [ ] Run:

```sh
swift test --filter SSHConnectionPoolTests
swift test --filter SSHCommandArgumentsTests
swift test --filter NativeTmuxSessionCoordinatorTests
```

  Expect all three focused suites to pass.
- [ ] Run `make format`.
- [ ] Commit: `Use kwt snapshots for SSH connection preparation`.

### Task 5: Replace resolver ownership in trust and authentication

**Files:**

- Modify: `Sources/App/SSHHostTrustManager.swift`
- Modify: `Sources/App/SSHAuthenticationSession.swift`
- Modify: `Sources/App/SSHAuthenticationView.swift`
- Modify: `Tests/App/SSHHostTrustManagerTests.swift`
- Modify: `Tests/App/SSHAuthenticationSessionTests.swift`

- [ ] Write failing tests proving trust and authentication consume the same snapshot identity as the pool, preserve the current host-key policy/crypto constraints and native prompt behavior, and reject a snapshot changed before work begins.
- [ ] Replace Swift resolver callbacks with the injected kwt snapshot provider. Trust uses only the snapshot's trust/crypto projection plus its existing explicit OpenSSH overrides; authentication uses only the authentication/private-config projection.
- [ ] Keep Swift askpass, FIFO, host-key approval, password, passphrase, keyboard-interactive, cancellation, and retry ownership unchanged. This stage must not partially adopt daemon prompts.
- [ ] Ensure the native views continue receiving credential-free display target, algorithm, and fingerprint values, never raw projection options.
- [ ] Run:

```sh
swift test --filter SSHHostTrustManagerTests
swift test --filter SSHAuthenticationSessionTests
```

  Expect both suites to pass.
- [ ] Run `make format`.
- [ ] Commit: `Use kwt snapshots for SSH trust and authentication`.

### Task 6: Migrate residual resolver consumers and delete the Swift resolver

**Files:**

- Delete: `Sources/App/SSHConfigurationResolver.swift`
- Modify: `Sources/App/TailscaleDiscovery.swift`
- Modify: `Sources/App/WorkspaceSceneModel.swift`
- Modify: `Sources/App/WorkspaceSceneBootstrap.swift`
- Modify: any remaining production file returned by `rg 'SSHConfigurationResolver|ssh -G' Sources`
- Migrate: `Tests/App/SSHConfigurationResolverTests.swift` into the owning kwt contract or focused consumer suites, then delete the obsolete suite.
- Modify: `Tests/App/TailscaleDiscoveryTests.swift`
- Modify: relevant `WorkspaceSceneModel` tests.

- [ ] Search the complete production and test surface before editing:

```sh
rg -n 'SSHConfigurationResolver|EffectiveSSHConfiguration|ssh -G|snapshot(Authentication|Connection)Arguments' Sources Tests
```

- [ ] Add or update behavior tests for every remaining consumer before moving it. Tailscale discovery supplies a structured target only; scene restoration resolves current policy before connection and preserves attach-only recovery.
- [ ] Delete `SSHConfigurationResolver.swift` only after every production caller compiles against the kwt snapshot contract. Do not add a forwarding compatibility wrapper.
- [ ] Move retained behavioral cases into `PinnedKwtContractTests`, `KwtSSHResolutionClientTests`, or the actual consumer suite. Do not add a test that merely asserts the old file is absent.
- [ ] Run `rg -n 'SSHConfigurationResolver|EffectiveSSHConfiguration|ssh -G' Sources Tests`; expect no Swift production resolver invocation and only intentional contract-fixture text.
- [ ] Run `make format`.
- [ ] Commit: `Remove the Swift SSH configuration resolver`.

### Task 7: Advance the pin and document the interim boundary

**Files:**

- Modify: `KWT_REVISION`
- Modify: `docs/architecture.md`
- Modify: `docs/threat-model.md`
- Modify: `docs/terminal-sessions.md`
- Modify: `docs/release.md`
- Modify: `CHANGELOG.md` only if this work is included in an accepted release entry.

- [ ] Pin the full merged kwt commit containing `ssh.resolve.v1`; do not use a branch, tag alias, or local override.
- [ ] Document the Stage 1 interim boundary exactly: kwt owns route resolution, route identity, and execution projection; Swift still owns pool, trust, authentication, askpass, and connection lifecycle until the atomic Stage 2 cutover.
- [ ] Update the threat model for the pinned kwt resolver, its daemon, private projection values, login-shell execution, and the rule that Ghosthub never connects to daemon HTTP directly.
- [ ] Update release documentation for the new kwt capability gate and pinned contract evidence. No release packaging behavior changes beyond the pin.
- [ ] Run every required gate for remote/terminal changes:

```sh
make test-libghostty-bootstrap
make python-test
swift test
make test-essential-workflows
make build
```

  Expect all commands to pass.
- [ ] Run `git diff --check` and confirm the implementation branch contains no `docs/superpowers/specs/` or `docs/superpowers/plans/` artifacts.
- [ ] Commit: `Cut SSH route resolution over to kwt`.

## Acceptance gate

Ghosthub may ship Stage 1 when its exact pinned helper passes the projection contract and every existing SSH workflow still uses Swift lifecycle ownership against a kwt-owned immutable snapshot. Do not remove `SSHConnectionPool`, `SSHHostTrustManager`, or `SSHAuthenticationSession` in this plan. Their atomic Stage 2 removal begins only after ordered operation streaming and daemon-owned trust/auth/lease parity are independently complete.
