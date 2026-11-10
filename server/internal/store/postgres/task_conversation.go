package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Store) CreateConversation(ctx context.Context, value conversation.Conversation, key app.IdempotencyRecord) (conversation.Conversation, bool, error) {
	return s.createConversation(ctx, value, key, "")
}

func (s *Store) CreateConversationForServiceInstance(ctx context.Context, value conversation.Conversation, key app.IdempotencyRecord, instanceID string) (conversation.Conversation, bool, error) {
	if !domain.ValidServiceInstanceID(instanceID) {
		return conversation.Conversation{}, false, domain.InvalidArgument("service_instance_id", "service instance id is invalid")
	}
	return s.createConversation(ctx, value, key, instanceID)
}

func (s *Store) createConversation(ctx context.Context, value conversation.Conversation, key app.IdempotencyRecord, instanceID string) (conversation.Conversation, bool, error) {
	if err := validateConversation(value); err != nil {
		return conversation.Conversation{}, false, err
	}
	if value.ProjectID != "" || value.OwnerID != key.OwnerID {
		return conversation.Conversation{}, false, domain.InvalidArgument("owner_id", "conversation owner does not match idempotency owner")
	}
	var result conversation.Conversation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if instanceID != "" {
			if err := requireConversationServiceInstance(ctx, tx, instanceID); err != nil {
				return err
			}
		}
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
	return s.createProjectConversation(ctx, actorID, value, key, "")
}

func (s *Store) CreateProjectConversationForServiceInstance(ctx context.Context, actorID string, value conversation.Conversation, key app.IdempotencyRecord, instanceID string) (conversation.Conversation, bool, error) {
	if !domain.ValidServiceInstanceID(instanceID) {
		return conversation.Conversation{}, false, domain.InvalidArgument("service_instance_id", "service instance id is invalid")
	}
	return s.createProjectConversation(ctx, actorID, value, key, instanceID)
}

func (s *Store) createProjectConversation(ctx context.Context, actorID string, value conversation.Conversation, key app.IdempotencyRecord, instanceID string) (conversation.Conversation, bool, error) {
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
		if instanceID != "" {
			if err := requireConversationServiceInstance(ctx, tx, instanceID); err != nil {
				return err
			}
		}
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
  (SELECT count(*) FROM conversations candidate WHERE candidate.owner_id=$1 AND candidate.project_id IS NULL AND candidate.archived_at IS NULL)
FROM service_identity i
LEFT JOIN account_main_conversations p ON p.service_instance_id=i.instance_id AND p.owner_id=$1
LEFT JOIN conversations c ON c.id=p.conversation_id AND c.owner_id=p.owner_id AND c.project_id IS NULL
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
			result, err = replayAvailableMainConversation(ctx, tx, value.OwnerID, stored)
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
			result, err = replayAvailableMainConversation(ctx, tx, value.OwnerID, stored)
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

		rows, err := tx.Query(ctx, `SELECT id FROM conversations WHERE owner_id=$1 AND project_id IS NULL AND archived_at IS NULL ORDER BY created_at,id LIMIT 2 FOR SHARE`, value.OwnerID)
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
			result, err = replayAvailableMainConversation(ctx, tx, ownerID, stored)
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
			result, err = replayAvailableMainConversation(ctx, tx, ownerID, stored)
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

func replayAvailableMainConversation(ctx context.Context, tx pgx.Tx, ownerID string, stored sqlcgen.IdempotencyKey) (conversation.MainConversation, error) {
	result, err := replayMainConversation(stored)
	if err != nil {
		return conversation.MainConversation{}, err
	}
	if err := lockAvailableConversationInTx(ctx, tx, ownerID, result.ConversationID); err != nil {
		return conversation.MainConversation{}, err
	}
	return result, nil
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
	rows, err := s.pool.Query(ctx, `SELECT m.id,m.conversation_id,m.owner_id,m.role,m.content,m.task_id,m.created_at,m.model_id,m.source_kind,
m.source_exchange_id,m.requested_conversation_id,m.sequence,m.delivery_status,m.actor_id,
COALESCE(r.client_message_id,''),COALESCE(r.state,''),COALESCE(r.failure_reason,''),
CASE WHEN m.output_status='streaming' AND NOT EXISTS (
 SELECT 1 FROM tasks live WHERE live.id=m.task_id AND live.status='running' AND live.lease_expires_at>clock_timestamp()
  AND live.current_revision=live.desired_revision AND live.current_revision=m.output_task_revision
) THEN 'interrupted' ELSE COALESCE(m.output_status,'') END,m.output_task_revision,m.output_result_event_sequence
FROM conversation_messages m LEFT JOIN conversation_message_routes r ON r.message_id=m.id
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.source_kind<>'task_result_review'
  AND ($3::bigint=0 OR m.sequence<$3::bigint)
ORDER BY m.sequence DESC LIMIT $4`, ownerID, conversationID, before, limit+1)
	if err != nil {
		return app.MessagePage{}, fmt.Errorf("list conversation messages: %w", err)
	}
	items := make([]conversation.Message, 0)
	for rows.Next() {
		var row sqlcgen.ConversationMessage
		var deliveryStatus, clientID, routingStatus, routingFailure string
		var outputStatus string
		var outputRevision, outputEvent pgtype.Int8
		if err := rows.Scan(&row.ID, &row.ConversationID, &row.OwnerID, &row.Role, &row.Content, &row.TaskID,
			&row.CreatedAt, &row.ModelID, &row.SourceKind, &row.SourceExchangeID, &row.RequestedConversationID,
			&row.Sequence, &deliveryStatus, &row.ActorID, &clientID, &routingStatus, &routingFailure,
			&outputStatus, &outputRevision, &outputEvent); err != nil {
			rows.Close()
			return app.MessagePage{}, fmt.Errorf("scan conversation message: %w", err)
		}
		message := messageDomain(row)
		message.DeliveryStatus = conversation.DeliveryStatus(deliveryStatus)
		message.ClientMessageID = clientID
		message.RoutingStatus = conversation.RoutingStatus(routingStatus)
		message.RoutingFailureReason = conversation.RoutingFailureReason(routingFailure)
		applyConversationOutputProjection(&message, outputStatus, outputRevision, outputEvent)
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
	if err := s.attachConversationResultSources(ctx, ownerID, conversationID, items); err != nil {
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
m.source_exchange_id,m.requested_conversation_id,m.sequence,m.delivery_status,m.actor_id,
CASE WHEN r.actor_id=$2 THEN COALESCE(r.client_message_id,'') ELSE '' END,COALESCE(r.state,''),COALESCE(r.failure_reason,''),
CASE WHEN m.output_status='streaming' AND NOT EXISTS (
 SELECT 1 FROM tasks live WHERE live.id=m.task_id AND live.status='running' AND live.lease_expires_at>clock_timestamp()
  AND live.current_revision=live.desired_revision AND live.current_revision=m.output_task_revision
) THEN 'interrupted' ELSE COALESCE(m.output_status,'') END,m.output_task_revision,m.output_result_event_sequence
FROM conversation_messages m
JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id AND c.project_id=$1
JOIN project_members pm ON pm.project_id=c.project_id AND pm.user_id=$2
JOIN users u ON u.id=pm.user_id AND u.status='approved'
LEFT JOIN conversation_message_routes r ON r.message_id=m.id
WHERE m.conversation_id=$3 AND m.source_kind<>'task_result_review'
  AND ($4::bigint=0 OR m.sequence<$4::bigint)
ORDER BY m.sequence DESC LIMIT $5`, projectID, actorID, conversationID, before, limit+1)
	if err != nil {
		return app.MessagePage{}, fmt.Errorf("list project conversation messages: %w", err)
	}
	items := make([]conversation.Message, 0)
	for rows.Next() {
		var row sqlcgen.ConversationMessage
		var clientID, routingStatus, routingFailure string
		var outputStatus string
		var outputRevision, outputEvent pgtype.Int8
		if err := rows.Scan(&row.ID, &row.ConversationID, &row.OwnerID, &row.Role, &row.Content, &row.TaskID,
			&row.CreatedAt, &row.ModelID, &row.SourceKind, &row.SourceExchangeID, &row.RequestedConversationID,
			&row.Sequence, &row.DeliveryStatus, &row.ActorID, &clientID, &routingStatus, &routingFailure,
			&outputStatus, &outputRevision, &outputEvent); err != nil {
			rows.Close()
			return app.MessagePage{}, fmt.Errorf("scan project conversation message: %w", err)
		}
		message := messageDomain(row)
		message.DeliveryStatus = conversation.DeliveryStatus(row.DeliveryStatus)
		message.ClientMessageID = clientID
		message.RoutingStatus = conversation.RoutingStatus(routingStatus)
		message.RoutingFailureReason = conversation.RoutingFailureReason(routingFailure)
		applyConversationOutputProjection(&message, outputStatus, outputRevision, outputEvent)
		items = append(items, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.MessagePage{}, fmt.Errorf("iterate project conversation messages: %w", err)
	}
	rows.Close()
	if err := s.attachProjectConversationMessageAttachmentIDs(ctx, actorID, projectID, conversationID, items); err != nil {
		return app.MessagePage{}, err
	}
	if err := s.attachConversationResultSources(ctx, actorID, conversationID, items); err != nil {
		return app.MessagePage{}, err
	}
	result := app.MessagePage{Items: items}
	if int32(len(items)) > limit {
		result.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(items[limit-1].Sequence, 10)))
		result.Items = items[:limit]
	}
	return result, nil
}

func applyConversationOutputProjection(message *conversation.Message, status string, revision, event pgtype.Int8) {
	message.OutputStatus = conversation.OutputStatus(status)
	if revision.Valid {
		message.OutputTaskRevision = &revision.Int64
	}
	if event.Valid {
		message.OutputResultEventSequence = &event.Int64
	}
}

func (s *Store) attachConversationResultSources(ctx context.Context, actorID, conversationID string, items []conversation.Message) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Role == conversation.RoleAssistant {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sources, err := s.ListConversationResultSources(ctx, actorID, conversationID, ids)
	if err != nil {
		return err
	}
	for index := range items {
		for _, source := range sources[items[index].ID] {
			items[index].Sources = append(items[index].Sources, conversation.MessageSource{UserMessageID: source.UserMessageID,
				TaskID: source.TaskID, TaskRevision: source.TaskRevision, EventSequence: source.EventSequence, Content: source.Content})
		}
	}
	return nil
}

func (s *Store) ListConversationActiveTasks(ctx context.Context, actorID, projectID, conversationID string, hostIDs []string, page app.PageRequest) (app.TaskPage, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidConversationID(conversationID) || (projectID != "" && !project.ValidID(projectID)) {
		return app.TaskPage{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	for _, hostID := range hostIDs {
		if !domain.ValidHostID(hostID) {
			return app.TaskPage{}, domain.InvalidArgument("host_ids", "host id is invalid")
		}
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.TaskPage{}, err
	}
	var rows []sqlcgen.Task
	var items []task.Task
	var k2Projections map[string]app.TaskK2Projection
	err = s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		if projectID != "" {
			if _, err := loadProjectConversationForActorInTx(ctx, tx, actorID, projectID, conversationID, false); err != nil {
				return err
			}
		} else {
			value, err := sqlcgen.New(tx).GetConversation(ctx, sqlcgen.GetConversationParams{ID: conversationID, OwnerID: actorID})
			if err != nil {
				return storeError("get conversation", "conversation", err)
			}
			if value.ProjectID.Valid {
				return domain.NewError(domain.CodeNotFound, "conversation not found")
			}
		}
		var err error
		rows, err = sqlcgen.New(tx).ListConversationActiveTasks(ctx, sqlcgen.ListConversationActiveTasksParams{
			ConversationID: conversationID, ProjectID: nullableText(projectID), ActorID: actorID,
			HostIds: hostIDs, CursorAt: cursorAt, CursorID: cursorID, PageLimit: limit + 1,
		})
		if err != nil {
			return err
		}
		items = mapRows(rows, rowTaskDomain)
		if err := populateTaskStepCounts(ctx, tx, items); err != nil {
			return err
		}
		if err := populateTaskRevisionProjection(ctx, tx, items); err != nil {
			return err
		}
		projectionItems := items
		if int32(len(projectionItems)) > limit {
			projectionItems = projectionItems[:limit]
		}
		k2Projections, err = loadTaskK2Projections(ctx, tx, projectionItems)
		return err
	})
	if err != nil {
		return app.TaskPage{}, fmt.Errorf("list active conversation tasks: %w", err)
	}
	result := app.TaskPage{Items: items, K2Projections: k2Projections}
	result.NextCursor = nextPageCursor(items, limit, func(value task.Task) (string, time.Time) { return value.ID, value.CreatedAt })
	if int32(len(items)) > limit {
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
		serviceInstanceID := ""
		if app.ExpectedServiceInstance(ctx) != "" || len(message.AttachmentIDs) != 0 {
			serviceInstanceID, err = personalServiceInstanceIDInTx(ctx, tx)
			if err != nil {
				return err
			}
		}
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
		if submission.Task.ProjectID != "" && submission.Task.WorkspaceID != "" {
			ownerID, _, err := lockProjectTaskWorkspaceAccess(ctx, tx, submission.Task.ProjectID,
				submission.Task.InitiatorID, submission.Task.WorkspaceID, false)
			if err != nil {
				return err
			}
			if ownerID != submission.Task.InitiatorID && submission.Task.HostID != "" {
				grant, err := lockProjectWorkspaceGrant(ctx, tx, submission.Task.InitiatorID, ownerID, submission.Task.WorkspaceID)
				if err != nil || grant.hostID != submission.Task.HostID {
					return domain.NewError(domain.CodeForbidden, "project Workspace is not bound to the selected Host")
				}
			}
			if submission.Task.HostID != "" {
				if err := lockProjectWorkspaceHost(ctx, tx, submission.Task.HostID, ownerID); err != nil {
					return err
				}
			}
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
		if len(message.AttachmentIDs) != 0 {
			uploadIDs, err := lockAndValidateHistoryAttachments(ctx, tx, serviceInstanceID, message.OwnerID, message.AttachmentIDs)
			if err != nil {
				return err
			}
			if err := insertHistoryMessageAttachments(ctx, tx, serviceInstanceID, message.OwnerID, message.ConversationID, message.ID, message.AttachmentIDs, uploadIDs); err != nil {
				return err
			}
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

// Hold the Host lock so deletion and member revocation cannot race a task
// target check. The actor may be the owner or an approved member with control.
func lockTaskTargetHost(ctx context.Context, tx pgx.Tx, hostID, actorID string) error {
	var ownerID string
	var available bool
	err := tx.QueryRow(ctx, `
SELECT owner_id,state<>'deleted' AND deleted_at IS NULL
FROM hosts WHERE id=$1 FOR SHARE`, hostID).Scan(&ownerID, &available)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !available) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("lock task target Host: %w", err)
	}
	allowed, err := hasControlPermission(ctx, tx, hostID, ownerID, actorID, string(host.PermissionDesktopControl))
	if err != nil {
		return fmt.Errorf("authorize task target Host: %w", err)
	}
	if !allowed {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	return nil
}

func lockTaskWorkspaceInput(ctx context.Context, tx pgx.Tx, value task.Task) error {
	if value.WorkspaceID == "" {
		return nil
	}
	if value.InputRevision == nil || len(value.ContextDigest) != 32 {
		return domain.InvalidArgument("workspace_id", "workspace input snapshot is incomplete")
	}
	workspaceOwnerID := value.InitiatorID
	if value.ProjectID != "" {
		ownerID, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID, value.InitiatorID, value.WorkspaceID, false)
		if err != nil {
			return err
		}
		if value.WorkspaceOwnerID != "" && value.WorkspaceOwnerID != ownerID ||
			value.WorkspaceProjectResourceVersion != nil && *value.WorkspaceProjectResourceVersion != version {
			return domain.NewError(domain.CodeConflict, "project Workspace resource changed")
		}
		workspaceOwnerID = ownerID
	}
	if value.HostID == "" {
		var ownerID, state string
		var revision int64
		err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, value.WorkspaceID).
			Scan(&ownerID, &state, &revision)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ownerID != workspaceOwnerID || state != string(workspace.StateReady))) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if err != nil {
			return fmt.Errorf("lock Hostless task input Workspace: %w", err)
		}
		if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, value.WorkspaceID); err != nil {
			return err
		}
		if err := requireNoPendingWorkspaceLegacyImport(ctx, tx, value.WorkspaceID); err != nil {
			return err
		}
		if err := ensureNoWorkspaceSyncReceive(ctx, tx, value.WorkspaceID, ""); err != nil {
			return err
		}
		if revision != *value.InputRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace changed before task submission")
		}
		return nil
	}
	var ownerID, state string
	var revision int64
	err := tx.QueryRow(ctx, `SELECT owner_id, state, revision FROM workspaces WHERE id=$1 FOR UPDATE`, value.WorkspaceID).
		Scan(&ownerID, &state, &revision)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ownerID != workspaceOwnerID || state != string(workspace.StateReady))) {
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
	if bindingOwner != workspaceOwnerID || bindingState != string(workspace.BindingActive) {
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
	instanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return app.TaskSubmissionResult{}, err
	}
	message.AttachmentIDs, err = loadHistoryMessageAttachmentIDs(ctx, tx, instanceID, ownerID, message.ConversationID, message.ID)
	if err != nil {
		return app.TaskSubmissionResult{}, err
	}
	return app.TaskSubmissionResult{
		Task: taskDomain(taskRow), Message: message, Conversation: conversationDomain(conversationRow),
	}, nil
}

func insertTask(ctx context.Context, tx pgx.Tx, value task.Task, eventSequence int64, executionEpoch int64) error {
	value = normalizeNewTask(value)
	if executionEpoch < 1 {
		return missingExecutionGateError()
	}
	if value.WorkspaceID != "" {
		if value.ProjectID == "" {
			value.WorkspaceOwnerID = value.InitiatorID
		} else {
			ownerID, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID, value.InitiatorID, value.WorkspaceID, false)
			if err != nil {
				return err
			}
			value.WorkspaceOwnerID, value.WorkspaceProjectResourceVersion = ownerID, &version
		}
	}
	_, err := tx.Exec(ctx, `
INSERT INTO tasks (
  id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind, auth_token_id, auth_scopes,
  budget_active_seconds, budget_tool_calls, budget_model_tokens, usage_active_seconds, usage_tool_calls,
  usage_model_tokens, lease_owner, lease_version, lease_expires_at, event_seq, current_step, result, waiting_reason, finished_at, execution_epoch, application_id,
  auth_client_installation_id, execution_location, workspace_id, input_revision, workspace_checkpoint_revision, context_digest, auth_version, auth_expires_at,
  file_delivery_intent, project_id, initiator_id, workspace_owner_id, workspace_project_resource_version
) VALUES (
  $1,$2,$3,$4,$5,$6,$7,$8,$9,COALESCE($10::text[], '{}'::text[]),$11,$12,$13,$14,$15,$16,'',0,NULL,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36
)`, value.ID, value.OwnerID, value.ConversationID, nullableText(value.HostID), value.ModelID, value.Objective,
		string(value.Status), value.Authorization.Kind, value.Authorization.TokenID, value.Authorization.Scopes,
		value.Budget.ActiveSeconds, value.Budget.ToolCalls, value.Budget.ModelTokens,
		value.Usage.ActiveSeconds, value.Usage.ToolCalls, int64(0),
		eventSequence, value.CurrentStep, value.Result, value.WaitingReason, nullableTimestamp(value.FinishedAt), executionEpoch, value.ApplicationID,
		nullableText(value.Authorization.ClientInstallationID), value.ExecutionLocation, nullableText(value.WorkspaceID),
		value.InputRevision, value.WorkspaceCheckpointRevision, value.ContextDigest, value.Authorization.Version, value.Authorization.ExpiresAt,
		string(value.FileDeliveryIntent), nullableText(value.ProjectID), value.InitiatorID,
		nullableText(value.WorkspaceOwnerID), value.WorkspaceProjectResourceVersion)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	if value.ProjectID != "" && value.WorkspaceID != "" {
		if err := pinProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID,
			value.WorkspaceOwnerID, value.WorkspaceID); err != nil {
			return err
		}
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
