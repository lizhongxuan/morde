package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

var _ app.ProjectTaskLinkAuthorizer = (*Store)(nil)
var _ app.ProjectDeliveryAuthorizer = (*Store)(nil)

func (s *Store) AuthorizeProjectTaskLink(ctx context.Context, principal identity.Principal, projectID, taskID string) error {
	actorID, err := projectWorkItemActorID(principal)
	if err != nil {
		return err
	}
	if !project.ValidID(projectID) || !domain.ValidTaskID(taskID) {
		return domain.NewError(domain.CodeNotFound, "project work item Task not found")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		if _, err := requireProjectWorkItemMember(ctx, tx, actorID, projectID, project.RoleMember); err != nil {
			return err
		}
		_, err := requireProjectWorkItemTaskInTx(ctx, tx, projectID, actorID, taskID)
		return err
	})
}

func (s *Store) AuthorizeSavedProjectDelivery(ctx context.Context, principal identity.Principal, projectID string, evidence project.WorkItemDelivery) error {
	actorID, err := projectWorkItemActorID(principal)
	if err != nil {
		return err
	}
	if !project.ValidID(projectID) || !domain.ValidTaskID(evidence.TaskID) || !domain.ValidWorkspaceID(evidence.WorkspaceID) || evidence.Revision < 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "project work item delivery evidence is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedProjectUserInTx(ctx, tx, actorID); err != nil {
			return err
		}
		if _, err := requireProjectWorkItemMember(ctx, tx, actorID, projectID, project.RoleMember); err != nil {
			return err
		}
		return requireProjectWorkItemDeliveryEvidenceInTx(ctx, tx, projectID, evidence)
	})
}

func requireProjectWorkItemDeliveryEvidenceInTx(ctx context.Context, tx pgx.Tx, projectID string, evidence project.WorkItemDelivery) error {
	if !project.ValidID(projectID) || !domain.ValidTaskID(evidence.TaskID) || !domain.ValidWorkspaceID(evidence.WorkspaceID) || evidence.Revision < 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "project work item delivery evidence is invalid")
	}
	var taskProjectID, taskWorkspaceID pgtype.Text
	var checkpointRevision pgtype.Int8
	err := tx.QueryRow(ctx, `SELECT project_id,workspace_id,workspace_checkpoint_revision
FROM tasks WHERE id=$1 FOR SHARE`, evidence.TaskID).Scan(&taskProjectID, &taskWorkspaceID, &checkpointRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery Task is not available to this project")
	}
	if err != nil {
		return fmt.Errorf("read project delivery Task evidence: %w", err)
	}
	if !taskProjectID.Valid || taskProjectID.String != projectID || !taskWorkspaceID.Valid || taskWorkspaceID.String != evidence.WorkspaceID ||
		!checkpointRevision.Valid || checkpointRevision.Int64 != evidence.Revision {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery evidence does not match the Task Workspace checkpoint")
	}

	var saved bool
	err = tx.QueryRow(ctx, `SELECT true
FROM project_resources pr
JOIN workspaces w ON w.id=pr.workspace_id AND w.owner_id=pr.resource_owner_id AND w.state='ready'
JOIN workspace_revisions r ON r.workspace_id=w.id AND r.revision=$3
	WHERE pr.project_id=$1 AND pr.kind='workspace' AND pr.resource_id=$2 AND pr.workspace_id=$2 AND pr.active
	FOR SHARE OF pr,w,r`, projectID, evidence.WorkspaceID, evidence.Revision).Scan(&saved)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery Workspace revision is not registered to this project")
	}
	if err != nil {
		return fmt.Errorf("lock project delivery Workspace resource: %w", err)
	}

	var receiptCount int64
	rows, err := tx.Query(ctx, `SELECT 1 FROM task_workspace_publish_receipts
	WHERE task_id=$1 AND workspace_id=$2 AND state='succeeded' AND published_revision=$3 FOR SHARE`,
		evidence.TaskID, evidence.WorkspaceID, evidence.Revision)
	if err != nil {
		return fmt.Errorf("lock successful Task Workspace publish receipt: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		receiptCount++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read successful Task Workspace publish receipts: %w", err)
	}
	if receiptCount != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "delivery requires one unambiguous successful Task Workspace publish receipt")
	}
	return nil
}

func projectWorkItemActorID(principal identity.Principal) (string, error) {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return "", domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if principal.Role != identity.RoleUser {
		return "", domain.NewError(domain.CodeForbidden, "project access requires a user account")
	}
	switch principal.Kind {
	case identity.KindSession:
		if principal.HostScope != "" || len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.Scopes) != 0 {
			return "", domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	case identity.KindClient:
		if !domain.ValidClientInstallationID(principal.ClientInstallationID) || principal.HostScope != "" ||
			len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.Scopes) != 0 {
			return "", domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
	default:
		return "", domain.NewError(domain.CodeForbidden, "project access requires a user session or client installation")
	}
	return principal.UserID, nil
}
