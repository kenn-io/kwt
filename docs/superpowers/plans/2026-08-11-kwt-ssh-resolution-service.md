# Kwt SSH Resolution Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make kwt the single owner of observed OpenSSH route resolution and the versioned execution projection, exposed through the root Go package, daemon API, and CLI without yet moving trust, authentication, or connection lifecycle.

**Architecture:** The service invokes kit's resolver through a platform adapter. POSIX uses the account login shell with nonce-framed stdout; Windows invokes system OpenSSH directly. The complete canonical `ssh -G` stream is retained only as route-identity input. A positive `projection.v1` emits the bounded options Ghosthub needs under `-F /dev/null`; every unlisted directive is identity-only. The public service is transport-neutral, while daemon and Cobra adapters preserve one implementation owner.

**Tech Stack:** Go 1.26, `go.kenn.io/kit/openssh` v0.19.1+, Huma v2, Cobra, system OpenSSH.

---

## Scope boundary

This plan implements Stage 1 only. It does not create ControlMaster sockets, leases, askpass helpers, host-key approvals, password prompts, or operation streams. It must land with `ssh.resolve.v1` only. Stage 2 remains gated on the ordered-stream plan and the pinned Ghosthub parity suite.

### Task 1: Encode and prove projection policy v1

**Files:**

- Create: `internal/ssh/projection.go`
- Create: `internal/ssh/projection_test.go`
- Create: `internal/ssh/testdata/projection_v1.json`

- [ ] Port the selective argument cases from Ghosthub's `SSHConfigurationResolverTests.swift` into a data fixture containing normalized effective options and the expected structural, trust/crypto, authentication, and private-config projection.
- [ ] Write failing table tests proving:
  - repeated `IdentityFile`, `CertificateFile`, `SendEnv`, and `SetEnv` values retain their defined order;
  - quoting-sensitive values remain one argument or one safely quoted private-config line;
  - endpoint fields are emitted in fixed order under `-F /dev/null`;
  - local/remote forwards, arbitrary commands, proxy commands, user ControlMaster settings, and unknown future directives are absent from execution but remain in identity input;
  - a policy-version change changes route identity even when effective config is unchanged.
- [ ] Implement the exact positive sets from the approved specification under:

```go
const projectionPolicyV1 = "kwt.openssh.projection.v1"

type projection struct {
    PolicyVersion string
    Arguments     []string
    PrivateConfig []string
}
```

- [ ] Write `SetEnv` and path-bearing authentication options only through an owner-private ephemeral config representation; never render values into diagnostics.
- [ ] Use `openssh.RouteIdentity(projectionPolicyV1, route)` or, if kit cannot include the policy without losing full config coverage, extend kit first with the smallest app-neutral scope API. Do not duplicate kit identity encoding.
- [ ] Run `go test ./internal/ssh -run 'TestProjectionV1'` and expect all matrix cases to pass.
- [ ] Commit: `Define OpenSSH execution projection v1`.

### Task 2: Add route snapshot contracts and stable errors

**Files:**

- Create: `internal/ssh/types.go`
- Create: `internal/ssh/errors.go`
- Modify: `service/error.go`
- Test: `internal/ssh/types_test.go`
- Test: `service/error_test.go`

- [ ] Write failing tests for credential-free display values, opaque route identity, JSON omission of the full canonical stream, IPv6 display/command distinction, and typed error preservation.
- [ ] Define internal contracts and root-facing aliases for:

```go
type Target struct {
    Hostname string `json:"hostname"`
    User     string `json:"user,omitempty"`
    Port     int    `json:"port,omitempty"`
}

type ResolveRequest struct {
    Target Target `json:"target"`
}

type RouteSnapshot struct {
    LogicalTarget    Target           `json:"logical_target"`
    Targets          []ResolvedTarget `json:"targets"`
    RouteIdentity    string           `json:"route_identity"`
    ProjectionPolicy string           `json:"projection_policy"`
    ObservedAt       time.Time        `json:"observed_at"`
}
```

- [ ] Keep canonical option streams in a private observation type with no JSON tags. Public resolved targets contain semantic route fields plus the bounded projection needed by the interim Swift client.
- [ ] Add stable codes `ssh_invalid_target`, `ssh_resolution_failed`, `ssh_route_unreviewable`, and `ssh_configuration_changed`. Map unexpected failures to `internal` with private cause only.
- [ ] Run `go test ./internal/ssh ./service` and expect the new contract tests to pass.
- [ ] Commit: `Define SSH route snapshot contracts`.

### Task 3: Implement POSIX login-shell and Windows resolver adapters

**Files:**

- Create: `internal/ssh/resolver.go`
- Create: `internal/ssh/resolver_posix.go`
- Create: `internal/ssh/resolver_windows.go`
- Create: `internal/ssh/resolver_test.go`
- Create: `internal/ssh/resolver_posix_test.go`
- Create: `internal/ssh/resolver_windows_test.go`

- [ ] Write failing tests for explicit user/port precedence, aliases, IPv4, raw IPv6, login-shell banners, marker-like stderr, malformed/missing markers, cancellation, deadline, unsafe targets rejected before invocation, ProxyJump ordering, and opaque/nested proxy rejection.
- [ ] Inject an `OutputRunner`, account shell lookup, nonce source, clock, and request environment. Tests must not depend on the developer's real SSH configuration.
- [ ] On POSIX, invoke the account's configured shell in login mode and frame only stdout from `ssh -G` between cryptographically random markers. Quote the validated target and explicit arguments without interpolating untrusted shell syntax.
- [ ] On Windows, invoke system `ssh -G` directly with the request-scoped sanitized environment. Keep persistent connections explicitly out of scope.
- [ ] Resolve each direct ProxyJump target independently through `openssh.ResolveRoute`; reject ProxyCommand and a hop that introduces another proxy route.
- [ ] Preserve `ctx.Err()` in every subprocess failure chain so `errors.Is` detects cancellation and deadlines.
- [ ] Run `go test -race ./internal/ssh` on the host platform, then compile the package with `GOOS=windows go test -c ./internal/ssh` and remove the temporary test binary.
- [ ] Commit: `Resolve OpenSSH routes through platform boundaries`.

### Task 4: Add the transport-neutral service and thin root API

**Files:**

- Create: `internal/ssh/service.go`
- Create: `internal/ssh/service_test.go`
- Create: `ssh.go`
- Modify: `public_test.go`

- [ ] Write failing service tests proving the resolver is invoked once per target, the full normalized stream affects identity, projection output follows v1, `ObservedAt` uses the injected clock, and cancellation is not normalized away.
- [ ] Implement:

```go
type ServiceOptions struct {
    Resolver Resolver
    Now      func() time.Time
}

type Service struct { /* private fields */ }

func NewService(ServiceOptions) *Service
func (s *Service) Resolve(context.Context, ResolveRequest) (RouteSnapshot, error)
```

- [ ] Add only aliases, `NewSSHService`, and constants required by consumers to root `ssh.go`. Do not expose the internal runner or kit types and do not add a `go.kenn.io/kwt/ssh` package.
- [ ] Add a compile-time public-surface test importing `go.kenn.io/kwt` and resolving through an injected service. Do not test source-file layout.
- [ ] Run `go test ./...` and expect the root public API and service tests to pass.
- [ ] Commit: `Expose the kwt SSH resolution service`.

### Task 5: Add the daemon resolution route

**Files:**

- Modify: `internal/daemon/types.go`
- Modify: `internal/daemon/server.go`
- Modify: `internal/daemon/host.go`
- Modify: `internal/daemon/runtime.go`
- Modify: `internal/daemon/client.go`
- Create: `internal/daemon/ssh_resolution_test.go`

- [ ] Write failing daemon tests for successful round-trip, stable error descriptors, cancellation, unsupported capability refusal, raw-option omission, capability sorting, and one daemon-owned service instance reused across requests.
- [ ] Add `CapabilitySSHResolve = "ssh.resolve.v1"`, bump `APISchemaVersion` to the next additive minor from the branch's current value, and advertise the capability in both status and runtime records.
- [ ] Add `SSHResolver` to `ServerOptions` and register `POST /api/v1/ssh/resolve`. The route reserves ordinary work, calls the same public service, and returns `SSHRouteSnapshot`.
- [ ] Add `Client.ResolveSSH(ctx, request)` using an SSH-specific timeout with enough headroom for login-shell startup and multi-hop `ssh -G`; do not reuse the two-second control client.
- [ ] Sanitize private causes through the existing daemon logging boundary. Include request-scoped secret values in redaction without publishing raw config or environment data.
- [ ] Run `go test -race ./internal/daemon -run 'Test.*SSHResolve'` and expect all transport tests to pass.
- [ ] Commit: `Serve SSH route resolution from the daemon`.

### Task 6: Add `kwt ssh resolve`

**Files:**

- Create: `internal/cmd/ssh.go`
- Create: `internal/cmd/ssh_test.go`
- Create: `internal/cmd/ssh_subprocess_test.go`
- Modify: `internal/cmd/root.go`
- Modify: `internal/cmd/daemon_client.go`

- [ ] Write failing command tests for structured hostname/user/port input, aliases, IPv6, JSON success, JSON stable errors, daemon auto-start/reuse, drain wait then fail, and exit status never being 255 for kwt domain failures.
- [ ] Implement `kwt ssh resolve <hostname> [--user USER] [--port PORT] --json`. Require JSON for the machine contract; a concise human view may print endpoint and route hops without raw option values.
- [ ] Make the command a daemon client only. Do not retain a hidden direct resolution path.
- [ ] Preserve exact stable descriptors in JSON and use the existing daemon-failure exit mapping. Do not normalize unrelated command exit behavior.
- [ ] Add a subprocess fixture whose fake account shell emits startup banners and framed output, proving the real command adapter ignores everything outside the nonce frame.
- [ ] Run `go test -race ./internal/cmd -run 'TestSSH'` and expect command and subprocess tests to pass.
- [ ] Commit: `Add the kwt SSH resolve command`.

### Task 7: Record projection review and Stage 1 security boundaries

**Files:**

- Modify: `docs/reference/cli.md`
- Modify: `docs/design/daemon.md`
- Modify: `docs/development/threat-model.md`
- Modify: `docs/development/contributing.md`

- [ ] Document `ssh.resolve.v1`, the exact v1 allowlist, the default identity-only rule for unlisted directives, private `SetEnv`, login-shell framing, ProxyJump limits, and the absence of any Stage 1 connection ownership.
- [ ] Add the maintenance rule: any supported OpenSSH version change in CI requires reviewing new/changed `ssh -G` directives against `projection_v1.json`; any projection change creates a new policy version and Ghosthub parity update.
- [ ] Record the spike evidence that total `ssh -G` replay is invalid because supported OpenSSH emits directives such as `Host` that are not command-line options. Do not pin a test to one incidental vendor version string.
- [ ] Run `gofmt -w ssh.go internal/ssh/*.go internal/daemon/*.go internal/cmd/ssh*.go` on touched Go files.
- [ ] Run `go test -race ./...`, `make lint`, `make build`, and `git diff --check`; expect all checks to pass.
- [ ] Remove `docs/superpowers/specs/` and `docs/superpowers/plans/` changes from the implementation branch before opening its PR.
- [ ] Commit: `Document kwt SSH route resolution`.

## Handoff gate

Stage 1 is ready for Ghosthub only after the Go, daemon, and CLI layers return identical snapshots and errors, the pinned projection fixture matches Ghosthub's selective Swift matrix, and no raw canonical option stream appears in the wire result. Do not begin Stage 2 trust/authentication or leases until ordered operation streaming has independently merged.
