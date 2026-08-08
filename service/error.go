// Package service defines contracts shared by kwt's embeddable services.
package service

import "errors"

// Code identifies a transport-neutral service failure category.
type Code string

const (
	InvalidRequest      Code = "invalid_request"
	Conflict            Code = "conflict"
	Busy                Code = "busy"
	NotFound            Code = "not_found"
	InteractionRequired Code = "interaction_required"
	PermissionDenied    Code = "permission_denied"
	ConnectionChanged   Code = "connection_changed"
	Unsupported         Code = "unsupported"
	TransportFailure    Code = "transport_failure"
	Internal            Code = "internal"
)

// Error carries a stable category and retry policy across in-process and HTTP
// service boundaries.
type Error struct {
	Code      Code
	Message   string
	Retryable bool
	Details   map[string]any
	Err       error
}

// NewError constructs a typed service failure.
func NewError(
	code Code,
	message string,
	retryable bool,
	details map[string]any,
	cause error,
) *Error {
	return &Error{
		Code:      code,
		Message:   message,
		Retryable: retryable,
		Details:   details,
		Err:       cause,
	}
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return e.Message
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AsError preserves typed service failures and normalizes unexpected errors.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	return NewError(Internal, "internal failure", false, nil, err)
}

// IsCode reports whether err contains a service error with the given code.
func IsCode(err error, code Code) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Code == code
}
