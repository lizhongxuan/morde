package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Store) CreateConversation(ctx context.Context, value conversation.Conversation, key app.IdempotencyRecord) (conversation.Conversation, bool, error) {
	if err := validateConversation(value); err != nil {
		return conversation.Conversation{}, false, err
	}
	if value.ProjectID != "" || value.OwnerID != key.OwnerID {
		return conversation.Conversation{}, false, domain.InvalidArgument("owner_id", "conversation owner does not match idempotency owner")
	}
	var result conversation.Conversation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, replay, err := reserveIdempotency(ctx, tx, key, value.ID, "")
		if err != nil {
			return err
		}
		if replay {
			row, err := sqlcgen.New(tx).GetConversation(ctx, sqlcgen.GetConversationParams{ID: stored.ResourceID, OwnerID: key.OwnerID})
			if err != nil {
				return fmt.Errorf("load replayed conversation: %w", err)
			}
			result, replayed = conversationDomain(row), true
			return nil
		}
		_, err = tx.Exec(ctx, `
INSERT INTO conversations (id, owner_id, host_id, title, model_id)
VALUES ($1, $2, $3, $4, $5)`, value.ID, value.OwnerID, nullableText(value.HostID), value.Title, value.ModelID)
		if err != nil {
			return fmt.Errorf("create conversation: %w", err)
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{
			OwnerID: value.OwnerID, ObjectType: "conversation", ObjectID: value.ID, Operation: "upsert", MutationID: key.Key,
		}); err != nil {
			return err
		}
		created, err := sqlcgen.New(tx).GetConversation(ctx, sqlcgen.GetConversationParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return fmt.Errorf("read created conversation: %w", err)
		}
		result = conversationDomain(created)
		return nil
	})
	return result, replayed, err
}

// GetLegacyInstantChatReplay returns the canonical conversation and message
// pair copied by the instant-chat migration. It fails closed if the key exists
// but its canonical history is missing or inconsistent.
func (s *Store) GetLegacyInstantChatReplay(ctx context.Context, ownerID, key string, requestHash []byte) (app.LegacyInstantChatReplay, bool, error) {
	var storedHash []byte
	var userMessageID, assistantMessageID string
	err := s.pool.QueryRow(ctx, `SELECT request_hash, resource_id, related_id
FROM idempotency_keys WHERE owner_id=$1 AND operation='instant_chat.send' AND key=$2`, ownerID, key).
		Scan(&storedHash, &userMessageID, &assistantMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.LegacyInstantChatReplay{}, false, nil
	}
	if err != nil {
		return app.LegacyInstantChatReplay{}, false, fmt.Errorf("load legacy instant-chat idempotency record: %w", err)
	}
	if !bytes.Equal(storedHash, requestHash) {
		return app.LegacyInstantChatReplay{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused with different request")
	}

	var result app.LegacyInstantChatReplay
	var userRole, assistantRole string
	err = s.pool.QueryRow(ctx, `SELECT
  c.id, c.owner_id, c.model_id, c.title, c.created_at, c.updated_at,
  u.id, u.conversation_id, u.owner_id, u.role, u.content, u.model_id, u.sequence,
  u.source_kind, u.source_exchange_id, u.requested_conversation_id, u.created_at,
  a.id, a.conversation_id, a.owner_id, a.role, a.content, a.model_id, a.sequence,
  a.source_kind, a.source_exchange_id, a.requested_conversation_id, a.created_at
FROM idempotency_keys k
JOIN conversation_messages u ON u.id=k.resource_id AND u.owner_id=k.owner_id
JOIN conversation_messages a ON a.id=k.related_id AND a.owner_id=k.owner_id
JOIN conversations c ON c.id=u.conversation_id AND c.owner_id=u.owner_id
WHERE k.owner_id=$1 AND k.operation='instant_chat.send' AND k.key=$2
  AND u.id=$3 AND a.id=$4 AND u.conversation_id=a.conversation_id AND c.host_id IS NULL
  AND c.source_kind='legacy_instant' AND u.task_id IS NULL AND a.task_id IS NULL
  AND u.role='user' AND a.role='assistant'
  AND u.source_kind='legacy_instant' AND a.source_kind='legacy_instant'
  AND u.source_exchange_id<>'' AND u.source_exchange_id=a.source_exchange_id
  AND u.requested_conversation_id=a.requested_conversation_id
  AND u.model_id=c.model_id AND a.model_id=c.model_id`, ownerID, key, userMessageID, assistantMessageID).Scan(
		&result.Conversation.ID, &result.Conversation.OwnerID, &result.Conversation.ModelID, &result.Conversation.Title,
		&result.Conversation.CreatedAt, &result.Conversation.UpdatedAt,
		&result.UserMessage.ID, &result.UserMessage.ConversationID, &result.UserMessage.OwnerID, &userRole,
		&result.UserMessage.Content, &result.UserMessage.ModelID, &result.UserMessage.Sequence,
		&result.UserMessage.SourceKind, &result.UserMessage.SourceExchangeID, &result.UserMessage.RequestedConversationID, &result.UserMessage.CreatedAt,
		&result.AssistantMessage.ID, &result.AssistantMessage.ConversationID, &result.AssistantMessage.OwnerID, &assistantRole,
		&result.AssistantMessage.Content, &result.AssistantMessage.ModelID, &result.AssistantMessage.Sequence,
		&result.AssistantMessage.SourceKind, &result.AssistantMessage.SourceExchangeID, &result.AssistantMessage.RequestedConversationID, &result.AssistantMessage.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.LegacyInstantChatReplay{}, false, domain.NewError(domain.CodeInternal, "legacy instant-chat history is unavailable")
	}
	if err != nil {
		return app.LegacyInstantChatReplay{}, false, fmt.Errorf("load legacy instant-chat history: %w", err)
	}
	result.UserMessage.Role = conversation.Role(userRole)
	result.AssistantMessage.Role = conversation.Role(assistantRole)
	return result, true, nil
}

func (s *Store) GetConversation(ctx context.Context, id, ownerID string) (conversation.Conversation, error) {
	row, err := s.q.GetConversation(ctx, sqlcgen.GetConversationParams{ID: id, OwnerID: ownerID})
	if err != nil {
		return conversation.Conversation{}, storeError("get conversation", "conversation", err)
	}
	if row.ProjectID.Valid {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	return conversationDomain(row), nil
}

func (s *Store) CreateProjectConversation(ctx context.Context, actorID string, value conversation.Conversation, key app.IdempotencyRecord) (conversation.Conversation, bool, error) {
	if err := validateConversation(value); err != nil {
		return conversation.Conversation{}, false, err
	}
	if !project.ValidID(value.ProjectID) || value.OwnerID != actorID || key.OwnerID != actorID ||
		key.Operation != "project_conversation.create" || !domain.ValidUserID(actorID) {
		return conversation.Conversation{}, false, domain.InvalidArgument("conversation", "project conversation identity is invalid")
	}
	var result conversation.Conversation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, value.ProjectID, project.RoleMember, true); err != nil {
			return err
		}
		stored, replay, err := reserveIdempotency(ctx, tx, key, value.ID, "")
		if err != nil {
			return err
		}
		if replay {
			loaded, err := loadProjectConversationForActorInTx(ctx, tx, actorID, value.ProjectID, stored.ResourceID, false)
			if err != nil {
				return err
			}
			result, replayed = loaded, true
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversations (id, owner_id, project_id, host_id, title, model_id)
VALUES ($1, $2, $3, NULL, $4, $5)`, value.ID, value.OwnerID, value.ProjectID, value.Title, value.ModelID); err != nil {
			return fmt.Errorf("create project conversation: %w", err)
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{
			OwnerID: actorID, ObjectType: "conversation", ObjectID: value.ID, Operation: "upsert", MutationID: key.Key,
		}); err != nil {
			return err
		}
		result, err = loadProjectConversationForActorInTx(ctx, tx, actorID, value.ProjectID, value.ID, false)
		return err
	})
	return result, replayed, err
}

func (s *Store) GetProjectConversation(ctx context.Context, actorID, projectID, conversationID string) (conversation.Conversation, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidConversationID(conversationID) {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "project conversation not found")
	}
	var row sqlcgen.Conversation
	err := s.pool.QueryRow(ctx, `SELECT c.id,c.owner_id,c.host_id,c.title,c.created_at,c.updated_at,c.model_id,c.source_kind,c.message_seq,c.archived_at,c.project_id
FROM conversations c
JOIN project_members m ON m.project_id=c.project_id AND m.user_id=$2
JOIN users u ON u.id=m.user_id AND u.status='approved'
WHERE c.project_id=$1 AND c.id=$3 AND c.archived_at IS NULL`, projectID, actorID, conversationID).
		Scan(&row.ID, &row.OwnerID, &row.HostID, &row.Title, &row.CreatedAt, &row.UpdatedAt, &row.ModelID,
			&row.SourceKind, &row.MessageSeq, &row.ArchivedAt, &row.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "project conversation not found")
	}
	if err != nil {
		return conversation.Conversation{}, fmt.Errorf("get project conversation: %w", err)
	}
	return conversationDomain(row), nil
}

func (s *Store) ListProjectConversations(ctx context.Context, actorID, projectID string, page app.PageRequest) (app.ConversationPage, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return app.ConversationPage{}, domain.NewError(domain.CodeNotFound, "project not found")
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ConversationPage{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.owner_id,c.host_id,c.title,c.created_at,c.updated_at,c.model_id,c.source_kind,c.message_seq,c.archived_at,c.project_id
FROM conversations c
JOIN project_members m ON m.project_id=c.project_id AND m.user_id=$2
JOIN users u ON u.id=m.user_id AND u.status='approved'
WHERE c.project_id=$1 AND c.archived_at IS NULL
  AND ($3::timestamptz IS NULL OR c.created_at<$3 OR (c.created_at=$3 AND c.id<$4))
ORDER BY c.created_at DESC,c.id DESC LIMIT $5`, projectID, actorID, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.ConversationPage{}, fmt.Errorf("list project conversations: %w", err)
	}
	items := make([]conversation.Conversation, 0)
	for rows.Next() {
		var row sqlcgen.Conversation
		if err := rows.Scan(&row.ID, &row.OwnerID, &row.HostID, &row.Title, &row.CreatedAt, &row.UpdatedAt,
			&row.ModelID, &row.SourceKind, &row.MessageSeq, &row.ArchivedAt, &row.ProjectID); err != nil {
			rows.Close()
			return app.ConversationPage{}, fmt.Errorf("scan project conversation: %w", err)
		}
		items = append(items, conversationDomain(row))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.ConversationPage{}, fmt.Errorf("iterate project conversations: %w", err)
	}
	rows.Close()
	result := app.ConversationPage{Items: items}
	result.NextCursor = nextPageCursor(items, limit, func(value conversation.Conversation) (string, time.Time) {
		return value.ID, value.CreatedAt
	})
	if int32(len(items)) > limit {
		result.Items = items[:limit]
	}
	return result, nil
}

func loadProjectConversationForActorInTx(ctx context.Context, tx pgx.Tx, actorID, projectID, conversationID string, forUpdate bool) (conversation.Conversation, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF c"
	}
	var row sqlcgen.Conversation
	err := tx.QueryRow(ctx, `SELECT c.id,c.owner_id,c.host_id,c.title,c.created_at,c.updated_at,c.model_id,c.source_kind,c.message_seq,c.archived_at,c.project_id
FROM conversations c
JOIN project_members m ON m.project_id=c.project_id AND m.user_id=$2
JOIN users u ON u.id=m.user_id AND u.status='approved'
WHERE c.project_id=$1 AND c.id=$3 AND c.archived_at IS NULL`+lock, projectID, actorID, conversationID).
		Scan(&row.ID, &row.OwnerID, &row.HostID, &row.Title, &row.CreatedAt, &row.UpdatedAt, &row.ModelID,
			&row.SourceKind, &row.MessageSeq, &row.ArchivedAt, &row.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "project conversation not found")
	}
	if err != nil {
		return conversation.Conversation{}, fmt.Errorf("load project conversation: %w", err)
	}
	return conversationDomain(row), nil
}

func (s *Store) GetMainConversation(ctx context.Context, ownerID string) (conversation.MainConversation, error) {
	if !domain.ValidUserID(ownerID) {
		return conversation.MainConversation{}, domain.NewError(domain.CodeNotFound, "main conversation not found")
	}
	var id, storedConversationID pgtype.Text
	var version pgtype.Int8
	var archivedAt pgtype.Timestamptz
	var candidateCount int64
	err := s.pool.QueryRow(ctx, `SELECT p.conversation_id,p.version,c.id,c.archived_at,
  (SELECT count(*) FROM conversations candidate WHERE candidate.owner_id=$1 AND candidate.archived_at IS NULL)
FROM service_identity i
LEFT JOIN account_main_conversations p ON p.service_instance_id=i.instance_id AND p.owner_id=$1
LEFT JOIN conversations c ON c.id=p.conversation_id AND c.owner_id=p.owner_id
WHERE i.singleton=true`, ownerID).Scan(&id, &version, &storedConversationID, &archivedAt, &candidateCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.MainConversation{}, domain.NewError(domain.CodeInternal, "service instance identity is unavailable")
	}
	if err != nil {
		return conversation.MainConversation{}, fmt.Errorf("read main conversation: %w", err)
	}
	if id.Valid {
		if !version.Valid || !storedConversationID.Valid || archivedAt.Valid {
			return conversation.MainConversation{}, domain.NewError(domain.CodeInternal, "main conversation pointer is invalid")
		}
		return conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: id.String, Version: version.Int64}, nil
	}
	status := conversation.MainConversationUninitialized
	if candidateCount > 1 {
		status = conversation.MainConversationSelectionRequired
	}
	return conversation.MainConversation{Status: status}, nil
}

func (s *Store) EnsureMainConversation(ctx context.Context, value conversation.Conversation, record app.IdempotencyRecord) (conversation.MainConversation, bool, error) {
	if err := validateConversation(value); err != nil {
		return conversation.MainConversation{}, false, err
	}
	if record.OwnerID != value.OwnerID || record.Operation != "main_conversation.ensure" {
		return conversation.MainConversation{}, false, domain.InvalidArgument("main_conversation", "main conversation ensure fields are invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return conversation.MainConversation{}, false, err
	}
	var result conversation.MainConversation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		stored, replay, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replay {
			result, err = replayMainConversation(stored)
			replayed = err == nil
			return err
		}
		if err := lockConversationOwnerInTx(ctx, tx, value.OwnerID); err != nil {
			return err
		}
		stored, replay, err = findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replay {
			result, err = replayMainConversation(stored)
			replayed = err == nil
			return err
		}
		if id, version, exists, err := loadMainConversationPointer(ctx, tx, serviceInstanceID, value.OwnerID, true); err != nil {
			return err
		} else if exists {
			if err := lockAvailableConversationInTx(ctx, tx, value.OwnerID, id); err != nil {
				return err
			}
			result = conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: id, Version: version}
			return reserveMainConversationResult(ctx, tx, record, result)
		}

		rows, err := tx.Query(ctx, `SELECT id FROM conversations WHERE owner_id=$1 AND archived_at IS NULL ORDER BY created_at,id LIMIT 2 FOR SHARE`, value.OwnerID)
		if err != nil {
			return fmt.Errorf("list main conversation candidates: %w", err)
		}
		candidates := make([]string, 0, 2)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("read main conversation candidate: %w", err)
			}
			candidates = append(candidates, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read main conversation candidates: %w", err)
		}
		rows.Close()
		if len(candidates) > 1 {
			result = conversation.MainConversation{Status: conversation.MainConversationSelectionRequired}
			return nil
		}
		conversationID := value.ID
		if len(candidates) == 1 {
			conversationID = candidates[0]
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO conversations (id,owner_id,host_id,title) VALUES ($1,$2,NULL,'')`, value.ID, value.OwnerID); err != nil {
				return fmt.Errorf("create main conversation: %w", err)
			}
			if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{
				OwnerID: value.OwnerID, ObjectType: "conversation", ObjectID: value.ID, Operation: "upsert", MutationID: record.Key,
			}); err != nil {
				return err
			}
		}
		result = conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: conversationID, Version: 1}
		if _, err := tx.Exec(ctx, `INSERT INTO account_main_conversations (service_instance_id,owner_id,conversation_id,version)
VALUES ($1::uuid,$2,$3,$4)`, serviceInstanceID, value.OwnerID, conversationID, result.Version); err != nil {
			return fmt.Errorf("bind main conversation: %w", err)
		}
		return reserveMainConversationResult(ctx, tx, record, result)
	})
	return result, replayed, err
}

func (s *Store) SetMainConversation(ctx context.Context, ownerID, conversationID string, expectedVersion int64, record app.IdempotencyRecord) (conversation.MainConversation, bool, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidConversationID(conversationID) || expectedVersion < 0 ||
		record.OwnerID != ownerID || record.Operation != "main_conversation.set" {
		return conversation.MainConversation{}, false, domain.InvalidArgument("main_conversation", "main conversation selection is invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return conversation.MainConversation{}, false, err
	}
	var result conversation.MainConversation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		stored, replay, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replay {
			result, err = replayMainConversation(stored)
			replayed = err == nil
			return err
		}
		if err := lockConversationOwnerInTx(ctx, tx, ownerID); err != nil {
			return err
		}
		stored, replay, err = findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if replay {
			result, err = replayMainConversation(stored)
			replayed = err == nil
			return err
		}
		if err := lockAvailableConversationInTx(ctx, tx, ownerID, conversationID); err != nil {
			return err
		}
		_, currentVersion, exists, err := loadMainConversationPointer(ctx, tx, serviceInstanceID, ownerID, true)
		if err != nil {
			return err
		}
		if !exists {
			currentVersion = 0
		}
		if currentVersion != expectedVersion {
			return mainConversationVersionConflict(expectedVersion, currentVersion)
		}
		if currentVersion == math.MaxInt64 {
			return domain.NewError(domain.CodeConflict, "main conversation version is exhausted")
		}
		result = conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: conversationID, Version: currentVersion + 1}
		if _, err := tx.Exec(ctx, `INSERT INTO account_main_conversations (service_instance_id,owner_id,conversation_id,version)
VALUES ($1::uuid,$2,$3,$4)
ON CONFLICT (service_instance_id,owner_id) DO UPDATE SET
conversation_id=EXCLUDED.conversation_id,version=EXCLUDED.version,updated_at=clock_timestamp()`, serviceInstanceID, ownerID, conversationID, result.Version); err != nil {
			return fmt.Errorf("set main conversation: %w", err)
		}
		return reserveMainConversationResult(ctx, tx, record, result)
	})
	return result, replayed, err
}

func lockConversationOwnerInTx(ctx context.Context, tx pgx.Tx, ownerID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, ownerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "conversation owner is unavailable")
	}
	if err != nil {
		return fmt.Errorf("lock conversation owner: %w", err)
	}
	return nil
}

func loadMainConversationPointer(ctx context.Context, tx pgx.Tx, serviceInstanceID, ownerID string, forUpdate bool) (string, int64, bool, error) {
	query := `SELECT conversation_id,version FROM account_main_conversations WHERE service_instance_id=$1::uuid AND owner_id=$2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var conversationID string
	var version int64
	err := tx.QueryRow(ctx, query, serviceInstanceID, ownerID).Scan(&conversationID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("load main conversation pointer: %w", err)
	}
	return conversationID, version, true, nil
}

func lockAvailableConversationInTx(ctx context.Context, tx pgx.Tx, ownerID, conversationID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 AND project_id IS NULL AND archived_at IS NULL FOR SHARE`, conversationID, ownerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if err != nil {
		return fmt.Errorf("validate main conversation: %w", err)
	}
	return nil
}

func reserveMainConversationResult(ctx context.Context, tx pgx.Tx, record app.IdempotencyRecord, result conversation.MainConversation) error {
	_, _, err := reserveIdempotency(ctx, tx, record, result.ConversationID, strconv.FormatInt(result.Version, 10))
	return err
}

func replayMainConversation(stored sqlcgen.IdempotencyKey) (conversation.MainConversation, error) {
	version, err := strconv.ParseInt(stored.RelatedID, 10, 64)
	if err != nil || !domain.ValidConversationID(stored.ResourceID) || version < 1 {
		return conversation.MainConversation{}, domain.NewError(domain.CodeInternal, "main conversation idempotency result is invalid")
	}
	return conversation.MainConversation{Status: conversation.MainConversationReady, ConversationID: stored.ResourceID, Version: version}, nil
}

func mainConversationVersionConflict(expected, current int64) error {
	conflict := domain.NewError(domain.CodeConflict, "main conversation version changed")
	conflict.Details["expected_version"] = expected
	conflict.Details["current_version"] = current
	return conflict
}

func (s *Store) ListConversations(ctx context.Context, ownerID string, hostIDs []string, page app.PageRequest) (app.ConversationPage, error) {
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.ConversationPage{}, err
	}
	for _, hostID := range hostIDs {
		if !domain.ValidHostID(hostID) {
			return app.ConversationPage{}, domain.InvalidArgument("host_ids", "host id is invalid")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id,owner_id,host_id,title,created_at,updated_at,model_id,source_kind,message_seq,archived_at,project_id
FROM conversations
WHERE owner_id=$1 AND project_id IS NULL
  AND ($2::text[] IS NULL OR host_id=ANY($2::text[]))
  AND ($3::timestamptz IS NULL OR created_at<$3 OR (created_at=$3 AND id<$4))
ORDER BY created_at DESC,id DESC LIMIT $5`, ownerID, hostIDs, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.ConversationPage{}, fmt.Errorf("list conversations: %w", err)
	}
	items := make([]conversation.Conversation, 0)
	for rows.Next() {
		var row sqlcgen.Conversation
		if err := rows.Scan(&row.ID, &row.OwnerID, &row.HostID, &row.Title, &row.CreatedAt, &row.UpdatedAt,
			&row.ModelID, &row.SourceKind, &row.MessageSeq, &row.ArchivedAt, &row.ProjectID); err != nil {
			rows.Close()
			return app.ConversationPage{}, fmt.Errorf("scan conversation: %w", err)
		}
		items = append(items, conversationDomain(row))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.ConversationPage{}, fmt.Errorf("iterate conversations: %w", err)
	}
	rows.Close()
	pageResult := app.ConversationPage{Items: items}
	pageResult.NextCursor = nextPageCursor(items, limit, func(value conversation.Conversation) (string, time.Time) {
		return value.ID, value.CreatedAt
	})
	if int32(len(items)) > limit {
		pageResult.Items = items[:limit]
	}
	return pageResult, nil
}

func (s *Store) ListMessages(ctx context.Context, ownerID, conversationID string, page app.PageRequest) (app.MessagePage, error) {
	if _, err := s.GetConversation(ctx, conversationID, ownerID); err != nil {
		return app.MessagePage{}, err
	}
	limit, _, _, err := pagination(page.Limit, "")
	if err != nil {
		return app.MessagePage{}, err
	}
	var before int64
	if page.Cursor != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(page.Cursor)
		if decodeErr != nil {
			return app.MessagePage{}, domain.InvalidArgument("cursor", "message cursor is invalid")
		}
		before, decodeErr = strconv.ParseInt(string(decoded), 10, 64)
		if decodeErr != nil || before < 1 {
			return app.MessagePage{}, domain.InvalidArgument("cursor", "message cursor is invalid")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id,conversation_id,owner_id,role,content,task_id,created_at,model_id,source_kind,
source_exchange_id,requested_conversation_id,sequence,delivery_status,actor_id
FROM conversation_messages
WHERE owner_id=$1 AND conversation_id=$2 AND ($3::bigint=0 OR sequence<$3::bigint)
ORDER BY sequence DESC LIMIT $4`, ownerID, conversationID, before, limit+1)
	if err != nil {
		return app.MessagePage{}, fmt.Errorf("list conversation messages: %w", err)
	}
	items := make([]conversation.Message, 0)
	for rows.Next() {
		var row sqlcgen.ConversationMessage
		var deliveryStatus string
		if err := rows.Scan(&row.ID, &row.ConversationID, &row.OwnerID, &row.Role, &row.Content, &row.TaskID,
			&row.CreatedAt, &row.ModelID, &row.SourceKind, &row.SourceExchangeID, &row.RequestedConversationID,
			&row.Sequence, &deliveryStatus, &row.ActorID); err != nil {
			rows.Close()
			return app.MessagePage{}, fmt.Errorf("scan conversation message: %w", err)
		}
		message := messageDomain(row)
		message.DeliveryStatus = conversation.DeliveryStatus(deliveryStatus)
		items = append(items, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.MessagePage{}, fmt.Errorf("list conversation message rows: %w", err)
	}
	rows.Close()
	if err := s.attachConversationMessageAttachmentIDs(ctx, ownerID, conversationID, items); err != nil {
		return app.MessagePage{}, err
	}
	pageResult := app.MessagePage{Items: items}
	if int32(len(items)) > limit {
		pageResult.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(items[limit-1].Sequence, 10)))
		pageResult.Items = items[:limit]
	}
	return pageResult, nil
}

func (s *Store) ListProjectConversationMessages(ctx context.Context, actorID, projectID, conversationID string, page app.PageRequest) (app.MessagePage, error) {
	if _, err := s.GetProjectConversation(ctx, actorID, projectID, conversationID); err != nil {
		return app.MessagePage{}, err
	}
	limit, _, _, err := pagination(page.Limit, "")
	if err != nil {
		return app.MessagePage{}, err
	}
	var before int64
	if page.Cursor != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(page.Cursor)
		if decodeErr != nil {
			return app.MessagePage{}, domain.InvalidArgument("cursor", "message cursor is invalid")
		}
		before, decodeErr = strconv.ParseInt(string(decoded), 10, 64)
		if decodeErr != nil || before < 1 {
			return app.MessagePage{}, domain.InvalidArgument("cursor", "message cursor is invalid")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT m.id,m.conversation_id,m.owner_id,m.role,m.content,m.task_id,m.created_at,m.model_id,m.source_kind,
m.source_exchange_id,m.requested_conversation_id,m.sequence,m.delivery_status,m.actor_id
FROM conversation_messages m
JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id AND c.project_id=$1
JOIN project_members pm ON pm.project_id=c.project_id AND pm.user_id=$2
JOIN users u ON u.id=pm.user_id AND u.status='approved'
WHERE m.conversation_id=$3 AND ($4::bigint=0 OR m.sequence<$4::bigint)
ORDER BY m.sequence DESC LIMIT $5`, projectID, actorID, conversationID, before, limit+1)
	if err != nil {
		return app.MessagePage{}, fmt.Errorf("list project conversation messages: %w", err)
	}
	items := make([]conversation.Message, 0)
	for rows.Next() {
		var row sqlcgen.ConversationMessage
		if err := rows.Scan(&row.ID, &row.ConversationID, &row.OwnerID, &row.Role, &row.Content, &row.TaskID,
			&row.CreatedAt, &row.ModelID, &row.SourceKind, &row.SourceExchangeID, &row.RequestedConversationID,
			&row.Sequence, &row.DeliveryStatus, &row.ActorID); err != nil {
			rows.Close()
			return app.MessagePage{}, fmt.Errorf("scan project conversation message: %w", err)
		}
		message := messageDomain(row)
		message.DeliveryStatus = conversation.DeliveryStatus(row.DeliveryStatus)
		items = append(items, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.MessagePage{}, fmt.Errorf("iterate project conversation messages: %w", err)
	}
	rows.Close()
	result := app.MessagePage{Items: items}
	if int32(len(items)) > limit {
		result.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(items[limit-1].Sequence, 10)))
		result.Items = items[:limit]
	}
	return result, nil
}

func (s *Store) attachConversationMessageAttachmentIDs(ctx context.Context, ownerID, conversationID string, items []conversation.Message) error {
	if len(items) == 0 {
		return nil
	}
	instanceID, err := s.ServiceInstanceID(ctx)
	if err != nil {
		return err
	}
	messageIDs := make([]string, len(items))
	byMessageID := make(map[string]int, len(items))
	for index, item := range items {
		messageIDs[index] = item.ID
		byMessageID[item.ID] = index
	}
	rows, err := s.pool.Query(ctx, `SELECT message_id, attachment_id
FROM conversation_message_attachments
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND conversation_id=$3 AND message_id=ANY($4::text[])
ORDER BY message_id, attachment_id`, instanceID, ownerID, conversationID, messageIDs)
	if err != nil {
		return fmt.Errorf("list conversation message attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, attachmentID string
		if err := rows.Scan(&messageID, &attachmentID); err != nil {
			return fmt.Errorf("scan conversation message attachment: %w", err)
		}
		index, exists := byMessageID[messageID]
		if !exists || !personal.ValidPersonalAttachmentID(attachmentID) {
			return domain.NewError(domain.CodeInternal, "conversation message attachment is invalid")
		}
		items[index].AttachmentIDs = append(items[index].AttachmentIDs, attachmentID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list conversation message attachment rows: %w", err)
	}
	return nil
}

func (s *Store) SubmitTask(ctx context.Context, submission app.TaskSubmission) (app.TaskSubmissionResult, error) {
	submission.Task = normalizeNewTask(submission.Task)
	if submission.Task.InitiatorID == "" {
		submission.Task.InitiatorID = submission.Task.OwnerID
	}
	newConversation := submission.Conversation != nil
	if submission.Task.ConversationID == "" {
		switch {
		case submission.Message.ConversationID != "":
			submission.Task.ConversationID = submission.Message.ConversationID
		case submission.Conversation != nil && submission.Conversation.ID != "":
			submission.Task.ConversationID = submission.Conversation.ID
		default:
			id, err := domain.NewConversationID()
			if err != nil {
				return app.TaskSubmissionResult{}, fmt.Errorf("generate task conversation id: %w", err)
			}
			submission.Task.ConversationID = id
			newConversation = true
		}
	}
	if newConversation && submission.Conversation == nil {
		submission.Conversation = &conversation.Conversation{ID: submission.Task.ConversationID, OwnerID: submission.Task.OwnerID,
			ProjectID: submission.Task.ProjectID, HostID: submission.Task.HostID}
	}
	if submission.Conversation != nil {
		if submission.Conversation.ID == "" {
			submission.Conversation.ID = submission.Task.ConversationID
		}
		if submission.Conversation.HostID == "" {
			submission.Conversation.HostID = submission.Task.HostID
		}
		if submission.Conversation.ModelID == "" {
			submission.Conversation.ModelID = submission.Task.ModelID
		}
	}
	if submission.Message.ConversationID == "" {
		submission.Message.ConversationID = submission.Task.ConversationID
	}
	if submission.Message.ActorID == "" && submission.Task.ProjectID == "" {
		submission.Message.ActorID = submission.Message.OwnerID
	}
	if err := validateTaskConversationScope(submission.Task, submission.Task.ProjectID); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if err := validateTask(submission.Task); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if err := validateInitialFileDeliveryClarification(submission); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	message := submission.Message
	message.TaskID = submission.Task.ID
	if message.Role != conversation.RoleUser {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("message.role", "new task submissions must start with a user message")
	}
	if err := validateMessage(message); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if err := validateMessageConversationScope(message, submission.Task.ProjectID); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if submission.Task.OwnerID != message.OwnerID || submission.Task.ConversationID != message.ConversationID {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("task", "task and message associations do not match")
	}
	if submission.Conversation != nil {
		if err := validateConversation(*submission.Conversation); err != nil {
			return app.TaskSubmissionResult{}, err
		}
		if submission.Conversation.ID != submission.Task.ConversationID || submission.Conversation.OwnerID != submission.Task.OwnerID ||
			submission.Conversation.ProjectID != submission.Task.ProjectID {
			return app.TaskSubmissionResult{}, domain.InvalidArgument("conversation", "conversation and task associations do not match")
		}
	}
	eventPayload, err := validateInitialTaskEvent(submission.InitialEvent.Type, submission.InitialEvent.Payload)
	if err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if err := validateBatchItems(submission.Task.ID, submission.BatchItems); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if submission.Idempotency.OwnerID != submission.Task.InitiatorID {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("owner_id", "task initiator does not match idempotency owner")
	}

	var result app.TaskSubmissionResult
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if submission.Task.ProjectID != "" {
			if _, _, err := lockProjectForActorInTx(ctx, tx, submission.Task.InitiatorID, submission.Task.ProjectID, project.RoleMember, true); err != nil {
				return err
			}
		}
		stored, replay, err := reserveIdempotency(ctx, tx, submission.Idempotency, submission.Task.ID, message.ID)
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSubmissionResult(ctx, tx, submission.Task.OwnerID, stored.ResourceID, stored.RelatedID)
			if err == nil {
				result.Replayed = true
			}
			return err
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, submission.Task.InitiatorID, nil)
		if err != nil {
			return err
		}
		if submission.Task.HostID != "" {
			if err := lockTaskTargetHost(ctx, tx, submission.Task.HostID, submission.Task.InitiatorID); err != nil {
				return err
			}
		}
		if err := lockTaskWorkspaceInput(ctx, tx, submission.Task); err != nil {
			return err
		}
		if submission.Conversation != nil {
			_, err := tx.Exec(ctx, `INSERT INTO conversations (id, owner_id, project_id, host_id, title, model_id) VALUES ($1, $2, $3, $4, $5, $6)`,
				submission.Conversation.ID, submission.Conversation.OwnerID, nullableText(submission.Conversation.ProjectID), nullableText(submission.Conversation.HostID),
				submission.Conversation.Title, submission.Conversation.ModelID)
			if err != nil {
				return fmt.Errorf("create task conversation: %w", err)
			}
		} else {
			var id string
			err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 AND project_id IS NOT DISTINCT FROM $3::text FOR UPDATE`,
				submission.Task.ConversationID, submission.Task.OwnerID, nullableText(submission.Task.ProjectID)).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "conversation not found")
			}
			if err != nil {
				return fmt.Errorf("lock task conversation: %w", err)
			}
		}
		var activeTaskID string
		err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE conversation_id=$1 AND status IN ('queued','running','waiting_user','pausing','paused','cancelling') FOR UPDATE`,
			submission.Task.ConversationID).Scan(&activeTaskID)
		if err == nil {
			return domain.NewError(domain.CodeInvalidState, "conversation already has an active task")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check active conversation task: %w", err)
		}
		_, contextManifest, err := captureTaskConversationContextForTask(ctx, tx, submission.Task)
		if err != nil {
			return err
		}
		eventPayload, err = attachTaskConversationContextManifest(eventPayload, contextManifest)
		if err != nil {
			return err
		}
		if err := insertTask(ctx, tx, submission.Task, 1, gate.epoch); err != nil {
			return err
		}
		if err := insertInitialFileDeliveryClarification(ctx, tx, submission); err != nil {
			return err
		}
		if err := insertTaskMCPConnectorGrants(ctx, tx, submission.Task, submission.MCPConnectorIDs); err != nil {
			return err
		}
		if err := insertMessage(ctx, tx, message); err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{
			TaskID: submission.Task.ID, Sequence: 1, Type: submission.InitialEvent.Type, Payload: eventPayload,
		}); err != nil {
			return err
		}
		if err := insertBatchItems(ctx, tx, submission.Task.ID, submission.BatchItems); err != nil {
			return err
		}
		conversationHostID := nullableText(submission.Task.HostID)
		if submission.Task.ProjectID != "" {
			conversationHostID = pgtype.Text{}
		}
		_, err = tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp(), model_id=$3, host_id=$4 WHERE id=$1 AND owner_id=$2`,
			submission.Task.ConversationID, submission.Task.OwnerID, submission.Task.ModelID, conversationHostID)
		if err != nil {
			return fmt.Errorf("touch task conversation: %w", err)
		}
		if err := recordAccountObjectChanges(ctx, tx,
			accountObjectChange{OwnerID: message.OwnerID, ObjectType: "conversation", ObjectID: message.ConversationID, Operation: "upsert", MutationID: submission.Idempotency.Key},
			accountObjectChange{OwnerID: message.OwnerID, ObjectType: "message", ObjectID: message.ID, Operation: "upsert", MutationID: submission.Idempotency.Key},
		); err != nil {
			return err
		}
		result, err = loadSubmissionResult(ctx, tx, submission.Task.OwnerID, submission.Task.ID, message.ID)
		return err
	})
	return result, err
}

func (s *Store) AppendMessage(ctx context.Context, submission app.TaskSubmission) (app.TaskSubmissionResult, error) {
	submission.Task = normalizeNewTask(submission.Task)
	if submission.Task.InitiatorID == "" {
		submission.Task.InitiatorID = submission.Task.OwnerID
	}
	message := submission.Message
	if message.ActorID == "" && submission.Task.ProjectID == "" {
		message.ActorID = message.OwnerID
	}
	if !domain.ValidConversationID(message.ConversationID) || !domain.ValidUserID(message.OwnerID) ||
		message.Role != conversation.RoleUser || message.Content == "" || !domain.ValidMessageID(message.ID) {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("message", "message fields are invalid")
	}
	if submission.Task.OwnerID != message.OwnerID || submission.Task.ConversationID != message.ConversationID ||
		(submission.Task.HostID != "" && !domain.ValidHostID(submission.Task.HostID)) || submission.Task.ModelID == "" {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("task", "task routing fields do not match message")
	}
	if err := validateTaskConversationScope(submission.Task, submission.Task.ProjectID); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if err := validateMessageConversationScope(message, submission.Task.ProjectID); err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if submission.Idempotency.OwnerID != message.ActorID {
		return app.TaskSubmissionResult{}, domain.InvalidArgument("actor_id", "message actor does not match idempotency owner")
	}
	var result app.TaskSubmissionResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if submission.Task.ProjectID != "" {
			if _, _, err := lockProjectForActorInTx(ctx, tx, message.ActorID, submission.Task.ProjectID, project.RoleMember, true); err != nil {
				return err
			}
		} else if submission.Task.InitiatorID != message.OwnerID {
			return domain.InvalidArgument("actor_id", "personal task initiator must own the conversation")
		}
		stored, replay, err := findIdempotency(ctx, tx, submission.Idempotency)
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSubmissionResult(ctx, tx, message.OwnerID, stored.ResourceID, stored.RelatedID)
			if err == nil {
				result.Replayed = true
			}
			return err
		}
		var conversationRow sqlcgen.Conversation
		if submission.Task.ProjectID != "" {
			loaded, err := loadProjectConversationForActorInTx(ctx, tx, message.ActorID, submission.Task.ProjectID, message.ConversationID, true)
			if err != nil {
				return err
			}
			conversationRow.ID, conversationRow.OwnerID, conversationRow.Title, conversationRow.CreatedAt, conversationRow.UpdatedAt =
				loaded.ID, loaded.OwnerID, loaded.Title, pgtype.Timestamptz{Time: loaded.CreatedAt, Valid: true}, pgtype.Timestamptz{Time: loaded.UpdatedAt, Valid: true}
			conversationRow.HostID = nullableText(loaded.HostID)
			conversationRow.ProjectID = nullableText(loaded.ProjectID)
		} else {
			err := tx.QueryRow(ctx, `SELECT id, owner_id, host_id, title, created_at, updated_at, project_id FROM conversations
WHERE id=$1 AND owner_id=$2 AND project_id IS NULL FOR UPDATE`, message.ConversationID, message.OwnerID).
				Scan(&conversationRow.ID, &conversationRow.OwnerID, &conversationRow.HostID, &conversationRow.Title,
					&conversationRow.CreatedAt, &conversationRow.UpdatedAt, &conversationRow.ProjectID)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "conversation not found")
			}
			if err != nil {
				return fmt.Errorf("lock message conversation: %w", err)
			}
		}
		var active sqlcgen.Task
		err = tx.QueryRow(ctx, `SELECT id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes, auth_client_installation_id,
budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls, usage_model_tokens,
lease_owner, lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, created_at, updated_at, finished_at, application_id,
workspace_id, input_revision, context_digest, file_delivery_intent,
workspace_checkpoint_revision, project_id, initiator_id
FROM tasks WHERE conversation_id=$1 AND owner_id=$2 AND project_id IS NOT DISTINCT FROM $3::text
  AND status IN ('queued','running','waiting_user','pausing','paused','cancelling') FOR UPDATE`,
			message.ConversationID, message.OwnerID, nullableText(submission.Task.ProjectID)).Scan(
			&active.ID, &active.OwnerID, &active.ConversationID, &active.HostID, &active.ModelID, &active.Objective, &active.Status,
			&active.AuthKind, &active.AuthTokenID, &active.AuthScopes, &active.AuthClientInstallationID, &active.BudgetActiveSeconds, &active.BudgetToolCalls,
			&active.BudgetModelTokens, &active.UsageActiveSeconds, &active.UsageToolCalls, &active.UsageModelTokens,
			&active.LeaseOwner, &active.LeaseVersion, &active.LeaseExpiresAt, &active.EventSeq, &active.CurrentStep,
			&active.Result, &active.WaitingReason, &active.CreatedAt, &active.UpdatedAt, &active.FinishedAt, &active.ApplicationID,
			&active.WorkspaceID, &active.InputRevision, &active.ContextDigest, &active.FileDeliveryIntent,
			&active.WorkspaceCheckpointRevision, &active.ProjectID, &active.InitiatorID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find active conversation task: %w", err)
		}
		var targetTask task.Task
		newTask := errors.Is(err, pgx.ErrNoRows)
		if submission.ExpectedTaskID != "" && (newTask || active.ID != submission.ExpectedTaskID) {
			return domain.NewError(domain.CodeConflict, "active conversation task changed before the message was appended")
		}
		if newTask {
			candidate := submission.Task
			if candidate.OwnerID != message.OwnerID || candidate.ConversationID != message.ConversationID || candidate.ProjectID != conversationRow.ProjectID.String {
				return domain.InvalidArgument("task", "task scope does not match conversation")
			}
			if err := validateTask(candidate); err != nil {
				return err
			}
			if err := validateNormalizedTaskMCPConnectorIDs(submission.MCPConnectorIDs); err != nil {
				return err
			}
			targetTask = candidate
		} else {
			targetTask = taskDomain(active)
			if targetTask.ProjectID != submission.Task.ProjectID {
				return domain.NewError(domain.CodeInternal, "active task conversation scope is inconsistent")
			}
			if targetTask.Status == task.StatusPausing || targetTask.Status == task.StatusCancelling {
				return domain.NewError(domain.CodeInvalidState, "task is stopping and cannot accept another message")
			}
			var pendingApproval, pendingClarification bool
			if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM task_pending_requests WHERE task_id=$1 AND status='pending'),
EXISTS (SELECT 1 FROM task_clarification_requests WHERE task_id=$1 AND status='pending')`, targetTask.ID).
				Scan(&pendingApproval, &pendingClarification); err != nil {
				return fmt.Errorf("check unresolved task requests before appending message: %w", err)
			}
			if pendingApproval || pendingClarification {
				return domain.NewError(domain.CodeInvalidState, "task has an unresolved approval or clarification")
			}
			if targetTask.ProjectID != "" {
				if _, _, err := lockProjectForActorInTx(ctx, tx, targetTask.InitiatorID, targetTask.ProjectID, project.RoleMember, true); err != nil {
					return err
				}
			}
			if err := validateNormalizedTaskMCPConnectorIDs(submission.MCPConnectorIDs); err != nil {
				return err
			}
			if len(submission.MCPConnectorIDs) > 0 {
				if err := requireTaskMCPConnectorGrantSet(ctx, tx, targetTask.OwnerID, targetTask.ID, submission.MCPConnectorIDs); err != nil {
					return err
				}
			}
			var taskEpoch int64
			if err := tx.QueryRow(ctx, `SELECT execution_epoch FROM tasks WHERE id=$1 AND owner_id=$2`, targetTask.ID, targetTask.OwnerID).Scan(&taskEpoch); err != nil {
				return fmt.Errorf("read active task execution epoch: %w", err)
			}
			gate, err := requireOwnerExecutionEnabled(ctx, tx, targetTask.InitiatorID, nil)
			if err != nil {
				return err
			}
			if taskEpoch != gate.epoch {
				return ownerExecutionDisabledError(gate)
			}
			if targetTask.HostID != submission.Task.HostID || targetTask.ModelID != submission.Task.ModelID ||
				targetTask.ApplicationID != submission.Task.ApplicationID {
				return domain.NewError(domain.CodeInvalidState, "message host, model and application must match the active task")
			}
			if submission.Task.WorkspaceID != "" && (targetTask.WorkspaceID != submission.Task.WorkspaceID ||
				!sameTaskInputRevision(targetTask.InputRevision, submission.Task.InputRevision) ||
				!bytes.Equal(targetTask.ContextDigest, submission.Task.ContextDigest)) {
				return domain.NewError(domain.CodeInvalidState, "message workspace input must match the active task")
			}
		}
		var gate executionGate
		if newTask {
			gate, err = requireOwnerExecutionEnabled(ctx, tx, targetTask.InitiatorID, nil)
			if err != nil {
				return err
			}
		}
		if targetTask.HostID != "" {
			if err := lockTaskTargetHost(ctx, tx, targetTask.HostID, targetTask.InitiatorID); err != nil {
				return err
			}
		}
		if newTask {
			if err := lockTaskWorkspaceInput(ctx, tx, targetTask); err != nil {
				return err
			}
		} else if err := lockTaskWorkspaceForExecution(ctx, tx, targetTask.WorkspaceID, targetTask.InitiatorID); err != nil {
			return err
		}
		message.TaskID = targetTask.ID
		message.DeliveryStatus = conversation.DeliveryApplied
		if !newTask {
			switch targetTask.Status {
			case task.StatusPausing, task.StatusCancelling:
				message.DeliveryStatus = conversation.DeliveryUnknown
			default:
				message.DeliveryStatus = conversation.DeliveryPending
			}
		}
		if err := validateMessage(message); err != nil {
			return err
		}
		if err := validateMessageConversationScope(message, targetTask.ProjectID); err != nil {
			return err
		}
		stored, replay, err = reserveIdempotency(ctx, tx, submission.Idempotency, targetTask.ID, message.ID)
		if err != nil {
			return err
		}
		if replay {
			result, err = loadSubmissionResult(ctx, tx, message.OwnerID, stored.ResourceID, stored.RelatedID)
			if err == nil {
				result.Replayed = true
			}
			return err
		}
		if newTask {
			if err := validateInitialFileDeliveryClarification(submission); err != nil {
				return err
			}
			payload, err := validateInitialTaskEvent(submission.InitialEvent.Type, submission.InitialEvent.Payload)
			if err != nil {
				return err
			}
			_, contextManifest, err := captureTaskConversationContextForTask(ctx, tx, targetTask)
			if err != nil {
				return err
			}
			payload, err = attachTaskConversationContextManifest(payload, contextManifest)
			if err != nil {
				return err
			}
			if err := validateBatchItems(targetTask.ID, submission.BatchItems); err != nil {
				return err
			}
			if err := insertTask(ctx, tx, targetTask, 1, gate.epoch); err != nil {
				return err
			}
			if err := insertInitialFileDeliveryClarification(ctx, tx, submission); err != nil {
				return err
			}
			if err := insertTaskMCPConnectorGrants(ctx, tx, targetTask, submission.MCPConnectorIDs); err != nil {
				return err
			}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			if err := insertTaskEvent(ctx, tx, task.Event{TaskID: targetTask.ID, Sequence: 1, Type: submission.InitialEvent.Type, Payload: payload}); err != nil {
				return err
			}
			if err := insertBatchItems(ctx, tx, targetTask.ID, submission.BatchItems); err != nil {
				return err
			}
		} else if message.DeliveryStatus == conversation.DeliveryUnknown || targetTask.Status == task.StatusPaused {
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
		} else {
			payload, err := json.Marshal(map[string]string{"message_id": message.ID, "role": string(message.Role), "content": message.Content})
			if err != nil {
				return fmt.Errorf("encode task message event: %w", err)
			}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			status := string(targetTask.Status)
			var sequence int64
			err = tx.QueryRow(ctx, `UPDATE tasks SET status=$2,
file_delivery_intent=CASE
  WHEN file_delivery_intent='required' OR $3='required' THEN 'required'
  WHEN file_delivery_intent='unclear' OR $3='unclear' THEN 'unclear'
  ELSE 'none'
END,
event_seq=event_seq+1, updated_at=clock_timestamp()
WHERE id=$1 RETURNING event_seq`, targetTask.ID, status, string(submission.Task.FileDeliveryIntent)).Scan(&sequence)
			if err != nil {
				return fmt.Errorf("advance task after message: %w", err)
			}
			if err := insertTaskEvent(ctx, tx, task.Event{TaskID: targetTask.ID, Sequence: sequence, Type: "task.message", Payload: payload}); err != nil {
				return err
			}
		}
		conversationHostID := nullableText(targetTask.HostID)
		if targetTask.ProjectID != "" {
			conversationHostID = pgtype.Text{}
		}
		_, err = tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp(), model_id=$3, host_id=$4 WHERE id=$1 AND owner_id=$2`,
			message.ConversationID, message.OwnerID, targetTask.ModelID, conversationHostID)
		if err != nil {
			return fmt.Errorf("touch conversation after message: %w", err)
		}
		if err := recordAccountObjectChanges(ctx, tx,
			accountObjectChange{OwnerID: message.OwnerID, ObjectType: "conversation", ObjectID: message.ConversationID, Operation: "upsert", MutationID: submission.Idempotency.Key},
			accountObjectChange{OwnerID: message.OwnerID, ObjectType: "message", ObjectID: message.ID, Operation: "upsert", MutationID: submission.Idempotency.Key},
		); err != nil {
			return err
		}
		result, err = loadSubmissionResult(ctx, tx, message.OwnerID, targetTask.ID, message.ID)
		return err
	})
	return result, err
}

func validateTaskConversationScope(value task.Task, projectID string) error {
	if value.ProjectID != projectID {
		return domain.InvalidArgument("task.project_id", "task and conversation scopes do not match")
	}
	if projectID == "" {
		if value.InitiatorID != value.OwnerID {
			return domain.InvalidArgument("task.initiator_id", "personal task initiator must own the conversation")
		}
		return nil
	}
	if !project.ValidID(projectID) || !domain.ValidUserID(value.InitiatorID) {
		return domain.InvalidArgument("task.initiator_id", "project task initiator is invalid")
	}
	return nil
}

func validateMessageConversationScope(value conversation.Message, projectID string) error {
	if !domain.ValidUserID(value.ActorID) {
		return domain.InvalidArgument("message.actor_id", "message actor is invalid")
	}
	if projectID == "" && value.ActorID != value.OwnerID {
		return domain.InvalidArgument("message.actor_id", "personal message actor must own the conversation")
	}
	if projectID != "" && !project.ValidID(projectID) {
		return domain.InvalidArgument("message.actor_id", "project conversation scope is invalid")
	}
	return nil
}

func validateInitialFileDeliveryClarification(submission app.TaskSubmission) error {
	request := submission.InitialClarificationRequest
	if request == nil {
		if len(submission.InitialSteps) != 0 || submission.Task.Status != task.StatusQueued {
			return domain.InvalidArgument("clarification", "initial file delivery clarification is incomplete")
		}
		return nil
	}
	if len(submission.InitialSteps) != 2 || submission.Task.Status != task.StatusWaitingUser ||
		submission.Task.WaitingReason != "clarification_required" || submission.Task.FileDeliveryIntent != task.FileDeliveryUnclear ||
		request.TaskID != submission.Task.ID || request.Version != 1 || request.CallID != app.FileDeliveryIntentClarificationCallID ||
		request.Status != task.ClarificationPending || !domain.ValidPendingRequestID(request.ID) ||
		!validClarificationQuestion(request.Question) || request.Answer != "" || request.RespondedAt != nil {
		return domain.InvalidArgument("clarification", "initial file delivery clarification is invalid")
	}
	classification, clarification := submission.InitialSteps[0], submission.InitialSteps[1]
	if !domain.ValidStepID(classification.ID) || !domain.ValidStepID(clarification.ID) ||
		classification.ID == clarification.ID || classification.TaskID != submission.Task.ID || clarification.TaskID != submission.Task.ID ||
		classification.Sequence != 1 || clarification.Sequence != 2 || classification.Kind != "verification" ||
		classification.Status != task.StepSucceeded || clarification.Kind != "clarification" ||
		clarification.Status != task.StepWaitingUser || submission.Task.CurrentStep != clarification.ID ||
		request.ModelStepID != classification.ID || clarification.Summary != request.Question ||
		submission.InitialEvent.Type != "task.waiting_user" {
		return domain.InvalidArgument("clarification", "initial file delivery clarification steps do not match the task")
	}
	return nil
}

func insertInitialFileDeliveryClarification(ctx context.Context, tx pgx.Tx, submission app.TaskSubmission) error {
	if submission.InitialClarificationRequest == nil {
		return nil
	}
	for _, step := range submission.InitialSteps {
		if _, err := tx.Exec(ctx, `INSERT INTO task_steps (id, task_id, sequence, kind, status, title, summary) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			step.ID, step.TaskID, step.Sequence, step.Kind, string(step.Status), step.Title, step.Summary); err != nil {
			return fmt.Errorf("persist initial task step: %w", err)
		}
	}
	request := submission.InitialClarificationRequest
	if _, err := tx.Exec(ctx, `INSERT INTO task_clarification_requests (id, task_id, version, model_step_id, call_id, question, status)
VALUES ($1,$2,$3,$4,$5,$6,'pending')`, request.ID, request.TaskID, request.Version, request.ModelStepID, request.CallID, request.Question); err != nil {
		return fmt.Errorf("persist initial task clarification: %w", err)
	}
	return nil
}

// Hold the Host lock so deletion cannot race a task target check.
func lockTaskTargetHost(ctx context.Context, tx pgx.Tx, hostID, ownerID string) error {
	var exists bool
	err := tx.QueryRow(ctx, `
SELECT state<>'deleted' AND deleted_at IS NULL
FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE`, hostID, ownerID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !exists) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("lock task target Host: %w", err)
	}
	return nil
}

func lockTaskWorkspaceInput(ctx context.Context, tx pgx.Tx, value task.Task) error {
	if value.WorkspaceID == "" {
		return nil
	}
	if value.HostID == "" || value.InputRevision == nil || len(value.ContextDigest) != 32 {
		return domain.InvalidArgument("workspace_id", "workspace input snapshot is incomplete")
	}
	var ownerID, state string
	var revision int64
	err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, value.WorkspaceID).
		Scan(&ownerID, &state, &revision)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ownerID != value.InitiatorID || state != string(workspace.StateReady))) {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if err != nil {
		return fmt.Errorf("lock task input workspace: %w", err)
	}
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, value.WorkspaceID); err != nil {
		return err
	}
	if revision != *value.InputRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed before task submission")
	}
	var bindingOwner, bindingState, syncState string
	var desiredRevision, appliedRevision int64
	err = tx.QueryRow(ctx, `SELECT owner_id, state, sync_state, desired_revision, applied_revision
FROM workspace_bindings WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, value.HostID, value.WorkspaceID).
		Scan(&bindingOwner, &bindingState, &syncState, &desiredRevision, &appliedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if err != nil {
		return fmt.Errorf("lock task input workspace binding: %w", err)
	}
	if bindingOwner != value.InitiatorID || bindingState != string(workspace.BindingActive) {
		return domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if syncState != string(workspace.BindingSyncHealthy) || desiredRevision != revision || appliedRevision != revision {
		return domain.NewError(domain.CodeWorkspaceConflict, "Host has not applied the committed workspace input")
	}
	return nil
}

func lockTaskWorkspaceForExecution(ctx context.Context, tx pgx.Tx, workspaceID, initiatorID string) error {
	if workspaceID == "" {
		return nil
	}
	ownerID, err := lockWorkspaceOwnershipForRead(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if ownerID != initiatorID {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace is not available to this task initiator")
	}
	return nil
}

func sameTaskInputRevision(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// lockAIHost holds a shared Host row lock until the surrounding transaction
// ends. Control transitions take an exclusive Host lock before changing the
// fence, so task writes linearize before or after the AI-control change.
func lockAIHost(ctx context.Context, tx pgx.Tx, hostID, ownerID string) error {
	var allowed bool
	err := tx.QueryRow(ctx, `
SELECT state='running' AND control_state='ai' AND deleted_at IS NULL
FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE`, hostID, ownerID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("lock Host AI control fence: %w", err)
	}
	if !allowed {
		return domain.NewError(domain.CodeInvalidState, "host is not available for AI tasks")
	}
	return nil
}

func loadSubmissionResult(ctx context.Context, tx pgx.Tx, ownerID, taskID, messageID string) (app.TaskSubmissionResult, error) {
	queries := sqlcgen.New(tx)
	taskRow, err := queries.GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
	if err != nil {
		return app.TaskSubmissionResult{}, storeError("load submitted task", "task", err)
	}
	messageRow, err := queries.GetMessage(ctx, sqlcgen.GetMessageParams{ID: messageID, OwnerID: ownerID})
	if err != nil {
		return app.TaskSubmissionResult{}, storeError("load submitted message", "message", err)
	}
	var deliveryStatus string
	if err := tx.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1 AND owner_id=$2`, messageID, ownerID).Scan(&deliveryStatus); err != nil {
		return app.TaskSubmissionResult{}, fmt.Errorf("read submitted message delivery status: %w", err)
	}
	conversationRow, err := queries.GetConversation(ctx, sqlcgen.GetConversationParams{ID: taskRow.ConversationID, OwnerID: ownerID})
	if err != nil {
		return app.TaskSubmissionResult{}, storeError("load submitted conversation", "conversation", err)
	}
	message := messageDomain(messageRow)
	message.DeliveryStatus = conversation.DeliveryStatus(deliveryStatus)
	return app.TaskSubmissionResult{
		Task: taskDomain(taskRow), Message: message, Conversation: conversationDomain(conversationRow),
	}, nil
}

func insertTask(ctx context.Context, tx pgx.Tx, value task.Task, eventSequence int64, executionEpoch int64) error {
	value = normalizeNewTask(value)
	if executionEpoch < 1 {
		return missingExecutionGateError()
	}
	_, err := tx.Exec(ctx, `
INSERT INTO tasks (
  id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes,
  budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls,
  usage_model_tokens, lease_owner, lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, finished_at, execution_epoch, application_id,
  auth_client_installation_id, execution_location, workspace_id, input_revision, workspace_checkpoint_revision, context_digest, auth_version, auth_expires_at,
  file_delivery_intent, project_id, initiator_id
) VALUES (
  $1,$2,$3,$4,$5,$6,$7,$8,$9,COALESCE($10::text[], '{}'::text[]),$11,$12,$13,$14,$15,$16,'',0,NULL,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34
)`, value.ID, value.OwnerID, value.ConversationID, nullableText(value.HostID), value.ModelID, value.Objective,
		string(value.Status), value.Authorization.Kind, value.Authorization.TokenID, value.Authorization.Scopes,
		value.Budget.ActiveSeconds, value.Budget.ToolCalls, value.Budget.ModelTokens,
		value.Usage.ActiveSeconds, value.Usage.ToolCalls, int64(0),
		eventSequence, value.CurrentStep, value.Result, value.WaitingReason, nullableTimestamp(value.FinishedAt), executionEpoch, value.ApplicationID,
		nullableText(value.Authorization.ClientInstallationID), value.ExecutionLocation, nullableText(value.WorkspaceID),
		value.InputRevision, value.WorkspaceCheckpointRevision, value.ContextDigest, value.Authorization.Version, value.Authorization.ExpiresAt,
		string(value.FileDeliveryIntent), nullableText(value.ProjectID), value.InitiatorID)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func insertMessage(ctx context.Context, tx pgx.Tx, value conversation.Message) error {
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE conversations SET message_seq=message_seq+1 WHERE id=$1 AND owner_id=$2 RETURNING message_seq`,
		value.ConversationID, value.OwnerID).Scan(&sequence); err != nil {
		return storeError("reserve message sequence", "conversation", err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO conversation_messages
(id, conversation_id, owner_id, role, content, task_id, model_id, source_kind, source_exchange_id, requested_conversation_id, sequence, delivery_status, actor_id)
VALUES ($1,$2,$3,$4,$5,$6,COALESCE(NULLIF($7, ''), (SELECT model_id FROM tasks WHERE id=$6), ''),$8,$9,$10,$11,COALESCE(NULLIF($12,''),'applied'),COALESCE(NULLIF($13,''),(SELECT initiator_id FROM tasks WHERE id=$6),$3))`,
		value.ID, value.ConversationID, value.OwnerID, string(value.Role), value.Content, nullableText(value.TaskID), value.ModelID,
		stringOrDefault(value.SourceKind, "task"), value.SourceExchangeID, value.RequestedConversationID, sequence, string(value.DeliveryStatus), value.ActorID)
	if err != nil {
		return fmt.Errorf("insert conversation message: %w", err)
	}
	return nil
}

func insertTaskEvent(ctx context.Context, tx pgx.Tx, value task.Event) error {
	if !domain.ValidTaskID(value.TaskID) || value.Sequence < 1 || !validTaskEventType(value.Type) {
		return domain.InvalidArgument("event", "task event fields are invalid")
	}
	payload, err := eventPayload(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_events (task_id, sequence, event_type, payload) VALUES ($1,$2,$3,$4)`,
		value.TaskID, value.Sequence, value.Type, payload)
	if err != nil {
		return fmt.Errorf("insert task event: %w", err)
	}
	return nil
}

func validateBatchItems(taskID string, values []task.BatchItem) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !domain.ValidBatchItemID(value.ID) || (value.TaskID != "" && value.TaskID != taskID) || value.Key == "" || len(value.Key) > 255 || value.Input == "" || !validBatchStatus(value.Status) {
			return domain.InvalidArgument("batch_items", "batch item fields are invalid")
		}
		if _, exists := seen[value.Key]; exists {
			return domain.InvalidArgument("batch_items", "batch item keys must be unique")
		}
		seen[value.Key] = struct{}{}
	}
	return nil
}

func validBatchStatus(value task.BatchItemStatus) bool {
	switch value {
	case "", task.BatchItemQueued:
		return true
	default:
		return false
	}
}

func insertBatchItems(ctx context.Context, tx pgx.Tx, taskID string, values []task.BatchItem) error {
	for _, value := range values {
		_, err := tx.Exec(ctx, `INSERT INTO task_batch_items (id, task_id, item_key, input, status, result, error) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			value.ID, taskID, value.Key, value.Input, stringOrDefault(string(value.Status), string(task.BatchItemQueued)), value.Result, value.Error)
		if err != nil {
			return fmt.Errorf("insert task batch item: %w", err)
		}
	}
	return nil
}

func stringOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
