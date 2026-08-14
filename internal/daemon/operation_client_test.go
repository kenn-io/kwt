package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kwt/service"
)

func TestOperationClientResumesInterruptedStreamFromLastSequence(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/x-ndjson")
		sequence := requests.Add(1)
		switch sequence {
		case 1:
			assert.Equal(t, "0", r.URL.Query().Get("after_sequence"))
			_ = json.NewEncoder(w).Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 1,
				Kind: service.OperationEventProgress, Message: "started",
			})
		case 2:
			assert.Equal(t, "1", r.URL.Query().Get("after_sequence"))
			_ = json.NewEncoder(w).Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 2,
				Kind: service.OperationEventComplete, Result: json.RawMessage(`{"status":"ready"}`),
			})
		default:
			t.Fatalf("unexpected stream request %d", sequence)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	var messages []string
	result, err := client.FollowOperation(
		context.Background(),
		"operation-1",
		0,
		OperationCallbacks{Event: func(event service.OperationEvent) error {
			if event.Message != "" {
				messages = append(messages, event.Message)
			}
			return nil
		}},
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"ready"}`, string(result))
	assert.Equal(t, []string{"started"}, messages)
	assert.Equal(t, int32(2), requests.Load())
}

func TestOperationClientHandlesMultipleBoundPromptRounds(t *testing.T) {
	responses := make(chan service.OperationResponse, 2)
	answered := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			flusher := w.(http.Flusher)
			encoder := json.NewEncoder(w)
			for sequence, promptID := range []string{"prompt-1", "prompt-2"} {
				_ = encoder.Encode(service.OperationEvent{
					OperationID: "operation-1", Sequence: uint64(sequence + 1),
					Kind: service.OperationEventPrompt,
					Prompt: &service.OperationPrompt{
						ID: promptID, Kind: "password", Message: "Password:", Sensitive: true,
					},
				})
				flusher.Flush()
				<-answered
			}
			_ = encoder.Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 3,
				Kind: service.OperationEventComplete, Result: json.RawMessage(`{}`),
			})
			flusher.Flush()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses"):
			var response service.OperationResponse
			require.NoError(t, json.NewDecoder(r.Body).Decode(&response))
			responses <- response
			answered <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	var promptIDs []string
	_, err := client.FollowOperation(
		context.Background(), "operation-1", 0,
		OperationCallbacks{Prompt: func(
			_ context.Context,
			prompt service.OperationPrompt,
		) (string, error) {
			promptIDs = append(promptIDs, prompt.ID)
			if prompt.ID == "prompt-2" {
				return "", nil
			}
			return "wrong", nil
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"prompt-1", "prompt-2"}, promptIDs)
	first := <-responses
	second := <-responses
	assert.Equal(t, service.OperationResponse{PromptID: "prompt-1", Value: "wrong"}, first)
	assert.Equal(t, service.OperationResponse{PromptID: "prompt-2"}, second)
}

func TestOperationClientWaitsForTerminalFailureAfterPromptDeadline(t *testing.T) {
	deadline := time.Now().Add(30 * time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher := w.(http.Flusher)
		encoder := json.NewEncoder(w)
		_ = encoder.Encode(service.OperationEvent{
			OperationID: "operation-1", Sequence: 1,
			Kind: service.OperationEventPrompt,
			Prompt: &service.OperationPrompt{
				ID: "prompt-1", Kind: "password", Message: "Password:",
				Sensitive: true, Deadline: &deadline,
			},
		})
		flusher.Flush()
		time.Sleep(50 * time.Millisecond)
		_ = encoder.Encode(service.OperationEvent{
			OperationID: "operation-1", Sequence: 2,
			Kind: service.OperationEventComplete,
			Failure: &service.Descriptor{
				Code: service.SSHPromptTimedOut, Message: "SSH prompt timed out",
			},
		})
		flusher.Flush()
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	var observedDeadline time.Time

	_, err := client.FollowOperation(
		context.Background(), "operation-1", 0,
		OperationCallbacks{Prompt: func(ctx context.Context, _ service.OperationPrompt) (string, error) {
			observedDeadline, _ = ctx.Deadline()
			<-ctx.Done()
			return "", ctx.Err()
		}},
	)
	assert.True(t, service.IsCode(err, service.SSHPromptTimedOut), err)
	assert.WithinDuration(t, deadline, observedDeadline, time.Millisecond)
}

func TestOperationClientWaitsForTerminalFailureWhenPromptExpiresDuringResponse(t *testing.T) {
	deadline := time.Now().Add(500 * time.Millisecond)
	responseStarted := make(chan struct{})
	responseFinished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			flusher := w.(http.Flusher)
			encoder := json.NewEncoder(w)
			_ = encoder.Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 1,
				Kind: service.OperationEventPrompt,
				Prompt: &service.OperationPrompt{
					ID: "prompt-1", Kind: "password", Message: "Password:",
					Sensitive: true, Deadline: &deadline,
				},
			})
			flusher.Flush()
			<-responseFinished
			_ = encoder.Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 2,
				Kind: service.OperationEventComplete,
				Failure: &service.Descriptor{
					Code: service.SSHPromptTimedOut, Message: "SSH prompt timed out",
				},
			})
			flusher.Flush()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses"):
			close(responseStarted)
			time.Sleep(time.Until(deadline) + 10*time.Millisecond)
			writeProblem(w, newProblem(http.StatusConflict, service.Descriptor{
				Code: service.Conflict, Message: "operation prompt is no longer current",
			}))
			close(responseFinished)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	_, err := client.FollowOperation(
		context.Background(), "operation-1", 0,
		OperationCallbacks{Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "secret", nil
		}},
	)
	assert.True(t, service.IsCode(err, service.SSHPromptTimedOut), err)
	select {
	case <-responseStarted:
	default:
		require.Fail(t, "prompt response was not submitted")
	}
}

func TestOperationClientReconnectReplaysUnansweredPrompt(t *testing.T) {
	var streamRequests atomic.Int32
	var promptCalls atomic.Int32
	answered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			request := streamRequests.Add(1)
			if after := r.URL.Query().Get("after_sequence"); after != "0" {
				assert.Equal(t, "0", after)
				http.Error(w, "unexpected cursor", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			encoder := json.NewEncoder(w)
			_ = encoder.Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 1,
				Kind: service.OperationEventPrompt,
				Prompt: &service.OperationPrompt{
					ID: "prompt-1", Kind: "password", Message: "Password:", Sensitive: true,
				},
			})
			w.(http.Flusher).Flush()
			if request == 1 {
				return
			}
			select {
			case <-answered:
			case <-r.Context().Done():
				return
			}
			_ = encoder.Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 2,
				Kind: service.OperationEventComplete, Result: json.RawMessage(`{}`),
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses"):
			close(answered)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	_, err := client.FollowOperation(
		context.Background(), "operation-1", 0,
		OperationCallbacks{Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			if promptCalls.Add(1) == 1 {
				return "", errOperationStreamInterrupted
			}
			return "secret", nil
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, int32(2), streamRequests.Load())
	assert.Equal(t, int32(2), promptCalls.Load())
}

func TestOperationClientRejectsInvalidSequenceAsUnknownOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(service.OperationEvent{
			OperationID: "operation-1", Sequence: 2,
			Kind: service.OperationEventComplete, Result: json.RawMessage(`{}`),
		})
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
}

func TestOperationClientPreservesTerminalServiceFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(service.OperationEvent{
			OperationID: "operation-1", Sequence: 1,
			Kind: service.OperationEventComplete,
			Failure: &service.Descriptor{
				Code: service.Conflict, Message: "target changed", Retryable: true,
			},
		})
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.Conflict), err)
}

func TestOperationClientCancelsAfterPreterminalEventCallbackFailure(t *testing.T) {
	callbackErr := errors.New("progress writer failed")
	var cancellations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 1,
				Kind: service.OperationEventProgress, Message: "started",
			})
		case http.MethodDelete:
			cancellations.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	_, err := client.FollowOperation(
		context.Background(), "operation-1", 0,
		OperationCallbacks{Event: func(service.OperationEvent) error { return callbackErr }},
	)
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
	assert.ErrorIs(t, err, callbackErr)
	assert.Equal(t, int32(1), cancellations.Load())
}

func TestOperationClientTerminalResultSurvivesEventCallbackFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(service.OperationEvent{
			OperationID: "operation-1", Sequence: 1,
			Kind: service.OperationEventComplete, Result: json.RawMessage(`{"status":"ready"}`),
		})
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	callbackErr := errors.New("completion writer failed")
	result, err := client.FollowOperation(
		context.Background(), "operation-1", 0,
		OperationCallbacks{Event: func(service.OperationEvent) error {
			return callbackErr
		}},
	)
	assert.ErrorIs(t, err, callbackErr)
	assert.JSONEq(t, `{"status":"ready"}`, string(result))
}

func TestOperationClientCancelsAfterPromptHandlerFailure(t *testing.T) {
	promptErr := errors.New("prompt input failed")
	var cancellations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(service.OperationEvent{
				OperationID: "operation-1", Sequence: 1,
				Kind: service.OperationEventPrompt,
				Prompt: &service.OperationPrompt{
					ID: "prompt-1", Kind: "password", Message: "Password:", Sensitive: true,
				},
			})
		case http.MethodDelete:
			cancellations.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	_, err := client.FollowOperation(
		context.Background(), "operation-1", 0,
		OperationCallbacks{Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "", promptErr
		}},
	)
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
	assert.ErrorIs(t, err, promptErr)
	assert.Equal(t, int32(1), cancellations.Load())
}

func TestOperationClientPreservesInteractionRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(service.OperationEvent{
			OperationID: "operation-1", Sequence: 1,
			Kind: service.OperationEventPrompt,
			Prompt: &service.OperationPrompt{
				ID: "prompt-1", Kind: "password", Message: "Password:", Sensitive: true,
			},
		})
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.InteractionRequired), err)
}

func TestOperationClientBoundsStreamResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"operation_id":"operation-1","sequence":1,"kind":"progress","message":"`)
		_, _ = io.WriteString(w, strings.Repeat("x", int(operationStreamResponseLimit)))
		_, _ = io.WriteString(w, `"}`+"\n")
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
	assert.ErrorIs(t, err, ErrResponseTooLarge)
}

func TestOperationClientMapsEndpointLossToUnknownOutcome(t *testing.T) {
	client := newClient(
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
		"secret",
		&http.Client{Transport: failingRoundTripper{}},
	)
	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
	assert.False(t, service.AsError(err).Retryable)
}

func TestVerifiedOperationClientBoundsStreamHeaderWait(t *testing.T) {
	t.Parallel()
	token := "test-secret"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	record := runtimeRecordForServer(t, server, token)
	proof, err := kitdaemon.NewProof([]byte(token))
	require.NoError(t, err)
	ping, err := proof.NewPingHandler(record)
	require.NoError(t, err)
	mux.Handle("/api/ping", ping)
	mux.HandleFunc("/api/v1/operations/operation-1/events", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	client, err := NewVerifiedClient(context.Background(), record, token)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, _, err = client.followOperationAttempt(ctx, "operation-1", 0, OperationCallbacks{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errOperationStreamInterrupted)
	assert.NoError(t, ctx.Err(), "stream header wait reached the caller deadline")
}

func TestVerifiedOperationClientBoundsPromptResponse(t *testing.T) {
	t.Parallel()
	token := "test-secret"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	record := runtimeRecordForServer(t, server, token)
	proof, err := kitdaemon.NewProof([]byte(token))
	require.NoError(t, err)
	ping, err := proof.NewPingHandler(record)
	require.NoError(t, err)
	mux.Handle("/api/ping", ping)
	mux.HandleFunc("/api/v1/operations/operation-1/responses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "{")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	client, err := NewVerifiedClient(context.Background(), record, token)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = client.sendOperationResponse(ctx, "operation-1", service.OperationResponse{PromptID: "prompt-1"})
	require.Error(t, err)
	assert.NoError(t, ctx.Err(), "prompt response reached the caller deadline")
}

func TestOperationClientMapsReplacementAuthenticationFailureToUnknownOutcome(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeProblem(w, newProblem(http.StatusUnauthorized, service.Descriptor{
			Code: service.PermissionDenied, Message: "a valid daemon bearer token is required",
		}))
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "stale-secret")

	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
	assert.Equal(t, int32(1), requests.Load())
}

func TestOperationClientRetriesMalformedFailureThenReportsUnknownOutcome(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"code":`)
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
	assert.Equal(t, int32(2), requests.Load())
}

func TestOperationClientRetriesSubscriptionFailureThenReportsUnknownOutcome(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/operations/operation-1/events", r.URL.Path)
		requests.Add(1)
		writeProblem(w, newProblem(http.StatusServiceUnavailable, service.Descriptor{
			Code:      service.OperationCapacityExhausted,
			Message:   "operation subscriber capacity is exhausted",
			Retryable: true,
		}))
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")

	_, err := client.FollowOperation(context.Background(), "operation-1", 0, OperationCallbacks{})
	assert.True(t, service.IsCode(err, service.OperationOutcomeUnknown), err)
	assert.False(t, service.IsCode(err, service.OperationCapacityExhausted), err)
	assert.Equal(t, int32(2), requests.Load())
}

func TestOperationClientCancelsOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/v1/operations/operation-1", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := clientForUnverifiedServer(t, server, "secret")
	require.NoError(t, client.CancelOperation(context.Background(), "operation-1"))
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("endpoint lost")
}
