package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kwt/service"
)

const operationRoutePrefix = "/api/v1/operations/"
const operationEventWriteTimeout = 5 * time.Second

func registerOperationRoutes(
	mux *http.ServeMux,
	hub *OperationHub,
	opts ServerOptions,
) {
	if hub == nil {
		return
	}
	mux.HandleFunc(operationRoutePrefix, func(w http.ResponseWriter, r *http.Request) {
		serveOperationRoute(w, r, hub, opts)
	})
}

func serveOperationRoute(
	w http.ResponseWriter,
	r *http.Request,
	hub *OperationHub,
	opts ServerOptions,
) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, operationRoutePrefix), "/")
	switch {
	case len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete:
		if err := hub.Cancel(parts[0]); err != nil {
			writeProblem(w, *reportProblem(opts, r.URL.Path, err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[0] != "" && parts[1] == "events" && r.Method == http.MethodGet:
		serveOperationEvents(w, r, hub, opts, parts[0])
	case len(parts) == 2 && parts[0] != "" && parts[1] == "responses" && r.Method == http.MethodPost:
		serveOperationResponse(w, r, hub, opts, parts[0])
	default:
		writeProblem(w, newProblem(http.StatusMethodNotAllowed, service.Descriptor{
			Code: service.InvalidRequest, Message: "unsupported operation stream request",
		}))
	}
}

func serveOperationEvents(
	w http.ResponseWriter,
	r *http.Request,
	hub *OperationHub,
	opts ServerOptions,
	operationID string,
) {
	afterSequence := uint64(0)
	if raw := r.URL.Query().Get("after_sequence"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeProblem(w, newProblem(http.StatusBadRequest, service.Descriptor{
				Code: service.InvalidRequest, Message: "after_sequence must be an unsigned integer",
			}))
			return
		}
		afterSequence = parsed
	}
	subscription, err := hub.Subscribe(operationID, afterSequence)
	if err != nil {
		writeProblem(w, *reportProblem(opts, r.URL.Path, err))
		return
	}
	defer subscription.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, newProblem(http.StatusInternalServerError, service.Descriptor{
			Code: service.Internal, Message: "internal failure",
		}))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := withOperationWriteDeadline(w, func() error {
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		return nil
	}); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-subscription.Events():
			if !open {
				return
			}
			if err := withOperationWriteDeadline(w, func() error {
				if _, err := io.WriteString(w, event.encoded); err != nil {
					return err
				}
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
				flusher.Flush()
				return nil
			}); err != nil {
				return
			}
		}
	}
}

func withOperationWriteDeadline(w http.ResponseWriter, write func() error) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(operationEventWriteTimeout)); err != nil {
		return err
	}
	writeErr := write()
	clearErr := controller.SetWriteDeadline(time.Time{})
	return errors.Join(writeErr, clearErr)
}

func serveOperationResponse(
	w http.ResponseWriter,
	r *http.Request,
	hub *OperationHub,
	opts ServerOptions,
	operationID string,
) {
	var response service.OperationResponse
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		writeProblem(w, newProblem(http.StatusBadRequest, service.Descriptor{
			Code: service.InvalidRequest, Message: "decode operation response: " + err.Error(),
		}))
		return
	}
	if err := requireJSONEnd(decoder); err != nil {
		writeProblem(w, newProblem(http.StatusBadRequest, service.Descriptor{
			Code: service.InvalidRequest, Message: "operation response must contain one JSON value",
		}))
		return
	}
	if err := hub.Respond(operationID, response); err != nil {
		writeProblem(w, *reportProblem(opts, r.URL.Path, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func requireJSONEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected additional JSON value")
	}
	return err
}
