package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"
)

func (s *Store) ListScheduledPersonalContext(ctx context.Context, ownerID, runID, taskID string,
	includeMemory bool, limit int32) ([]personal.Object, bool, error) {
	if !domain.ValidUserID(ownerID) || !schedule.ValidRunID(runID) || !domain.ValidTaskID(taskID) || limit < 1 || limit > 100 {
		return nil, false, domain.InvalidArgument("scheduled_context", "scheduled personal context request is invalid")
	}
	objectTypes := []string{personal.ObjectTypeProfile, personal.ObjectTypeSoul, personal.ObjectTypeGoal}
	if includeMemory {
		objectTypes = append(objectTypes, personal.ObjectTypeMemory)
	}
	var result []personal.Object
	var hasMore bool
	err := s.withPersonalSnapshot(ctx, func(tx pgx.Tx) error {
		if err := authorizeScheduledPersonalContextInTx(ctx, tx, ownerID, runID, taskID); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT v.object_type,v.object_id,v.revision,p.body
FROM account_object_versions v
JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=ANY($3::text[])
  AND NOT v.is_deleted AND p.body IS NOT NULL
ORDER BY CASE v.object_type WHEN 'profile' THEN 0 WHEN 'memory' THEN 1 WHEN 'soul' THEN 2 WHEN 'goal' THEN 3 ELSE 4 END,
  v.object_id
LIMIT $4`, instanceID, ownerID, objectTypes, limit+1)
		if err != nil {
			return fmt.Errorf("query scheduled personal context: %w", err)
		}
		defer rows.Close()
		items := make([]personal.Object, 0, limit+1)
		for rows.Next() {
			var item personal.Object
			var body []byte
			if err := rows.Scan(&item.ObjectType, &item.ObjectID, &item.Revision, &body); err != nil {
				return fmt.Errorf("scan scheduled personal context: %w", err)
			}
			if item.Revision < 1 || len(body) == 0 || !json.Valid(body) ||
				personal.ValidateObjectIdentifier(item.ObjectType, item.ObjectID) != nil ||
				personal.ValidateObjectBody(item.ObjectType, item.ObjectID, body) != nil {
				return domain.NewError(domain.CodeInternal, "scheduled personal context object is invalid")
			}
			item.Body = append(json.RawMessage(nil), body...)
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate scheduled personal context: %w", err)
		}
		hasMore = int32(len(items)) > limit
		if hasMore {
			items = items[:limit]
		}
		result = items
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, hasMore, nil
}

func (s *Store) GetScheduledPersonalContextObject(ctx context.Context, ownerID, runID, taskID, objectType, objectID string) (personal.Object, error) {
	if !domain.ValidUserID(ownerID) || !schedule.ValidRunID(runID) || !domain.ValidTaskID(taskID) ||
		personal.ValidateObjectIdentifier(objectType, objectID) != nil {
		return personal.Object{}, domain.InvalidArgument("scheduled_context", "scheduled personal object request is invalid")
	}
	switch objectType {
	case personal.ObjectTypeProfile, personal.ObjectTypeSoul, personal.ObjectTypeMemory, personal.ObjectTypeGoal, personal.ObjectTypeSkillSetting:
	default:
		return personal.Object{}, domain.NewError(domain.CodeForbidden, "scheduled personal object type is not available")
	}
	var result personal.Object
	err := s.withPersonalSnapshot(ctx, func(tx pgx.Tx) error {
		if err := authorizeScheduledPersonalContextInTx(ctx, tx, ownerID, runID, taskID); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		var body []byte
		err = tx.QueryRow(ctx, `SELECT v.revision,p.body
FROM account_object_versions v
JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id=$4
  AND NOT v.is_deleted AND p.body IS NOT NULL`, instanceID, ownerID, objectType, objectID).Scan(&result.Revision, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal object not found")
		}
		if err != nil {
			return fmt.Errorf("read scheduled personal object: %w", err)
		}
		if result.Revision < 1 || !json.Valid(body) || personal.ValidateObjectBody(objectType, objectID, body) != nil {
			return domain.NewError(domain.CodeInternal, "scheduled personal object is invalid")
		}
		result.ObjectType, result.ObjectID = objectType, objectID
		result.Body = append(json.RawMessage(nil), body...)
		return nil
	})
	if err != nil {
		return personal.Object{}, err
	}
	return result, nil
}

func authorizeScheduledPersonalContextInTx(ctx context.Context, tx pgx.Tx, ownerID, runID, taskID string) error {
	var scopes []string
	var hostBound bool
	err := tx.QueryRow(ctx, `SELECT t.auth_scopes,t.host_id IS NOT NULL
FROM schedule_runs r
JOIN schedules s ON s.id=r.schedule_id AND s.owner_id=r.owner_id
JOIN tasks t ON t.id=r.task_id AND t.owner_id=r.owner_id AND t.initiator_id=r.owner_id
JOIN users u ON u.id=r.owner_id AND u.status='approved'
WHERE r.id=$1 AND r.owner_id=$2 AND r.task_id=$3
  AND s.host_id IS NOT DISTINCT FROM t.host_id
  AND t.project_id IS NULL AND t.execution_location='server' AND t.status='running'
  AND t.auth_kind=$4 AND t.auth_token_id=r.id`, runID, ownerID, taskID, string(identity.KindScheduleTask)).
		Scan(&scopes, &hostBound)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "scheduled personal context authorization is unavailable")
	}
	if err != nil {
		return fmt.Errorf("authorize scheduled personal context: %w", err)
	}
	if !scheduleTaskScopeStringsValidForHost(scopes, hostBound) {
		return domain.NewError(domain.CodeUnauthenticated, "scheduled personal context authorization is unavailable")
	}
	return nil
}

var _ interface {
	ListScheduledPersonalContext(context.Context, string, string, string, bool, int32) ([]personal.Object, bool, error)
	GetScheduledPersonalContextObject(context.Context, string, string, string, string, string) (personal.Object, error)
} = (*Store)(nil)
