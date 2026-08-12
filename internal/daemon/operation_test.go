package daemon

import (
	"context"
	"encoding/json"
	"errors"
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

func TestOperationHubTerminatesAtEventCapacity(t *testing.T) {
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
	if len(events) != 2 || events[1].Failure == nil || events[1].Failure.Code != service.OperationCapacityExhausted {
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
	for event := range subscription.Events() {
		events = append(events, event)
	}
	return events
}

func receiveOperationEvent(
	t *testing.T,
	events <-chan service.OperationEvent,
) service.OperationEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("operation event stream closed")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for operation event")
		return service.OperationEvent{}
	}
}
