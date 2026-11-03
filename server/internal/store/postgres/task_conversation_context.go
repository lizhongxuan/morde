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
)

const (
	taskConversationContextScopePersonal = "personal"
	taskConversationContextScopeProject  = "project"
	taskConversationContextQueryLimit    = 256
	taskConversationContextMaxBytes      = 32 << 10
)

type taskConversationContextManifest struct {
	OwnerID         string `json:"owner_id"`
	ProjectID       string `json:"project_id,omitempty"`
	InitiatorID     string `json:"initiator_id,omitempty"`
	ConversationID  string `json:"conversation_id"`
	Scope           string `json:"scope"`
	ThroughSequence int64  `json:"through_sequence"`
	MessageCount    int    `json:"message_count"`
	DigestSHA256    string `json:"digest_sha256"`
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
	WHERE owner_id=$1 AND conversation_id=$2`, value.OwnerID, value.ConversationID).Scan(&through)
	if err != nil {
		return task.ConversationContext{}, taskConversationContextManifest{}, fmt.Errorf("read conversation history boundary: %w", err)
	}
	messages, err := readTaskConversationContextMessages(ctx, tx, value.OwnerID, value.ConversationID, through)
	if err != nil {
		return task.ConversationContext{}, taskConversationContextManifest{}, err
	}
	contextValue := buildTaskConversationContext(value.OwnerID, value.ProjectID, value.InitiatorID, value.ConversationID, through, messages)
	manifest := taskConversationContextManifest{
		OwnerID: contextValue.OwnerID, ProjectID: contextValue.ProjectID, InitiatorID: contextValue.InitiatorID,
		ConversationID: contextValue.ConversationID, Scope: contextValue.Scope,
		ThroughSequence: contextValue.ThroughSequence, MessageCount: len(contextValue.Messages), DigestSHA256: hex.EncodeToString(contextValue.Digest),
	}
	return contextValue, manifest, nil
}

func readTaskConversationContext(ctx context.Context, tx pgx.Tx, value task.Task) (*task.ConversationContext, error) {
	var initialPayload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND sequence=1`, value.ID).Scan(&initialPayload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NewError(domain.CodeInvalidState, "Task initial event is unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("read initial Task event for conversation context: %w", err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(initialPayload, &event); err != nil || event == nil {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation context metadata is unavailable")
	}
	rawManifest, ok := event["conversation_context"]
	if !ok {
		return nil, nil
	}
	var manifest taskConversationContextManifest
	expectedScope := taskConversationContextScopePersonal
	if value.ProjectID != "" {
		expectedScope = taskConversationContextScopeProject
		if _, _, err := lockProjectForActorInTx(ctx, tx, value.InitiatorID, value.ProjectID, project.RoleMember, true); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation context metadata is invalid")
	}
	manifestMatchesIdentity := (manifest.ProjectID == "" && manifest.InitiatorID == "" && value.ProjectID == "") ||
		(manifest.ProjectID == value.ProjectID && manifest.InitiatorID == value.InitiatorID)
	if manifest.OwnerID != value.OwnerID ||
		manifest.ConversationID != value.ConversationID || manifest.Scope != expectedScope || !manifestMatchesIdentity ||
		manifest.ThroughSequence < 0 || manifest.MessageCount < 0 || manifest.MessageCount > task.MaxConversationContextMessages {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation context metadata is invalid")
	}
	if manifest.ProjectID != "" && (manifest.ProjectID != value.ProjectID || manifest.InitiatorID != value.InitiatorID) {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation context identity is invalid")
	}
	var conversationProjectID string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM conversations WHERE id=$1 AND owner_id=$2`, value.ConversationID, value.OwnerID).Scan(&conversationProjectID); err != nil || conversationProjectID != value.ProjectID {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation context scope changed")
	}
	expectedDigest, err := hex.DecodeString(manifest.DigestSHA256)
	if err != nil || len(expectedDigest) != sha256.Size {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation context digest is invalid")
	}
	var firstTaskMessageSequence int64
	err = tx.QueryRow(ctx, `SELECT MIN(sequence) FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND conversation_id=$3 AND role='user'`, value.ID, value.OwnerID, value.ConversationID).Scan(&firstTaskMessageSequence)
	if err != nil || firstTaskMessageSequence <= manifest.ThroughSequence {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation context boundary is invalid")
	}
	messages, err := readTaskConversationContextMessages(ctx, tx, value.OwnerID, value.ConversationID, manifest.ThroughSequence)
	if err != nil {
		return nil, err
	}
	contextValue := buildTaskConversationContext(value.OwnerID, value.ProjectID, value.InitiatorID, value.ConversationID, manifest.ThroughSequence, messages)
	if manifest.MessageCount != len(contextValue.Messages) || !bytes.Equal(expectedDigest, contextValue.Digest) {
		return nil, domain.NewError(domain.CodeInvalidState, "Task conversation history no longer matches its fixed boundary")
	}
	return &contextValue, nil
}

func readTaskConversationContextMessages(ctx context.Context, tx pgx.Tx, ownerID, conversationID string, through int64) ([]task.ConversationContextMessage, error) {
	rows, err := tx.Query(ctx, `SELECT m.id,m.sequence,m.role,m.content
FROM conversation_messages m
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.sequence <= $3
  AND (
    m.role='user'
    OR (m.role='assistant' AND (
      (m.task_id IS NULL AND m.source_kind IN ('history_import','legacy_instant'))
      OR (m.task_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM task_assistant_message_snapshots s
        JOIN tasks t ON t.id=s.task_id
        WHERE t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
          AND s.task_id=m.task_id AND s.message_id=m.id AND s.message_sequence=m.sequence
          AND s.content=m.content AND s.status='completed'
      ))
    ))
  )
ORDER BY m.sequence DESC LIMIT $4`, ownerID, conversationID, through, taskConversationContextQueryLimit)
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
			return nil, domain.NewError(domain.CodeInvalidState, "conversation history contains an invalid message")
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
