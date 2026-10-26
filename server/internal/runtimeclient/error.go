package runtimeclient

import "morde/server/internal/domain"

const (
	ErrorInvalidRequest          = "invalid_request"
	ErrorAuthenticationFailed    = "runtime_authentication_failed"
	ErrorFenceRejected           = "fence_rejected"
	ErrorOperationConflict       = "operation_conflict"
	ErrorProtocolIncompatible    = "protocol_incompatible"
	ErrorCapabilityUnavailable   = "capability_unavailable"
	ErrorOperationNotFound       = "operation_not_found"
	ErrorResultTooLarge          = "result_too_large"
	ErrorRuntimeStateUnavailable = "runtime_state_unavailable"
	ErrorTransport               = "runtime_transport_error"
	ErrorTransportTimeout        = "runtime_transport_timeout"
	ErrorResponseTooLarge        = "runtime_response_too_large"
	ErrorInvalidResponse         = "runtime_invalid_response"
)

// Error contains only a stable code and HTTP status. It deliberately excludes
// response bodies and transport error strings, which can contain secrets.
type Error struct {
	Code       string
	StatusCode int
}

func (e *Error) Error() string {
	if e == nil || e.Code == "" {
		return "runtime request failed"
	}
	return "runtime request failed: " + e.Code
}

func (e *Error) PublicCode() domain.ErrorCode {
	if e == nil {
		return domain.CodeUpstreamError
	}
	switch e.Code {
	case ErrorInvalidRequest:
		return domain.CodeInvalidArgument
	case ErrorAuthenticationFailed:
		return domain.CodeUnauthenticated
	case ErrorFenceRejected:
		return domain.CodeInvalidState
	case ErrorOperationConflict:
		return domain.CodeIdempotencyConflict
	case ErrorCapabilityUnavailable:
		return domain.CodeUnsupported
	case ErrorOperationNotFound:
		return domain.CodeNotFound
	case ErrorResultTooLarge, ErrorResponseTooLarge:
		return domain.CodeResultTooLarge
	case ErrorTransportTimeout:
		return domain.CodeUpstreamTimeout
	case ErrorProtocolIncompatible, ErrorRuntimeStateUnavailable, ErrorTransport, ErrorInvalidResponse:
		return domain.CodeUpstreamError
	default:
		return domain.CodeUpstreamError
	}
}
