package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperationStreamValidatorAcceptsOrderedPromptAndCompletion(t *testing.T) {
	validator, err := NewOperationStreamValidator("operation-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	events := []OperationEvent{
		{OperationID: "operation-1", Sequence: 1, Kind: OperationEventProgress, Message: "resolving"},
		{
			OperationID: "operation-1",
			Sequence:    2,
			Kind:        OperationEventPrompt,
			Prompt: &OperationPrompt{
				ID: "prompt-1", Kind: "password", Message: "Password:", Sensitive: true,
			},
		},
		{
			OperationID: "operation-1", Sequence: 3, Kind: OperationEventComplete,
			Result: json.RawMessage(`{"status":"ready"}`),
		},
	}
	for _, event := range events {
		if err := validator.Accept(event); err != nil {
			t.Fatalf("accept event %d: %v", event.Sequence, err)
		}
	}
	if !validator.Terminal() {
		t.Fatal("completion did not make the stream terminal")
	}
}

func TestOperationStreamValidatorRejectsSequenceGap(t *testing.T) {
	validator, err := NewOperationStreamValidator("operation-1", 4)
	if err != nil {
		t.Fatal(err)
	}
	err = validator.Accept(OperationEvent{
		OperationID: "operation-1", Sequence: 6, Kind: OperationEventProgress,
	})
	if err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("expected sequence failure, got %v", err)
	}
}

func TestOperationStreamValidatorRejectsEventAfterCompletion(t *testing.T) {
	validator, err := NewOperationStreamValidator("operation-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Accept(OperationEvent{
		OperationID: "operation-1", Sequence: 1, Kind: OperationEventComplete,
		Result: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	err = validator.Accept(OperationEvent{
		OperationID: "operation-1", Sequence: 2, Kind: OperationEventWarning,
	})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("expected terminal failure, got %v", err)
	}
}

func TestOperationStreamValidatorRequiresPromptID(t *testing.T) {
	validator, err := NewOperationStreamValidator("operation-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	err = validator.Accept(OperationEvent{
		OperationID: "operation-1", Sequence: 1, Kind: OperationEventPrompt,
		Prompt: &OperationPrompt{Kind: "password", Message: "Password:"},
	})
	if err == nil || !strings.Contains(err.Error(), "prompt ID") {
		t.Fatalf("expected prompt ID failure, got %v", err)
	}
}

func TestOperationStreamValidatorBoundsMessages(t *testing.T) {
	validator, err := NewOperationStreamValidator("operation-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	err = validator.Accept(OperationEvent{
		OperationID: "operation-1", Sequence: 1, Kind: OperationEventProgress,
		Message: strings.Repeat("x", MaxOperationMessageBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "message") {
		t.Fatalf("expected message bound failure, got %v", err)
	}
}

func TestValidateOperationResponseAllowsEmptyBoundResponse(t *testing.T) {
	if err := ValidateOperationResponse(OperationResponse{PromptID: "prompt-1"}); err != nil {
		t.Fatalf("empty prompt response should be valid: %v", err)
	}
	err := ValidateOperationResponse(OperationResponse{
		PromptID: "prompt-1",
		Value:    strings.Repeat("x", MaxOperationResponseBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "response") {
		t.Fatalf("expected response bound failure, got %v", err)
	}
}
