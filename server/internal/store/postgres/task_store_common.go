package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const defaultPageSize int32 = 20
const maxPageSize int32 = 100

type pageCursor struct {
	CreatedAt time.Time
	ID        string
}

func pagination(pageSize int32, cursor string) (int32, pgtype.Timestamptz, string, error) {
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if pageSize < 1 || pageSize > maxPageSize {
		return 0, pgtype.Timestamptz{}, "", domain.InvalidArgument("limit", "page limit is out of range")
	}
	parsed, err := decodePageCursor(cursor)
	if err != nil {
		return 0, pgtype.Timestamptz{}, "", err
	}
	if parsed == nil {
		return pageSize, pgtype.Timestamptz{}, "", nil
	}
	return pageSize, taskTimestamp(parsed.CreatedAt), parsed.ID, nil
}

func encodePageCursor(createdAt time.Time, id string) string {
	value := createdAt.UTC().Format(time.RFC3339Nano) + "\n" + id
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodePageCursor(value string) (*pageCursor, error) {
	if value == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, domain.InvalidArgument("cursor", "page cursor is invalid")
	}
	parts := strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, domain.InvalidArgument("cursor", "page cursor is invalid")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, domain.InvalidArgument("cursor", "page cursor is invalid")
	}
	return &pageCursor{CreatedAt: createdAt, ID: parts[1]}, nil
}

func nextPageCursor[T any](items []T, pageSize int32, idAndTime func(T) (string, time.Time)) string {
	if int32(len(items)) <= pageSize || len(items) == 0 {
		return ""
	}
	item := items[pageSize-1]
	id, createdAt := idAndTime(item)
	return encodePageCursor(createdAt, id)
}

func reserveIdempotency(ctx context.Context, tx pgx.Tx, record app.IdempotencyRecord, resourceID, relatedID string) (sqlcgen.IdempotencyKey, bool, error) {
	if err := validateIdempotency(record); err != nil {
		return sqlcgen.IdempotencyKey{}, false, err
	}
	result, err := tx.Exec(ctx, `
INSERT INTO idempotency_keys (owner_id, operation, key, request_hash, resource_id, related_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (owner_id, operation, key) DO NOTHING`,
		record.OwnerID, record.Operation, record.Key, record.RequestHash, resourceID, relatedID)
	if err != nil {
		return sqlcgen.IdempotencyKey{}, false, fmt.Errorf("reserve idempotency key: %w", err)
	}
	if result.RowsAffected() == 1 {
		return sqlcgen.IdempotencyKey{
			OwnerID: record.OwnerID, Operation: record.Operation, Key: record.Key,
			RequestHash: record.RequestHash, ResourceID: resourceID, RelatedID: relatedID,
		}, false, nil
	}
	stored, err := getIdempotency(ctx, tx, record)
	if err != nil {
		return sqlcgen.IdempotencyKey{}, false, err
	}
	if !bytes.Equal(stored.RequestHash, record.RequestHash) {
		return sqlcgen.IdempotencyKey{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different request")
	}
	return stored, true, nil
}

func findIdempotency(ctx context.Context, tx pgx.Tx, record app.IdempotencyRecord) (sqlcgen.IdempotencyKey, bool, error) {
	if err := validateIdempotency(record); err != nil {
		return sqlcgen.IdempotencyKey{}, false, err
	}
	stored, err := getIdempotency(ctx, tx, record)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.IdempotencyKey{}, false, nil
	}
	if err != nil {
		return sqlcgen.IdempotencyKey{}, false, fmt.Errorf("find idempotency key: %w", err)
	}
	if !bytes.Equal(stored.RequestHash, record.RequestHash) {
		return sqlcgen.IdempotencyKey{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different request")
	}
	return stored, true, nil
}

func getIdempotency(ctx context.Context, tx pgx.Tx, record app.IdempotencyRecord) (sqlcgen.IdempotencyKey, error) {
	var row sqlcgen.IdempotencyKey
	err := tx.QueryRow(ctx, `
SELECT owner_id, operation, key, request_hash, resource_id, related_id, created_at
FROM idempotency_keys WHERE owner_id = $1 AND operation = $2 AND key = $3`,
		record.OwnerID, record.Operation, record.Key,
	).Scan(&row.OwnerID, &row.Operation, &row.Key, &row.RequestHash, &row.ResourceID, &row.RelatedID, &row.CreatedAt)
	return row, err
}

func validateIdempotency(record app.IdempotencyRecord) error {
	if !domain.ValidUserID(record.OwnerID) || record.Operation == "" || len(record.Operation) > 100 ||
		record.Key == "" || len(record.Key) > 255 || len(record.RequestHash) != 32 {
		return domain.InvalidArgument("idempotency_key", "idempotency fields are invalid")
	}
	return nil
}

func validateConversation(value conversation.Conversation) error {
	if !domain.ValidConversationID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		(value.HostID != "" && !domain.ValidHostID(value.HostID)) {
		return domain.InvalidArgument("conversation", "conversation fields are invalid")
	}
	return nil
}

func validateMessage(value conversation.Message) error {
	if !domain.ValidMessageID(value.ID) || !domain.ValidConversationID(value.ConversationID) ||
		!domain.ValidUserID(value.OwnerID) || (value.TaskID != "" && !domain.ValidTaskID(value.TaskID)) ||
		(value.Role != conversation.RoleUser && value.Role != conversation.RoleAssistant) || strings.TrimSpace(value.Content) == "" {
		return domain.InvalidArgument("message", "message fields are invalid")
	}
	return nil
}

func validateTask(value task.Task) error {
	validInitialStatus := value.Status == task.StatusQueued && value.WaitingReason == "" && value.CurrentStep == ""
	validInitialClarification := value.Status == task.StatusWaitingUser && value.WaitingReason == "clarification_required" &&
		value.FileDeliveryIntent == task.FileDeliveryUnclear && domain.ValidStepID(value.CurrentStep)
	if !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidConversationID(value.ConversationID) || (value.HostID != "" && !domain.ValidHostID(value.HostID)) ||
		value.ModelID == "" || strings.TrimSpace(value.Objective) == "" || (!validInitialStatus && !validInitialClarification) ||
		value.ExecutionLocation != "server" || value.Authorization.Version < 1 ||
		(value.ApplicationID != "" && (value.HostID == "" || !app.ValidHostApplicationID(value.ApplicationID))) ||
		!value.FileDeliveryIntent.Valid() ||
		(value.WorkspaceID == "") != (value.InputRevision == nil) ||
		(value.WorkspaceID != "" && !domain.ValidWorkspaceID(value.WorkspaceID)) ||
		(value.InputRevision != nil && *value.InputRevision < 0) || (len(value.ContextDigest) != 0 && len(value.ContextDigest) != 32) ||
		(value.WorkspaceID != "" && len(value.ContextDigest) != 32) ||
		(value.WorkspaceID == "") != (len(value.ContextDigest) == 0) ||
		value.AgentInstanceID != "" || value.AgentGeneration != 0 || value.AgentSessionID != "" ||
		value.AgentEngineVersion != "" || value.AgentProfileVersion != "" || len(value.CheckpointDigest) != 0 {
		return domain.InvalidArgument("task", "task fields are invalid")
	}
	if err := value.Budget.Validate(); err != nil {
		return domain.InvalidArgument("budget", err.Error())
	}
	if value.Authorization.Kind == "" {
		return domain.InvalidArgument("authorization", "authorization snapshot is required")
	}
	if value.Usage.ActiveSeconds != 0 || value.Usage.ToolCalls != 0 || value.Usage.ModelTokens != nil ||
		value.Lease.Owner != "" || value.Lease.Version != 0 || value.Lease.ExpiresAt != nil || value.FinishedAt != nil {
		return domain.InvalidArgument("task", "new tasks cannot include prior usage, lease, or completion data")
	}
	return nil
}

func normalizeNewTask(value task.Task) task.Task {
	if value.ExecutionLocation == "" {
		value.ExecutionLocation = "server"
	}
	if value.Authorization.Version == 0 {
		value.Authorization.Version = 1
	}
	if !value.FileDeliveryIntent.Valid() {
		value.FileDeliveryIntent = app.ClassifyFileDeliveryIntent(value.Objective, value.WorkspaceID != "")
	}
	return value
}

func validateInitialTaskEvent(eventType string, payload []byte) ([]byte, error) {
	if !validTaskEventType(eventType) {
		return nil, domain.InvalidArgument("event.type", "initial task event type is required")
	}
	if len(payload) == 0 {
		return []byte(`{}`), nil
	}
	if !json.Valid(payload) {
		return nil, domain.InvalidArgument("event.payload", "event payload must be valid JSON")
	}
	return payload, nil
}

func validTaskEventType(value string) bool {
	switch value {
	case "task.status", "task.step", "task.operation", "task.message", "task.batch_item", "task.artifact", "task.waiting_user", "task.response", "task.result":
		return true
	default:
		return false
	}
}

func invalidTaskField(field, message string) error {
	return domain.InvalidArgument(field, message)
}

func idempotencyConflict() error {
	return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different request")
}

func missingTaskError() error {
	return domain.NewError(domain.CodeNotFound, "task not found")
}
