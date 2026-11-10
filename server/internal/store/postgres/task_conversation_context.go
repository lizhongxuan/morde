package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	taskConversationContextScopePersonal = "personal"
	taskConversationContextScopeProject  = "project"
	taskConversationContextQueryLimit    = 256
	taskConversationContextMaxBytes      = 32 << 10
)

// projectSharedHistoryEvidenceSQL may only be used after the caller has
// authorized membership in m's project conversation. It proves that another
// member's message belongs to a completed, published result in that same
// conversation. Batch sources remain verifiable even if old events are
// pruned; reviewed reply text is not required to match source results.
const projectSharedHistoryEvidenceSQL = `EXISTS (
  SELECT 1 FROM conversation_messages project_result
  JOIN conversations project_scope ON project_scope.id=project_result.conversation_id
    AND project_scope.owner_id=project_result.owner_id AND project_scope.project_id IS NOT NULL
  LEFT JOIN tasks project_task ON project_task.id=project_result.task_id
    AND project_task.owner_id=project_result.owner_id AND project_task.conversation_id=project_result.conversation_id
    AND project_task.initiator_id=project_result.actor_id AND project_task.project_id=project_scope.project_id
  LEFT JOIN conversation_result_batches project_batch ON project_batch.message_id=project_result.id
    AND project_batch.published_message_id=project_result.id AND project_batch.state='published'
    AND project_batch.owner_id=project_result.owner_id AND project_batch.actor_id=project_result.actor_id
    AND project_batch.conversation_id=project_result.conversation_id
  WHERE project_result.owner_id=m.owner_id AND project_result.conversation_id=m.conversation_id
    AND project_result.actor_id=m.actor_id AND project_result.role='assistant'
    AND project_result.source_kind NOT IN ('history_import','legacy_instant') AND project_result.delivery_status='applied'
    AND ((m.role='assistant' AND project_result.id=m.id) OR (m.role='user' AND m.task_id IS NOT NULL
      AND m.source_kind NOT IN ('history_import','legacy_instant') AND m.delivery_status='applied'
      AND NOT EXISTS (SELECT 1 FROM conversation_message_routes project_pending
        WHERE project_pending.message_id=m.id AND project_pending.state<>'routed') AND (
      project_result.task_id=m.task_id OR EXISTS (
        SELECT 1 FROM conversation_result_sources project_link
        WHERE project_link.batch_message_id=project_result.id AND project_link.task_id=m.task_id
          AND project_link.owner_id=m.owner_id AND project_link.actor_id=m.actor_id
          AND project_link.conversation_id=m.conversation_id AND project_link.state='published'
      )
    )))
    AND ((project_result.output_status='completed' AND project_task.status='completed'
      AND project_task.current_revision=project_result.output_task_revision
      AND project_task.desired_revision=project_result.output_task_revision AND EXISTS (
        SELECT 1 FROM task_events project_event WHERE project_event.task_id=project_task.id
          AND project_event.sequence=project_result.output_result_event_sequence
          AND project_event.task_revision=project_result.output_task_revision AND project_event.event_type='task.result'
          AND project_event.result_content_sha256=sha256(convert_to(project_result.content,'UTF8'))
      )) OR (project_result.output_status IS NULL AND project_batch.message_id IS NOT NULL
      AND project_batch.service_instance_id=(SELECT instance_id FROM service_identity WHERE singleton=true)
      AND EXISTS (SELECT 1 FROM conversation_result_sources project_source
        WHERE project_source.batch_message_id=project_batch.message_id AND project_source.state='published')
      AND NOT EXISTS (SELECT 1 FROM conversation_result_sources project_source
        LEFT JOIN tasks project_source_task ON project_source_task.id=project_source.task_id
          AND project_source_task.owner_id=project_source.owner_id AND project_source_task.initiator_id=project_source.actor_id
          AND project_source_task.conversation_id=project_source.conversation_id
          AND project_source_task.project_id=project_scope.project_id
        WHERE project_source.batch_message_id=project_batch.message_id AND project_source.state='published'
          AND (project_source_task.id IS NULL OR project_source_task.status<>'completed'
            OR project_source_task.current_revision<>project_source.task_revision
            OR project_source_task.desired_revision<>project_source.task_revision
            OR project_source_task.result IS DISTINCT FROM project_source.content
            OR project_source_task.current_step IS DISTINCT FROM project_source.turn_id
            OR project_source_task.auth_version IS DISTINCT FROM project_source.authorization_version
            OR project_source.authorization_version IS DISTINCT FROM project_batch.authorization_version
            OR project_source.payload_sha256 IS DISTINCT FROM sha256(convert_to(project_source.content,'UTF8'))))
    ))
)`

var errTaskContextSnapshotUnavailable = errors.New("task conversation snapshot unavailable")

func taskContextSnapshotError(message string) error {
	err := domain.NewError(domain.CodeInvalidState, message)
	err.Err = errTaskContextSnapshotUnavailable
	return err
}

type taskConversationContextManifest struct {
	OwnerID         string   `json:"owner_id"`
	ProjectID       string   `json:"project_id,omitempty"`
	InitiatorID     string   `json:"initiator_id,omitempty"`
	ConversationID  string   `json:"conversation_id"`
	Scope           string   `json:"scope"`
	ThroughSequence int64    `json:"through_sequence"`
	MessageCount    int      `json:"message_count"`
	MessageIDs      []string `json:"message_ids"`
	DigestSHA256    string   `json:"digest_sha256"`
}

func captureTaskConversationContext(ctx context.Context, tx pgx.Tx, ownerID, conversationID string) (task.ConversationContext, taskConversationContextManifest, error) {
	return captureTaskConversationContextForTask(ctx, tx, task.Task{
		OwnerID: ownerID, InitiatorID: ownerID, ConversationID: conversationID,
	})
}

func captureTaskConversationContextForTask(ctx context.Context, tx pgx.Tx, value task.Task) (task.ConversationContext, taskConversationContextManifest, error) {
	if value.ProjectID == "" {
		if value.InitiatorID != value.OwnerID {
			return task.ConversationContext{}, taskConversationContextManifest{}, domain.InvalidArgument("task.initiator_id", "personal context must use the conversation owner")
		}
	} else {
		if !project.ValidID(value.ProjectID) || !domain.ValidUserID(value.InitiatorID) {
			return task.ConversationContext{}, taskConversationContextManifest{}, domain.InvalidArgument("task.project_id", "project context scope is invalid")
		}
		if _, _, err := lockProjectForActorInTx(ctx, tx, value.InitiatorID, value.ProjectID, project.RoleMember, true); err != nil {
			return task.ConversationContext{}, taskConversationContextManifest{}, err
		}
	}
	var projectID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM conversations WHERE id=$1 AND owner_id=$2 FOR SHARE`, value.ConversationID, value.OwnerID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && projectID != value.ProjectID {
		return task.ConversationContext{}, taskConversationContextManifest{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if err != nil {
		return task.ConversationContext{}, taskConversationContextManifest{}, fmt.Errorf("verify conversation context scope: %w", err)
	}
	var through int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0) FROM conversation_messages
	WHERE owner_id=$1 AND conversation_id=$2 AND source_kind<>'task_result_review'`, value.OwnerID, value.ConversationID).Scan(&through)
	if err != nil {
		return task.ConversationContext{}, taskConversationContextManifest{}, fmt.Errorf("read conversation history boundary: %w", err)
	}
	messages, err := readTaskConversationContextMessages(ctx, tx, value.OwnerID, value.ConversationID, through, nil, true)
	if err != nil {
		return task.ConversationContext{}, taskConversationContextManifest{}, err
	}
	if value.ProjectID != "" {
		messages, err = filterConversationRouteTaskHistory(ctx, tx, value, messages)
		if err != nil {
			return task.ConversationContext{}, taskConversationContextManifest{}, err
		}
	}
	contextValue := buildTaskConversationContext(value.OwnerID, value.ProjectID, value.InitiatorID, value.ConversationID, through, messages)
	messageIDs := make([]string, 0, len(contextValue.Messages))
	for _, message := range contextValue.Messages {
		messageIDs = append(messageIDs, message.ID)
	}
	manifest := taskConversationContextManifest{
		OwnerID: contextValue.OwnerID, ProjectID: contextValue.ProjectID, InitiatorID: contextValue.InitiatorID,
		ConversationID: contextValue.ConversationID, Scope: contextValue.Scope,
		ThroughSequence: contextValue.ThroughSequence, MessageCount: len(contextValue.Messages), MessageIDs: messageIDs,
		DigestSHA256: hex.EncodeToString(contextValue.Digest),
	}
	return contextValue, manifest, nil
}

func readTaskConversationContext(ctx context.Context, tx pgx.Tx, value task.Task) (*task.ConversationContext, error) {
	return readTaskConversationContextWithProjectLock(ctx, tx, value, false)
}

// The recovery transaction already holds the project membership lock before
// locking the Task. Reusing that lock avoids a lock upgrade after the Task.
func readTaskConversationContextWithProjectLock(ctx context.Context, tx pgx.Tx, value task.Task, projectLocked bool) (*task.ConversationContext, error) {
	var initialPayload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND sequence=1`, value.ID).Scan(&initialPayload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, taskContextSnapshotError("Task initial event is unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("read initial Task event for conversation context: %w", err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(initialPayload, &event); err != nil || event == nil {
		return nil, taskContextSnapshotError("Task conversation context metadata is unavailable")
	}
	rawManifest, ok := event["conversation_context"]
	if !ok {
		return nil, taskContextSnapshotError("Task fixed conversation snapshot is unavailable; create a new task")
	}
	var manifest taskConversationContextManifest
	expectedScope := taskConversationContextScopePersonal
	if value.ProjectID != "" {
		expectedScope = taskConversationContextScopeProject
		if !projectLocked {
			if _, _, err := lockProjectForActorInTx(ctx, tx, value.InitiatorID, value.ProjectID, project.RoleMember, true); err != nil {
				return nil, err
			}
		}
	}
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, taskContextSnapshotError("Task conversation context metadata is invalid")
	}
	manifestMatchesIdentity := (manifest.ProjectID == "" && manifest.InitiatorID == "" && value.ProjectID == "") ||
		(manifest.ProjectID == value.ProjectID && manifest.InitiatorID == value.InitiatorID)
	if manifest.OwnerID != value.OwnerID ||
		manifest.ConversationID != value.ConversationID || manifest.Scope != expectedScope || !manifestMatchesIdentity ||
		manifest.ThroughSequence < 0 || manifest.MessageCount < 0 || manifest.MessageCount > task.MaxConversationContextMessages {
		return nil, taskContextSnapshotError("Task conversation context metadata is invalid")
	}
	if manifest.ProjectID != "" && (manifest.ProjectID != value.ProjectID || manifest.InitiatorID != value.InitiatorID) {
		return nil, taskContextSnapshotError("Task conversation context identity is invalid")
	}
	if manifest.MessageIDs == nil {
		return nil, taskContextSnapshotError("Task fixed conversation snapshot is unavailable; create a new task")
	}
	if len(manifest.MessageIDs) != manifest.MessageCount {
		return nil, taskContextSnapshotError("Task conversation context message references are invalid")
	}
	seen := make(map[string]struct{}, len(manifest.MessageIDs))
	for _, id := range manifest.MessageIDs {
		if !domain.ValidMessageID(id) {
			return nil, taskContextSnapshotError("Task conversation context message references are invalid")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, taskContextSnapshotError("Task conversation context message references are invalid")
		}
		seen[id] = struct{}{}
	}
	var conversationProjectID string
	err = tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM conversations WHERE id=$1 AND owner_id=$2`, value.ConversationID, value.OwnerID).Scan(&conversationProjectID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && conversationProjectID != value.ProjectID {
		return nil, taskContextSnapshotError("Task conversation context scope changed")
	}
	if err != nil {
		return nil, fmt.Errorf("read Task conversation context scope: %w", err)
	}
	expectedDigest, err := hex.DecodeString(manifest.DigestSHA256)
	if err != nil || len(expectedDigest) != sha256.Size {
		return nil, taskContextSnapshotError("Task conversation context digest is invalid")
	}
	var firstTaskMessageSequence pgtype.Int8
	err = tx.QueryRow(ctx, `SELECT MIN(m.sequence) FROM conversation_messages m
WHERE m.task_id=$1 AND m.owner_id=$2 AND m.conversation_id=$3 AND m.role='user'
 AND m.source_kind<>'task_result_review'
 AND NOT EXISTS (SELECT 1 FROM task_revision_intents revision WHERE revision.message_id=m.id AND revision.task_id=m.task_id)`,
		value.ID, value.OwnerID, value.ConversationID).Scan(&firstTaskMessageSequence)
	if err != nil {
		return nil, fmt.Errorf("read Task first message sequence: %w", err)
	}
	if !firstTaskMessageSequence.Valid || firstTaskMessageSequence.Int64 <= manifest.ThroughSequence {
		return nil, taskContextSnapshotError("Task conversation context boundary is invalid")
	}
	messages, err := readTaskConversationContextMessages(ctx, tx, value.OwnerID, value.ConversationID, manifest.ThroughSequence, manifest.MessageIDs, false)
	if err != nil {
		return nil, err
	}
	contextValue := buildTaskConversationContext(value.OwnerID, value.ProjectID, value.InitiatorID, value.ConversationID, manifest.ThroughSequence, messages)
	if manifest.MessageCount != len(contextValue.Messages) || !bytes.Equal(expectedDigest, contextValue.Digest) {
		return nil, taskContextSnapshotError("Task conversation history no longer matches its fixed boundary")
	}
	return &contextValue, nil
}

func readTaskConversationContextMessages(ctx context.Context, tx pgx.Tx, ownerID, conversationID string, through int64, messageIDs []string, completedOnly bool) ([]task.ConversationContextMessage, error) {
	rows, err := tx.Query(ctx, `SELECT m.id,m.sequence,m.role,m.content
FROM conversation_messages m
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.sequence <= $3
  AND m.source_kind<>'task_result_review'
  AND ($5::text[] IS NULL OR m.id=ANY($5::text[]))
  AND EXISTS (SELECT 1 FROM conversations scoped WHERE scoped.id=m.conversation_id AND scoped.owner_id=m.owner_id
    AND (scoped.project_id IS NOT NULL OR m.actor_id=scoped.owner_id))
  AND (NOT $6::boolean OR (m.delivery_status='applied' AND NOT EXISTS (
    SELECT 1 FROM conversation_message_routes pending WHERE pending.message_id=m.id AND pending.state<>'routed'
  )))
  AND (NOT $6::boolean OR m.task_id IS NULL OR EXISTS (
    SELECT 1 FROM tasks completed WHERE completed.id=m.task_id AND completed.status='completed'
      AND completed.owner_id=m.owner_id AND completed.conversation_id=m.conversation_id
  ))
  AND (
    m.role='user'
    OR (m.role='assistant' AND ((m.output_status IS NULL AND (
      (m.task_id IS NULL AND m.source_kind IN ('history_import','legacy_instant'))
      OR (m.task_id IS NULL AND EXISTS (
        SELECT 1 FROM conversation_result_batches b
        JOIN conversation_result_sources source ON source.batch_message_id=b.message_id
          AND source.owner_id=b.owner_id AND source.actor_id=b.actor_id AND source.conversation_id=b.conversation_id
          AND source.state='published'
        JOIN service_identity instance ON instance.singleton=true AND instance.instance_id=b.service_instance_id
        WHERE b.state='published' AND b.message_id=m.id AND b.published_message_id=m.id
          AND b.owner_id=m.owner_id AND b.actor_id=m.actor_id AND b.conversation_id=m.conversation_id
      ))
      OR (m.task_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM task_assistant_message_snapshots s
        JOIN tasks t ON t.id=s.task_id
        WHERE t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
          AND s.task_id=m.task_id AND s.message_id=m.id AND s.message_sequence=m.sequence
          AND s.content=m.content AND s.status='completed'
      ))
    )) OR (m.output_status='completed' AND EXISTS (
      SELECT 1 FROM tasks t JOIN task_events result ON result.task_id=t.id
      WHERE t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
        AND t.initiator_id=m.actor_id AND result.sequence=m.output_result_event_sequence
        AND result.task_revision=m.output_task_revision AND result.event_type='task.result'
        AND result.result_content_sha256=sha256(convert_to(m.content,'UTF8'))
    ))))
  )
ORDER BY m.sequence DESC LIMIT $4`, ownerID, conversationID, through, taskConversationContextQueryLimit, messageIDs, completedOnly)
	if err != nil {
		return nil, fmt.Errorf("read authorized conversation history: %w", err)
	}
	defer rows.Close()
	messages := make([]task.ConversationContextMessage, 0)
	for rows.Next() {
		var message task.ConversationContextMessage
		if err := rows.Scan(&message.ID, &message.Sequence, &message.Role, &message.Content); err != nil {
			return nil, fmt.Errorf("scan authorized conversation history: %w", err)
		}
		if message.ID == "" || message.Sequence < 1 || (message.Role != "user" && message.Role != "assistant") || !utf8.ValidString(message.Content) {
			return nil, taskContextSnapshotError("conversation history contains an invalid message")
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorized conversation history: %w", err)
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}

func buildTaskConversationContext(ownerID, projectID, initiatorID, conversationID string, through int64, messages []task.ConversationContextMessage) task.ConversationContext {
	bounded := boundTaskConversationContextMessages(messages)
	scope := taskConversationContextScopePersonal
	if projectID != "" {
		scope = taskConversationContextScopeProject
	}
	var encodedInput any
	if scope == taskConversationContextScopePersonal {
		encodedInput = struct {
			OwnerID         string                            `json:"owner_id"`
			ConversationID  string                            `json:"conversation_id"`
			Scope           string                            `json:"scope"`
			ThroughSequence int64                             `json:"through_sequence"`
			Messages        []task.ConversationContextMessage `json:"messages"`
		}{ownerID, conversationID, scope, through, bounded}
	} else {
		encodedInput = struct {
			OwnerID         string                            `json:"owner_id"`
			ProjectID       string                            `json:"project_id"`
			InitiatorID     string                            `json:"initiator_id"`
			ConversationID  string                            `json:"conversation_id"`
			Scope           string                            `json:"scope"`
			ThroughSequence int64                             `json:"through_sequence"`
			Messages        []task.ConversationContextMessage `json:"messages"`
		}{ownerID, projectID, initiatorID, conversationID, scope, through, bounded}
	}
	encoded, _ := json.Marshal(encodedInput)
	digest := sha256.Sum256(encoded)
	return task.ConversationContext{OwnerID: ownerID, ConversationID: conversationID,
		ProjectID: projectID, InitiatorID: initiatorID, Scope: scope, ThroughSequence: through,
		Digest: digest[:], Messages: bounded}
}

func boundTaskConversationContextMessages(messages []task.ConversationContextMessage) []task.ConversationContextMessage {
	selected := make([]task.ConversationContextMessage, 0, minInt(len(messages), task.MaxConversationContextMessages))
	remaining := taskConversationContextMaxBytes
	for index := len(messages) - 1; index >= 0 && len(selected) < task.MaxConversationContextMessages; index-- {
		message := messages[index]
		overhead := len(message.Role) + 8
		available := remaining - overhead
		if available <= 0 {
			break
		}
		if len(message.Content) > available {
			if len(selected) > 0 {
				break
			}
			message.Content = truncateTaskConversationContextText(message.Content, available)
			message.Truncated = true
			selected = append(selected, message)
			break
		}
		selected = append(selected, message)
		remaining -= len(message.Content) + overhead
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return selected
}

func truncateTaskConversationContextText(value string, limit int) string {
	const suffix = " …[truncated]"
	if limit <= len(suffix) {
		return suffix[:limit]
	}
	end := limit - len(suffix)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + suffix
}

func attachTaskConversationContextManifest(payload []byte, manifest taskConversationContextManifest) ([]byte, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil || event == nil {
		return nil, domain.InvalidArgument("event.payload", "initial Task event must be a JSON object")
	}
	if _, exists := event["conversation_context"]; exists {
		return nil, domain.InvalidArgument("event.payload", "initial Task event already contains conversation context metadata")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode Task conversation context manifest: %w", err)
	}
	event["conversation_context"] = encoded
	encoded, err = json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode initial Task event with conversation context: %w", err)
	}
	return encoded, nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
