package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

// LoadProjectTaskContext is the second phase of a project checkpoint claim.
// Its project/member, execution-control, conversation and Task locks stay held
// until the original authorization and fixed history have all been verified.
// Claiming alone never authorizes a Runtime to start for this recovery path.
func (s *Store) LoadProjectTaskContext(ctx context.Context, value task.Task) (*task.ConversationContext, error) {
	if !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.InitiatorID) ||
		!domain.ValidConversationID(value.ConversationID) || !project.ValidID(value.ProjectID) ||
		value.Status != task.StatusRunning || value.Lease.Owner == "" || value.Lease.Version < 1 ||
		value.Authorization.Version < 1 || value.AgentInstanceID == "" || value.AgentGeneration < 1 ||
		value.AgentSessionID == "" || value.CheckpointThroughSequence < 0 || len(value.CheckpointDigest) != 32 {
		return nil, domain.InvalidArgument("project_task", "project task recovery identity is invalid")
	}
	var result *task.ConversationContext
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if value.WorkspaceID != "" {
			if err := lockWorkspaceMutationFence(ctx, tx, value.WorkspaceID, false); err != nil {
				return err
			}
		}
		var serviceInstanceID string
		if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true FOR SHARE`).Scan(&serviceInstanceID); err != nil {
			return fmt.Errorf("lock project recovery service identity: %w", err)
		}
		if _, _, err := lockProjectForActorInTx(ctx, tx, value.InitiatorID, value.ProjectID, project.RoleMember, false); err != nil {
			return err
		}
		// Inspect the candidate without taking its Task lock, then acquire the
		// project resource and Host locks in the same order as revocation and
		// publication. The Task is locked and compared again below.
		candidate, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return domain.NewError(domain.CodeConflict, "project recovery task is unavailable")
		}
		candidateTask := rowTaskDomain(candidate)
		if candidateTask.ProjectID != value.ProjectID || candidateTask.InitiatorID != value.InitiatorID ||
			candidateTask.HostID != value.HostID || candidateTask.WorkspaceID != value.WorkspaceID ||
			candidateTask.WorkspaceOwnerID != value.WorkspaceOwnerID ||
			!reflect.DeepEqual(candidateTask.WorkspaceProjectResourceVersion, value.WorkspaceProjectResourceVersion) ||
			candidateTask.AgentInstanceID != value.AgentInstanceID || candidateTask.AgentGeneration != value.AgentGeneration {
			return domain.NewError(domain.CodeConflict, "project recovery resource identity changed")
		}
		if candidate.WorkspaceID.Valid {
			workspaceOwner, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID,
				value.InitiatorID, candidate.WorkspaceID.String, false)
			if err != nil {
				return err
			}
			if workspaceOwner != value.WorkspaceOwnerID || value.WorkspaceProjectResourceVersion == nil ||
				version != *value.WorkspaceProjectResourceVersion {
				return domain.NewError(domain.CodeUnauthenticated, "project recovery Workspace authorization changed")
			}
			if err := verifyProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, workspaceOwner, value.WorkspaceID); err != nil {
				return err
			}
		}
		if candidate.HostID.Valid {
			var observedHostOwner string
			if err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1 AND deleted_at IS NULL`, value.HostID).
				Scan(&observedHostOwner); err != nil {
				return domain.NewError(domain.CodeUnauthenticated, "project recovery Host authorization is unavailable")
			}
			if candidate.WorkspaceID.Valid {
				if err := lockProjectWorkspaceHost(ctx, tx, value.HostID, value.WorkspaceOwnerID); err != nil {
					return err
				}
			} else if err := lockTaskHostOwnerGates(ctx, tx, value.InitiatorID, observedHostOwner); err != nil {
				return err
			}
			target, err := lockAgentHostTargetIdentity(ctx, tx, value.ID, value.OwnerID, value.HostID, value.InitiatorID)
			if err != nil || target.TaskHostID != value.HostID || target.TaskActorID != value.InitiatorID ||
				target.OwnerID != observedHostOwner {
				return domain.NewError(domain.CodeUnauthenticated, "project recovery Host authorization is unavailable")
			}
		}
		if candidate.WorkspaceID.Valid {
			var workspaceOwner string
			if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1 AND deleted_at IS NULL FOR SHARE`,
				value.WorkspaceID).Scan(&workspaceOwner); err != nil || workspaceOwner != value.WorkspaceOwnerID {
				return domain.NewError(domain.CodeUnauthenticated, "project recovery Workspace is unavailable")
			}
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, value.InitiatorID, nil)
		if err != nil {
			return err
		}
		var conversationID string
		err = tx.QueryRow(ctx, `SELECT id FROM conversations
WHERE id=$1 AND owner_id=$2 AND project_id=$3 AND archived_at IS NULL FOR SHARE`,
			value.ConversationID, value.OwnerID, value.ProjectID).Scan(&conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "project conversation is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock project recovery conversation: %w", err)
		}
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM tasks
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND project_id=$4 AND conversation_id=$5
 AND status='running' AND execution_location='server' AND execution_epoch=$6
 AND lease_owner=$7 AND lease_version=$8 AND lease_expires_at>clock_timestamp()
 AND current_revision=desired_revision AND auth_version=$9
 AND agent_instance_id=$10 AND agent_generation=$11 AND agent_session_id=$12
 AND agent_engine_version=$13 AND agent_profile_version=$14
 AND checkpoint_through_seq=$15 AND checkpoint_digest=$16
 AND EXISTS (SELECT 1 FROM task_agent_instances agent
   WHERE agent.task_id=tasks.id AND agent.owner_id=tasks.owner_id
     AND agent.agent_instance_id=tasks.agent_instance_id AND agent.agent_generation=tasks.agent_generation
     AND agent.agent_session_id=tasks.agent_session_id AND agent.service_instance_id=$17::uuid)
FOR UPDATE`, value.ID, value.OwnerID, value.InitiatorID, value.ProjectID, value.ConversationID, gate.epoch,
			value.Lease.Owner, value.Lease.Version, value.Authorization.Version, value.AgentInstanceID,
			value.AgentGeneration, value.AgentSessionID, value.AgentEngineVersion, value.AgentProfileVersion,
			value.CheckpointThroughSequence, value.CheckpointDigest, serviceInstanceID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "project task recovery lease or checkpoint changed")
		}
		if err != nil {
			return fmt.Errorf("lock project recovery task: %w", err)
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: id, OwnerID: value.OwnerID})
		if err != nil {
			return fmt.Errorf("read project recovery task: %w", err)
		}
		current := rowTaskDomain(row)
		if !reflect.DeepEqual(current.Authorization, value.Authorization) || current.ModelID != value.ModelID ||
			current.Objective != value.Objective || current.ExecutionLocation != value.ExecutionLocation ||
			current.ApplicationID != value.ApplicationID || current.HostID != value.HostID || current.WorkspaceID != value.WorkspaceID ||
			current.WorkspaceOwnerID != value.WorkspaceOwnerID || !reflect.DeepEqual(current.WorkspaceProjectResourceVersion, value.WorkspaceProjectResourceVersion) ||
			current.FileDeliveryIntent != value.FileDeliveryIntent || !reflect.DeepEqual(current.InputRevision, value.InputRevision) ||
			!reflect.DeepEqual(current.WorkspaceCheckpointRevision, value.WorkspaceCheckpointRevision) || !bytes.Equal(current.ContextDigest, value.ContextDigest) ||
			!reflect.DeepEqual(current.Budget, value.Budget) || !bytes.Equal(current.CheckpointDigest, value.CheckpointDigest) {
			return domain.NewError(domain.CodeConflict, "project task recovery identity changed")
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, row)
		if err != nil {
			return err
		}
		if !authorized {
			return domain.NewError(domain.CodeUnauthenticated, "project task authorization is unavailable")
		}
		if err := lockProjectRecoveryResources(ctx, tx, row); err != nil {
			return err
		}
		result, err = readTaskConversationContextWithProjectLock(ctx, tx, current, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// PauseProjectTaskRecovery fences only the exact claimed recovery lease. It
// schedules the normal stop protocol; it never assumes an old Agent stopped.
func (s *Store) PauseProjectTaskRecovery(ctx context.Context, value task.Task) error {
	if !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.InitiatorID) ||
		!project.ValidID(value.ProjectID) || value.Lease.Owner == "" || value.Lease.Version < 1 ||
		value.Authorization.Version < 1 || value.AgentGeneration < 1 || value.CheckpointThroughSequence < 0 || len(value.CheckpointDigest) != 32 {
		return domain.InvalidArgument("project_task", "project task recovery identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var safeAfter time.Time
		err := tx.QueryRow(ctx, `SELECT GREATEST(COALESCE(runtime_lease_safe_after,'-infinity'::timestamptz),
COALESCE(lease_expires_at,'-infinity'::timestamptz),clock_timestamp()) FROM tasks
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND project_id=$4 AND conversation_id=$5
 AND status='running' AND lease_owner=$6 AND lease_version=$7 AND lease_expires_at>clock_timestamp()
 AND auth_version=$8 AND agent_instance_id=$9 AND agent_generation=$10 AND agent_session_id=$11
 AND checkpoint_through_seq=$12 AND checkpoint_digest=$13 AND current_revision=desired_revision
 AND NOT EXISTS (SELECT 1 FROM task_stop_requests stop WHERE stop.task_id=tasks.id AND stop.status='pending')
FOR UPDATE`, value.ID, value.OwnerID, value.InitiatorID, value.ProjectID, value.ConversationID,
			value.Lease.Owner, value.Lease.Version, value.Authorization.Version, value.AgentInstanceID,
			value.AgentGeneration, value.AgentSessionID, value.CheckpointThroughSequence, value.CheckpointDigest).Scan(&safeAfter)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "project recovery lease is no longer current")
		}
		if err != nil {
			return fmt.Errorf("lock project recovery failure: %w", err)
		}
		if value.Lease.Version == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "task lease version is exhausted")
		}
		requestID, err := domain.NewTaskActionID()
		if err != nil {
			return fmt.Errorf("generate project recovery stop request: %w", err)
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='pausing',waiting_reason='project_recovery_unavailable',
lease_version=lease_version+1,lease_owner='',lease_expires_at=NULL,event_seq=event_seq+1,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='running' AND lease_owner=$3 AND lease_version=$4 RETURNING event_seq`,
			value.ID, value.OwnerID, value.Lease.Owner, value.Lease.Version).Scan(&sequence)
		if err != nil {
			return fmt.Errorf("fence project recovery failure: %w", err)
		}
		if err := interruptAssistantMessageSnapshot(ctx, tx, value.ID, "", true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations SET cancel_requested=true,updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, value.ID); err != nil {
			return fmt.Errorf("fence project recovery connector calls: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_stop_requests(id,task_id,action,status,safe_after)
VALUES ($1,$2,'pause','pending',$3)`, requestID, value.ID, safeAfter); err != nil {
			return fmt.Errorf("persist project recovery stop request: %w", err)
		}
		payload, err := json.Marshal(map[string]string{"status": "pausing", "reason": "project_recovery_unavailable",
			"message": "项目任务的权限或原始上下文已变化，正在安全停止此前执行。"})
		if err != nil {
			return fmt.Errorf("encode project recovery stop event: %w", err)
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: value.ID, Sequence: sequence, Type: "task.status", Payload: payload})
	})
}

// Previously bound tools may have contributed data to the checkpoint. Recheck
// their authorization without requiring a temporarily offline Host to run.
func lockProjectRecoveryResources(ctx context.Context, tx pgx.Tx, value sqlcgen.Task) error {
	if value.HostID.Valid {
		var priorHost, priorOwner, priorActor, priorKind, priorGrant string
		err := tx.QueryRow(ctx, `SELECT COALESCE(host_id,''),COALESCE(host_owner_id,''),COALESCE(host_actor_id,''),
COALESCE(host_authorization_kind,''),COALESCE(host_grant_fingerprint,'') FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_instance_id=$3 AND agent_generation=$4 FOR SHARE`,
			value.ID, value.OwnerID, value.AgentInstanceID, value.AgentGeneration).
			Scan(&priorHost, &priorOwner, &priorActor, &priorKind, &priorGrant)
		if err != nil || priorHost != value.HostID.String || priorActor != value.InitiatorID {
			return domain.NewError(domain.CodeUnauthenticated, "project recovery Host authorization is unavailable")
		}
		target, err := lockAgentHostTarget(ctx, tx, value.ID, value.OwnerID)
		if err != nil || target.Deleted || target.OwnerID != priorOwner {
			return domain.NewError(domain.CodeUnauthenticated, "project recovery Host authorization is unavailable")
		}
		if target.OwnerID == value.InitiatorID {
			if priorKind != "owner" || priorGrant != "" {
				return domain.NewError(domain.CodeUnauthenticated, "project recovery Host authorization changed")
			}
		} else {
			var grantVersion string
			var createdAt time.Time
			var permissions []string
			err := tx.QueryRow(ctx, `SELECT grant_version::text,created_at,permissions FROM host_members
WHERE host_id=$1 AND owner_id=$2 AND user_id=$3 AND revoked_at IS NULL FOR SHARE`,
				value.HostID.String, target.OwnerID, value.InitiatorID).Scan(&grantVersion, &createdAt, &permissions)
			if err != nil || priorKind != "member" || priorGrant == "" ||
				fingerprintHostMemberGrant(createdAt, grantVersion, permissions) != priorGrant {
				return domain.NewError(domain.CodeUnauthenticated, "project recovery Host authorization changed")
			}
			allowed, err := hasControlPermission(ctx, tx, value.HostID.String, target.OwnerID, value.InitiatorID, "desktop:control")
			if err != nil || !allowed {
				return domain.NewError(domain.CodeUnauthenticated, "project recovery Host authorization is unavailable")
			}
		}
	}
	if value.WorkspaceID.Valid {
		workspaceOwner, resourceVersion, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID.String, value.InitiatorID, value.WorkspaceID.String, false)
		if err != nil {
			return err
		}
		var storedOwner string
		var storedVersion *int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(workspace_owner_id,''),workspace_project_resource_version FROM tasks WHERE id=$1`, value.ID).
			Scan(&storedOwner, &storedVersion); err != nil {
			return fmt.Errorf("read project recovery Workspace authorization: %w", err)
		}
		if storedOwner != workspaceOwner || storedVersion == nil || *storedVersion != resourceVersion {
			return domain.NewError(domain.CodeUnauthenticated, "project recovery Workspace authorization changed")
		}
		var revision int64
		var ready bool
		if err := tx.QueryRow(ctx, `SELECT revision,state='ready' AND deleted_at IS NULL FROM workspaces
WHERE id=$1 AND owner_id=$2 FOR SHARE`, value.WorkspaceID.String, workspaceOwner).Scan(&revision, &ready); err != nil || !ready {
			return domain.NewError(domain.CodeUnauthenticated, "project recovery Workspace is unavailable")
		}
		if !value.WorkspaceCheckpointRevision.Valid || revision != value.WorkspaceCheckpointRevision.Int64 {
			return domain.NewError(domain.CodeConflict, "project recovery Workspace snapshot changed")
		}
		var transferred bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_ownership_transfers
WHERE workspace_id=$1 AND state='accepted' AND accepted_at >= $2)`, value.WorkspaceID.String, value.CreatedAt.Time).Scan(&transferred); err != nil {
			return fmt.Errorf("verify project recovery Workspace ownership history: %w", err)
		}
		if transferred {
			return domain.NewError(domain.CodeUnauthenticated, "project recovery Workspace ownership changed")
		}
	}
	return nil
}
