package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

func enqueueGoalNotificationEventsInTx(ctx context.Context, tx pgx.Tx, instanceID string, principal identity.Principal,
	ownerID, goalID string, revision int64, previous, current []byte) error {
	if len(previous) == 0 || len(current) == 0 {
		return nil
	}
	var before, after struct {
		Status string `json:"status"`
		Steps  []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(previous, &before); err != nil {
		return fmt.Errorf("decode previous Goal state for notifications: %w", err)
	}
	if err := json.Unmarshal(current, &after); err != nil {
		return fmt.Errorf("decode updated Goal state for notifications: %w", err)
	}
	if before.Status != "done" && after.Status == "done" {
		if err := enqueueNotificationSourceEventInTx(ctx, tx, notification.SourceGoal, goalID, revision, "", ownerID, notification.KindGoalCompleted); err != nil {
			return err
		}
	}
	triggerEnabled := false
	if principal.Kind == identity.KindSession && principal.HasScope(identity.ScopeTaskSubmit) &&
		principal.HasScope(identity.ScopeScheduleManage) && domain.ValidTokenID(principal.TokenID) {
		if err := tx.QueryRow(ctx, `SELECT COALESCE((p.body->>'enabled')::boolean,FALSE)
FROM personal_objects p JOIN account_object_versions v
  ON v.service_instance_id=p.service_instance_id AND v.owner_id=p.owner_id
 AND v.object_type=p.object_type AND v.object_id=p.object_id AND NOT v.is_deleted
WHERE p.service_instance_id=$1::uuid AND p.owner_id=$2 AND p.object_type=$3 AND p.object_id='builtin_goal_followups'`,
			instanceID, ownerID, personal.ObjectTypeSkillSetting).Scan(&triggerEnabled); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read Goal agent trigger setting: %w", err)
		}
	}
	blocked := make(map[string]bool, len(before.Steps))
	for _, step := range before.Steps {
		blocked[step.ID] = step.Status == "blocked"
	}
	for _, step := range after.Steps {
		if step.Status == "blocked" && !blocked[step.ID] {
			if err := enqueueNotificationSourceEventInTx(ctx, tx, notification.SourceGoal, goalID, revision, "", ownerID, notification.KindGoalStepBlocked); err != nil {
				return err
			}
			if triggerEnabled && after.Status == "active" {
				if err := enqueueGoalAgentTriggerEventInTx(ctx, tx, instanceID, ownerID, goalID, revision, step.ID,
					task.Authorization{Kind: string(identity.KindSession), TokenID: principal.TokenID}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func enqueueGoalAgentTriggerEventInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID, goalID string,
	revision int64, stepID string, authorization task.Authorization) error {
	if !domain.ValidServiceInstanceID(instanceID) || !domain.ValidUserID(ownerID) || personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, goalID) != nil ||
		revision < 1 || stepID == "" || authorization.Kind != string(identity.KindSession) || !domain.ValidTokenID(authorization.TokenID) {
		return domain.NewError(domain.CodeInvalidState, "Goal agent trigger identity is invalid")
	}
	encoded, err := json.Marshal(authorization)
	if err != nil {
		return fmt.Errorf("encode Goal agent trigger authorization: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO goal_agent_trigger_events
(service_instance_id,owner_id,goal_id,goal_revision,step_id,authorization_snapshot)
VALUES ($1::uuid,$2,$3,$4,$5,$6::jsonb) ON CONFLICT DO NOTHING`,
		instanceID, ownerID, goalID, revision, stepID, string(encoded))
	if err != nil {
		return fmt.Errorf("enqueue Goal agent trigger: %w", err)
	}
	return nil
}

func enqueueProjectWorkItemNotificationEventInTx(ctx context.Context, tx pgx.Tx, item project.WorkItem, kind notification.Kind) error {
	if kind != notification.KindProjectItemBlocked && kind != notification.KindProjectItemDelivered {
		return fmt.Errorf("unsupported project work item notification kind %q", kind)
	}
	_, err := tx.Exec(ctx, `INSERT INTO notification_source_events
(source_kind,source_id,source_version,event_kind,recipient_id,project_id)
SELECT 'project_work_item',$2,$3,$4,pm.user_id,$1
FROM project_members pm WHERE pm.project_id=$1
ON CONFLICT DO NOTHING`, item.ProjectID, item.ID, item.Version, string(kind))
	if err != nil {
		return fmt.Errorf("enqueue project work item notification: %w", err)
	}
	return nil
}

func enqueueNotificationSourceEventInTx(ctx context.Context, tx pgx.Tx, sourceKind, sourceID string, sourceVersion int64, projectID, recipientID string, kind notification.Kind) error {
	_, err := tx.Exec(ctx, `INSERT INTO notification_source_events
(source_kind,source_id,source_version,event_kind,recipient_id,project_id)
VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')) ON CONFLICT DO NOTHING`, sourceKind, sourceID, sourceVersion, string(kind), recipientID, projectID)
	if err != nil {
		return fmt.Errorf("enqueue notification source event: %w", err)
	}
	return nil
}
