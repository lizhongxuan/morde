package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/project"

	"github.com/jackc/pgx/v5"
)

func enqueueGoalNotificationEventsInTx(ctx context.Context, tx pgx.Tx, ownerID, goalID string, revision int64, previous, current []byte) error {
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
	blocked := make(map[string]bool, len(before.Steps))
	for _, step := range before.Steps {
		blocked[step.ID] = step.Status == "blocked"
	}
	for _, step := range after.Steps {
		if step.Status == "blocked" && !blocked[step.ID] {
			if err := enqueueNotificationSourceEventInTx(ctx, tx, notification.SourceGoal, goalID, revision, "", ownerID, notification.KindGoalStepBlocked); err != nil {
				return err
			}
		}
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
