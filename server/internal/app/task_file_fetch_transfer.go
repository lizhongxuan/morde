package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type TaskFileFetchManifestRequest struct {
	SourceVersion       string `json:"source_version"`
	SourceSHA256        string `json:"source_sha256"`
	SizeBytes           int64  `json:"size_bytes"`
	MediaType           string `json:"media_type"`
	SourceDirectoryName string `json:"source_directory_name"`
}

type TaskFileFetchManifestReceipt struct {
	FetchID            string  `json:"fetch_id"`
	SourceVersion      string  `json:"source_version"`
	Status             string  `json:"status"`
	ConfirmedSequences []int32 `json:"confirmed_sequences"`
}

type TaskFileFetchChunkReceipt struct {
	FetchID        string `json:"fetch_id"`
	Sequence       int32  `json:"sequence"`
	ConfirmedBytes int64  `json:"confirmed_bytes"`
	Replayed       bool   `json:"replayed"`
}

type TaskFileFetchCommitRequest struct {
	SourceVersion string `json:"source_version"`
	SourceSHA256  string `json:"source_sha256"`
	SizeBytes     int64  `json:"size_bytes"`
}

type TaskFileFetchCommitReceipt struct {
	FetchID            string    `json:"fetch_id"`
	TaskRevision       int64     `json:"task_revision"`
	TargetGrantVersion int64     `json:"target_grant_version"`
	Status             string    `json:"status"`
	SourceVersion      string    `json:"source_version"`
	SourceSHA256       string    `json:"source_sha256"`
	SizeBytes          int64     `json:"size_bytes"`
	ContentID          string    `json:"content_id"`
	ExpiresAt          time.Time `json:"expires_at"`
}

type TaskFileFetchContent struct {
	Body         io.ReadCloser
	MediaType    string
	SizeBytes    int64
	SHA256       string
	FileName     string
	ContentState task.FileFetchContentState
}

// TaskFileFetchDataStore owns every transfer state change in a transaction.
// Its PC methods accept only a ticket hash; plaintext tickets are never stored.
type TaskFileFetchDataStore interface {
	AcceptTaskFileFetchManifest(context.Context, string, string, []byte, TaskFileFetchManifestRequest) (TaskFileFetchManifestReceipt, error)
	PutTaskFileFetchChunk(context.Context, string, string, []byte, int32, string, []byte) (TaskFileFetchChunkReceipt, error)
	CommitTaskFileFetch(context.Context, string, string, []byte, TaskFileFetchCommitRequest, string) (TaskFileFetchCommitReceipt, error)
	AbortTaskFileFetch(context.Context, string, string, []byte) error
	CancelTaskFileFetch(context.Context, string, string, string) (TaskFileFetch, error)
	ListTaskFileFetches(context.Context, string, string) ([]TaskFileFetch, error)
	OpenTaskFileFetchContent(context.Context, string, string, string) (io.ReadCloser, TaskFileFetch, error)
	DeleteTaskFileFetchContent(context.Context, string, string, string) (TaskFileFetch, error)
	SetTaskFileFetchParseResult(context.Context, string, string, string, task.FileFetchParseStatus, string) (TaskFileFetch, error)
	CleanupExpiredTaskFileFetches(context.Context, time.Time, int32) (int32, error)
}

func (s *TaskService) ManifestTaskFileFetch(ctx context.Context, taskID, fetchID, ticket string,
	request TaskFileFetchManifestRequest) (TaskFileFetchManifestReceipt, error) {
	store, ok := s.store.(TaskFileFetchDataStore)
	if !ok || store == nil {
		return TaskFileFetchManifestReceipt{}, domain.NewError(domain.CodeUnsupported, "Task file fetch transfer storage is unavailable")
	}
	ticketHash, err := hashTaskFileFetchTicket(ticket)
	if err != nil {
		return TaskFileFetchManifestReceipt{}, err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) || !validSHA256Hex(request.SourceVersion) ||
		!validSHA256Hex(request.SourceSHA256) || request.SizeBytes < 1 || request.SizeBytes > TaskFileFetchMaxFileBytes ||
		!taskFileFetchMediaTypeAllowed(request.MediaType) || !ValidTaskFileFetchDirectoryName(request.SourceDirectoryName) {
		return TaskFileFetchManifestReceipt{}, domain.InvalidArgument("manifest", "Task file fetch manifest is invalid")
	}
	return store.AcceptTaskFileFetchManifest(ctx, taskID, fetchID, ticketHash, request)
}

func (s *TaskService) PutTaskFileFetchChunk(ctx context.Context, taskID, fetchID, ticket string,
	sequence int32, digest string, body io.Reader) (TaskFileFetchChunkReceipt, error) {
	store, ok := s.store.(TaskFileFetchDataStore)
	if !ok || store == nil {
		return TaskFileFetchChunkReceipt{}, domain.NewError(domain.CodeUnsupported, "Task file fetch transfer storage is unavailable")
	}
	ticketHash, err := hashTaskFileFetchTicket(ticket)
	if err != nil {
		return TaskFileFetchChunkReceipt{}, err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) || sequence < 0 || sequence >= int32(TaskFileFetchMaxFileBytes/TaskFileFetchChunkBytes) ||
		!validSHA256Hex(digest) || body == nil {
		return TaskFileFetchChunkReceipt{}, domain.InvalidArgument("chunk", "Task file fetch chunk is invalid")
	}
	content, err := io.ReadAll(io.LimitReader(body, TaskFileFetchChunkBytes+1))
	if err != nil {
		return TaskFileFetchChunkReceipt{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Task file fetch chunk could not be read")
	}
	if int64(len(content)) > TaskFileFetchChunkBytes {
		return TaskFileFetchChunkReceipt{}, domain.NewError(domain.CodeRequestTooLarge, "Task file fetch chunk exceeds the size limit")
	}
	if len(content) == 0 || digestSHA256Hex(content) != digest {
		return TaskFileFetchChunkReceipt{}, domain.InvalidArgument("chunk", "Task file fetch chunk size or digest is invalid")
	}
	return store.PutTaskFileFetchChunk(ctx, taskID, fetchID, ticketHash, sequence, digest, content)
}

func (s *TaskService) CommitTaskFileFetch(ctx context.Context, taskID, fetchID, ticket string,
	request TaskFileFetchCommitRequest) (TaskFileFetchCommitReceipt, error) {
	store, ok := s.store.(TaskFileFetchDataStore)
	if !ok || store == nil {
		return TaskFileFetchCommitReceipt{}, domain.NewError(domain.CodeUnsupported, "Task file fetch transfer storage is unavailable")
	}
	ticketHash, err := hashTaskFileFetchTicket(ticket)
	if err != nil {
		return TaskFileFetchCommitReceipt{}, err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) || !validSHA256Hex(request.SourceVersion) ||
		!validSHA256Hex(request.SourceSHA256) || request.SizeBytes < 1 || request.SizeBytes > TaskFileFetchMaxFileBytes {
		return TaskFileFetchCommitReceipt{}, domain.InvalidArgument("commit", "Task file fetch commit is invalid")
	}
	contentID, err := domain.NewOperationID()
	if err != nil {
		return TaskFileFetchCommitReceipt{}, err
	}
	return store.CommitTaskFileFetch(ctx, taskID, fetchID, ticketHash, request, contentID)
}

func (s *TaskService) AbortTaskFileFetch(ctx context.Context, taskID, fetchID, ticket string) error {
	store, ok := s.store.(TaskFileFetchDataStore)
	if !ok || store == nil {
		return domain.NewError(domain.CodeUnsupported, "Task file fetch transfer storage is unavailable")
	}
	ticketHash, err := hashTaskFileFetchTicket(ticket)
	if err != nil {
		return err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) {
		return domain.InvalidArgument("file_fetch", "Task file fetch identity is invalid")
	}
	return store.AbortTaskFileFetch(ctx, taskID, fetchID, ticketHash)
}

func (s *TaskService) CancelTaskFileFetch(ctx context.Context, principal identity.Principal, taskID, fetchID string) (TaskFileFetch, error) {
	store, err := s.taskFileFetchOwnerStore(ctx, principal, taskID, fetchID)
	if err != nil {
		return TaskFileFetch{}, err
	}
	value, err := store.CancelTaskFileFetch(ctx, principal.UserID, taskID, fetchID)
	if err != nil {
		return TaskFileFetch{}, err
	}
	if !validTaskFileFetchProjection(value) {
		return TaskFileFetch{}, domain.NewError(domain.CodeInternal, "Task file fetch projection is invalid")
	}
	return value, nil
}

func (s *TaskService) ListTaskFileFetches(ctx context.Context, principal identity.Principal, taskID string) ([]TaskFileFetch, error) {
	if err := s.authorizeTaskFileFetchOwnerSession(ctx, principal); err != nil {
		return nil, err
	}
	details, err := s.Get(ctx, principal, taskID)
	if err != nil || details.Task.OwnerID != principal.UserID {
		if err != nil {
			return nil, err
		}
		return nil, domain.NewError(domain.CodeNotFound, "Task not found")
	}
	store, ok := s.store.(TaskFileFetchDataStore)
	if !ok || store == nil {
		return nil, domain.NewError(domain.CodeUnsupported, "Task file fetch transfer storage is unavailable")
	}
	values, err := store.ListTaskFileFetches(ctx, principal.UserID, taskID)
	if err != nil {
		return nil, err
	}
	if values == nil {
		values = []TaskFileFetch{}
	}
	for _, value := range values {
		if !validTaskFileFetchProjection(value) {
			return nil, domain.NewError(domain.CodeInternal, "Task file fetch projection is invalid")
		}
	}
	return values, nil
}

func (s *TaskService) OpenTaskFileFetchContent(ctx context.Context, principal identity.Principal, taskID, fetchID string) (TaskFileFetchContent, error) {
	store, err := s.taskFileFetchOwnerStore(ctx, principal, taskID, fetchID)
	if err != nil {
		return TaskFileFetchContent{}, err
	}
	body, value, err := store.OpenTaskFileFetchContent(ctx, principal.UserID, taskID, fetchID)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return TaskFileFetchContent{}, err
	}
	if body == nil || !validTaskFileFetchProjection(value) || value.Status != task.FileFetchCompleted ||
		value.ContentState != task.FileFetchContentRetained || value.MediaType == "" || value.SizeBytes < 1 || !validSHA256Hex(value.SourceSHA256) {
		if body != nil {
			_ = body.Close()
		}
		return TaskFileFetchContent{}, domain.NewError(domain.CodeInternal, "Task file fetch content metadata is invalid")
	}
	extension := strings.ToLower(path.Ext(value.RelativePath))
	return TaskFileFetchContent{Body: body, MediaType: value.MediaType, SizeBytes: value.SizeBytes,
		SHA256: value.SourceSHA256, FileName: "fetch-" + value.ID + extension, ContentState: value.ContentState}, nil
}

func (s *TaskService) DeleteTaskFileFetchContent(ctx context.Context, principal identity.Principal, taskID, fetchID string) (TaskFileFetch, error) {
	store, err := s.taskFileFetchOwnerStore(ctx, principal, taskID, fetchID)
	if err != nil {
		return TaskFileFetch{}, err
	}
	value, err := store.DeleteTaskFileFetchContent(ctx, principal.UserID, taskID, fetchID)
	if err != nil {
		return TaskFileFetch{}, err
	}
	if !validTaskFileFetchProjection(value) {
		return TaskFileFetch{}, domain.NewError(domain.CodeInternal, "Task file fetch projection is invalid")
	}
	return value, nil
}

func (s *TaskService) CleanupExpiredTaskFileFetches(ctx context.Context, now time.Time, limit int32) (int32, error) {
	if now.IsZero() || limit < 1 || limit > TaskFileFetchCleanupBatch {
		return 0, domain.InvalidArgument("cleanup", "Task file fetch cleanup parameters are invalid")
	}
	store, ok := s.store.(TaskFileFetchDataStore)
	if !ok || store == nil {
		return 0, domain.NewError(domain.CodeUnsupported, "Task file fetch transfer storage is unavailable")
	}
	return store.CleanupExpiredTaskFileFetches(ctx, now.UTC(), limit)
}

func (s *TaskService) taskFileFetchOwnerStore(ctx context.Context, principal identity.Principal, taskID, fetchID string) (TaskFileFetchDataStore, error) {
	if err := s.authorizeTaskFileFetchOwnerSession(ctx, principal); err != nil {
		return nil, err
	}
	if !domain.ValidOperationID(fetchID) {
		return nil, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	details, err := s.Get(ctx, principal, taskID)
	if err != nil {
		return nil, err
	}
	if details.Task.OwnerID != principal.UserID {
		return nil, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	store, ok := s.store.(TaskFileFetchDataStore)
	if !ok || store == nil {
		return nil, domain.NewError(domain.CodeUnsupported, "Task file fetch transfer storage is unavailable")
	}
	return store, nil
}

func (s *TaskService) authorizeTaskFileFetchOwnerSession(ctx context.Context, principal identity.Principal) error {
	if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	if principal.ClientInstallationID == "" {
		return nil
	}
	authorizer, ok := s.store.(TaskFileFetchOwnerSessionAuthorizer)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "Task file fetch session authorization is unavailable")
	}
	allowed, err := authorizer.IsDesktopClientWebSession(ctx, principal.UserID, principal.ClientInstallationID)
	if err != nil {
		return err
	}
	if !allowed {
		return domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	return nil
}

func hashTaskFileFetchTicket(ticket string) ([]byte, error) {
	if len(ticket) != 43 {
		return nil, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != ticket {
		return nil, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	hash := sha256.Sum256([]byte(ticket))
	return hash[:], nil
}

func digestSHA256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return fmt.Sprintf("%x", digest[:])
}

func taskFileFetchMediaTypeForPath(relativePath string) string {
	switch strings.ToLower(path.Ext(relativePath)) {
	case ".txt":
		return "text/plain"
	case ".md":
		return "text/markdown"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".pdf":
		return "application/pdf"
	default:
		return ""
	}
}

// TaskFileFetchMediaTypeForPath returns the fixed K4 media type for an allowed
// relative path extension. The extension list is deliberately closed.
func TaskFileFetchMediaTypeForPath(relativePath string) string {
	return taskFileFetchMediaTypeForPath(relativePath)
}

func taskFileFetchMediaTypeAllowed(value string) bool {
	parsed, params, err := mime.ParseMediaType(value)
	return err == nil && len(params) == 0 && parsed == value && taskFileFetchMediaTypeForPath("file"+extensionForTaskFileFetchMediaType(value)) != ""
}

func TaskFileFetchMediaTypeValid(value string) bool { return taskFileFetchMediaTypeAllowed(value) }

func extensionForTaskFileFetchMediaType(value string) string {
	switch value {
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "application/pdf":
		return ".pdf"
	default:
		return ""
	}
}
