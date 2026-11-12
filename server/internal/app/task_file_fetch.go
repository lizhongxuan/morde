package app

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

const (
	TaskFileFetchMaxPerTask      = 16
	TaskFileFetchMaxFileBytes    = int64(16 << 20)
	TaskFileFetchChunkBytes      = int64(512 << 10)
	TaskFileFetchStagingBytes    = int64(64 << 20)
	TaskFileFetchRetainedBytes   = int64(300 << 20)
	TaskFileFetchTicketLifetime  = 5 * time.Minute
	TaskFileFetchStagingLifetime = time.Hour
	TaskFileFetchRetention       = 7 * 24 * time.Hour
	TaskFileFetchCleanupInterval = 15 * time.Minute
	TaskFileFetchCleanupBatch    = int32(500)
)

// TaskFileFetch is the safe status projection. It contains no ticket, ticket
// hash, or PC directory path.
type TaskFileFetch struct {
	ID                       string
	OwnerID                  string
	TaskID                   string
	OperationKey             string
	SourceInstallationID     string
	SourceDeviceName         string
	SourceDirectoryName      string
	TargetGrantVersion       int64
	TaskAuthorizationVersion int64
	TaskRevision             int64
	RelativePath             string
	Status                   task.FileFetchStatus
	WaitingReason            string
	FailureReason            string
	SourceVersion            string
	SourceSHA256             string
	MediaType                string
	SizeBytes                int64
	ConfirmedBytes           int64
	ContentID                string
	ExpiresAt                *time.Time
	ContentState             task.FileFetchContentState
	ParseStatus              task.FileFetchParseStatus
	ParseError               string
	DeletedAt                *time.Time
	StagingExpiresAt         *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// TaskFileFetchCreate contains no transfer credential. A hash and expiry are
// added atomically at dispatch and plaintext is sent only in the Hub call.
type TaskFileFetchCreate struct {
	Operation TaskHubOperation
	Fetch     TaskFileFetch
}

type TaskFileFetchStore interface {
	CreateTaskFileFetch(context.Context, TaskFileFetchCreate, string) (TaskFileFetch, bool, error)
	GetTaskFileFetchByOperation(context.Context, string, string, string) (TaskFileFetch, error)
	DispatchTaskFileFetch(context.Context, TaskHubOperation, string, []byte, *time.Time, bool, string) (bool, error)
	MarkTaskFileFetchWaiting(context.Context, TaskHubOperation, string, string) (TaskFileFetch, error)
	FailTaskFileFetch(context.Context, TaskHubOperation, string, string) error
	GetTaskFileFetch(context.Context, string, string, string) (TaskFileFetch, error)
}

type TaskFileFetchOwnerSessionAuthorizer interface {
	IsDesktopClientWebSession(context.Context, string, string) (bool, error)
}

func TaskFileFetchFailureReason(code string) string {
	switch code {
	case "denied", "forbidden", "not_allowed", "permission_required", "unauthenticated":
		return task.FileFetchFailureAccessDenied
	case "conflict", "source_changed":
		return task.FileFetchFailureSourceChanged
	case "not_found":
		return task.FileFetchFailureUnavailable
	case "too_large", "quota_exceeded":
		return task.FileFetchFailureLimitExceeded
	case "unsupported":
		return task.FileFetchFailureUnsupported
	case "timeout", "device_unavailable":
		return task.FileFetchFailureSourceUnavailable
	default:
		return task.FileFetchFailureTransfer
	}
}

func (s *TaskService) GetTaskFileFetch(ctx context.Context, principal identity.Principal, taskID, fetchID string) (TaskFileFetch, error) {
	if err := s.authorizeTaskFileFetchOwnerSession(ctx, principal); err != nil {
		return TaskFileFetch{}, err
	}
	details, err := s.Get(ctx, principal, taskID)
	if err != nil {
		return TaskFileFetch{}, err
	}
	if details.Task.OwnerID != principal.UserID || !domain.ValidOperationID(fetchID) {
		return TaskFileFetch{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	store, ok := s.store.(TaskFileFetchStore)
	if !ok {
		return TaskFileFetch{}, domain.NewError(domain.CodeUnsupported, "task file fetch storage is unavailable")
	}
	fetch, err := store.GetTaskFileFetch(ctx, principal.UserID, taskID, fetchID)
	if err != nil {
		return TaskFileFetch{}, err
	}
	if !validTaskFileFetchProjection(fetch) {
		return TaskFileFetch{}, domain.NewError(domain.CodeInternal, "task file fetch projection is invalid")
	}
	return fetch, nil
}

func validTaskFileFetchProjection(value TaskFileFetch) bool {
	return domain.ValidOperationID(value.ID) && domain.ValidUserID(value.OwnerID) && domain.ValidTaskID(value.TaskID) &&
		validTaskFileFetchOperationKey(value.OperationKey) && domain.ValidClientInstallationID(value.SourceInstallationID) &&
		value.SourceDeviceName != "" &&
		(value.SourceDirectoryName == "" || ValidTaskFileFetchDirectoryName(value.SourceDirectoryName)) &&
		value.TargetGrantVersion > 0 && value.TaskAuthorizationVersion > 0 && value.TaskRevision >= 0 &&
		workspace.ValidateRelativePath(value.RelativePath) == nil && task.ValidFileFetchStatus(value.Status) &&
		(value.FailureReason == "" || value.Status == task.FileFetchFailed && task.ValidFileFetchFailureReason(value.FailureReason)) &&
		task.ValidFileFetchContentState(value.ContentState) &&
		task.ValidFileFetchParseStatus(value.ParseStatus) && validTaskFileFetchParseProjection(value.ParseStatus, value.ParseError) &&
		(value.Status == task.FileFetchCompleted || value.ParseStatus == task.FileFetchParsePending) &&
		value.SizeBytes >= 0 && value.ConfirmedBytes >= 0 && value.ConfirmedBytes <= value.SizeBytes &&
		(value.SourceSHA256 == "" || validSHA256Hex(value.SourceSHA256)) &&
		(value.SourceVersion == "" || validSHA256Hex(value.SourceVersion)) &&
		(value.ContentID == "" || domain.ValidOperationID(value.ContentID)) &&
		((value.ContentState == task.FileFetchContentNone) == (value.ContentID == "")) &&
		((value.ContentState == task.FileFetchContentDeleted) == (value.DeletedAt != nil)) &&
		(value.MediaType == "" || taskFileFetchMediaTypeAllowed(value.MediaType)) && !value.CreatedAt.IsZero() && !value.UpdatedAt.IsZero()
}

func ValidTaskFileFetchDirectoryName(value string) bool {
	if len(value) < 1 || len(value) > 100 || !utf8.ValidString(value) || value == "." || value == ".." || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validTaskFileFetchParseProjection(status task.FileFetchParseStatus, parseError string) bool {
	if status == task.FileFetchParseFailed || status == task.FileFetchParseUnavailable {
		switch parseError {
		case "invalid_encoding", "invalid_document", "result_too_large", "content_unavailable", "integrity_failed":
			return true
		default:
			return false
		}
	}
	return parseError == ""
}

func ValidTaskFileFetchParseResult(status task.FileFetchParseStatus, parseError string) bool {
	return validTaskFileFetchParseProjection(status, parseError)
}

func validTaskFileFetchOperationKey(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00') && strings.TrimSpace(value) == value
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
