# Ordered Daemon Operation Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add the bounded, reconnectable, prompt-capable daemon operation channel required before kwt can own interactive OpenSSH work.

**Architecture:** A daemon-owned operation hub assigns opaque operation and prompt IDs, sequences events, retains a bounded in-memory replay window, and exposes authenticated HTTP event, response, and cancellation routes. Domain services create operations through the hub; the transport never executes domain logic. Clients resume by operation ID and sequence cursor, bind every response to one prompt, and report an indeterminate outcome if the daemon or retained stream disappears.

**Tech Stack:** Go 1.26, Huma v2, `net/http`, kit daemon endpoints, Cobra, newline-delimited JSON, existing `service.Error` descriptors.

---

## Contract decisions

- The first implementation is same-daemon reconnectable, not crash-durable. Daemon loss returns `operation_outcome_unknown`; no disk journal or compatibility path is added.
- An operation can emit many prompt rounds. Exactly one response is accepted for each current prompt ID; stale, duplicate, or cross-operation responses fail closed.
- Events are ordered by a strictly increasing sequence number. A reconnect supplies `after_sequence` and receives retained later events before live events.
- Each operation retains at most 256 events and 1 MiB of event payload. A producer that would exceed the bound terminates the operation with `operation_capacity_exhausted` rather than dropping a prompt or completion.
- Completed operations remain available for five minutes, bounded to 128 retained operations. Capacity exhaustion rejects new operations rather than evicting active work.
- Human progress and warnings are written immediately to stderr. Machine-readable final output remains the owning command's stdout contract.

### Task 1: Define transport-neutral operation contracts

**Files:**

- Create: `service/operation.go`
- Modify: `service/error.go`
- Test: `service/operation_test.go`

- [ ] Write failing tests for event validation: increasing sequence, bounded message text, prompt events requiring a prompt ID, and completion being terminal.
- [ ] Add the transport-neutral types:

```go
type OperationEventKind string

const (
    OperationProgress OperationEventKind = "progress"
    OperationWarning  OperationEventKind = "warning"
    OperationPrompt   OperationEventKind = "prompt"
    OperationComplete OperationEventKind = "complete"
)

type OperationEvent struct {
    OperationID string            `json:"operation_id"`
    Sequence    uint64            `json:"sequence"`
    Kind        OperationEventKind `json:"kind"`
    Message     string            `json:"message,omitempty"`
    Prompt      *OperationPrompt  `json:"prompt,omitempty"`
    Result      json.RawMessage   `json:"result,omitempty"`
    Failure     *Descriptor       `json:"failure,omitempty"`
}

type OperationPrompt struct {
    ID        string         `json:"id"`
    Kind      string         `json:"kind"`
    Message   string         `json:"message"`
    Sensitive bool           `json:"sensitive"`
    Details   map[string]any `json:"details,omitempty"`
}

type OperationResponse struct {
    PromptID string `json:"prompt_id"`
    Value    string `json:"value"`
}
```

- [ ] Promote `OperationIDConflict`, `OperationCapacityExhausted`, and `OperationOutcomeUnknown` from reserved comments to active stable codes. Keep `OperationJournalUnavailable` reserved until a durable journal exists.
- [ ] Run `go test ./service` and expect the new contract tests to pass.
- [ ] Commit: `Define ordered operation stream contracts`.

### Task 2: Implement the bounded daemon operation hub

**Files:**

- Create: `internal/daemon/operation.go`
- Test: `internal/daemon/operation_test.go`

- [ ] Write failing tests for ordered emission, multiple prompt rounds, empty responses, duplicate-response rejection, cancellation, completion retention, resume from a cursor, and capacity exhaustion.
- [ ] Implement `OperationHub`, `Operation`, and `OperationSubscription`. Keep all state under one hub mutex, but never hold it while a subscriber callback or domain producer blocks.
- [ ] Generate operation and prompt IDs with `crypto/rand`; accept a caller-supplied operation ID only together with a request digest, so a retry can reuse the existing operation while a conflicting request returns `operation_id_conflict`.
- [ ] Give the domain worker a service-owned context canceled by explicit cancellation, bounded backpressure, daemon drain deadline, or final subscriber loss after the configured grace period.
- [ ] Make prompt response delivery a capacity-one private channel tied to the exact active prompt. Clear the response and prompt state immediately after the round finishes.
- [ ] Run `go test -race ./internal/daemon -run 'TestOperation'` and expect all operation-hub tests to pass without races.
- [ ] Commit: `Add bounded daemon operation hub`.

### Task 3: Expose authenticated operation control routes

**Files:**

- Modify: `internal/daemon/server.go`
- Modify: `internal/daemon/types.go`
- Test: `internal/daemon/server_test.go`

- [ ] Write failing HTTP tests for:
  - `GET /api/v1/operations/{id}/events?after_sequence=N` replaying retained events and then streaming live NDJSON;
  - `POST /api/v1/operations/{id}/responses` accepting only the current prompt ID;
  - `DELETE /api/v1/operations/{id}` canceling the domain context;
  - authentication, host-header, body-limit, and unknown-operation failures using the existing stable problem envelope.
- [ ] Add `Operations *OperationHub` to `ServerOptions` and register routes only when it is non-nil. Do not add a fake production start route; future domain routes create operations through the injected hub.
- [ ] Flush each encoded event immediately with `http.Flusher`. Stop the subscription on request cancellation without assuming the domain operation was canceled.
- [ ] Add `CapabilityOperationStream = "operation.stream.v1"` and bump `APISchemaVersion` from `1.6.0` to `1.7.0`.
- [ ] Run `go test -race ./internal/daemon -run 'Test.*Operation'` and expect the route and hub tests to pass.
- [ ] Commit: `Serve authenticated operation streams`.

### Task 4: Wire the hub into daemon host lifecycle and drain

**Files:**

- Modify: `internal/daemon/host.go`
- Modify: `internal/daemon/runtime.go`
- Modify: `internal/daemon/gate.go`
- Test: `internal/daemon/host_test.go`
- Test: `internal/daemon/runtime_test.go`

- [ ] Write failing tests proving active operations count as daemon work, new operations are refused while draining, the advertised capability lists remain sorted, and drain deadline cancellation produces a terminal event before HTTP shutdown force-closes clients.
- [ ] Construct one hub per daemon host and pass it to `NewServer`.
- [ ] Reserve `ReservationWork` for every live domain operation and release it exactly once at terminal completion.
- [ ] On drain, stop admitting operations, permit existing operations through the shared replacement deadline, then cancel remaining workers and close their subscribers before removing the runtime record.
- [ ] Add `operation.stream.v1` to both runtime-record and status capability lists in sorted order.
- [ ] Run `go test -race ./internal/daemon` and expect the daemon suite to pass.
- [ ] Commit: `Integrate operation streams with daemon drain`.

### Task 5: Add the reconnecting daemon client

**Files:**

- Modify: `internal/daemon/client.go`
- Create: `internal/daemon/operation_client.go`
- Test: `internal/daemon/client_test.go`

- [ ] Write failing tests for ordered decoding, cursor resume after response loss, multi-round response binding, explicit cancellation, invalid sequence rejection, response-size bounds, and daemon loss mapping to `operation_outcome_unknown`.
- [ ] Add:

```go
type OperationCallbacks struct {
    Event  func(service.OperationEvent) error
    Prompt func(context.Context, service.OperationPrompt) (string, error)
}

func (c *Client) FollowOperation(
    ctx context.Context,
    operationID string,
    after uint64,
    callbacks OperationCallbacks,
) (json.RawMessage, error)
```

- [ ] Use a dedicated streaming HTTP client with no whole-request timeout; the caller context and server drain deadline bound the operation. Keep control and inventory clients unchanged.
- [ ] Reconnect only to the same proof-verified daemon and operation ID. If the runtime owner changes, or the requested cursor has fallen outside retention, return `operation_outcome_unknown` rather than replaying the domain request.
- [ ] Run `go test -race ./internal/daemon -run 'Test.*OperationClient'` and expect the client tests to pass.
- [ ] Commit: `Add reconnecting operation stream client`.

### Task 6: Add immediate CLI event rendering

**Files:**

- Create: `internal/cmd/operation_stream.go`
- Test: `internal/cmd/operation_stream_test.go`

- [ ] Write failing tests with pipes proving progress and warnings are available before completion, prompts read one response at a time, sensitive prompt responses never reach stdout/stderr, and JSON final output remains unmodified.
- [ ] Implement one renderer used by future long-running commands. Flush human event lines to `cmd.ErrOrStderr()` immediately; do not buffer until `RunE` returns.
- [ ] Require an explicit prompt callback from native consumers. The ordinary CLI callback may read from a terminal only when stdin is interactive; otherwise return `interaction_required`.
- [ ] Wrap stream-owning commands with `withGracefulSignals` when they are introduced so SIGINT/SIGTERM cancel the operation and retain 130/143 behavior.
- [ ] Run `go test -race ./internal/cmd -run 'TestOperationStream'` and expect all renderer tests to pass.
- [ ] Commit: `Add streaming CLI operation renderer`.

### Task 7: Document and verify the foundation

**Files:**

- Modify: `docs/design/daemon.md`
- Modify: `docs/development/threat-model.md`
- Modify: `docs/reference/cli.md`

- [ ] Document event ordering, retention bounds, prompt binding, same-daemon resume, loss semantics, stderr/stdout ownership, and the fact that this capability alone starts no domain operation.
- [ ] Document that operation IDs and bearer authentication are both required; knowing an operation ID is not authorization.
- [ ] Run `gofmt -w service/operation.go service/operation_test.go internal/daemon/operation*.go internal/cmd/operation_stream*.go`.
- [ ] Run `go test -race ./...`, `make lint`, and `make build`; expect all three to pass.
- [ ] Run `git diff --check` and confirm the worktree contains no generated or superpowers planning artifacts intended for the PR.
- [ ] Commit: `Document ordered daemon operation streaming`.

## Handoff gate

Do not start daemon-owned SSH trust or authentication until this plan is merged and its multi-round prompt, bounded backpressure, cancellation, and same-daemon resume tests are green. Stage 1 resolution may be developed in parallel only because it is noninteractive and does not use the stream.
