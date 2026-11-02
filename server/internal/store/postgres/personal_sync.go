package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Store) GetPersonalSnapshotBatch(ctx context.Context, principal identity.Principal, cursor *personal.SnapshotCursor, limit int32) (personal.SnapshotBatch, error) {
	if err := validatePersonalSyncStoreRequest(principal, limit); err != nil {
		return personal.SnapshotBatch{}, err
	}
	if cursor != nil && (cursor.OwnerID != principal.UserID || cursor.ObjectType != personal.SnapshotScopeObjectType ||
		cursor.ObjectID != personal.SnapshotScopeObjectID || cursor.Watermark < 0 ||
		(cursor.AfterObjectType == "") != (cursor.AfterObjectID == "") ||
		(cursor.AfterObjectType != "" && (!personal.IsPersonalSyncObjectType(cursor.AfterObjectType) ||
			personal.ValidateObjectIdentifier(cursor.AfterObjectType, cursor.AfterObjectID) != nil))) {
		return personal.SnapshotBatch{}, domain.InvalidArgument("cursor", "personal snapshot cursor is invalid")
	}
	var result personal.SnapshotBatch
	err := s.withPersonalSnapshot(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		currentWatermark, err := readPersonalAccountWatermark(ctx, tx, instanceID, principal.UserID)
		if err != nil {
			return err
		}
		watermark := currentWatermark
		afterType, afterID := "", ""
		if cursor != nil {
			if cursor.ServiceInstanceID != instanceID || cursor.Watermark > currentWatermark {
				return domain.InvalidArgument("cursor", "personal snapshot cursor is no longer valid")
			}
			watermark = cursor.Watermark
			afterType, afterID = cursor.AfterObjectType, cursor.AfterObjectID
		}
		rows, err := tx.Query(ctx, `SELECT v.object_type, v.object_id, v.revision, p.body
FROM account_object_versions v
LEFT JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2
  AND v.object_type=ANY($3::text[]) AND v.object_type<>'schedule' AND NOT v.is_deleted
  AND (v.object_type,v.object_id) > ($4,$5)
ORDER BY v.object_type,v.object_id
LIMIT $6`, instanceID, principal.UserID, personal.PersonalSyncObjectTypes(), afterType, afterID, limit+1)
		if err != nil {
			return fmt.Errorf("query personal snapshot: %w", err)
		}
		defer rows.Close()
		items := make([]personal.Object, 0, limit+1)
		for rows.Next() {
			var item personal.Object
			var body []byte
			if err := rows.Scan(&item.ObjectType, &item.ObjectID, &item.Revision, &body); err != nil {
				return fmt.Errorf("scan personal snapshot object: %w", err)
			}
			item.Body = append(json.RawMessage(nil), body...)
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate personal snapshot: %w", err)
		}
		scheduleRows, err := tx.Query(ctx, `SELECT `+scheduleColumns+`
FROM schedules s WHERE s.owner_id=$1 AND ('schedule',s.id) > ($2,$3)
ORDER BY s.id LIMIT $4`, principal.UserID, afterType, afterID, limit+1)
		if err != nil {
			return fmt.Errorf("query schedule snapshot: %w", err)
		}
		for scheduleRows.Next() {
			value, err := scanSchedule(scheduleRows)
			if err != nil {
				scheduleRows.Close()
				return fmt.Errorf("scan schedule snapshot: %w", err)
			}
			definition := schedule.NewSyncDefinition(value)
			body, err := json.Marshal(definition)
			if err != nil {
				scheduleRows.Close()
				return fmt.Errorf("encode schedule snapshot: %w", err)
			}
			items = append(items, personal.Object{
				ObjectType: "schedule", ObjectID: value.ID, Revision: value.Version, Body: body,
			})
		}
		if err := scheduleRows.Err(); err != nil {
			scheduleRows.Close()
			return fmt.Errorf("iterate schedule snapshot: %w", err)
		}
		scheduleRows.Close()
		sort.Slice(items, func(i, j int) bool {
			if items[i].ObjectType != items[j].ObjectType {
				return items[i].ObjectType < items[j].ObjectType
			}
			return items[i].ObjectID < items[j].ObjectID
		})
		if len(items) > int(limit+1) {
			items = items[:limit+1]
		}
		for _, item := range items {
			if item.Revision < 1 || len(item.Body) == 0 || !json.Valid(item.Body) ||
				personal.ValidateObjectIdentifier(item.ObjectType, item.ObjectID) != nil ||
				personal.ValidateObjectBody(item.ObjectType, item.ObjectID, item.Body) != nil {
				return domain.NewError(domain.CodeInternal, "personal snapshot object is invalid")
			}
		}
		hasMore := int32(len(items)) > limit
		if hasMore {
			items = items[:limit]
		}
		result = personal.SnapshotBatch{ServiceInstanceID: instanceID, Watermark: watermark, Items: items, HasMore: hasMore}
		if hasMore && len(items) > 0 {
			last := items[len(items)-1]
			result.AfterObjectType, result.AfterObjectID = last.ObjectType, last.ObjectID
		}
		return nil
	})
	if err != nil {
		return personal.SnapshotBatch{}, err
	}
	return result, nil
}

func (s *Store) GetPersonalChanges(ctx context.Context, principal identity.Principal, afterSequence int64, throughSequence *int64, limit int32) (personal.ChangePage, error) {
	if err := validatePersonalSyncStoreRequest(principal, limit); err != nil {
		return personal.ChangePage{}, err
	}
	if afterSequence < 0 || (throughSequence != nil && *throughSequence < afterSequence) {
		return personal.ChangePage{}, domain.InvalidArgument("after_sequence", "personal change cursor is invalid")
	}
	// shortcut: account change log retained for account lifetime; add a retention floor and forced snapshot recovery before pruning.
	var result personal.ChangePage
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		current, err := readPersonalAccountWatermark(ctx, tx, instanceID, principal.UserID)
		if err != nil {
			return err
		}
		through := current
		if throughSequence != nil {
			through = *throughSequence
		}
		if afterSequence > current || through > current {
			return domain.InvalidArgument("through_sequence", "personal change cursor is ahead of the account")
		}
		rows, err := tx.Query(ctx, `SELECT change_sequence, object_type, object_id, revision, operation, mutation_id
FROM account_change_log
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=ANY($3::text[])
  AND change_sequence>$4 AND change_sequence<=$5
ORDER BY change_sequence
LIMIT $6`, instanceID, principal.UserID, personal.PersonalSyncObjectTypes(), afterSequence, through, limit+1)
		if err != nil {
			return fmt.Errorf("query personal changes: %w", err)
		}
		defer rows.Close()
		items := make([]personal.Change, 0, limit+1)
		for rows.Next() {
			var item personal.Change
			var mutationID pgtype.Text
			if err := rows.Scan(&item.ChangeSequence, &item.ObjectType, &item.ObjectID, &item.Revision, &item.Operation, &mutationID); err != nil {
				return fmt.Errorf("scan personal change: %w", err)
			}
			if item.ChangeSequence <= afterSequence || item.ChangeSequence > through || item.Revision < 1 ||
				!personal.IsPersonalSyncObjectType(item.ObjectType) ||
				personal.ValidateObjectIdentifier(item.ObjectType, item.ObjectID) != nil ||
				(item.Operation != "upsert" && item.Operation != "delete") {
				return domain.NewError(domain.CodeInternal, "stored personal change is invalid")
			}
			if mutationID.Valid {
				item.MutationID = mutationID.String
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate personal changes: %w", err)
		}
		hasMore := int32(len(items)) > limit
		if hasMore {
			items = items[:limit]
		}
		nextAfter := through
		if hasMore {
			nextAfter = items[len(items)-1].ChangeSequence
		}
		result = personal.ChangePage{
			Items: items, ThroughSequence: through, NextAfterSequence: nextAfter, HasMore: hasMore,
		}
		return nil
	})
	if err != nil {
		return personal.ChangePage{}, err
	}
	return result, nil
}

// withPersonalSnapshot keeps one page's watermark and rows in the same database view.
func (s *Store) withPersonalSnapshot(ctx context.Context, fn func(pgx.Tx) error) error {
	return withTx(ctx, func(ctx context.Context) (pgx.Tx, error) {
		return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	}, fn)
}

func validatePersonalSyncStoreRequest(principal identity.Principal, limit int32) error {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if limit < 1 || limit > 100 {
		return domain.InvalidArgument("limit", "personal sync page limit is out of range")
	}
	return nil
}

func readPersonalAccountWatermark(ctx context.Context, tx pgx.Tx, instanceID, ownerID string) (int64, error) {
	var watermark int64
	err := tx.QueryRow(ctx, `SELECT last_sequence FROM account_change_counters
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, ownerID).Scan(&watermark)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read personal account change watermark: %w", err)
	}
	if watermark < 0 {
		return 0, domain.NewError(domain.CodeInternal, "personal account change watermark is invalid")
	}
	return watermark, nil
}
