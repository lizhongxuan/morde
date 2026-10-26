package domain

import (
	"errors"
	"strings"
	"time"
)

type ErrorCode string

const (
	CodeInvalidArgument     ErrorCode = "invalid_argument"
	CodeUnauthenticated     ErrorCode = "unauthenticated"
	CodeForbidden           ErrorCode = "forbidden"
	CodeNotFound            ErrorCode = "not_found"
	CodeQuotaExceeded       ErrorCode = "quota_exceeded"
	CodeCapacityExceeded    ErrorCode = "capacity_exceeded"
	CodeWorkspaceBusy       ErrorCode = "workspace_busy"
	CodeWorkspaceConflict   ErrorCode = "workspace_conflict"
	CodeTransferExpired     ErrorCode = "transfer_expired"
	CodeInvalidState        ErrorCode = "invalid_state"
	CodeConflict            ErrorCode = "conflict"
	CodeIdempotencyConflict ErrorCode = "idempotency_conflict"
	CodeEventCursorExpired  ErrorCode = "event_cursor_expired"
	CodeUnsupported         ErrorCode = "unsupported"
	CodeRequestTooLarge     ErrorCode = "request_too_large"
	CodeResultTooLarge      ErrorCode = "result_too_large"
	CodeRateLimited         ErrorCode = "rate_limited"
	CodeInternal            ErrorCode = "internal"
	CodeUpstreamError       ErrorCode = "upstream_error"
	CodeHostNotReady        ErrorCode = "host_not_ready"
	CodeUpstreamTimeout     ErrorCode = "upstream_timeout"
)

type Error struct {
	Code       ErrorCode      `json:"code"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details"`
	RetryAfter time.Duration  `json:"-"`
	Err        error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := strings.ToLower(strings.TrimSpace(e.Message))
	return strings.TrimRight(message, ".")
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message, Details: map[string]any{}}
}

func CodeOf(err error) ErrorCode {
	var de *Error
	if errors.As(err, &de) && de.Code != "" {
		return de.Code
	}
	return CodeInternal
}

func InvalidArgument(field, message string) *Error {
	err := NewError(CodeInvalidArgument, message)
	err.Details["field"] = field
	return err
}
