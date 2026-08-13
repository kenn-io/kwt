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
	defaultMaxOperationEvents         = 256
	defaultMaxOperationBytes          = 1 << 20
	defaultMaxActiveOperations        = 128
	defaultMaxCompletedOperations     = 128
	defaultOperationRetention         = 5 * time.Minute
	defaultOperationSubscriberGrace   = 5 * time.Second
	defaultOperationSubscriberQueue   = 32
	defaultMaxSubscribersPerOperation = 8
	defaultMaxOperationSubscribers    = 128
	operationCompletionReserveBytes   = 16 << 10
	operationCriticalQueueReserve     = 1
)

type OperationHubOptions struct {
	Now                        func() time.Time
	IDSource                   func() (string, error)
	Reserve                    func() (func(), error)
	MaxEvents                  int
	MaxBytes                   int
	MaxActiveOperations        int
	MaxCompletedOperations     int
	CompletedRetention         time.Duration
	SubscriberGrace            time.Duration
	SubscriberQueue            int
	MaxSubscribersPerOperation int
	MaxSubscribers             int
}

type OperationStart struct {
	ID            string
	RequestDigest string
	Run           func(context.Context, *Operation) (json.RawMessage, error)
}

type OperationHub struct {
	context context.Context
	options OperationHubOptions

	mu            sync.Mutex
	operations    map[string]*operationEntry
	active        int
	subscribers   int
	draining      bool
	drainDeadline time.Time
}

type operationEntry struct {
	id              string
	requestDigest   string
	operation       *Operation
	context         context.Context
	cancel          context.CancelFunc
	validator       *service.OperationStreamValidator
	events          []retainedOperationEvent
	eventBytes      int
	terminal        bool
	completedAt     time.Time
	prompt          *operationPromptState
	subscribers     map[*OperationSubscription]chan retainedOperationEvent
	subscriberCount int
	lossTimer       *time.Timer
	lossTimerToken  uint64
	release         func()
	workerActive    bool
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
	hub     *OperationHub
	entry   *operationEntry
	events  <-chan retainedOperationEvent
	counted bool
	once    sync.Once
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
	if options.MaxSubscribersPerOperation <= 0 {
		options.MaxSubscribersPerOperation = defaultMaxSubscribersPerOperation
	}
	if options.MaxSubscribers <= 0 {
		options.MaxSubscribers = defaultMaxOperationSubscribers
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
		if !validOperationID(id) {
			return nil, false, service.NewError(
				service.InvalidRequest,
				"operation ID must contain only ASCII letters, digits, hyphens, and underscores",
				false,
				nil,
				nil,
			)
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
		if h.draining {
			deadline := h.drainDeadline
			h.mu.Unlock()
			return nil, false, service.NewError(
				service.DaemonDraining,
				"daemon is draining",
				true,
				map[string]any{"drain_deadline": deadline},
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
		var release func()
		if h.options.Reserve != nil {
			var reserveErr error
			release, reserveErr = h.options.Reserve()
			if reserveErr != nil {
				h.mu.Unlock()
				return nil, false, reserveErr
			}
		}
		workerContext, cancel := context.WithCancel(h.context)
		validator, err := service.NewOperationStreamValidator(id, 0)
		if err != nil {
			cancel()
			if release != nil {
				release()
			}
			h.mu.Unlock()
			return nil, false, err
		}
		entry := &operationEntry{
			id:            id,
			requestDigest: start.RequestDigest,
			context:       workerContext,
			cancel:        cancel,
			validator:     validator,
			subscribers:   make(map[*OperationSubscription]chan retainedOperationEvent),
			release:       release,
			workerActive:  true,
		}
		entry.operation = &Operation{hub: h, entry: entry}
		h.operations[id] = entry
		h.active++
		h.scheduleSubscriberLossLocked(entry)
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
	h.releaseWorker(entry)
}

func (h *OperationHub) releaseWorker(entry *operationEntry) {
	h.mu.Lock()
	release := entry.release
	h.mu.Unlock()
	if release != nil {
		release()
	}
	h.mu.Lock()
	entry.release = nil
	entry.workerActive = false
	h.active--
	h.pruneLocked()
	h.mu.Unlock()
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

func (h *OperationHub) BeginDrain(deadline time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return
	}
	h.draining = true
	h.drainDeadline = deadline
}

func (h *OperationHub) CancelActiveForDrain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.operations {
		if entry.terminal {
			continue
		}
		failure := service.Descriptor{
			Code: service.OperationOutcomeUnknown, Message: "daemon drain deadline expired", Retryable: false,
		}
		_ = h.appendLocked(entry, service.OperationEvent{
			Kind: service.OperationEventComplete, Failure: &failure,
		})
		if !entry.terminal {
			h.markTerminalLocked(entry)
		}
	}
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
		lastSequence = entry.events[len(entry.events)-1].sequence
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
	if len(entry.events) > 0 && afterSequence+1 < entry.events[0].sequence {
		return nil, service.NewError(
			service.OperationOutcomeUnknown,
			"operation events are no longer retained",
			false,
			nil,
			nil,
		)
	}
	if entry.subscriberCount >= h.options.MaxSubscribersPerOperation ||
		h.subscribers >= h.options.MaxSubscribers {
		return nil, service.NewError(
			service.OperationCapacityExhausted,
			"daemon operation subscriber capacity is exhausted",
			true,
			nil,
			nil,
		)
	}
	replay := make([]retainedOperationEvent, 0, len(entry.events))
	for _, event := range entry.events {
		if event.sequence > afterSequence {
			replay = append(replay, event)
		}
	}
	queueSize := len(replay) + h.options.SubscriberQueue + operationCriticalQueueReserve
	events := make(chan retainedOperationEvent, queueSize)
	for _, event := range replay {
		events <- event
	}
	subscription := &OperationSubscription{
		hub: h, entry: entry, events: events,
	}
	if entry.lossTimer != nil {
		entry.lossTimer.Stop()
		entry.lossTimer = nil
	}
	entry.subscribers[subscription] = events
	entry.subscriberCount++
	subscription.counted = true
	h.subscribers++
	if entry.terminal {
		close(events)
	}
	return subscription, nil
}

func (s *OperationSubscription) Events() <-chan retainedOperationEvent {
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
	if ok {
		delete(entry.subscribers, subscription)
		if !entry.terminal {
			close(events)
		}
	}
	if !subscription.counted {
		return
	}
	subscription.counted = false
	entry.subscriberCount--
	h.subscribers--
	h.scheduleSubscriberLossLocked(entry)
}

func (h *OperationHub) scheduleSubscriberLossLocked(entry *operationEntry) {
	if entry.terminal || len(entry.subscribers) != 0 || entry.lossTimer != nil {
		return
	}
	entry.lossTimerToken++
	token := entry.lossTimerToken
	entry.lossTimer = time.AfterFunc(h.options.SubscriberGrace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if entry.lossTimerToken != token {
			return
		}
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
	limit := h.options.MaxBytes
	if event.Kind != service.OperationEventComplete {
		limit = h.nonterminalByteLimit()
	}
	if event.Kind != service.OperationEventComplete && len(entry.events) >= h.options.MaxEvents-1 {
		h.terminateCapacityLocked(entry)
		return service.NewError(
			service.OperationCapacityExhausted,
			"operation event capacity is exhausted",
			true,
			nil,
			nil,
		)
	}
	if event.Kind == service.OperationEventComplete && len(entry.events) >= h.options.MaxEvents {
		h.terminateCapacityLocked(entry)
		return service.NewError(
			service.OperationCapacityExhausted,
			"operation completion exceeds event capacity",
			true,
			nil,
			nil,
		)
	}
	encoded, err := encodeRetainedOperationEvent(event, limit-entry.eventBytes)
	if err != nil {
		if errors.Is(err, errOperationEventTooLarge) {
			h.terminateCapacityLocked(entry)
			return service.NewError(
				service.OperationCapacityExhausted,
				"operation event capacity is exhausted",
				true,
				nil,
				err,
			)
		}
		return fmt.Errorf("encode operation event: %w", err)
	}
	if err := entry.validator.Accept(event); err != nil {
		return fmt.Errorf("validate operation event: %w", err)
	}
	entry.events = append(entry.events, encoded)
	entry.eventBytes += len(encoded.encoded)
	h.dispatchLocked(entry, encoded)
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
		Code:      service.OperationOutcomeUnknown,
		Message:   "operation outcome is unknown after event capacity was exhausted",
		Retryable: false,
	}
	event := service.OperationEvent{
		OperationID: entry.id,
		Sequence:    uint64(len(entry.events) + 1),
		Kind:        service.OperationEventComplete,
		Failure:     &failure,
	}
	encoded, err := encodeRetainedOperationEvent(event, h.options.MaxBytes-entry.eventBytes)
	if err == nil && len(entry.events) < h.options.MaxEvents {
		if entry.validator.Accept(event) == nil {
			entry.events = append(entry.events, encoded)
			entry.eventBytes += len(encoded.encoded)
			h.dispatchLocked(entry, encoded)
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
		failure := publicErrorDescriptor(fmt.Errorf("publish operation completion: %w", appendErr))
		if fallbackErr := h.appendLocked(entry, service.OperationEvent{
			Kind: service.OperationEventComplete, Failure: &failure,
		}); fallbackErr != nil && !entry.terminal {
			h.terminateCapacityLocked(entry)
		}
	}
	if !entry.terminal {
		h.markTerminalLocked(entry)
	}
}

func operationFailure(err error) service.Descriptor {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return service.Descriptor{
			Code: service.OperationOutcomeUnknown, Message: "operation was canceled", Retryable: false,
		}
	}
	return publicErrorDescriptor(err)
}

func (h *OperationHub) markTerminalLocked(entry *operationEntry) {
	if entry.terminal {
		return
	}
	entry.terminal = true
	entry.completedAt = h.options.Now()
	entry.prompt = nil
	entry.cancel()
	if entry.lossTimer != nil {
		entry.lossTimer.Stop()
		entry.lossTimer = nil
	}
	for _, events := range entry.subscribers {
		close(events)
	}
	h.pruneCompletedLocked()
}

func (h *OperationHub) dispatchLocked(
	entry *operationEntry,
	event retainedOperationEvent,
) {
	for subscription, events := range entry.subscribers {
		limit := cap(events)
		if event.kind != service.OperationEventPrompt && event.kind != service.OperationEventComplete {
			limit -= operationCriticalQueueReserve
		}
		if len(events) >= limit {
			delete(entry.subscribers, subscription)
			close(events)
			continue
		}
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
		if entry.terminal && !entry.workerActive &&
			!now.Before(entry.completedAt.Add(h.options.CompletedRetention)) {
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
			if entry.workerActive {
				continue
			}
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

func validOperationID(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}
