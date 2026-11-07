package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

func deferredTargetFence(value task.Task) app.AgentInstanceFence {
	return app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		AgentInstanceID: value.AgentInstanceID, AgentGeneration: value.AgentGeneration, AgentSessionID: value.AgentSessionID,
		LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version, AuthorizationVersion: value.Authorization.Version,
		EngineVersion: value.AgentEngineVersion, ProfileVersion: value.AgentProfileVersion}
}

func matchesDeferredTaskAgent(current agentTaskLock, value task.Task) bool {
	return current.InitiatorID == value.InitiatorID && current.ProjectID == value.ProjectID &&
		current.ConversationID == value.ConversationID && current.InstanceID != "" && current.Generation > 0 &&
		current.InstanceID == value.AgentInstanceID && current.Generation == value.AgentGeneration &&
		current.SessionID == value.AgentSessionID && current.EngineVersion == value.AgentEngineVersion &&
		current.ProfileVersion == value.AgentProfileVersion
}

type deferredTargetSelection struct {
	target                                         app.DeferredTaskTarget
	hostSource, workspaceSource, applicationSource string
}

func loadDeferredTaskTargetInTx(ctx context.Context, tx pgx.Tx, value task.Task) (deferredTargetSelection, bool, error) {
	var raw []byte
	var sourceInstance, currentInstance string
	err := tx.QueryRow(ctx, `SELECT r.request_metadata,r.service_instance_id::text,i.instance_id::text FROM conversation_message_routes r
JOIN service_identity i ON i.singleton=true
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id
 AND m.conversation_id=r.conversation_id AND m.actor_id=r.actor_id
WHERE m.task_id=$1 AND m.owner_id=$2 AND m.conversation_id=$3 AND m.actor_id=$4
 AND m.role='user' AND r.state='routed'
 AND NOT EXISTS (SELECT 1 FROM task_revision_intents revision WHERE revision.message_id=m.id)
ORDER BY m.sequence ASC LIMIT 1`, value.ID, value.OwnerID, value.ConversationID, value.InitiatorID).Scan(&raw, &sourceInstance, &currentInstance)
	if errors.Is(err, pgx.ErrNoRows) {
		return deferredTargetSelection{}, false, nil
	}
	if err != nil {
		return deferredTargetSelection{}, false, fmt.Errorf("read deferred Task target intent: %w", err)
	}
	if sourceInstance != currentInstance {
		return deferredTargetSelection{target: app.DeferredTaskTarget{Unavailable: true}}, true, nil
	}
	var input app.ConversationRoutingInput
	if json.Unmarshal(raw, &input) != nil || input.ProjectID != value.ProjectID ||
		(input.HostID != "" && !domain.ValidHostID(input.HostID)) ||
		(input.WorkspaceID != "" && !domain.ValidWorkspaceID(input.WorkspaceID)) ||
		(input.ApplicationID != "" && !app.ValidHostApplicationID(input.ApplicationID)) {
		return deferredTargetSelection{}, false, domain.NewError(domain.CodeInvalidState, "deferred Task target intent is invalid")
	}
	selection := deferredTargetSelection{target: app.DeferredTaskTarget{HostID: input.HostID, WorkspaceID: input.WorkspaceID, ApplicationID: input.ApplicationID}}
	rows, err := tx.Query(ctx, `SELECT r.request_metadata,r.service_instance_id::text,i.instance_id::text,intent.message_id FROM task_revision_intents intent
JOIN conversation_message_routes r ON r.message_id=intent.message_id AND r.owner_id=intent.owner_id
 AND r.actor_id=intent.initiator_id AND r.conversation_id=intent.conversation_id
JOIN service_identity i ON i.singleton=true
JOIN tasks t ON t.id=intent.task_id AND t.owner_id=intent.owner_id AND t.initiator_id=intent.initiator_id
WHERE intent.task_id=$1 AND intent.owner_id=$2 AND intent.initiator_id=$3 AND intent.conversation_id=$4
 AND intent.state='applied' AND intent.expected_revision<t.current_revision
ORDER BY intent.expected_revision`, value.ID, value.OwnerID, value.InitiatorID, value.ConversationID)
	if err != nil {
		return deferredTargetSelection{}, false, fmt.Errorf("read revised Task target intent: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var metadata []byte
		var routeInstance, instanceID, messageID string
		var update app.ConversationRoutingInput
		if err := rows.Scan(&metadata, &routeInstance, &instanceID, &messageID); err != nil {
			return deferredTargetSelection{}, false, fmt.Errorf("scan revised Task target intent: %w", err)
		}
		if routeInstance != instanceID {
			return deferredTargetSelection{target: app.DeferredTaskTarget{Unavailable: true}}, true, nil
		}
		if json.Unmarshal(metadata, &update) != nil || update.ProjectID != value.ProjectID ||
			(update.HostID != "" && !domain.ValidHostID(update.HostID)) ||
			(update.WorkspaceID != "" && !domain.ValidWorkspaceID(update.WorkspaceID)) ||
			(update.ApplicationID != "" && !app.ValidHostApplicationID(update.ApplicationID)) {
			return deferredTargetSelection{}, false, domain.NewError(domain.CodeInvalidState, "revised Task target intent is invalid")
		}
		if update.HostID != "" {
			if update.HostID != selection.target.HostID {
				selection.target.HostID, selection.target.WorkspaceID, selection.target.ApplicationID = update.HostID, "", ""
				selection.workspaceSource, selection.applicationSource = "", ""
			}
			selection.hostSource = messageID
		}
		if update.WorkspaceID != "" {
			selection.target.WorkspaceID, selection.workspaceSource = update.WorkspaceID, messageID
		}
		if update.ApplicationID != "" {
			selection.target.ApplicationID, selection.applicationSource = update.ApplicationID, messageID
		}
	}
	if err := rows.Err(); err != nil {
		return deferredTargetSelection{}, false, fmt.Errorf("iterate revised Task target intent: %w", err)
	}
	return selection, true, nil
}

// Until Host/Workspace operation ledgers carry a separate selection-source
// fence, only revisions made with the Task's own credential may select a
// target. Task dispatch already rechecks that credential atomically. This
// check happens only when a tool is actually used, never while starting chat.
func verifyDeferredTargetSources(ctx context.Context, tx pgx.Tx, value task.Task, selection deferredTargetSelection, host, workspace bool) error {
	if selection.target.Unavailable {
		return domain.NewError(domain.CodeForbidden, "task target belongs to another service instance")
	}
	ids := make(map[string]struct{})
	if host {
		ids[selection.hostSource] = struct{}{}
		ids[selection.applicationSource] = struct{}{}
	}
	if workspace {
		ids[selection.workspaceSource] = struct{}{}
	}
	delete(ids, "")
	for id := range ids {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT r.authorization_snapshot FROM conversation_message_routes r
JOIN service_identity i ON i.singleton=true AND i.instance_id=r.service_instance_id
WHERE r.message_id=$1 AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4 FOR SHARE OF r`,
			id, value.OwnerID, value.InitiatorID, value.ConversationID).Scan(&raw)
		if err != nil {
			return domain.NewError(domain.CodeForbidden, "task target selection authorization is unavailable")
		}
		var source task.Authorization
		if json.Unmarshal(raw, &source) != nil || source.Kind != value.Authorization.Kind ||
			source.TokenID != value.Authorization.TokenID || source.ClientInstallationID != value.Authorization.ClientInstallationID ||
			!sameStringSlices(source.Scopes, value.Authorization.Scopes) {
			return domain.NewError(domain.CodeForbidden, "task target selection requires the original Task credential")
		}
		claim := app.ConversationRouteClaim{Message: conversation.Message{ActorID: value.InitiatorID}, Authorization: source}
		allowed, err := revisionAuthorizationCurrent(ctx, tx, claim)
		if err != nil {
			return err
		}
		if !allowed {
			return domain.NewError(domain.CodeForbidden, "task target selection authorization is unavailable")
		}
	}
	return nil
}

// LoadDeferredTaskTarget reads user intent without consulting Host health or
// Workspace state. The active task lease and initiating account are checked.
func (s *Store) LoadDeferredTaskTarget(ctx context.Context, value task.Task) (app.DeferredTaskTarget, bool, error) {
	if ctx == nil || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidUserID(value.InitiatorID) || !domain.ValidConversationID(value.ConversationID) {
		return app.DeferredTaskTarget{}, false, domain.InvalidArgument("task", "deferred Task identity is invalid")
	}
	var target app.DeferredTaskTarget
	var found bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, deferredTargetFence(value))
		if err != nil {
			return err
		}
		if !matchesDeferredTaskAgent(current, value) {
			return domain.NewError(domain.CodeInvalidState, "deferred Task Agent changed")
		}
		selection, selectionFound, err := loadDeferredTaskTargetInTx(ctx, tx, value)
		target, found = selection.target, selectionFound
		return err
	})
	return target, found, err
}

// BindDeferredTaskWorkspace freezes the selected personal Workspace without
// consulting a Host. Both the intent and the Agent lease are rechecked under
// the same transaction as the immutable input revision.
func (s *Store) BindDeferredTaskWorkspace(ctx context.Context, value task.Task, workspaceID string, revision int64, digest []byte) (task.Task, error) {
	if ctx == nil || !domain.ValidWorkspaceID(workspaceID) || revision < 0 || len(digest) != 32 ||
		(value.ProjectID == "" && value.OwnerID != value.InitiatorID) {
		return task.Task{}, domain.InvalidArgument("workspace", "deferred Workspace target is invalid")
	}
	var result task.Task
	err := s.withProjectWorkspaceSelectionTx(ctx, value, workspaceID, "", func(tx pgx.Tx) error {
		workspaceOwner := value.InitiatorID
		var resourceVersion *int64
		if value.ProjectID != "" {
			owner, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID, value.InitiatorID, workspaceID, false)
			if err != nil {
				return err
			}
			workspaceOwner, resourceVersion = owner, &version
		}
		current, err := lockAuthorizedAgentTask(ctx, tx, deferredTargetFence(value))
		if err != nil {
			return err
		}
		if !matchesDeferredTaskAgent(current, value) {
			return domain.NewError(domain.CodeInvalidState, "deferred Task Agent changed")
		}
		intent, found, err := loadDeferredTaskTargetInTx(ctx, tx, value)
		if err != nil {
			return err
		}
		if !found || intent.target.WorkspaceID != workspaceID {
			return domain.NewError(domain.CodeForbidden, "Workspace does not match the user selection")
		}
		if err := verifyDeferredTargetSources(ctx, tx, value, intent, false, true); err != nil {
			return err
		}
		var revisionCurrent bool
		if err := tx.QueryRow(ctx, `SELECT current_revision=desired_revision FROM tasks WHERE id=$1`, value.ID).Scan(&revisionCurrent); err != nil {
			return fmt.Errorf("check deferred Workspace Task revision: %w", err)
		}
		if !revisionCurrent {
			return domain.NewError(domain.CodeConflict, "task revision is being adjusted")
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return storeError("read deferred Workspace Task", "task", err)
		}
		bound := rowTaskDomain(row)
		if bound.HostID != "" || (bound.WorkspaceID != "" && bound.WorkspaceID != workspaceID) {
			return domain.NewError(domain.CodeConflict, "Task target cannot be changed")
		}
		instance, err := lockAgentInstance(ctx, tx, value.ID, value.OwnerID, value.AgentGeneration)
		if err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM task_agent_instances
WHERE task_id=$1 AND agent_generation=$2 AND state='active'`, value.ID, value.AgentGeneration).Scan(&active); err != nil || !active ||
			instance.Fence.AgentInstanceID != value.AgentInstanceID || instance.Fence.AgentSessionID != value.AgentSessionID ||
			instance.Fence.LeaseOwner != value.Lease.Owner || instance.Fence.LeaseVersion != value.Lease.Version ||
			instance.Fence.AuthorizationVersion != value.Authorization.Version || instance.HostFence.HostID != "" {
			return domain.NewError(domain.CodeInvalidState, "deferred Workspace Agent lease changed")
		}
		if bound.WorkspaceID == "" {
			var unresolved bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS (SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS (SELECT 1 FROM task_hub_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE task_id=$1 AND state='pending')`, value.ID).Scan(&unresolved); err != nil {
				return fmt.Errorf("check deferred Workspace operation barriers: %w", err)
			}
			if unresolved {
				return domain.NewError(domain.CodeConflict, "existing task operations require reconciliation")
			}
			bound.WorkspaceID, bound.WorkspaceOwnerID, bound.WorkspaceProjectResourceVersion = workspaceID, workspaceOwner, resourceVersion
			bound.InputRevision, bound.WorkspaceCheckpointRevision, bound.ContextDigest = &revision, &revision, digest
			if err := lockTaskWorkspaceInput(ctx, tx, bound); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET workspace_id=$2,input_revision=$3,workspace_checkpoint_revision=$3,context_digest=$4,
workspace_owner_id=$5,workspace_project_resource_version=$6 WHERE id=$1`,
				value.ID, workspaceID, revision, digest, workspaceOwner, resourceVersion); err != nil {
				return fmt.Errorf("bind deferred Task Workspace: %w", err)
			}
			if value.ProjectID != "" {
				if err := pinProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, workspaceOwner, workspaceID); err != nil {
					return err
				}
			}
		} else if bound.InputRevision == nil || *bound.InputRevision != revision || !bytes.Equal(bound.ContextDigest, digest) ||
			bound.WorkspaceOwnerID != workspaceOwner || !sameTaskInputRevision(bound.WorkspaceProjectResourceVersion, resourceVersion) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace snapshot is already fixed")
		} else if value.ProjectID != "" {
			if err := verifyProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, workspaceOwner, workspaceID); err != nil {
				return err
			}
		}
		row, err = sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return storeError("read bound Workspace Task", "task", err)
		}
		result = rowTaskDomain(row)
		result.ConversationContext = value.ConversationContext
		return nil
	})
	return result, err
}

// BindDeferredTaskTarget atomically binds only the target selected in the
// original message, together with the active Agent's Host authorization fence.
// The snapshot is prepared outside the transaction and revalidated under the
// Workspace lock before it is attached to the task.
func (s *Store) BindDeferredTaskTarget(ctx context.Context, value task.Task, selected app.DeferredTaskTarget, revision *int64, digest []byte) (task.Task, error) {
	if ctx == nil || !domain.ValidHostID(selected.HostID) ||
		(selected.WorkspaceID == "") != (revision == nil) ||
		(selected.WorkspaceID == "") != (len(digest) == 0) ||
		(selected.WorkspaceID != "" && (!domain.ValidWorkspaceID(selected.WorkspaceID) || *revision < 0 || len(digest) != 32)) {
		return task.Task{}, domain.InvalidArgument("target", "deferred Task target is invalid")
	}
	var result task.Task
	err := s.withProjectWorkspaceSelectionTx(ctx, value, selected.WorkspaceID, selected.HostID, func(tx pgx.Tx) error {
		workspaceOwner := value.InitiatorID
		var resourceVersion *int64
		if value.ProjectID != "" {
			if _, _, err := lockProjectForActorInTx(ctx, tx, value.InitiatorID, value.ProjectID, project.RoleMember, false); err != nil {
				return err
			}
			if selected.WorkspaceID != "" {
				owner, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID, value.InitiatorID, selected.WorkspaceID, false)
				if err != nil {
					return err
				}
				workspaceOwner, resourceVersion = owner, &version
				if owner != value.InitiatorID {
					grant, err := lockProjectWorkspaceGrant(ctx, tx, value.InitiatorID, owner, selected.WorkspaceID)
					if err != nil || grant.hostID != selected.HostID {
						return domain.NewError(domain.CodeForbidden, "project Workspace is not bound to the selected Host")
					}
				}
			}
		}
		// Host control and membership changes lock Host first, before the Task.
		if err := lockTaskTargetHost(ctx, tx, selected.HostID, value.InitiatorID); err != nil {
			return err
		}
		current, err := lockAuthorizedAgentTask(ctx, tx, deferredTargetFence(value))
		if err != nil {
			return err
		}
		if !matchesDeferredTaskAgent(current, value) {
			return domain.NewError(domain.CodeInvalidState, "deferred Task Agent changed")
		}
		intent, found, err := loadDeferredTaskTargetInTx(ctx, tx, value)
		if err != nil {
			return err
		}
		if !found || intent.target.HostID != selected.HostID || intent.target.ApplicationID != selected.ApplicationID ||
			(selected.WorkspaceID != "" && intent.target.WorkspaceID != selected.WorkspaceID) {
			return domain.NewError(domain.CodeForbidden, "deferred Task target does not match the user selection")
		}
		if err := verifyDeferredTargetSources(ctx, tx, value, intent, true, selected.WorkspaceID != ""); err != nil {
			return err
		}
		var revisionCurrent bool
		if err := tx.QueryRow(ctx, `SELECT current_revision=desired_revision FROM tasks WHERE id=$1`, value.ID).Scan(&revisionCurrent); err != nil {
			return fmt.Errorf("check deferred Task revision: %w", err)
		}
		if !revisionCurrent {
			return domain.NewError(domain.CodeConflict, "task revision is being adjusted")
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return storeError("read deferred target Task", "task", err)
		}
		bound := rowTaskDomain(row)
		if (bound.HostID != "" && bound.HostID != selected.HostID) ||
			(bound.WorkspaceID != "" && selected.WorkspaceID != "" && bound.WorkspaceID != selected.WorkspaceID) ||
			(bound.ApplicationID != "" && bound.ApplicationID != selected.ApplicationID) {
			return domain.NewError(domain.CodeConflict, "task target cannot be changed")
		}
		instance, err := lockAgentInstance(ctx, tx, value.ID, value.OwnerID, value.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.State != "active" || instance.Fence.AgentInstanceID != value.AgentInstanceID ||
			instance.Fence.AgentSessionID != value.AgentSessionID || instance.Fence.LeaseOwner != value.Lease.Owner ||
			instance.Fence.LeaseVersion != value.Lease.Version || instance.Fence.AuthorizationVersion != value.Authorization.Version {
			return domain.NewError(domain.CodeInvalidState, "deferred Task Agent lease changed")
		}
		var agentLeaseActive bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`,
			value.ID, value.AgentGeneration).Scan(&agentLeaseActive); err != nil || !agentLeaseActive {
			return domain.NewError(domain.CodeInvalidState, "deferred Task Agent lease expired")
		}
		if !row.HostID.Valid || (selected.WorkspaceID != "" && !row.WorkspaceID.Valid) {
			var unresolved bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS (SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS (SELECT 1 FROM task_hub_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE task_id=$1 AND state='pending')`, value.ID).Scan(&unresolved); err != nil {
				return fmt.Errorf("check deferred target operation barriers: %w", err)
			}
			if unresolved || (!row.HostID.Valid && instance.HostFence.HostID != "") {
				return domain.NewError(domain.CodeConflict, "existing task operations require reconciliation before binding a target")
			}
		}
		if selected.WorkspaceID != "" && bound.WorkspaceID == "" {
			bound.HostID, bound.WorkspaceID = selected.HostID, selected.WorkspaceID
			bound.WorkspaceOwnerID, bound.WorkspaceProjectResourceVersion = workspaceOwner, resourceVersion
			bound.InputRevision, bound.WorkspaceCheckpointRevision, bound.ContextDigest = revision, revision, digest
			if err := lockTaskWorkspaceInput(ctx, tx, bound); err != nil {
				return err
			}
		} else if selected.WorkspaceID != "" &&
			(!sameTaskInputRevision(bound.InputRevision, revision) || !sameTaskInputRevision(bound.WorkspaceCheckpointRevision, revision) ||
				bound.WorkspaceOwnerID != workspaceOwner || !sameTaskInputRevision(bound.WorkspaceProjectResourceVersion, resourceVersion)) {
			return domain.NewError(domain.CodeWorkspaceConflict, "task Workspace snapshot is already fixed")
		}
		if !row.HostID.Valid {
			// The Agent's Host authorization snapshot is established exactly once.
			if _, err := tx.Exec(ctx, `UPDATE tasks SET host_id=$2,application_id=$3 WHERE id=$1`, value.ID, selected.HostID, selected.ApplicationID); err != nil {
				return fmt.Errorf("bind deferred Task Host: %w", err)
			}
		}
		hostTarget, err := lockAgentHostTarget(ctx, tx, value.ID, value.OwnerID)
		if err != nil {
			return err
		}
		hostFence, err := captureAgentHostFence(ctx, tx, agentTaskLock{HostID: selected.HostID, InitiatorID: value.InitiatorID}, hostTarget)
		if err != nil {
			return err
		}
		if instance.HostFence.HostID == "" {
			updated, err := tx.Exec(ctx, `UPDATE task_agent_instances SET host_id=$3,host_owner_id=$4,host_actor_id=$5,
host_permission=$6,host_authorization_kind=$7,host_runtime_generation=$8,host_runtime_instance_id=$9,
host_control_version=$10,host_grant_fingerprint=$11,updated_at=clock_timestamp()
WHERE task_id=$1 AND agent_generation=$2 AND host_id IS NULL AND state='active' AND lease_expires_at>clock_timestamp()`,
				value.ID, value.AgentGeneration, hostFence.HostID, hostFence.OwnerID, hostFence.ActorID, hostFence.Permission,
				hostFence.AuthorizationKind, hostFence.RuntimeGeneration, hostFence.RuntimeInstanceID, hostFence.ControlVersion,
				hostGrantFingerprintValue(hostFence))
			if err != nil || updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "deferred Task Agent Host fence changed")
			}
		} else if !sameAgentHostFence(instance.HostFence, hostFence) {
			return domain.NewError(domain.CodeConflict, "Task Host authorization changed")
		}
		if selected.WorkspaceID != "" && !row.WorkspaceID.Valid {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET workspace_id=$2,input_revision=$3,workspace_checkpoint_revision=$3,context_digest=$4,
workspace_owner_id=$5,workspace_project_resource_version=$6 WHERE id=$1`,
				value.ID, selected.WorkspaceID, *revision, digest, workspaceOwner, resourceVersion); err != nil {
				return fmt.Errorf("bind deferred Task Workspace: %w", err)
			}
			if value.ProjectID != "" {
				if err := pinProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, workspaceOwner, selected.WorkspaceID); err != nil {
					return err
				}
			}
		} else if selected.WorkspaceID != "" && value.ProjectID != "" {
			if err := verifyProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, workspaceOwner, selected.WorkspaceID); err != nil {
				return err
			}
		}
		row, err = sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return storeError("read bound Task", "task", err)
		}
		result = rowTaskDomain(row)
		result.ConversationContext = value.ConversationContext
		return nil
	})
	return result, err
}
