package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/kwt/service"
)

const (
	defaultMaxOperationEvents       = 256
	defaultMaxOperationBytes        = 1 << 20
	defaultMaxActiveOperations      = 128
	defaultMaxCompletedOperations   = 128
	defaultOperationRetention       = 5 * time.Minute
	defaultOperationSubscriberGrace = 5 * time.Second
	defaultOperationSubscriberQueue = 32
	operationCompletionReserveBytes = 16 << 10
)

type OperationHubOptions struct {
	Now                    func() time.Time
	IDSource               func() (string, error)
	MaxEvents              int
	MaxBytes               int
	MaxActiveOperations    int
	MaxCompletedOperations int
	CompletedRetention     time.Duration
	SubscriberGrace        time.Duration
	SubscriberQueue        int
}

type OperationStart struct {
	ID            string
	RequestDigest string
	Run           func(context.Context, *Operation) (json.RawMessage, error)
}

type OperationHub struct {
	context context.Context
	options OperationHubOptions

	mu         sync.Mutex
	operations map[string]*operationEntry
	active     int
}

type operationEntry struct {
	id             string
	requestDigest  string
	operation      *Operation
	context        context.Context
	cancel         context.CancelFunc
	validator      *service.OperationStreamValidator
	events         []service.OperationEvent
	eventBytes     int
	terminal       bool
	completedAt    time.Time
	prompt         *operationPromptState
	subscribers    map[*OperationSubscription]chan service.OperationEvent
	everSubscribed bool
	lossTimer      *time.Timer
}

type operationPromptState struct {
	id        string
	responses chan service.OperationResponse
	responded bool
}

type Operation struct {
	hub   *OperationHub
	entry *operationEntry
}

type OperationSubscription struct {
	hub    *OperationHub
	entry  *operationEntry
	events <-chan service.OperationEvent
	once   sync.Once
}

func NewOperationHub(ctx context.Context, options OperationHubOptions) *OperationHub {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.IDSource == nil {
		options.IDSource = randomOperationID
	}
	if options.MaxEvents <= 0 {
		options.MaxEvents = defaultMaxOperationEvents
	}
	if options.MaxEvents < 2 {
		options.MaxEvents = 2
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultMaxOperationBytes
	}
	if options.MaxActiveOperations <= 0 {
		options.MaxActiveOperations = defaultMaxActiveOperations
	}
	if options.MaxCompletedOperations <= 0 {
		options.MaxCompletedOperations = defaultMaxCompletedOperations
	}
	if options.CompletedRetention <= 0 {
		options.CompletedRetention = defaultOperationRetention
	}
	if options.SubscriberGrace <= 0 {
		options.SubscriberGrace = defaultOperationSubscriberGrace
	}
	if options.SubscriberQueue <= 0 {
		options.SubscriberQueue = defaultOperationSubscriberQueue
	}
	return &OperationHub{
		context:    ctx,
		options:    options,
		operations: make(map[string]*operationEntry),
	}
}

func (h *OperationHub) Start(start OperationStart) (*Operation, bool, error) {
	if start.RequestDigest == "" {
		return nil, false, service.NewError(
			service.InvalidRequest,
			"operation request digest is empty",
			false,
			nil,
			nil,
		)
	}
	for attempts := 0; attempts < 4; attempts++ {
		id := start.ID
		generatedID := id == ""
		if generatedID {
			var err error
			id, err = h.options.IDSource()
			if err != nil {
				return nil, false, fmt.Errorf("generate operation ID: %w", err)
			}
			if id == "" {
				return nil, false, errors.New("generated operation ID is empty")
			}
		}

		h.mu.Lock()
		h.pruneLocked()
		if existing := h.operations[id]; existing != nil {
			if generatedID {
				h.mu.Unlock()
				continue
			}
			if existing.requestDigest != start.RequestDigest {
				h.mu.Unlock()
				return nil, false, service.NewError(
					service.OperationIDConflict,
					"operation ID belongs to a different request",
					false,
					nil,
					nil,
				)
			}
			h.mu.Unlock()
			return existing.operation, false, nil
		}
		if start.Run == nil {
			h.mu.Unlock()
			return nil, false, service.NewError(
				service.InvalidRequest,
				"new operation has no worker",
				false,
				nil,
				nil,
			)
		}
		if h.active >= h.options.MaxActiveOperations {
			h.mu.Unlock()
			return nil, false, service.NewError(
				service.OperationCapacityExhausted,
				"daemon operation capacity is exhausted",
				true,
				nil,
				nil,
			)
		}
		workerContext, cancel := context.WithCancel(h.context)
		validator, err := service.NewOperationStreamValidator(id, 0)
		if err != nil {
			cancel()
			h.mu.Unlock()
			return nil, false, err
		}
		entry := &operationEntry{
			id:            id,
			requestDigest: start.RequestDigest,
			context:       workerContext,
			cancel:        cancel,
			validator:     validator,
			subscribers:   make(map[*OperationSubscription]chan service.OperationEvent),
		}
		entry.operation = &Operation{hub: h, entry: entry}
		h.operations[id] = entry
		h.active++
		h.mu.Unlock()

		go h.run(entry, start.Run)
		return entry.operation, true, nil
	}
	return nil, false, errors.New("generate unique operation ID")
}

func (h *OperationHub) run(
	entry *operationEntry,
	run func(context.Context, *Operation) (json.RawMessage, error),
) {
	result, err := run(entry.context, entry.operation)
	h.finish(entry, result, err)
}

func (o *Operation) ID() string {
	return o.entry.id
}

func (o *Operation) Progress(message string) error {
	return o.emit(service.OperationEvent{Kind: service.OperationEventProgress, Message: message})
}

func (o *Operation) Warning(message string) error {
	return o.emit(service.OperationEvent{Kind: service.OperationEventWarning, Message: message})
}

func (o *Operation) emit(event service.OperationEvent) error {
	o.hub.mu.Lock()
	defer o.hub.mu.Unlock()
	if o.entry.terminal {
		return service.NewError(
			service.Conflict,
			"operation is already complete",
			false,
			nil,
			nil,
		)
	}
	return o.hub.appendLocked(o.entry, event)
}

func (o *Operation) Prompt(
	ctx context.Context,
	prompt service.OperationPrompt,
) (string, error) {
	promptID, err := o.hub.options.IDSource()
	if err != nil {
		return "", fmt.Errorf("generate prompt ID: %w", err)
	}
	if promptID == "" {
		return "", errors.New("generated prompt ID is empty")
	}
	prompt.ID = promptID
	state := &operationPromptState{
		id:        promptID,
		responses: make(chan service.OperationResponse, 1),
	}

	o.hub.mu.Lock()
	if o.entry.terminal {
		o.hub.mu.Unlock()
		return "", service.NewError(service.Conflict, "operation is already complete", false, nil, nil)
	}
	if o.entry.prompt != nil {
		o.hub.mu.Unlock()
		return "", service.NewError(service.Conflict, "operation already has an active prompt", false, nil, nil)
	}
	o.entry.prompt = state
	err = o.hub.appendLocked(o.entry, service.OperationEvent{
		Kind:   service.OperationEventPrompt,
		Prompt: &prompt,
	})
	if err != nil {
		if o.entry.prompt == state {
			o.entry.prompt = nil
		}
		o.hub.mu.Unlock()
		return "", err
	}
	o.hub.mu.Unlock()

	defer func() {
		o.hub.mu.Lock()
		if o.entry.prompt == state {
			o.entry.prompt = nil
		}
		o.hub.mu.Unlock()
	}()

	select {
	case response := <-state.responses:
		return response.Value, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-o.entry.context.Done():
		return "", o.entry.context.Err()
	}
}

func (h *OperationHub) Respond(
	operationID string,
	response service.OperationResponse,
) error {
	if err := service.ValidateOperationResponse(response); err != nil {
		return service.NewError(service.InvalidRequest, err.Error(), false, nil, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.operations[operationID]
	if entry == nil {
		return service.NewError(service.NotFound, "operation was not found", false, nil, nil)
	}
	if entry.terminal {
		return service.NewError(service.Conflict, "operation is already complete", false, nil, nil)
	}
	if entry.prompt == nil || entry.prompt.id != response.PromptID {
		return service.NewError(service.Conflict, "operation prompt is no longer current", false, nil, nil)
	}
	if entry.prompt.responded {
		return service.NewError(service.Conflict, "operation prompt already has a response", false, nil, nil)
	}
	entry.prompt.responded = true
	entry.prompt.responses <- response
	return nil
}

func (h *OperationHub) Cancel(operationID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.operations[operationID]
	if entry == nil {
		return service.NewError(service.NotFound, "operation was not found", false, nil, nil)
	}
	if !entry.terminal {
		entry.cancel()
	}
	return nil
}

func (h *OperationHub) Subscribe(
	operationID string,
	afterSequence uint64,
) (*OperationSubscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked()
	entry := h.operations[operationID]
	if entry == nil {
		return nil, service.NewError(service.NotFound, "operation was not found", false, nil, nil)
	}
	lastSequence := uint64(0)
	if len(entry.events) > 0 {
		lastSequence = entry.events[len(entry.events)-1].Sequence
	}
	if afterSequence > lastSequence {
		return nil, service.NewError(
			service.InvalidRequest,
			"operation cursor is beyond the retained stream",
			false,
			nil,
			nil,
		)
	}
	if len(entry.events) > 0 && afterSequence+1 < entry.events[0].Sequence {
		return nil, service.NewError(
			service.OperationOutcomeUnknown,
			"operation events are no longer retained",
			true,
			nil,
			nil,
		)
	}
	replay := make([]service.OperationEvent, 0, len(entry.events))
	for _, event := range entry.events {
		if event.Sequence > afterSequence {
			replay = append(replay, event)
		}
	}
	queueSize := h.options.SubscriberQueue
	if len(replay) > queueSize {
		queueSize = len(replay)
	}
	events := make(chan service.OperationEvent, queueSize)
	for _, event := range replay {
		events <- event
	}
	subscription := &OperationSubscription{
		hub: h, entry: entry, events: events,
	}
	entry.everSubscribed = true
	if entry.lossTimer != nil {
		entry.lossTimer.Stop()
		entry.lossTimer = nil
	}
	if entry.terminal {
		close(events)
	} else {
		entry.subscribers[subscription] = events
	}
	return subscription, nil
}

func (s *OperationSubscription) Events() <-chan service.OperationEvent {
	return s.events
}

func (s *OperationSubscription) Close() {
	s.once.Do(func() {
		s.hub.removeSubscriber(s)
	})
}

func (h *OperationHub) removeSubscriber(subscription *OperationSubscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := subscription.entry
	events, ok := entry.subscribers[subscription]
	if !ok {
		return
	}
	delete(entry.subscribers, subscription)
	close(events)
	h.scheduleSubscriberLossLocked(entry)
}

func (h *OperationHub) scheduleSubscriberLossLocked(entry *operationEntry) {
	if entry.terminal || !entry.everSubscribed || len(entry.subscribers) != 0 || entry.lossTimer != nil {
		return
	}
	entry.lossTimer = time.AfterFunc(h.options.SubscriberGrace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.operations[entry.id] == entry && !entry.terminal && len(entry.subscribers) == 0 {
			entry.cancel()
		}
		entry.lossTimer = nil
	})
}

func (h *OperationHub) appendLocked(
	entry *operationEntry,
	event service.OperationEvent,
) error {
	event.OperationID = entry.id
	event.Sequence = uint64(len(entry.events) + 1)
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode operation event: %w", err)
	}
	if event.Kind != service.OperationEventComplete && (len(entry.events) >= h.options.MaxEvents-1 ||
		entry.eventBytes+len(encoded) > h.nonterminalByteLimit()) {
		h.terminateCapacityLocked(entry)
		return service.NewError(
			service.OperationCapacityExhausted,
			"operation event capacity is exhausted",
			true,
			nil,
			nil,
		)
	}
	if event.Kind == service.OperationEventComplete && (len(entry.events) >= h.options.MaxEvents ||
		entry.eventBytes+len(encoded) > h.options.MaxBytes) {
		h.terminateCapacityLocked(entry)
		return service.NewError(
			service.OperationCapacityExhausted,
			"operation completion exceeds event capacity",
			true,
			nil,
			nil,
		)
	}
	if err := entry.validator.Accept(event); err != nil {
		return fmt.Errorf("validate operation event: %w", err)
	}
	entry.events = append(entry.events, event)
	entry.eventBytes += len(encoded)
	h.dispatchLocked(entry, event)
	return nil
}

func (h *OperationHub) nonterminalByteLimit() int {
	reserve := operationCompletionReserveBytes
	if reserve > h.options.MaxBytes/2 {
		reserve = h.options.MaxBytes / 2
	}
	return h.options.MaxBytes - reserve
}

func (h *OperationHub) terminateCapacityLocked(entry *operationEntry) {
	if entry.terminal {
		return
	}
	failure := service.Descriptor{
		Code:      service.OperationCapacityExhausted,
		Message:   "operation event capacity is exhausted",
		Retryable: true,
	}
	event := service.OperationEvent{
		OperationID: entry.id,
		Sequence:    uint64(len(entry.events) + 1),
		Kind:        service.OperationEventComplete,
		Failure:     &failure,
	}
	encoded, err := json.Marshal(event)
	if err == nil && len(entry.events) < h.options.MaxEvents && entry.eventBytes+len(encoded) <= h.options.MaxBytes {
		if entry.validator.Accept(event) == nil {
			entry.events = append(entry.events, event)
			entry.eventBytes += len(encoded)
			h.dispatchLocked(entry, event)
		}
	}
	h.markTerminalLocked(entry)
}

func (h *OperationHub) finish(
	entry *operationEntry,
	result json.RawMessage,
	err error,
) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry.terminal {
		return
	}
	if err == nil && result == nil {
		result = json.RawMessage(`{}`)
	}
	event := service.OperationEvent{Kind: service.OperationEventComplete, Result: result}
	if err != nil {
		failure := operationFailure(err)
		event.Result = nil
		event.Failure = &failure
	}
	if appendErr := h.appendLocked(entry, event); appendErr != nil && !entry.terminal {
		h.terminateCapacityLocked(entry)
	}
	if !entry.terminal {
		h.markTerminalLocked(entry)
	}
}

func operationFailure(err error) service.Descriptor {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return service.Descriptor{
			Code: service.OperationOutcomeUnknown, Message: "operation was canceled", Retryable: true,
		}
	}
	return service.AsError(err).Descriptor
}

func (h *OperationHub) markTerminalLocked(entry *operationEntry) {
	if entry.terminal {
		return
	}
	entry.terminal = true
	entry.completedAt = h.options.Now()
	entry.prompt = nil
	entry.cancel()
	h.active--
	if entry.lossTimer != nil {
		entry.lossTimer.Stop()
		entry.lossTimer = nil
	}
	for subscription, events := range entry.subscribers {
		delete(entry.subscribers, subscription)
		close(events)
	}
	h.pruneCompletedLocked()
}

func (h *OperationHub) dispatchLocked(
	entry *operationEntry,
	event service.OperationEvent,
) {
	for subscription, events := range entry.subscribers {
		select {
		case events <- event:
		default:
			delete(entry.subscribers, subscription)
			close(events)
		}
	}
	h.scheduleSubscriberLossLocked(entry)
}

func (h *OperationHub) pruneLocked() {
	now := h.options.Now()
	for id, entry := range h.operations {
		if entry.terminal && !now.Before(entry.completedAt.Add(h.options.CompletedRetention)) {
			delete(h.operations, id)
		}
	}
	h.pruneCompletedLocked()
}

func (h *OperationHub) pruneCompletedLocked() {
	for {
		completed := 0
		var oldest *operationEntry
		for _, entry := range h.operations {
			if !entry.terminal {
				continue
			}
			completed++
			if oldest == nil || entry.completedAt.Before(oldest.completedAt) {
				oldest = entry
			}
		}
		if completed <= h.options.MaxCompletedOperations || oldest == nil {
			return
		}
		delete(h.operations, oldest.id)
	}
}

func randomOperationID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
