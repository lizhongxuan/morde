package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/project"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	projectWorkItemDefaultPageSize int32 = 50
	projectWorkItemMaxPageSize     int32 = 100
)

type projectWorkItemCursor struct {
	ProjectID string    `json:"project_id"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

type projectWorkItemStored struct {
	Item      project.WorkItem
	CreatedAt time.Time
}

type projectWorkItemIdempotency struct {
	ProjectID     string
	ResultKind    string
	ResultID      string
	ResultVersion int64
	RequestDigest []byte
}

type projectWorkItemTaskProof struct {
	WorkspaceID                 pgtype.Text
	WorkspaceCheckpointRevision pgtype.Int8
}

var _ app.ProjectWorkItemStore = (*Store)(nil)

func (s *Store) GetProjectWorkItem(ctx context.Context, actorID, projectID, workItemID string) (project.WorkItem, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !project.ValidID(workItemID) {
		return project.WorkItem{}, domain.NewError(domain.CodeNotFound, "project work item not found")
	}
	var result project.WorkItem
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		if _, err := requireProjectWorkItemMember(ctx, tx, actorID, projectID, project.RoleViewer); err != nil {
			return err
		}
		stored, err := loadProjectWorkItem(ctx, tx, projectID, workItemID, false)
		if err != nil {
			return err
		}
		result = stored.Item
		return nil
	})
	if err != nil {
		return project.WorkItem{}, err
	}
	return result, nil
}

func (s *Store) GetProjectWorkItemMutation(ctx context.Context, actorID, projectID string, mutation app.ProjectWorkItemMutation) (project.WorkItem, bool, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return project.WorkItem{}, false, domain.NewError(domain.CodeNotFound, "project not found")
	}
	if err := validateProjectWorkItemMutation(mutation); err != nil {
		return project.WorkItem{}, false, err
	}
	var result project.WorkItem
	replayed := false
	var mutationErr error
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		result, replayed, mutationErr = getProjectWorkItemMutationInTx(ctx, tx, actorID, projectID, mutation)
		return mutationErr
	})
	if err != nil {
		return project.WorkItem{}, false, err
	}
	return result, replayed, nil
}

func getProjectWorkItemMutationInTx(ctx context.Context, tx pgx.Tx, actorID, projectID string, mutation app.ProjectWorkItemMutation) (project.WorkItem, bool, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return project.WorkItem{}, false, domain.NewError(domain.CodeNotFound, "project not found")
	}
	if err := validateProjectWorkItemMutation(mutation); err != nil {
		return project.WorkItem{}, false, err
	}
	if _, err := requireProjectWorkItemMember(ctx, tx, actorID, projectID, project.RoleMember); err != nil {
		return project.WorkItem{}, false, err
	}
	record, found, err := findProjectWorkItemIdempotency(ctx, tx, actorID, projectID, mutation)
	if err != nil || !found {
		return project.WorkItem{}, false, err
	}
	stored, err := loadProjectWorkItem(ctx, tx, projectID, record.ResultID, true)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return project.WorkItem{}, false, domain.NewError(domain.CodeInternal, "idempotent project work item result is missing")
		}
		return project.WorkItem{}, false, err
	}
	if stored.Item.Version < record.ResultVersion {
		return project.WorkItem{}, false, domain.NewError(domain.CodeInternal, "idempotent project work item result has an invalid version")
	}
	if err := requireProjectWorkItemAssignee(ctx, tx, projectID, stored.Item.AssigneeID); err != nil {
		return project.WorkItem{}, false, err
	}
	if err := requireProjectWorkItemTaskLinksInTx(ctx, tx, projectID, stored.Item.ID); err != nil {
		return project.WorkItem{}, false, err
	}
	if stored.Item.Delivery != nil {
		if err := requireProjectWorkItemDeliveryInTx(ctx, tx, projectID, stored.Item.ID, *stored.Item.Delivery); err != nil {
			return project.WorkItem{}, false, err
		}
	}
	return stored.Item, true, nil
}

func (s *Store) ListProjectWorkItems(ctx context.Context, actorID, projectID string, page app.PageRequest) (app.ProjectWorkItemPage, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return app.ProjectWorkItemPage{}, domain.NewError(domain.CodeNotFound, "project not found")
	}
	if page.Limit == 0 {
		page.Limit = projectWorkItemDefaultPageSize
	}
	if page.Limit < 1 || page.Limit > projectWorkItemMaxPageSize || len(page.Cursor) > 1024 {
		return app.ProjectWorkItemPage{}, domain.InvalidArgument("limit", "project work item page request is out of range")
	}
	cursor, err := decodeProjectWorkItemCursor(page.Cursor, projectID)
	if err != nil {
		return app.ProjectWorkItemPage{}, err
	}

	result := app.ProjectWorkItemPage{Items: make([]project.WorkItem, 0)}
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		if _, err := requireProjectWorkItemMember(ctx, tx, actorID, projectID, project.RoleViewer); err != nil {
			return err
		}
		var afterTime any
		var afterID any
		if cursor != nil {
			afterTime, afterID = cursor.CreatedAt, cursor.ID
		}
		rows, err := tx.Query(ctx, projectWorkItemSelect+`
WHERE wi.project_id=$1 AND ($2::timestamptz IS NULL OR (wi.created_at, wi.id) < ($2::timestamptz, $3::text))
ORDER BY wi.created_at DESC, wi.id DESC LIMIT $4`, projectID, afterTime, afterID, page.Limit+1)
		if err != nil {
			return fmt.Errorf("list project work items: %w", err)
		}
		defer rows.Close()
		stored := make([]projectWorkItemStored, 0, page.Limit+1)
		for rows.Next() {
			value, err := scanProjectWorkItem(rows)
			if err != nil {
				return fmt.Errorf("scan project work item: %w", err)
			}
			if err := validateStoredProjectWorkItem(value.Item, projectID); err != nil {
				return err
			}
			stored = append(stored, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read project work item page: %w", err)
		}
		if int32(len(stored)) > page.Limit {
			stored = stored[:page.Limit]
			last := stored[len(stored)-1]
			result.NextCursor, err = encodeProjectWorkItemCursor(projectWorkItemCursor{
				ProjectID: projectID, CreatedAt: last.CreatedAt, ID: last.Item.ID,
			})
			if err != nil {
				return fmt.Errorf("encode project work item cursor: %w", err)
			}
		}
		for _, value := range stored {
			result.Items = append(result.Items, value.Item)
		}
		return nil
	})
	if err != nil {
		return app.ProjectWorkItemPage{}, err
	}
	return result, nil
}

func (s *Store) CreateProjectWorkItem(ctx context.Context, actorID string, item project.WorkItem, idempotencyKey string) (project.WorkItem, bool, error) {
	if !domain.ValidUserID(actorID) || !validProjectWorkItemIdempotencyKey(idempotencyKey) ||
		item.Validate() != nil || item.Version != 1 || item.State != project.WorkItemOpen || item.BlockedReason != "" ||
		len(item.TaskIDs) != 0 || item.Delivery != nil {
		return project.WorkItem{}, false, domain.InvalidArgument("work_item", "project work item creation is invalid")
	}
	requestDigest, err := hashProjectWorkItemCreate(item)
	if err != nil {
		return project.WorkItem{}, false, err
	}
	var result project.WorkItem
	var resultID string
	replayed := false
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		if _, err := requireProjectWorkItemMember(ctx, tx, actorID, item.ProjectID, project.RoleMember); err != nil {
			return err
		}
		replayed, resultID, err = reserveProjectWorkItemIdempotency(ctx, tx, actorID, "create_work_item", idempotencyKey,
			item.ProjectID, item.ID, 1, requestDigest)
		if err != nil {
			return err
		}
		if replayed {
			stored, err := loadProjectWorkItem(ctx, tx, item.ProjectID, resultID, true)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeNotFound {
					return domain.NewError(domain.CodeInternal, "idempotent project work item result is missing")
				}
				return err
			}
			if err := requireProjectWorkItemAssignee(ctx, tx, item.ProjectID, stored.Item.AssigneeID); err != nil {
				return err
			}
			if stored.Item.Version < 1 {
				return domain.NewError(domain.CodeInternal, "idempotent project work item result has an invalid version")
			}
			result = stored.Item
			return nil
		}

		if err := requireProjectWorkItemAssignee(ctx, tx, item.ProjectID, item.AssigneeID); err != nil {
			return err
		}
		criteria, err := marshalProjectWorkItemCriteria(item.AcceptanceCriteria)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO project_work_items
(project_id,id,title,description,acceptance_criteria,assignee_id,state,blocked_reason,
 delivery_task_id,delivery_workspace_id,delivery_revision,version,created_by,updated_by)
VALUES ($1,$2,$3,$4,$5::jsonb,$6,'open','',NULL,NULL,NULL,1,$7,$7)`,
			item.ProjectID, item.ID, item.Title, item.Description, criteria, item.AssigneeID, actorID)
		if err != nil {
			return fmt.Errorf("create project work item: %w", err)
		}
		result = item
		result.AcceptanceCriteria = nonNilStrings(result.AcceptanceCriteria)
		result.TaskIDs = []string{}
		return nil
	})
	if err != nil {
		return project.WorkItem{}, false, err
	}
	return result, replayed, nil
}

func (s *Store) UpdateProjectWorkItem(ctx context.Context, actorID string, item project.WorkItem, expectedVersion int64, mutation app.ProjectWorkItemMutation) (project.WorkItem, error) {
	if !domain.ValidUserID(actorID) || validateProjectWorkItemMutation(mutation) != nil || mutation.WorkItemID != item.ID ||
		!project.ValidID(item.ProjectID) || !project.ValidID(item.ID) || expectedVersion < 1 || expectedVersion == math.MaxInt64 ||
		item.Version != expectedVersion+1 || item.Validate() != nil {
		return project.WorkItem{}, domain.InvalidArgument("work_item", "project work item update is invalid")
	}
	var result project.WorkItem
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		if _, err := requireProjectWorkItemMember(ctx, tx, actorID, item.ProjectID, project.RoleMember); err != nil {
			return err
		}
		replayed, resultID, err := reserveProjectWorkItemIdempotency(ctx, tx, actorID, mutation.Operation, mutation.IdempotencyKey,
			item.ProjectID, item.ID, item.Version, mutation.RequestDigest)
		if err != nil {
			return err
		}
		if replayed {
			stored, err := loadProjectWorkItem(ctx, tx, item.ProjectID, resultID, true)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeNotFound {
					return domain.NewError(domain.CodeInternal, "idempotent project work item result is missing")
				}
				return err
			}
			if err := requireProjectWorkItemAssignee(ctx, tx, item.ProjectID, stored.Item.AssigneeID); err != nil {
				return err
			}
			if stored.Item.Version < item.Version {
				return domain.NewError(domain.CodeInternal, "idempotent project work item result has an invalid version")
			}
			if err := requireProjectWorkItemTaskLinksInTx(ctx, tx, item.ProjectID, stored.Item.ID); err != nil {
				return err
			}
			if stored.Item.Delivery != nil {
				if err := requireProjectWorkItemDeliveryInTx(ctx, tx, item.ProjectID, stored.Item.ID, *stored.Item.Delivery); err != nil {
					return err
				}
			}
			result = stored.Item
			return nil
		}

		if err := requireProjectWorkItemAssignee(ctx, tx, item.ProjectID, item.AssigneeID); err != nil {
			return err
		}
		current, err := loadProjectWorkItem(ctx, tx, item.ProjectID, item.ID, true)
		if err != nil {
			return err
		}
		if err := validateProjectWorkItemUpdate(current.Item, item, expectedVersion); err != nil {
			return err
		}
		newTaskIDs := newlyLinkedProjectWorkItemTasks(current.Item.TaskIDs, item.TaskIDs)
		for _, taskID := range newTaskIDs {
			if _, err := requireProjectWorkItemTaskInTx(ctx, tx, item.ProjectID, actorID, taskID); err != nil {
				return err
			}
		}
		if item.Delivery != nil {
			if err := requireProjectWorkItemDeliveryInTx(ctx, tx, item.ProjectID, item.ID, *item.Delivery); err != nil {
				return err
			}
		}
		criteria, err := marshalProjectWorkItemCriteria(item.AcceptanceCriteria)
		if err != nil {
			return err
		}
		var deliveryTaskID, deliveryWorkspaceID any
		var deliveryRevision any
		if item.Delivery != nil {
			deliveryTaskID = item.Delivery.TaskID
			deliveryWorkspaceID = item.Delivery.WorkspaceID
			deliveryRevision = item.Delivery.Revision
		}
		tag, err := tx.Exec(ctx, `UPDATE project_work_items SET title=$4,description=$5,acceptance_criteria=$6::jsonb,
 assignee_id=$7,state=$8,blocked_reason=$9,delivery_task_id=$10,delivery_workspace_id=$11,delivery_revision=$12,
 version=$13,updated_by=$14,updated_at=clock_timestamp()
WHERE project_id=$1 AND id=$2 AND version=$3`, item.ProjectID, item.ID, expectedVersion, item.Title, item.Description,
			criteria, item.AssigneeID, string(item.State), item.BlockedReason, deliveryTaskID, deliveryWorkspaceID,
			deliveryRevision, item.Version, actorID)
		if err != nil {
			return fmt.Errorf("update project work item: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "project work item version changed")
		}
		for _, taskID := range newTaskIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO project_work_item_tasks (project_id,work_item_id,task_id,linked_by)
VALUES ($1,$2,$3,$4) ON CONFLICT (project_id,work_item_id,task_id) DO NOTHING`,
				item.ProjectID, item.ID, taskID, actorID); err != nil {
				return fmt.Errorf("link Task to project work item: %w", err)
			}
		}
		var notificationKind notification.Kind
		switch {
		case current.Item.State != project.WorkItemBlocked && item.State == project.WorkItemBlocked:
			notificationKind = notification.KindProjectItemBlocked
		case current.Item.State != project.WorkItemDelivered && item.State == project.WorkItemDelivered:
			notificationKind = notification.KindProjectItemDelivered
		}
		if notificationKind != "" {
			if err := enqueueProjectWorkItemNotificationEventInTx(ctx, tx, item, notificationKind); err != nil {
				return err
			}
		}
		stored, err := loadProjectWorkItem(ctx, tx, item.ProjectID, item.ID, false)
		if err != nil {
			return err
		}
		result = stored.Item
		return nil
	})
	if err != nil {
		return project.WorkItem{}, err
	}
	return result, nil
}

const projectWorkItemSelect = `SELECT wi.project_id,wi.id,wi.title,wi.description,wi.acceptance_criteria,
 wi.assignee_id,wi.state,wi.blocked_reason,wi.delivery_task_id,wi.delivery_workspace_id,wi.delivery_revision,
 wi.version,wi.created_at,COALESCE(task_rows.task_ids,ARRAY[]::text[])
FROM project_work_items wi
LEFT JOIN LATERAL (
 SELECT array_agg(link.task_id ORDER BY link.created_at,link.task_id) AS task_ids
 FROM project_work_item_tasks link WHERE link.project_id=wi.project_id AND link.work_item_id=wi.id
) task_rows ON TRUE`

func loadProjectWorkItem(ctx context.Context, tx pgx.Tx, projectID, workItemID string, lock bool) (projectWorkItemStored, error) {
	query := projectWorkItemSelect + ` WHERE wi.project_id=$1 AND wi.id=$2`
	if lock {
		query += ` FOR UPDATE OF wi`
	}
	value, err := scanProjectWorkItem(tx.QueryRow(ctx, query, projectID, workItemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return projectWorkItemStored{}, domain.NewError(domain.CodeNotFound, "project work item not found")
	}
	if err != nil {
		return projectWorkItemStored{}, fmt.Errorf("load project work item: %w", err)
	}
	if err := validateStoredProjectWorkItem(value.Item, projectID); err != nil {
		return projectWorkItemStored{}, err
	}
	return value, nil
}

func scanProjectWorkItem(row interface{ Scan(...any) error }) (projectWorkItemStored, error) {
	var value projectWorkItemStored
	var criteria []byte
	var state string
	var deliveryTaskID, deliveryWorkspaceID pgtype.Text
	var deliveryRevision pgtype.Int8
	if err := row.Scan(&value.Item.ProjectID, &value.Item.ID, &value.Item.Title, &value.Item.Description, &criteria,
		&value.Item.AssigneeID, &state, &value.Item.BlockedReason, &deliveryTaskID, &deliveryWorkspaceID,
		&deliveryRevision, &value.Item.Version, &value.CreatedAt, &value.Item.TaskIDs); err != nil {
		return projectWorkItemStored{}, err
	}
	if err := json.Unmarshal(criteria, &value.Item.AcceptanceCriteria); err != nil {
		return projectWorkItemStored{}, fmt.Errorf("decode project work item acceptance criteria: %w", err)
	}
	value.Item.AcceptanceCriteria = nonNilStrings(value.Item.AcceptanceCriteria)
	value.Item.TaskIDs = nonNilStrings(value.Item.TaskIDs)
	value.Item.State = project.WorkItemState(state)
	if deliveryTaskID.Valid || deliveryWorkspaceID.Valid || deliveryRevision.Valid {
		if !deliveryTaskID.Valid || !deliveryWorkspaceID.Valid || !deliveryRevision.Valid {
			return projectWorkItemStored{}, errors.New("project work item delivery columns are incomplete")
		}
		value.Item.Delivery = &project.WorkItemDelivery{
			TaskID: deliveryTaskID.String, WorkspaceID: deliveryWorkspaceID.String, Revision: deliveryRevision.Int64,
		}
	}
	return value, nil
}

func requireProjectWorkItemMember(ctx context.Context, tx pgx.Tx, actorID, projectID string, minimum project.Role) (project.Role, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !project.ValidRole(minimum) {
		return "", domain.NewError(domain.CodeNotFound, "project not found")
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2 FOR SHARE`, projectID, actorID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.NewError(domain.CodeNotFound, "project not found")
	}
	if err != nil {
		return "", fmt.Errorf("check project work item membership: %w", err)
	}
	memberRole := project.Role(role)
	if !project.ValidRole(memberRole) {
		return "", domain.NewError(domain.CodeInternal, "project membership has an invalid role")
	}
	if !project.RoleAtLeast(memberRole, minimum) {
		return "", domain.NewError(domain.CodeForbidden, "project role does not allow this work item operation")
	}
	return memberRole, nil
}

func requireProjectWorkItemAssignee(ctx context.Context, tx pgx.Tx, projectID, assigneeID string) error {
	if !project.ValidID(projectID) || !domain.ValidUserID(assigneeID) {
		return domain.InvalidArgument("assignee_id", "project work item assignee is invalid")
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2 FOR SHARE`, projectID, assigneeID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "project work item assignee is no longer an active project member")
	}
	if err != nil {
		return fmt.Errorf("check project work item assignee membership: %w", err)
	}
	memberRole := project.Role(role)
	if !project.ValidRole(memberRole) {
		return domain.NewError(domain.CodeInternal, "project assignee membership has an invalid role")
	}
	if !project.RoleAtLeast(memberRole, project.RoleMember) {
		return domain.NewError(domain.CodeConflict, "project work item assignee is not an assignable project member")
	}
	return nil
}

func requireProjectWorkItemTaskInTx(ctx context.Context, tx pgx.Tx, projectID, initiatorID, taskID string) (projectWorkItemTaskProof, error) {
	if !project.ValidID(projectID) || !domain.ValidUserID(initiatorID) || !domain.ValidTaskID(taskID) {
		return projectWorkItemTaskProof{}, domain.NewError(domain.CodeForbidden, "Task is not available to this project member")
	}
	var storedProjectID pgtype.Text
	var storedInitiatorID string
	var proof projectWorkItemTaskProof
	err := tx.QueryRow(ctx, `SELECT project_id,initiator_id,workspace_id,workspace_checkpoint_revision
FROM tasks WHERE id=$1 FOR SHARE`, taskID).
		Scan(&storedProjectID, &storedInitiatorID, &proof.WorkspaceID, &proof.WorkspaceCheckpointRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectWorkItemTaskProof{}, domain.NewError(domain.CodeForbidden, "Task is not available to this project member")
	}
	if err != nil {
		return projectWorkItemTaskProof{}, fmt.Errorf("lock project Task authorization evidence: %w", err)
	}
	if !storedProjectID.Valid || storedProjectID.String != projectID || storedInitiatorID != initiatorID {
		return projectWorkItemTaskProof{}, domain.NewError(domain.CodeForbidden, "Task is not available to this project member")
	}
	return proof, nil
}

func requireProjectWorkItemTaskLinksInTx(ctx context.Context, tx pgx.Tx, projectID, workItemID string) error {
	rows, err := tx.Query(ctx, `SELECT task_id,linked_by FROM project_work_item_tasks
WHERE project_id=$1 AND work_item_id=$2 ORDER BY task_id FOR SHARE`, projectID, workItemID)
	if err != nil {
		return fmt.Errorf("lock project work item Task links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, linkedBy string
		if err := rows.Scan(&taskID, &linkedBy); err != nil {
			return fmt.Errorf("scan project work item Task link: %w", err)
		}
		if _, err := requireProjectWorkItemTaskInTx(ctx, tx, projectID, linkedBy, taskID); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read project work item Task links: %w", err)
	}
	return nil
}

func requireProjectWorkItemDeliveryInTx(ctx context.Context, tx pgx.Tx, projectID, workItemID string, evidence project.WorkItemDelivery) error {
	if !project.ValidID(projectID) || !project.ValidID(workItemID) || !domain.ValidTaskID(evidence.TaskID) ||
		!domain.ValidWorkspaceID(evidence.WorkspaceID) || evidence.Revision < 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "project work item delivery evidence is invalid")
	}
	var linkedBy string
	err := tx.QueryRow(ctx, `SELECT linked_by FROM project_work_item_tasks
WHERE project_id=$1 AND work_item_id=$2 AND task_id=$3 FOR SHARE`, projectID, workItemID, evidence.TaskID).Scan(&linkedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery Task is not linked to this project work item")
	}
	if err != nil {
		return fmt.Errorf("lock project work item delivery Task link: %w", err)
	}
	proof, err := requireProjectWorkItemTaskInTx(ctx, tx, projectID, linkedBy, evidence.TaskID)
	if err != nil {
		return err
	}
	if !proof.WorkspaceID.Valid || proof.WorkspaceID.String != evidence.WorkspaceID ||
		!proof.WorkspaceCheckpointRevision.Valid || proof.WorkspaceCheckpointRevision.Int64 != evidence.Revision {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery evidence does not match the Task Workspace checkpoint")
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2 FOR SHARE`,
		evidence.WorkspaceID, evidence.Revision).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery Workspace revision is unavailable")
	}
	if err != nil {
		return fmt.Errorf("lock project delivery Workspace revision: %w", err)
	}
	if !exists {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery Workspace revision is unavailable")
	}
	return requireProjectWorkItemDeliveryEvidenceInTx(ctx, tx, projectID, evidence)
}

func validateProjectWorkItemMutation(mutation app.ProjectWorkItemMutation) error {
	if mutation.Operation != "update_work_item" || !project.ValidID(mutation.WorkItemID) ||
		!validProjectWorkItemIdempotencyKey(mutation.IdempotencyKey) || len(mutation.RequestDigest) != sha256.Size {
		return domain.InvalidArgument("idempotency_key", "project work item mutation is invalid")
	}
	return nil
}

func findProjectWorkItemIdempotency(ctx context.Context, tx pgx.Tx, actorID, projectID string, mutation app.ProjectWorkItemMutation) (projectWorkItemIdempotency, bool, error) {
	var stored projectWorkItemIdempotency
	err := tx.QueryRow(ctx, `SELECT project_id,result_kind,result_id,result_version,request_digest
FROM project_idempotency_keys WHERE actor_id=$1 AND operation=$2 AND idempotency_key=$3 FOR SHARE`,
		actorID, mutation.Operation, mutation.IdempotencyKey).
		Scan(&stored.ProjectID, &stored.ResultKind, &stored.ResultID, &stored.ResultVersion, &stored.RequestDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectWorkItemIdempotency{}, false, nil
	}
	if err != nil {
		return projectWorkItemIdempotency{}, false, fmt.Errorf("find project work item idempotency key: %w", err)
	}
	if stored.ProjectID != projectID || stored.ResultKind != "work_item" || stored.ResultID != mutation.WorkItemID ||
		stored.ResultVersion < 2 || !equalBytes(stored.RequestDigest, mutation.RequestDigest) {
		return projectWorkItemIdempotency{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different project work item request")
	}
	return stored, true, nil
}

func reserveProjectWorkItemIdempotency(ctx context.Context, tx pgx.Tx, actorID, operation, key, projectID, proposedResultID string, proposedVersion int64, digest []byte) (bool, string, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !project.ValidID(proposedResultID) ||
		!validProjectWorkItemIdempotencyKey(key) || (operation != "create_work_item" && operation != "update_work_item") ||
		proposedVersion < 1 || len(digest) != sha256.Size {
		return false, "", domain.InvalidArgument("idempotency_key", "project work item idempotency fields are invalid")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO project_idempotency_keys
(actor_id,operation,idempotency_key,project_id,result_kind,result_id,result_version,request_digest)
VALUES ($1,$2,$3,$4,'work_item',$5,$6,$7)
ON CONFLICT (actor_id,operation,idempotency_key) DO NOTHING`, actorID, operation, key, projectID, proposedResultID, proposedVersion, digest)
	if err != nil {
		return false, "", fmt.Errorf("reserve project work item idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return false, proposedResultID, nil
	}
	var stored projectWorkItemIdempotency
	err = tx.QueryRow(ctx, `SELECT project_id,result_kind,result_id,result_version,request_digest
FROM project_idempotency_keys WHERE actor_id=$1 AND operation=$2 AND idempotency_key=$3 FOR UPDATE`, actorID, operation, key).
		Scan(&stored.ProjectID, &stored.ResultKind, &stored.ResultID, &stored.ResultVersion, &stored.RequestDigest)
	if err != nil {
		return false, "", fmt.Errorf("read project work item idempotency key: %w", err)
	}
	if stored.ProjectID != projectID || stored.ResultKind != "work_item" || !project.ValidID(stored.ResultID) ||
		stored.ResultVersion != proposedVersion ||
		!equalBytes(stored.RequestDigest, digest) {
		return false, "", domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different project work item request")
	}
	return true, stored.ResultID, nil
}

func validateProjectWorkItemUpdate(current, updated project.WorkItem, expectedVersion int64) error {
	if current.Validate() != nil || updated.Validate() != nil || current.ID != updated.ID || current.ProjectID != updated.ProjectID ||
		current.Version != expectedVersion || expectedVersion == math.MaxInt64 || updated.Version != expectedVersion+1 {
		return domain.NewError(domain.CodeConflict, "project work item version changed")
	}
	if current.State == project.WorkItemDelivered {
		return domain.NewError(domain.CodeConflict, "delivered project work items are immutable")
	}
	if current.State == project.WorkItemBlocked && updated.State == project.WorkItemDelivered {
		return domain.NewError(domain.CodeConflict, "blocked project work items must be resumed before delivery")
	}
	if current.State == project.WorkItemBlocked && updated.State == project.WorkItemBlocked && current.BlockedReason != updated.BlockedReason {
		return domain.NewError(domain.CodeConflict, "project work item blocker can only change through the block transition")
	}
	if current.Delivery != nil && !sameProjectWorkItemDelivery(current.Delivery, updated.Delivery) {
		return domain.NewError(domain.CodeConflict, "project work item delivery evidence cannot be changed")
	}
	for _, existing := range current.TaskIDs {
		if !containsProjectWorkItemTask(updated.TaskIDs, existing) {
			return domain.InvalidArgument("task_ids", "linked project work item tasks cannot be removed")
		}
	}
	if len(newlyLinkedProjectWorkItemTasks(current.TaskIDs, updated.TaskIDs)) > 1 {
		return domain.InvalidArgument("task_ids", "only one authorized Task can be linked per update")
	}
	return nil
}

func validateStoredProjectWorkItem(item project.WorkItem, expectedProjectID string) error {
	if item.ProjectID != expectedProjectID || item.Validate() != nil {
		return domain.NewError(domain.CodeInternal, "stored project work item is invalid")
	}
	return nil
}

func hashProjectWorkItemCreate(item project.WorkItem) ([]byte, error) {
	request := struct {
		ProjectID          string
		Title              string
		Description        string
		AcceptanceCriteria []string
		AssigneeID         string
	}{item.ProjectID, item.Title, item.Description, nonNilStrings(item.AcceptanceCriteria), item.AssigneeID}
	return hashProjectWorkItemRequest(request)
}

func hashProjectWorkItemRequest(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode project work item idempotency request: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hash[:], nil
}

func marshalProjectWorkItemCriteria(values []string) ([]byte, error) {
	encoded, err := json.Marshal(nonNilStrings(values))
	if err != nil {
		return nil, fmt.Errorf("encode project work item acceptance criteria: %w", err)
	}
	return encoded, nil
}

func encodeProjectWorkItemCursor(value projectWorkItemCursor) (string, error) {
	if !project.ValidID(value.ProjectID) || !project.ValidID(value.ID) || value.CreatedAt.IsZero() {
		return "", errors.New("project work item cursor fields are invalid")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeProjectWorkItemCursor(value, projectID string) (*projectWorkItemCursor, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > 1024 {
		return nil, domain.InvalidArgument("cursor", "project work item cursor is invalid")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, domain.InvalidArgument("cursor", "project work item cursor is invalid")
	}
	var cursor projectWorkItemCursor
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return nil, domain.InvalidArgument("cursor", "project work item cursor is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, domain.InvalidArgument("cursor", "project work item cursor is invalid")
	}
	if cursor.ProjectID != projectID || !project.ValidID(cursor.ProjectID) || !project.ValidID(cursor.ID) || cursor.CreatedAt.IsZero() {
		return nil, domain.InvalidArgument("cursor", "project work item cursor is invalid")
	}
	return &cursor, nil
}

func validProjectWorkItemIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') &&
			(char < '0' || char > '9') && char != '.' && char != '_' && char != '~' && char != '-' {
			return false
		}
	}
	return true
}

func newlyLinkedProjectWorkItemTasks(oldTasks, newTasks []string) []string {
	result := make([]string, 0)
	for _, taskID := range newTasks {
		if !containsProjectWorkItemTask(oldTasks, taskID) {
			result = append(result, taskID)
		}
	}
	return result
}

func containsProjectWorkItemTask(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sameProjectWorkItemDelivery(left, right *project.WorkItemDelivery) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var mismatch byte
	for index := range left {
		mismatch |= left[index] ^ right[index]
	}
	return mismatch == 0
}
