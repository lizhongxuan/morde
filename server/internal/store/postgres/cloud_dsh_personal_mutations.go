package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

func validateCloudDSHPersonalMutationFence(ctx context.Context, tx pgx.Tx, current task.Task,
	progress app.TaskProgress, commit app.CloudDSHPersonalMutationCommit) (task.Event, bool, error) {
	if !app.ValidCloudDSHPersonalMutationCommit(commit) || commit.TaskID != current.ID || commit.OwnerID != current.OwnerID ||
		current.ExecutionLocation != "server" || current.AgentSessionID != commit.AgentSessionID ||
		current.Authorization.Version != commit.AuthorizationVersion || progress.Status != task.StatusRunning ||
		progress.Event.Type != "task.step" || progress.AssistantMessage != nil || len(progress.Artifacts) != 0 {
		return task.Event{}, false, domain.NewError(domain.CodeInvalidState, "personal mutation Task authorization changed")
	}
	var authorized bool
	if err := tx.QueryRow(ctx, `SELECT auth_expires_at > clock_timestamp() FROM tasks WHERE id=$1`, current.ID).Scan(&authorized); err != nil {
		return task.Event{}, false, fmt.Errorf("check personal Task authorization expiry: %w", err)
	}
	if !authorized {
		return task.Event{}, false, domain.NewError(domain.CodeUnauthenticated, "personal Task authorization expired")
	}
	principal, err := cloudDSHPersonalTaskPrincipal(current)
	if err != nil {
		return task.Event{}, false, err
	}
	if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
		return task.Event{}, false, err
	}
	existing, found, err := findCloudDSHPersonalMutationEvent(ctx, tx, current.ID, commit.CallID)
	if err != nil {
		return task.Event{}, false, err
	}
	if found {
		if existing.Type != "task.step" || app.ValidateCloudDSHPersonalMutationEvent(existing.Payload, commit) != nil {
			return task.Event{}, false, domain.NewError(domain.CodeInvalidState, "personal mutation replay fingerprint changed")
		}
		return existing, true, nil
	}
	return task.Event{}, false, nil
}

func applyCloudDSHPersonalMutationInTx(ctx context.Context, tx pgx.Tx, current task.Task,
	commit app.CloudDSHPersonalMutationCommit, payload []byte) error {
	if !app.ValidCloudDSHPersonalMutationCommit(commit) || app.ValidateCloudDSHPersonalMutationEvent(payload, commit) != nil {
		return domain.NewError(domain.CodeInvalidState, "personal mutation request is invalid")
	}
	var personalConversation bool
	if err := tx.QueryRow(ctx, `SELECT project_id IS NULL FROM conversations WHERE id=$1 AND owner_id=$2`,
		current.ConversationID, current.OwnerID).Scan(&personalConversation); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "personal conversation not found")
	} else if err != nil {
		return fmt.Errorf("check personal conversation scope: %w", err)
	}
	if !personalConversation {
		return domain.NewError(domain.CodeForbidden, "personal data cannot be changed from a project conversation")
	}
	principal, err := cloudDSHPersonalTaskPrincipal(current)
	if err != nil {
		return err
	}
	if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
		return err
	}
	if commit.Mutation.ObjectType == personal.ObjectTypeMemory {
		// Personal mutations always lock the account counter before object rows.
		// Hold that same lock while checking consent and applying the mutation, so
		// a concurrent switch-off cannot commit between the check and the write.
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := lockAccountChangeCounter(ctx, tx, instanceID, current.OwnerID); err != nil {
			return err
		}
		var enabled bool
		err = tx.QueryRow(ctx, `SELECT COALESCE((p.body->>'enabled')::boolean, false)
FROM personal_objects p JOIN account_object_versions v
  ON v.service_instance_id=p.service_instance_id AND v.owner_id=p.owner_id
 AND v.object_type=p.object_type AND v.object_id=p.object_id
WHERE p.service_instance_id=$1::uuid AND p.owner_id=$2
  AND p.object_type='skill_setting' AND p.object_id='builtin_memory' AND NOT v.is_deleted`,
			instanceID, current.OwnerID).Scan(&enabled)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check personal memory consent: %w", err)
		}
		if !enabled {
			return domain.NewError(domain.CodeForbidden, "personal memory is disabled")
		}
	}
	if _, err := applyPersonalMutationInTx(ctx, tx, principal, commit.Mutation, ""); err != nil {
		return err
	}
	return nil
}

func cloudDSHPersonalTaskPrincipal(current task.Task) (identity.Principal, error) {
	kind := identity.Kind(current.Authorization.Kind)
	if current.ProjectID != "" || current.InitiatorID != current.OwnerID ||
		(kind != identity.KindSession && kind != identity.KindClient) || !domain.ValidUserID(current.OwnerID) ||
		!domain.ValidTokenID(current.Authorization.TokenID) || current.Authorization.ExpiresAt == nil {
		return identity.Principal{}, domain.NewError(domain.CodeForbidden, "personal Task credential is unavailable")
	}
	var taskRead, taskSubmit bool
	for _, scope := range current.Authorization.Scopes {
		taskRead = taskRead || scope == string(identity.ScopeTaskRead)
		taskSubmit = taskSubmit || scope == string(identity.ScopeTaskSubmit)
	}
	if !taskRead || !taskSubmit {
		return identity.Principal{}, domain.NewError(domain.CodeForbidden, "personal Task read and submit scopes are required")
	}
	principal := identity.Principal{
		UserID: current.OwnerID, Role: identity.RoleUser, Kind: kind, TokenID: current.Authorization.TokenID,
		ClientInstallationID: current.Authorization.ClientInstallationID,
	}
	if kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return identity.Principal{}, domain.NewError(domain.CodeForbidden, "personal Task installation is unavailable")
	}
	return principal, nil
}

func findCloudDSHPersonalMutationEvent(ctx context.Context, tx pgx.Tx, taskID, callID string) (task.Event, bool, error) {
	var value task.Event
	err := tx.QueryRow(ctx, `SELECT task_id,sequence,event_type,payload,created_at FROM task_events
WHERE task_id=$1 AND event_type='task.step' AND payload #>> '{detail,tool}' IN
 ('morde_personal_object_update','morde_goal_create','morde_goal_update','morde_goal_delete')
AND payload #>> '{detail,personal_call_id}'=$2 ORDER BY sequence LIMIT 1`, taskID, callID).
		Scan(&value.TaskID, &value.Sequence, &value.Type, &value.Payload, &value.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return task.Event{}, false, nil
	}
	if err != nil {
		return task.Event{}, false, fmt.Errorf("find prior personal mutation Task event: %w", err)
	}
	return value, true, nil
}
