package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	MaxOperationMessageBytes  = 16 << 10
	MaxOperationResponseBytes = 64 << 10
)

type OperationEventKind string

const (
	OperationEventProgress OperationEventKind = "progress"
	OperationEventWarning  OperationEventKind = "warning"
	OperationEventPrompt   OperationEventKind = "prompt"
	OperationEventComplete OperationEventKind = "complete"
)

type OperationEvent struct {
	OperationID string             `json:"operation_id"`
	Sequence    uint64             `json:"sequence"`
	Kind        OperationEventKind `json:"kind"`
	Message     string             `json:"message,omitempty"`
	Prompt      *OperationPrompt   `json:"prompt,omitempty"`
	Result      json.RawMessage    `json:"result,omitempty"`
	Failure     *Descriptor        `json:"failure,omitempty"`
}

type OperationPrompt struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Message   string         `json:"message"`
	Sensitive bool           `json:"sensitive"`
	Deadline  *time.Time     `json:"deadline,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

type OperationResponse struct {
	PromptID string `json:"prompt_id"`
	Value    string `json:"value"`
}

type OperationStreamValidator struct {
	operationID string
	sequence    uint64
	terminal    bool
}

func NewOperationStreamValidator(
	operationID string,
	afterSequence uint64,
) (*OperationStreamValidator, error) {
	if operationID == "" {
		return nil, errors.New("operation ID is empty")
	}
	return &OperationStreamValidator{
		operationID: operationID,
		sequence:    afterSequence,
	}, nil
}

func (v *OperationStreamValidator) Accept(event OperationEvent) error {
	if v.terminal {
		return errors.New("operation stream is terminal")
	}
	if event.OperationID != v.operationID {
		return fmt.Errorf("operation ID %q does not match %q", event.OperationID, v.operationID)
	}
	if event.Sequence != v.sequence+1 {
		return fmt.Errorf("operation event sequence is %d; expected %d", event.Sequence, v.sequence+1)
	}
	if err := validateOperationEventShape(event); err != nil {
		return err
	}
	v.sequence = event.Sequence
	if event.Kind == OperationEventComplete {
		v.terminal = true
	}
	return nil
}

func (v *OperationStreamValidator) Terminal() bool {
	return v.terminal
}

func validateOperationEventShape(event OperationEvent) error {
	if len(event.Message) > MaxOperationMessageBytes {
		return fmt.Errorf("operation event message exceeds %d bytes", MaxOperationMessageBytes)
	}
	switch event.Kind {
	case OperationEventProgress, OperationEventWarning:
		if event.Prompt != nil || event.Result != nil || event.Failure != nil {
			return fmt.Errorf("%s event carries incompatible payload", event.Kind)
		}
	case OperationEventPrompt:
		if event.Prompt == nil {
			return errors.New("prompt event has no prompt")
		}
		if event.Prompt.ID == "" {
			return errors.New("prompt event has an empty prompt ID")
		}
		if event.Prompt.Kind == "" {
			return errors.New("prompt event has an empty prompt kind")
		}
		if len(event.Prompt.Message) > MaxOperationMessageBytes {
			return fmt.Errorf("operation prompt message exceeds %d bytes", MaxOperationMessageBytes)
		}
		if event.Result != nil || event.Failure != nil {
			return errors.New("prompt event carries a terminal payload")
		}
	case OperationEventComplete:
		if event.Prompt != nil {
			return errors.New("completion event carries a prompt")
		}
		if (event.Result == nil) == (event.Failure == nil) {
			return errors.New("completion event must carry exactly one result or failure")
		}
	default:
		return fmt.Errorf("unknown operation event kind %q", event.Kind)
	}
	return nil
}

func ValidateOperationResponse(response OperationResponse) error {
	if response.PromptID == "" {
		return errors.New("operation response prompt ID is empty")
	}
	if len(response.Value) > MaxOperationResponseBytes {
		return fmt.Errorf("operation response exceeds %d bytes", MaxOperationResponseBytes)
	}
	return nil
}
