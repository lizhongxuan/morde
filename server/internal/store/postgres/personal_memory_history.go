package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetPersonalMemoryHistoryBatch(ctx context.Context, principal identity.Principal, objectID string,
	cursor *personal.MemoryHistoryCursor, limit int32) (personal.MemoryHistoryBatch, error) {
	if err := validatePersonalSyncStoreRequest(principal, limit); err != nil {
		return personal.MemoryHistoryBatch{}, err
	}
	if err := personal.ValidateObjectIdentifier(personal.ObjectTypeMemory, objectID); err != nil {
		return personal.MemoryHistoryBatch{}, err
	}
	if cursor != nil && (cursor.Version != personal.MemoryHistoryCursorVersion || cursor.OwnerID != principal.UserID || cursor.ObjectType != personal.ObjectTypeMemory ||
		cursor.ObjectID != objectID || cursor.AfterRevision < 1) {
		return personal.MemoryHistoryBatch{}, domain.InvalidArgument("cursor", "personal memory history cursor is invalid")
	}
	var result personal.MemoryHistoryBatch
	err := s.withPersonalSnapshot(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if cursor != nil && cursor.ServiceInstanceID != instanceID {
			return domain.InvalidArgument("cursor", "personal memory history cursor is no longer valid")
		}
		var currentRevision int64
		var currentBody []byte
		err = tx.QueryRow(ctx, `SELECT v.revision,p.body
FROM account_object_versions v
JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type='memory' AND v.object_id=$3
  AND NOT v.is_deleted AND p.body IS NOT NULL
FOR SHARE OF v,p`, instanceID, principal.UserID, objectID).Scan(&currentRevision, &currentBody)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal memory not found")
		}
		if err != nil {
			return fmt.Errorf("read current personal memory for history: %w", err)
		}
		if currentRevision < 1 || len(currentBody) == 0 {
			return domain.NewError(domain.CodeNotFound, "personal memory not found")
		}
		var afterRevision int64
		if cursor != nil {
			afterRevision = cursor.AfterRevision
		}
		rows, err := tx.Query(ctx, `WITH current_object AS (
  SELECT v.revision,p.body,COALESCE(current_change.created_at,v.updated_at) AS changed_at
  FROM account_object_versions v
  JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
    AND p.object_type=v.object_type AND p.object_id=v.object_id
  LEFT JOIN LATERAL (
    SELECT l.created_at FROM account_change_log l
    WHERE l.service_instance_id=v.service_instance_id AND l.owner_id=v.owner_id
      AND l.object_type='memory' AND l.object_id=v.object_id AND l.revision=v.revision AND l.operation='upsert'
    ORDER BY l.change_sequence DESC LIMIT 1
  ) current_change ON true
  WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type='memory' AND v.object_id=$3
    AND NOT v.is_deleted AND p.body IS NOT NULL
), revisions AS (
  SELECT log.revision,log.created_at AS changed_at,history.body
  FROM account_change_log log
  CROSS JOIN current_object current
  LEFT JOIN personal_memory_history history ON history.service_instance_id=log.service_instance_id
    AND history.owner_id=log.owner_id AND history.object_id=log.object_id AND history.object_revision=log.revision
  WHERE log.service_instance_id=$1::uuid AND log.owner_id=$2 AND log.object_type='memory' AND log.object_id=$3
    AND log.operation='upsert' AND log.revision<current.revision
  UNION ALL
  SELECT current.revision,current.changed_at,current.body FROM current_object current
)
SELECT revision,changed_at,body FROM revisions
WHERE ($4::bigint=0 OR revision<$4)
ORDER BY revision DESC LIMIT $5`, instanceID, principal.UserID, objectID, afterRevision, limit+1)
		if err != nil {
			return fmt.Errorf("query personal memory revisions: %w", err)
		}
		items := make([]personal.MemoryRevision, 0, limit+1)
		for rows.Next() {
			var item personal.MemoryRevision
			var body []byte
			if err := rows.Scan(&item.Revision, &item.ChangedAt, &body); err != nil {
				rows.Close()
				return fmt.Errorf("scan personal memory revision: %w", err)
			}
			item.Body = append([]byte(nil), body...)
			item.Sources = make([]personal.MemoryHistorySource, 0)
			if len(body) != 0 {
				item.Restorable = true
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate personal memory revisions: %w", err)
		}
		rows.Close()
		if err := annotatePersonalMemoryRevisionSourcesInTx(ctx, tx, instanceID, principal.UserID, objectID, items); err != nil {
			return err
		}
		for _, item := range items {
			if item.Revision < 1 || item.ChangedAt.IsZero() {
				return domain.NewError(domain.CodeInternal, "personal memory revision metadata is invalid")
			}
		}
		hasMore := int32(len(items)) > limit
		if hasMore {
			items = items[:limit]
		}
		result = personal.MemoryHistoryBatch{ServiceInstanceID: instanceID, Items: items, HasMore: hasMore}
		return nil
	})
	if err != nil {
		return personal.MemoryHistoryBatch{}, err
	}
	return result, nil
}

func annotatePersonalMemoryRevisionSourcesInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID, objectID string,
	items []personal.MemoryRevision) error {
	if len(items) == 0 {
		return nil
	}
	revisions := make([]int64, len(items))
	itemByRevision := make(map[int64]int, len(items))
	for index := range items {
		revisions[index] = items[index].Revision
		itemByRevision[items[index].Revision] = index
		items[index].SourceStatus = personal.MemoryHistorySourceUnrecorded
	}
	rows, err := tx.Query(ctx, `SELECT source.object_revision,source.source_order,source.source_kind,source.source_id,
COALESCE(source.source_conversation_id,''),
CASE WHEN source.source_kind='mutation_receipt' THEN EXISTS (
  SELECT 1 FROM personal_mutation_receipts receipt
  WHERE receipt.service_instance_id=source.service_instance_id AND receipt.owner_id=source.owner_id
    AND receipt.mutation_id=source.mutation_id AND receipt.request_hash=source.source_sha256
    AND receipt.object_type='memory' AND receipt.object_id=source.object_id
    AND receipt.result_revision=source.object_revision
) ELSE EXISTS (
  SELECT 1 FROM conversation_messages message
  JOIN conversations conversation ON conversation.id=message.conversation_id AND conversation.owner_id=message.owner_id
  JOIN tasks task ON task.id=source.source_task_id AND task.owner_id=message.owner_id
    AND task.initiator_id=message.actor_id AND task.conversation_id=message.conversation_id AND task.project_id IS NULL
  WHERE message.id=source.source_id AND message.owner_id=source.owner_id
    AND message.conversation_id=source.source_conversation_id AND message.sequence=source.source_sequence
    AND message.role='user' AND message.actor_id=source.owner_id AND message.delivery_status='applied'
    AND message.source_kind NOT IN ('history_import','legacy_instant','task_result_review')
    AND sha256(convert_to(message.content,'UTF8'))=source.source_sha256
    AND conversation.project_id IS NULL
    AND NOT EXISTS (SELECT 1 FROM personal_history_suppressions suppression
      WHERE suppression.service_instance_id=source.service_instance_id AND suppression.owner_id=source.owner_id
        AND suppression.message_id=source.source_id)
) END AS authorized
FROM personal_memory_sources source
WHERE source.service_instance_id=$1::uuid AND source.owner_id=$2 AND source.object_id=$3
  AND source.object_revision=ANY($4::bigint[])
ORDER BY source.object_revision DESC,source.source_order`, instanceID, ownerID, objectID, revisions)
	if err != nil {
		return fmt.Errorf("query personal memory provenance: %w", err)
	}
	type provenance struct {
		revision       int64
		order          int32
		kind           string
		id             string
		conversationID string
		authorized     bool
	}
	var values []provenance
	for rows.Next() {
		var value provenance
		if err := rows.Scan(&value.revision, &value.order, &value.kind, &value.id, &value.conversationID, &value.authorized); err != nil {
			rows.Close()
			return fmt.Errorf("scan personal memory provenance: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate personal memory provenance: %w", err)
	}
	rows.Close()
	for _, value := range values {
		index, ok := itemByRevision[value.revision]
		if !ok || value.order < 1 {
			return domain.NewError(domain.CodeInternal, "personal memory provenance is inconsistent")
		}
		item := &items[index]
		if !value.authorized {
			item.SourceStatus = personal.MemoryHistorySourceUnavailable
			item.Body = nil
			item.Restorable = false
			item.Sources = make([]personal.MemoryHistorySource, 0)
			continue
		}
		if item.SourceStatus == personal.MemoryHistorySourceUnavailable {
			continue
		}
		item.SourceStatus = personal.MemoryHistorySourceAvailable
		item.Sources = append(item.Sources, personal.MemoryHistorySource{
			Kind: value.kind, ID: value.id, ConversationID: value.conversationID,
		})
	}
	return nil
}

func storePersonalMemoryRevisionInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID, objectID string,
	revision int64, body []byte, fallbackChangedAt time.Time) (int64, error) {
	if revision < 1 || len(body) == 0 {
		return 0, domain.NewError(domain.CodeInternal, "current personal memory revision cannot be preserved")
	}
	var changedAt time.Time
	err := tx.QueryRow(ctx, `SELECT COALESCE((
  SELECT created_at FROM account_change_log
  WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type='memory' AND object_id=$3
    AND revision=$4 AND operation='upsert'
  ORDER BY change_sequence DESC LIMIT 1
),$5::timestamptz)`, instanceID, ownerID, objectID, revision, fallbackChangedAt).Scan(&changedAt)
	if err != nil {
		return 0, fmt.Errorf("read prior personal memory revision timestamp: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO personal_memory_history
(service_instance_id,owner_id,object_type,object_id,object_revision,body,changed_at)
VALUES ($1::uuid,$2,'memory',$3,$4,$5::jsonb,$6)
ON CONFLICT (service_instance_id,owner_id,object_id,object_revision) DO UPDATE
SET body=EXCLUDED.body,changed_at=EXCLUDED.changed_at
WHERE personal_memory_history.body IS NULL`, instanceID, ownerID, objectID, revision, string(body), changedAt)
	if err != nil {
		return 0, fmt.Errorf("preserve prior personal memory revision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, nil
	}
	return int64(len(body)), nil
}

func purgePersonalMemoryHistoryBodiesInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID, objectID string) (int64, error) {
	var bytes int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(body::text)),0)::bigint
FROM personal_memory_history
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_id=$3`, instanceID, ownerID, objectID).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("measure personal memory history for deletion: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE personal_memory_history SET body=NULL
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_id=$3 AND body IS NOT NULL`, instanceID, ownerID, objectID); err != nil {
		return 0, fmt.Errorf("purge deleted personal memory history bodies: %w", err)
	}
	if bytes < 0 {
		return 0, domain.NewError(domain.CodeInternal, "personal memory history usage is invalid")
	}
	return bytes, nil
}
