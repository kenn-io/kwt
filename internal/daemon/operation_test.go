package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/kwt/service"
)

func TestOperationHubReplaysOrderedCompletion(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	op, created, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(_ context.Context, operation *Operation) (json.RawMessage, error) {
			if err := operation.Progress("resolving"); err != nil {
				return nil, err
			}
			if err := operation.Warning("using fallback"); err != nil {
				return nil, err
			}
			return json.RawMessage(`{"status":"ready"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || op.ID() != "operation-1" {
		t.Fatalf("unexpected start result: created=%v id=%q", created, op.ID())
	}

	events := collectOperationEvents(t, hub, op.ID(), 0)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) {
			t.Fatalf("event %d has sequence %d", i, event.Sequence)
		}
	}
	if events[2].Kind != service.OperationEventComplete || string(events[2].Result) != `{"status":"ready"}` {
		t.Fatalf("unexpected completion: %#v", events[2])
	}
}

func TestOperationHubPublishesSanitizedTerminalFailure(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return nil, service.NewError(
				service.Internal,
				"private diagnostic",
				false,
				map[string]any{"path": "/safe/path", "private": "secret"},
				errors.New("private cause"),
			)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collectOperationEvents(t, hub, operation.ID(), 0)
	if len(events) != 1 || events[0].Failure == nil {
		t.Fatalf("unexpected operation events: %#v", events)
	}
	failure := events[0].Failure
	if failure.Message != "internal failure" {
		t.Fatalf("failure message = %q", failure.Message)
	}
	if len(failure.Details) != 1 || failure.Details["path"] != "/safe/path" {
		t.Fatalf("failure details = %#v", failure.Details)
	}
}

func TestOperationHubPreservesTypedPromptTimeout(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return nil, service.NewError(
				service.SSHPromptTimedOut,
				"SSH prompt timed out",
				false,
				nil,
				context.DeadlineExceeded,
			)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collectOperationEvents(t, hub, operation.ID(), 0)
	if len(events) != 1 || events[0].Failure == nil {
		t.Fatalf("unexpected operation events: %#v", events)
	}
	failure := events[0].Failure
	if failure.Code != service.SSHPromptTimedOut || failure.Message != "SSH prompt timed out" {
		t.Fatalf("prompt-timeout failure = %#v", failure)
	}
}

func TestOperationHubPublishesInternalFailureForMalformedResult(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{"incomplete":`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collectOperationEvents(t, hub, operation.ID(), 0)
	if len(events) != 1 || events[0].Failure == nil {
		t.Fatalf("unexpected operation events: %#v", events)
	}
	failure := events[0].Failure
	if failure.Code != service.Internal || failure.Message != "internal failure" || failure.Retryable {
		t.Fatalf("malformed-result failure = %#v", failure)
	}
}

func TestOperationHubRetainsImmutablePromptPayload(t *testing.T) {
	details := map[string]any{"host": "original.example"}
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(ctx context.Context, operation *Operation) (json.RawMessage, error) {
			_, promptErr := operation.Prompt(ctx, service.OperationPrompt{
				Kind: "password", Message: "Password:", Sensitive: true, Details: details,
			})
			return json.RawMessage(`{}`), promptErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	prompt := receiveOperationEvent(t, first.Events())
	if prompt.Prompt == nil {
		t.Fatalf("unexpected prompt event: %#v", prompt)
	}
	details["host"] = "replacement.example"

	replay, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	replayed := receiveOperationEvent(t, replay.Events())
	if replayed.Prompt == nil || replayed.Prompt.Details["host"] != "original.example" {
		t.Fatalf("replayed prompt = %#v", replayed.Prompt)
	}
	if err := hub.Respond(operation.ID(), service.OperationResponse{PromptID: prompt.Prompt.ID}); err != nil {
		t.Fatal(err)
	}
	_ = receiveOperationEvent(t, first.Events())
}

func TestOperationHubDeliversSSHPromptTargetDetails(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "ssh-prompt-targets",
		Run: func(ctx context.Context, operation *Operation) (json.RawMessage, error) {
			_, promptErr := operation.Prompt(ctx, service.OperationPrompt{
				Kind:      "ssh_host_key",
				Message:   "Continue connecting?",
				Sensitive: false,
				Details: map[string]any{
					"logical_target": map[string]any{
						"hostname": "relay.example.test",
					},
					"effective_target": map[string]any{
						"hostname": "192.0.2.10",
						"user":     "deploy",
						"port":     2200,
					},
					"display_target": "relay.example.test",
					"hop_index":      0,
					"hop_count":      1,
				},
			})
			return json.RawMessage(`{}`), promptErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	prompt := receiveOperationEvent(t, subscription.Events())
	if prompt.Prompt == nil {
		t.Fatalf("unexpected prompt event: %#v", prompt)
	}
	logical, ok := prompt.Prompt.Details["logical_target"].(map[string]any)
	if !ok || logical["hostname"] != "relay.example.test" {
		t.Fatalf("logical target details = %#v", prompt.Prompt.Details["logical_target"])
	}
	if err := hub.Respond(operation.ID(), service.OperationResponse{
		PromptID: prompt.Prompt.ID,
		Value:    "yes",
	}); err != nil {
		t.Fatal(err)
	}
	_ = receiveOperationEvent(t, subscription.Events())
}

func TestOperationHubResumesAfterSequence(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	op, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(_ context.Context, operation *Operation) (json.RawMessage, error) {
			if err := operation.Progress("one"); err != nil {
				return nil, err
			}
			if err := operation.Progress("two"); err != nil {
				return nil, err
			}
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	allEvents := collectOperationEvents(t, hub, op.ID(), 0)
	if len(allEvents) != 3 {
		t.Fatalf("got %d initial events, want 3", len(allEvents))
	}
	events := collectOperationEvents(t, hub, op.ID(), 1)
	if len(events) != 2 || events[0].Sequence != 2 || events[1].Sequence != 3 {
		t.Fatalf("unexpected resumed events: %#v", events)
	}
}

func TestOperationHubBindsMultiplePromptRounds(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	op, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(ctx context.Context, operation *Operation) (json.RawMessage, error) {
			first, err := operation.Prompt(ctx, service.OperationPrompt{
				Kind: "password", Message: "Password:", Sensitive: true,
			})
			if err != nil {
				return nil, err
			}
			if first != "wrong" {
				return nil, errors.New("unexpected first response")
			}
			second, err := operation.Prompt(ctx, service.OperationPrompt{
				Kind: "password", Message: "Password, again:", Sensitive: true,
			})
			if err != nil {
				return nil, err
			}
			if second != "" {
				return nil, errors.New("empty response was not preserved")
			}
			return json.RawMessage(`{"status":"ready"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := hub.Subscribe(op.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()

	first := receiveOperationEvent(t, subscription.Events())
	if first.Kind != service.OperationEventPrompt || first.Prompt == nil {
		t.Fatalf("unexpected first event: %#v", first)
	}
	if err := hub.Respond(op.ID(), service.OperationResponse{
		PromptID: first.Prompt.ID, Value: "wrong",
	}); err != nil {
		t.Fatal(err)
	}
	if err := hub.Respond(op.ID(), service.OperationResponse{
		PromptID: first.Prompt.ID, Value: "duplicate",
	}); !service.IsCode(err, service.Conflict) {
		t.Fatalf("duplicate response error = %v", err)
	}

	second := receiveOperationEvent(t, subscription.Events())
	if second.Kind != service.OperationEventPrompt || second.Prompt == nil || second.Prompt.ID == first.Prompt.ID {
		t.Fatalf("unexpected second event: %#v", second)
	}
	if err := hub.Respond(op.ID(), service.OperationResponse{PromptID: second.Prompt.ID}); err != nil {
		t.Fatal(err)
	}
	completion := receiveOperationEvent(t, subscription.Events())
	if completion.Kind != service.OperationEventComplete || completion.Failure != nil {
		t.Fatalf("unexpected completion: %#v", completion)
	}
}

func TestOperationHubRejectsConflictingRequestID(t *testing.T) {
	release := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	op, created, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil || !created {
		t.Fatalf("start: created=%v err=%v", created, err)
	}
	reused, created, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
	})
	if err != nil || created || reused != op {
		t.Fatalf("reuse: operation=%p created=%v err=%v", reused, created, err)
	}
	_, _, err = hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "different",
	})
	if !service.IsCode(err, service.OperationIDConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	close(release)
}

func TestOperationHubRejectsUnroutableOperationIDs(t *testing.T) {
	for _, id := range []string{"batch/1", "batch?1", "batch#1", ".", ".."} {
		t.Run(id, func(t *testing.T) {
			hub := NewOperationHub(context.Background(), OperationHubOptions{})
			_, created, err := hub.Start(OperationStart{
				ID: id, RequestDigest: "request-1",
				Run: func(context.Context, *Operation) (json.RawMessage, error) {
					return json.RawMessage(`{}`), nil
				},
			})
			if created || !service.IsCode(err, service.InvalidRequest) {
				t.Fatalf("start with ID %q: created=%v err=%v", id, created, err)
			}
		})
	}
}

func TestOperationHubRejectsUnroutableGeneratedOperationID(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		IDSource: func() (string, error) { return "batch/1", nil },
	})
	_, created, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if created || !service.IsCode(err, service.InvalidRequest) {
		t.Fatalf("start with generated ID: created=%v err=%v", created, err)
	}
}

func TestOperationHubCancellationReachesWorker(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{})
	op, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(ctx context.Context, _ *Operation) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := hub.Cancel(op.ID()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("worker did not observe cancellation")
	}
}

func TestOperationHubReportsUnknownOutcomeAtEventCapacity(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		MaxEvents: 2,
	})
	op, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(_ context.Context, operation *Operation) (json.RawMessage, error) {
			if err := operation.Progress("one"); err != nil {
				return nil, err
			}
			if err := operation.Progress("two"); err != nil {
				return nil, err
			}
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collectOperationEvents(t, hub, op.ID(), 0)
	if len(events) != 2 || events[1].Failure == nil ||
		events[1].Failure.Code != service.OperationOutcomeUnknown || events[1].Failure.Retryable {
		t.Fatalf("unexpected capacity events: %#v", events)
	}
}

func TestOperationHubRetriesGeneratedIDCollision(t *testing.T) {
	ids := make(chan string, 3)
	ids <- "operation-1"
	ids <- "operation-1"
	ids <- "operation-2"
	release := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		IDSource: func() (string, error) { return <-ids, nil },
	})
	first, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := hub.Start(OperationStart{
		RequestDigest: "request-2",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != "operation-1" || second.ID() != "operation-2" {
		t.Fatalf("generated IDs = %q, %q", first.ID(), second.ID())
	}
	close(release)
}

func TestOperationHubCancelsAfterFinalSubscriberGrace(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		SubscriberGrace: time.Millisecond,
	})
	op, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(ctx context.Context, _ *Operation) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	subscription, err := hub.Subscribe(op.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	subscription.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("worker survived final subscriber loss")
	}
}

func TestOperationHubIgnoresStaleSubscriberLossTimerAfterReconnect(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		SubscriberGrace: time.Millisecond,
	})
	op, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(ctx context.Context, _ *Operation) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	// Hold the hub lock until the initial loss timer has fired and its callback
	// is waiting. Replacing that timer models a subscriber reconnecting and
	// immediately disconnecting before the stale callback acquires the lock.
	hub.mu.Lock()
	time.Sleep(20 * time.Millisecond)
	staleTimer := op.entry.lossTimer
	if staleTimer == nil {
		hub.mu.Unlock()
		t.Fatal("initial subscriber-loss timer is missing")
	}
	staleTimer.Stop()
	op.entry.lossTimer = nil
	hub.options.SubscriberGrace = time.Hour
	hub.scheduleSubscriberLossLocked(op.entry)
	currentTimer := op.entry.lossTimer
	hub.mu.Unlock()

	select {
	case <-canceled:
		t.Fatal("stale subscriber-loss timer canceled the reconnected operation")
	case <-time.After(50 * time.Millisecond):
	}
	hub.mu.Lock()
	if op.entry.lossTimer != currentTimer {
		hub.mu.Unlock()
		t.Fatal("stale subscriber-loss timer replaced the current timer")
	}
	hub.mu.Unlock()

	op.entry.cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop during test cleanup")
	}
}

func TestOperationHubExpiresCompletedRetention(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		Now:                func() time.Time { return now },
		CompletedRetention: time.Minute,
	})
	op, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = collectOperationEvents(t, hub, op.ID(), 0)
	now = now.Add(time.Minute)
	_, _, err = hub.Start(OperationStart{
		ID: "operation-2", RequestDigest: "request-2",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = hub.Subscribe(op.ID(), 0)
	if !service.IsCode(err, service.NotFound) {
		t.Fatalf("expired operation error = %v", err)
	}
}

func TestOperationHubRetainsTerminalOperationUntilWorkerReleaseAfterExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	releaseStarted := make(chan struct{})
	allowRelease := make(chan struct{})
	released := make(chan struct{})
	var allowReleaseOnce sync.Once
	defer allowReleaseOnce.Do(func() { close(allowRelease) })
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		Now:                func() time.Time { return now },
		CompletedRetention: time.Minute,
		Reserve: func() (func(), error) {
			return func() {
				close(releaseStarted)
				<-allowRelease
				close(released)
			}, nil
		},
	})
	operation, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-releaseStarted
	now = now.Add(time.Minute)
	reused, created, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
	})
	if err != nil || created || reused != operation {
		t.Fatalf("reuse during worker release: operation=%p created=%v err=%v", reused, created, err)
	}
	allowReleaseOnce.Do(func() { close(allowRelease) })
	<-released
}

func TestOperationHubRetainsTerminalOperationUntilWorkerReleaseUnderCompletedCapacity(t *testing.T) {
	firstReleaseStarted := make(chan struct{})
	allowFirstRelease := make(chan struct{})
	firstReleased := make(chan struct{})
	secondReleased := make(chan struct{})
	var allowFirstReleaseOnce sync.Once
	defer allowFirstReleaseOnce.Do(func() { close(allowFirstRelease) })
	reservations := 0
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		MaxCompletedOperations: 1,
		Reserve: func() (func(), error) {
			reservations++
			if reservations == 1 {
				return func() {
					close(firstReleaseStarted)
					<-allowFirstRelease
					close(firstReleased)
				}, nil
			}
			return func() { close(secondReleased) }, nil
		},
	})
	first, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-firstReleaseStarted
	_, _, err = hub.Start(OperationStart{
		ID: "operation-2", RequestDigest: "request-2",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-secondReleased
	reused, created, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
	})
	if err != nil || created || reused != first {
		t.Fatalf("reuse under completed capacity: operation=%p created=%v err=%v", reused, created, err)
	}
	allowFirstReleaseOnce.Do(func() { close(allowFirstRelease) })
	<-firstReleased
}

func TestOperationHubRejectsNewWorkAtActiveCapacity(t *testing.T) {
	release := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		MaxActiveOperations: 1,
	})
	_, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = hub.Start(OperationStart{
		ID: "operation-2", RequestDigest: "request-2",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if !service.IsCode(err, service.OperationCapacityExhausted) {
		t.Fatalf("capacity error = %v", err)
	}
	close(release)
}

func TestOperationHubRetainsWorkerCapacityUntilCleanupReturns(t *testing.T) {
	cleanup := make(chan struct{})
	workerStarted := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		MaxActiveOperations: 1,
		MaxBytes:            256,
	})
	first, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			close(workerStarted)
			<-cleanup
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-workerStarted
	if progressErr := first.Progress(strings.Repeat("x", 256)); !service.IsCode(progressErr, service.OperationCapacityExhausted) {
		t.Fatalf("capacity-terminating progress = %v", progressErr)
	}

	_, _, err = hub.Start(OperationStart{
		ID: "operation-2", RequestDigest: "request-2",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if !service.IsCode(err, service.OperationCapacityExhausted) {
		t.Fatalf("new operation while terminal worker cleans up = %v", err)
	}
	close(cleanup)

	deadline := time.Now().Add(time.Second)
	for {
		second, _, startErr := hub.Start(OperationStart{
			ID: "operation-2", RequestDigest: "request-2",
			Run: func(context.Context, *Operation) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			},
		})
		if startErr == nil {
			_ = collectOperationEvents(t, hub, second.ID(), 0)
			break
		}
		if !service.IsCode(startErr, service.OperationCapacityExhausted) || time.Now().After(deadline) {
			t.Fatalf("operation after worker cleanup = %v", startErr)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOperationHubBoundsSubscribersPerOperationAndGlobally(t *testing.T) {
	t.Run("per operation", func(t *testing.T) {
		release := make(chan struct{})
		hub := NewOperationHub(context.Background(), OperationHubOptions{
			MaxSubscribersPerOperation: 1,
			MaxSubscribers:             2,
		})
		operation, _, err := hub.Start(OperationStart{
			RequestDigest: "request-1",
			Run: func(context.Context, *Operation) (json.RawMessage, error) {
				<-release
				return json.RawMessage(`{}`), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		subscription, err := hub.Subscribe(operation.ID(), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer subscription.Close()
		if _, err := hub.Subscribe(operation.ID(), 0); !service.IsCode(err, service.OperationCapacityExhausted) {
			t.Fatalf("second operation subscriber error = %v", err)
		}
		subscription.Close()
		replacement, err := hub.Subscribe(operation.ID(), 0)
		if err != nil {
			t.Fatalf("replacement subscriber after close: %v", err)
		}
		replacement.Close()
		close(release)
	})

	t.Run("global", func(t *testing.T) {
		release := make(chan struct{})
		hub := NewOperationHub(context.Background(), OperationHubOptions{
			MaxSubscribersPerOperation: 2,
			MaxSubscribers:             1,
		})
		operations := make([]*Operation, 0, 2)
		for index := range 2 {
			operation, _, err := hub.Start(OperationStart{
				RequestDigest: fmt.Sprintf("request-%d", index),
				Run: func(context.Context, *Operation) (json.RawMessage, error) {
					<-release
					return json.RawMessage(`{}`), nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			operations = append(operations, operation)
		}
		subscription, err := hub.Subscribe(operations[0].ID(), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer subscription.Close()
		if _, err := hub.Subscribe(operations[1].ID(), 0); !service.IsCode(err, service.OperationCapacityExhausted) {
			t.Fatalf("global subscriber error = %v", err)
		}
		subscription.Close()
		replacement, err := hub.Subscribe(operations[1].ID(), 0)
		if err != nil {
			t.Fatalf("global subscriber after capacity release: %v", err)
		}
		replacement.Close()
		close(release)
	})
}

func TestOperationHubCountsTerminalReplaySubscribersUntilClose(t *testing.T) {
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		MaxSubscribersPerOperation: 1,
		MaxSubscribers:             1,
	})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = collectOperationEvents(t, hub, operation.ID(), 0)

	first, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Subscribe(operation.ID(), 0); !service.IsCode(err, service.OperationCapacityExhausted) {
		t.Fatalf("second terminal subscriber error = %v", err)
	}
	first.Close()
	replacement, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatalf("terminal subscriber after close: %v", err)
	}
	replacement.Close()
}

func TestOperationHubCountsOverflowedSubscriberUntilClose(t *testing.T) {
	release := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		SubscriberQueue:            1,
		MaxSubscribersPerOperation: 1,
		MaxSubscribers:             1,
	})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			<-release
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := operation.Progress("fill subscriber queue"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hub.Subscribe(operation.ID(), 0); !service.IsCode(err, service.OperationCapacityExhausted) {
		t.Fatalf("overflowed subscriber capacity error = %v", err)
	}
	subscription.Close()
	replacement, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatalf("subscriber after overflowed stream closed: %v", err)
	}
	replacement.Close()
	close(release)
}

func TestOperationHubReservesLiveHeadroomAfterReplay(t *testing.T) {
	replayReady := make(chan struct{})
	emitLive := make(chan struct{})
	liveEmitted := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		SubscriberQueue: 1,
	})
	operation, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(_ context.Context, operation *Operation) (json.RawMessage, error) {
			if err := operation.Progress("one"); err != nil {
				return nil, err
			}
			if err := operation.Progress("two"); err != nil {
				return nil, err
			}
			close(replayReady)
			<-emitLive
			if err := operation.Progress("live"); err != nil {
				return nil, err
			}
			close(liveEmitted)
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-replayReady
	subscription, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	close(emitLive)
	<-liveEmitted

	for _, message := range []string{"one", "two", "live"} {
		event := receiveOperationEvent(t, subscription.Events())
		if event.Message != message {
			t.Fatalf("event message = %q, want %q", event.Message, message)
		}
	}
	if event := receiveOperationEvent(t, subscription.Events()); event.Kind != service.OperationEventComplete {
		t.Fatalf("terminal event kind = %q", event.Kind)
	}
}

func TestOperationHubReservesCriticalCapacityAfterLiveQueueSaturates(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(context.Context, *Operation) (json.RawMessage, error)
	}{
		{
			name: "prompt",
			run: func(ctx context.Context, operation *Operation) (json.RawMessage, error) {
				value, err := operation.Prompt(ctx, service.OperationPrompt{
					Kind: "password", Message: "Password:", Sensitive: true,
				})
				if err != nil {
					return nil, err
				}
				return json.Marshal(map[string]string{"value": value})
			},
		},
		{
			name: "completion",
			run: func(context.Context, *Operation) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			startWork := make(chan struct{})
			startCritical := make(chan struct{})
			queueSaturated := make(chan struct{})
			hub := NewOperationHub(context.Background(), OperationHubOptions{SubscriberQueue: 1})
			operation, _, err := hub.Start(OperationStart{
				RequestDigest: "request-1",
				Run: func(ctx context.Context, operation *Operation) (json.RawMessage, error) {
					<-startWork
					if err := operation.Progress("one"); err != nil {
						return nil, err
					}
					if err := operation.Warning("two"); err != nil {
						return nil, err
					}
					close(queueSaturated)
					<-startCritical
					return test.run(ctx, operation)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			subscription, err := hub.Subscribe(operation.ID(), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer subscription.Close()
			close(startWork)
			<-queueSaturated
			receiveOperationEvent(t, subscription.Events())
			select {
			case _, open := <-subscription.Events():
				if open {
					t.Fatal("noncritical saturation must detach before critical delivery")
				}
			case <-time.After(time.Second):
				t.Fatal("noncritical events consumed critical queue capacity")
			}

			replacement, err := hub.Subscribe(operation.ID(), 1)
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			event := receiveOperationEvent(t, replacement.Events())
			if event.Kind != service.OperationEventWarning {
				t.Fatalf("replayed event kind = %q", event.Kind)
			}
			close(startCritical)
			event = receiveOperationEvent(t, replacement.Events())
			if test.name == "prompt" {
				if event.Prompt == nil {
					t.Fatal("prompt event omitted prompt")
				}
				if err := hub.Respond(operation.ID(), service.OperationResponse{
					PromptID: event.Prompt.ID,
					Value:    "secret",
				}); err != nil {
					t.Fatal(err)
				}
				event = receiveOperationEvent(t, replacement.Events())
			}
			if event.Kind != service.OperationEventComplete {
				t.Fatalf("critical event kind = %q", event.Kind)
			}
		})
	}
}

func TestOperationHubCancelsWhenInitialSubscriberGraceExpires(t *testing.T) {
	canceled := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		SubscriberGrace: time.Millisecond,
	})
	_, _, err := hub.Start(OperationStart{
		RequestDigest: "request-1",
		Run: func(ctx context.Context, _ *Operation) (json.RawMessage, error) {
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("operation survived initial subscriber grace")
	}
}

func TestOperationHubReservesDaemonWorkUntilTerminalCompletion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := NewGate(now)
	releaseWorker := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		Reserve: func() (func(), error) {
			return gate.Reserve(ReservationWork, now)
		},
	})
	operation, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			<-releaseWorker
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if active := gate.Snapshot().ActiveWork; active != 1 {
		t.Fatalf("active work during operation = %d", active)
	}
	close(releaseWorker)
	_ = collectOperationEvents(t, hub, operation.ID(), 0)
	if active := gate.Snapshot().ActiveWork; active != 0 {
		t.Fatalf("active work after completion = %d", active)
	}
}

func TestOperationHubDrainRefusesNewWorkAndTerminatesActiveStreams(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := NewGate(now)
	started := make(chan struct{})
	cleanup := make(chan struct{})
	hub := NewOperationHub(context.Background(), OperationHubOptions{
		Reserve: func() (func(), error) {
			return gate.Reserve(ReservationWork, now)
		},
	})
	operation, _, err := hub.Start(OperationStart{
		ID: "operation-1", RequestDigest: "request-1",
		Run: func(ctx context.Context, _ *Operation) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			<-cleanup
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	subscription, err := hub.Subscribe(operation.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	deadline := now.Add(time.Minute)
	gate.BeginDrain(deadline)
	hub.BeginDrain(deadline)
	_, _, err = hub.Start(OperationStart{
		ID: "operation-2", RequestDigest: "request-2",
		Run: func(context.Context, *Operation) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	if !service.IsCode(err, service.DaemonDraining) {
		t.Fatalf("new operation while draining = %v", err)
	}
	hub.CancelActiveForDrain()
	event := receiveOperationEvent(t, subscription.Events())
	if event.Kind != service.OperationEventComplete || event.Failure == nil ||
		event.Failure.Code != service.OperationOutcomeUnknown || event.Failure.Retryable {
		t.Fatalf("unexpected drain event: %#v", event)
	}
	if active := gate.Snapshot().ActiveWork; active != 1 {
		t.Fatalf("active work before worker cleanup = %d", active)
	}
	close(cleanup)
	deadline = time.Now().Add(time.Second)
	for gate.Snapshot().ActiveWork != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active := gate.Snapshot().ActiveWork; active != 0 {
		t.Fatalf("active work after worker cleanup = %d", active)
	}
}

func collectOperationEvents(
	t *testing.T,
	hub *OperationHub,
	operationID string,
	after uint64,
) []service.OperationEvent {
	t.Helper()
	subscription, err := hub.Subscribe(operationID, after)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	var events []service.OperationEvent
	for retained := range subscription.Events() {
		var event service.OperationEvent
		if err := json.Unmarshal([]byte(retained.encoded), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func receiveOperationEvent(
	t *testing.T,
	events <-chan retainedOperationEvent,
) service.OperationEvent {
	t.Helper()
	select {
	case retained, ok := <-events:
		if !ok {
			t.Fatal("operation event stream closed")
		}
		var event service.OperationEvent
		if err := json.Unmarshal([]byte(retained.encoded), &event); err != nil {
			t.Fatal(err)
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for operation event")
		return service.OperationEvent{}
	}
}
