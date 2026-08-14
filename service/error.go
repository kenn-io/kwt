// Package service defines contracts shared by kwt's embeddable services.
package service

import "errors"

// Code identifies a transport-neutral service failure category.
type Code string

const (
	InvalidRequest                       Code = "invalid_request"
	Conflict                             Code = "conflict"
	Busy                                 Code = "busy"
	NotFound                             Code = "not_found"
	InteractionRequired                  Code = "interaction_required"
	PermissionDenied                     Code = "permission_denied"
	ConnectionChanged                    Code = "connection_changed"
	Unsupported                          Code = "unsupported"
	TransportFailure                     Code = "transport_failure"
	DaemonStartFailed                    Code = "daemon_start_failed"
	DaemonUnresponsive                   Code = "daemon_unresponsive"
	DaemonIncompatible                   Code = "daemon_incompatible"
	DaemonDowngradeRefused               Code = "daemon_downgrade_refused"
	DaemonBuildOrderUnknown              Code = "daemon_build_order_unknown"
	DaemonDraining                       Code = "daemon_draining"
	DaemonTransportFailed                Code = "daemon_transport_failed"
	InventoryTimeout                     Code = "inventory_timeout"
	InventoryFailed                      Code = "inventory_failed"
	RemovalFailed                        Code = "removal_failed"
	ProjectNotFound                      Code = "project_not_found"
	RegistrationChanged                  Code = "registration_changed"
	UnregistrationFailed                 Code = "unregistration_failed"
	ProtectedSessionLive                 Code = "protected_session_live"
	ProtectedEndpointInventoryIncomplete Code = "protected_endpoint_inventory_incomplete"
	OperationIDConflict                  Code = "operation_id_conflict"
	OperationCapacityExhausted           Code = "operation_capacity_exhausted"
	// OperationJournalUnavailable remains reserved until kwt adds a durable
	// operation journal. The initial stream is reconnectable only to the same
	// daemon process.
	OperationJournalUnavailable Code = "operation_journal_unavailable"
	OperationOutcomeUnknown     Code = "operation_outcome_unknown"
	SSHInvalidTarget            Code = "ssh_invalid_target"
	SSHResolutionFailed         Code = "ssh_resolution_failed"
	SSHRouteUnreviewable        Code = "ssh_route_unreviewable"
	SSHConfigurationChanged     Code = "ssh_configuration_changed"
	SSHUnsupportedVersion       Code = "ssh_unsupported_version"
	SSHInteractionRequired      Code = "ssh_interaction_required"
	SSHPromptRejected           Code = "ssh_prompt_rejected"
	SSHPromptTimedOut           Code = "ssh_prompt_timed_out"
	SSHConnectionFailed         Code = "ssh_connection_failed"
	SSHConnectionChanged        Code = "ssh_connection_changed"
	SSHControlPathOccupied      Code = "ssh_control_path_occupied"
	SSHCleanupFailed            Code = "ssh_cleanup_failed"
	Internal                    Code = "internal"
)

// Descriptor is the stable, transport-neutral representation of a service
// failure. Message is human-facing; Code, Retryable, and documented Details
// are the machine contract.
type Descriptor struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

// Error carries a stable category and retry policy across in-process and HTTP
// service boundaries.
type Error struct {
	Descriptor
	Err error `json:"-"`
}

// NewDescriptorError attaches a private cause to a stable descriptor.
func NewDescriptorError(descriptor Descriptor, cause error) *Error {
	return &Error{Descriptor: descriptor, Err: cause}
}

// NewError constructs a typed service failure.
func NewError(
	code Code,
	message string,
	retryable bool,
	details map[string]any,
	cause error,
) *Error {
	return NewDescriptorError(Descriptor{
		Code: code, Message: message, Retryable: retryable, Details: details,
	}, cause)
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
