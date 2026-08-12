package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"go.kenn.io/kwt/service"
)

const operationStreamResponseLimit int64 = defaultMaxOperationBytes + defaultMaxOperationEvents + 1

var errOperationStreamInterrupted = errors.New("operation stream interrupted")

type OperationCallbacks struct {
	Event  func(service.OperationEvent) error
	Prompt func(context.Context, service.OperationPrompt) (string, error)
}

func (c *Client) FollowOperation(
	ctx context.Context,
	operationID string,
	afterSequence uint64,
	callbacks OperationCallbacks,
) (json.RawMessage, error) {
	if operationID == "" {
		return nil, service.NewError(
			service.InvalidRequest,
			"operation ID is empty",
			false,
			nil,
			nil,
		)
	}
	sequence := afterSequence
	for attempt := 0; attempt < 2; attempt++ {
		result, lastSequence, err := c.followOperationAttempt(
			ctx,
			operationID,
			sequence,
			callbacks,
		)
		if err == nil {
			return result, nil
		}
		sequence = lastSequence
		if !errors.Is(err, errOperationStreamInterrupted) {
			return nil, err
		}
		if ctx.Err() != nil || attempt == 1 {
			return nil, operationOutcomeUnknown("operation stream outcome is unknown", err)
		}
	}
	return nil, operationOutcomeUnknown("operation stream outcome is unknown", nil)
}

func (c *Client) followOperationAttempt(
	ctx context.Context,
	operationID string,
	afterSequence uint64,
	callbacks OperationCallbacks,
) (json.RawMessage, uint64, error) {
	path := operationRoutePrefix + url.PathEscape(operationID) + "/events?after_sequence=" +
		strconv.FormatUint(afterSequence, 10)
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.endpoint.BaseURL()+path,
		nil,
	)
	if err != nil {
		return nil, afterSequence, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.streamHTTP.Do(request)
	if err != nil {
		return nil, afterSequence, errors.Join(errOperationStreamInterrupted, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		encoded, readErr := readResponse(response.Body, controlResponseLimit, path)
		if readErr != nil {
			return nil, afterSequence, errors.Join(errOperationStreamInterrupted, readErr)
		}
		problemErr := decodeProblem(response.StatusCode, bytes.NewReader(encoded))
		if service.IsCode(problemErr, service.NotFound) ||
			service.IsCode(problemErr, service.OperationOutcomeUnknown) {
			return nil, afterSequence, operationOutcomeUnknown(
				"operation is no longer available",
				problemErr,
			)
		}
		return nil, afterSequence, problemErr
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-ndjson" {
		return nil, afterSequence, operationOutcomeUnknown(
			"daemon returned an invalid operation stream",
			err,
		)
	}
	validator, err := service.NewOperationStreamValidator(operationID, afterSequence)
	if err != nil {
		return nil, afterSequence, err
	}
	limited := &io.LimitedReader{R: response.Body, N: operationStreamResponseLimit + 1}
	decoder := json.NewDecoder(limited)
	sequence := afterSequence
	for {
		var event service.OperationEvent
		if err := decoder.Decode(&event); err != nil {
			if limited.N == 0 {
				return nil, sequence, operationOutcomeUnknown(
					"operation stream response is too large",
					ErrResponseTooLarge,
				)
			}
			if errors.Is(err, io.EOF) {
				return nil, sequence, errOperationStreamInterrupted
			}
			return nil, sequence, errors.Join(errOperationStreamInterrupted, err)
		}
		if err := validator.Accept(event); err != nil {
			return nil, sequence, operationOutcomeUnknown(
				"daemon returned an invalid operation sequence",
				err,
			)
		}
		sequence = event.Sequence
		if callbacks.Event != nil {
			if err := callbacks.Event(event); err != nil {
				return nil, sequence, err
			}
		}
		switch event.Kind {
		case service.OperationEventPrompt:
			if callbacks.Prompt == nil {
				return nil, sequence, service.NewError(
					service.InteractionRequired,
					"operation requires interactive input",
					false,
					nil,
					nil,
				)
			}
			value, err := callbacks.Prompt(ctx, *event.Prompt)
			if err != nil {
				return nil, sequence, err
			}
			if err := c.sendOperationResponse(ctx, operationID, service.OperationResponse{
				PromptID: event.Prompt.ID,
				Value:    value,
			}); err != nil {
				return nil, sequence, operationOutcomeUnknown(
					"operation prompt response outcome is unknown",
					err,
				)
			}
		case service.OperationEventComplete:
			if event.Failure != nil {
				return nil, sequence, service.NewDescriptorError(*event.Failure, nil)
			}
			return event.Result, sequence, nil
		}
	}
}

func (c *Client) sendOperationResponse(
	ctx context.Context,
	operationID string,
	response service.OperationResponse,
) error {
	return c.doWith(
		ctx,
		c.streamHTTP,
		controlResponseLimit,
		http.MethodPost,
		operationRoutePrefix+url.PathEscape(operationID)+"/responses",
		response,
		nil,
	)
}

func (c *Client) CancelOperation(ctx context.Context, operationID string) error {
	if operationID == "" {
		return service.NewError(service.InvalidRequest, "operation ID is empty", false, nil, nil)
	}
	return c.doWith(
		ctx,
		c.controlHTTP,
		controlResponseLimit,
		http.MethodDelete,
		operationRoutePrefix+url.PathEscape(operationID),
		nil,
		nil,
	)
}

func operationOutcomeUnknown(message string, cause error) error {
	return service.NewError(
		service.OperationOutcomeUnknown,
		message,
		true,
		nil,
		cause,
	)
}
