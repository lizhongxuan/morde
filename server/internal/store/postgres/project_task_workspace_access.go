package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

// lockProjectTaskWorkspaceAccess keeps the project, resource and independent
// Workspace grant locked through the caller's transaction. Project membership
// alone never grants access, and the existing shared Workspace permission is
// read-only; writes continue to require the Workspace owner.
func lockProjectTaskWorkspaceAccess(ctx context.Context, tx pgx.Tx, projectID, actorID, workspaceID string, write bool) (string, int64, error) {
	if permit := projectWorkspacePermit(ctx, projectID, actorID, workspaceID, write); permit != nil {
		return permit.workspaceOwnerID, permit.resourceVersion, nil
	}
	if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleMember, false); err != nil {
		return "", 0, err
	}
	var ownerID string
	var version int64
	err := tx.QueryRow(ctx, `SELECT resource_owner_id,version FROM project_resources
WHERE project_id=$1 AND kind='workspace' AND resource_id=$2 AND workspace_id=$2 AND active FOR SHARE`,
		projectID, workspaceID).Scan(&ownerID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, domain.NewError(domain.CodeNotFound, "project Workspace is unavailable")
	}
	if err != nil {
		return "", 0, fmt.Errorf("lock project Task Workspace resource: %w", err)
	}
	if write && actorID != ownerID {
		return "", 0, domain.NewError(domain.CodeForbidden, "project Workspace write is not authorized")
	}
	// Host membership revocation locks its owner's execution gate before the
	// Host. Hold both relevant gates first, in a stable order, for shared
	// project resources where the actor and Workspace owner can differ.
	first, second := actorID, ownerID
	if second < first {
		first, second = second, first
	}
	if _, err := lockOwnerExecutionControl(ctx, tx, first, false); err != nil {
		return "", 0, err
	}
	if second != first {
		if _, err := lockOwnerExecutionControl(ctx, tx, second, false); err != nil {
			return "", 0, err
		}
	}
	if ownerID != actorID {
		if _, err := lockProjectWorkspaceGrant(ctx, tx, actorID, ownerID, workspaceID); err != nil {
			return "", 0, err
		}
	}
	// Binding and publication may later need this row exclusively; avoid a
	// shared row lock here. The caller rechecks the owner under its Task or
	// Workspace fence, and an outer mutation permit also prevents transfers.
	var currentOwner, state string
	if err := tx.QueryRow(ctx, `SELECT owner_id,state FROM workspaces WHERE id=$1 AND deleted_at IS NULL`, workspaceID).
		Scan(&currentOwner, &state); err != nil || currentOwner != ownerID || state != "ready" {
		return "", 0, domain.NewError(domain.CodeForbidden, "project Workspace authorization is unavailable")
	}
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID); err != nil {
		return "", 0, err
	}
	if version < 1 {
		return "", 0, domain.NewError(domain.CodeForbidden, "project Workspace authorization is unavailable")
	}
	return ownerID, version, nil
}

type projectWorkspaceGrant struct {
	hostID, version, fingerprint string
	bindingVersion               int64
}

// Host membership and binding changes take the Host lock first. Holding that
// lock keeps the selected binding and member grant stable without upgrading a
// Workspace row lock in callers which will later bind or publish.
func lockProjectWorkspaceGrant(ctx context.Context, tx pgx.Tx, actorID, ownerID, workspaceID string) (projectWorkspaceGrant, error) {
	var grant projectWorkspaceGrant
	err := tx.QueryRow(ctx, `SELECT host_id FROM workspace_bindings
WHERE workspace_id=$1 AND owner_id=$2 AND state='active'`, workspaceID, ownerID).Scan(&grant.hostID)
	if err != nil {
		return grant, domain.NewError(domain.CodeForbidden, "project Workspace grant is unavailable")
	}
	var hostOwner, hostState string
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT owner_id,state,deleted_at IS NOT NULL FROM hosts WHERE id=$1 FOR SHARE`,
		grant.hostID).Scan(&hostOwner, &hostState, &deleted); err != nil || hostOwner != ownerID || deleted ||
		hostState == "deleting" || hostState == "deleted" {
		return projectWorkspaceGrant{}, domain.NewError(domain.CodeForbidden, "project Workspace grant is unavailable")
	}
	if err := tx.QueryRow(ctx, `SELECT binding_version FROM workspace_bindings
WHERE workspace_id=$1 AND owner_id=$2 AND host_id=$3 AND state='active'`, workspaceID, ownerID, grant.hostID).
		Scan(&grant.bindingVersion); err != nil || grant.bindingVersion < 1 {
		return projectWorkspaceGrant{}, domain.NewError(domain.CodeForbidden, "project Workspace binding changed")
	}
	var createdAt time.Time
	var permissions []string
	if err := tx.QueryRow(ctx, `SELECT grant_version::text,created_at,permissions FROM host_members
WHERE host_id=$1 AND owner_id=$2 AND user_id=$3 AND revoked_at IS NULL FOR SHARE`,
		grant.hostID, ownerID, actorID).Scan(&grant.version, &createdAt, &permissions); err != nil ||
		grant.version == "" || createdAt.IsZero() || !hasWorkspaceProjectPermission(permissions) {
		return projectWorkspaceGrant{}, domain.NewError(domain.CodeForbidden, "project Workspace grant is unavailable")
	}
	grant.fingerprint = fingerprintHostMemberGrant(createdAt, grant.version, permissions)
	return grant, nil
}

// The original grant is immutable for a Task. Retrying the bind only succeeds
// if the same Host binding and member grant are still present.
func pinProjectTaskWorkspaceGrant(ctx context.Context, tx pgx.Tx, taskID, projectID, actorID, ownerID, workspaceID string) error {
	if actorID == ownerID {
		return nil
	}
	grant, err := lockProjectWorkspaceGrant(ctx, tx, actorID, ownerID, workspaceID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_workspace_grant_fences
(task_id,project_id,workspace_id,workspace_owner_id,actor_id,host_id,binding_version,grant_version,grant_fingerprint)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8::uuid,$9) ON CONFLICT (task_id) DO NOTHING`,
		taskID, projectID, workspaceID, ownerID, actorID, grant.hostID, grant.bindingVersion, grant.version, grant.fingerprint); err != nil {
		return fmt.Errorf("pin project Task Workspace grant: %w", err)
	}
	return verifyProjectTaskWorkspaceGrant(ctx, tx, taskID, projectID, actorID, ownerID, workspaceID)
}

func verifyProjectTaskWorkspaceGrant(ctx context.Context, tx pgx.Tx, taskID, projectID, actorID, ownerID, workspaceID string) error {
	if actorID == ownerID {
		return nil
	}
	grant, err := lockProjectWorkspaceGrant(ctx, tx, actorID, ownerID, workspaceID)
	if err != nil {
		return err
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_workspace_grant_fences
WHERE task_id=$1 AND project_id=$2 AND actor_id=$3 AND workspace_owner_id=$4 AND workspace_id=$5
 AND host_id=$6 AND binding_version=$7 AND grant_version=$8::uuid AND grant_fingerprint=$9)`,
		taskID, projectID, actorID, ownerID, workspaceID, grant.hostID, grant.bindingVersion, grant.version, grant.fingerprint).
		Scan(&matches); err != nil || !matches {
		return domain.NewError(domain.CodeForbidden, "project Task Workspace grant changed")
	}
	return nil
}

// Check the Host owner before taking its lock so a corrupted cross-owner
// binding cannot introduce a third execution-gate lock order.
func lockProjectWorkspaceHost(ctx context.Context, tx pgx.Tx, hostID, ownerID string) error {
	var observedOwner string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1 AND deleted_at IS NULL`, hostID).
		Scan(&observedOwner); err != nil || observedOwner != ownerID {
		return domain.NewError(domain.CodeForbidden, "project Workspace Host is unavailable")
	}
	var lockedOwner string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, hostID).
		Scan(&lockedOwner); err != nil || lockedOwner != ownerID {
		return domain.NewError(domain.CodeForbidden, "project Workspace Host is unavailable")
	}
	return nil
}

func projectWorkspacePermit(ctx context.Context, projectID, actorID, workspaceID string, write bool) *workspaceMutationPermit {
	permit, _ := ctx.Value(workspaceMutationPermitKey{}).(*workspaceMutationPermit)
	if permit == nil || !permit.active.Load() || permit.projectID != projectID || permit.actorID != actorID ||
		permit.workspaceID != workspaceID || write && !permit.write {
		return nil
	}
	return permit
}

func projectWorkspaceTaskPermit(ctx context.Context, taskID, actorID string) *workspaceMutationPermit {
	permit, _ := ctx.Value(workspaceMutationPermitKey{}).(*workspaceMutationPermit)
	if permit == nil || !permit.active.Load() || permit.taskID != taskID || permit.actorID != actorID {
		return nil
	}
	return permit
}

func projectWorkspaceAuthorizationPermit(ctx context.Context, value sqlcgen.Task) bool {
	if !value.ProjectID.Valid || !value.WorkspaceID.Valid {
		return false
	}
	permit := projectWorkspacePermit(ctx, value.ProjectID.String, value.InitiatorID, value.WorkspaceID.String, false)
	return permit != nil && permit.taskID == value.ID && permit.taskOwnerID == value.OwnerID &&
		value.HostID.String == permit.taskHostID && value.LeaseOwner == permit.leaseOwner && value.LeaseVersion == permit.leaseVersion &&
		value.WorkspaceOwnerID.String == permit.workspaceOwnerID &&
		value.WorkspaceProjectResourceVersion.Valid && value.WorkspaceProjectResourceVersion.Int64 == permit.resourceVersion &&
		value.AgentInstanceID == permit.agentID && value.AgentGeneration == permit.agentGeneration && value.AgentSessionID == permit.agentSessionID &&
		reflect.DeepEqual(rowTaskDomain(value).Authorization, permit.authorization)
}

type projectTaskWorkspaceFence struct {
	projectID, actorID, workspaceID, workspaceOwnerID, hostID string
	resourceVersion                                           *int64
}

// Read the identity without a Task lock, then lock membership and resource
// first. Callers must recheck the returned fence after locking the Task.
func lockProjectTaskWorkspaceFence(ctx context.Context, tx pgx.Tx, taskID string, write bool) (projectTaskWorkspaceFence, error) {
	var fence projectTaskWorkspaceFence
	err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,''),initiator_id,COALESCE(workspace_id,''),
COALESCE(workspace_owner_id,''),workspace_project_resource_version,COALESCE(host_id,'') FROM tasks WHERE id=$1`, taskID).
		Scan(&fence.projectID, &fence.actorID, &fence.workspaceID, &fence.workspaceOwnerID, &fence.resourceVersion, &fence.hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fence, missingTaskError()
	}
	if err != nil {
		return fence, fmt.Errorf("read project Task Workspace fence: %w", err)
	}
	if fence.projectID == "" {
		return fence, nil
	}
	if permit := projectWorkspacePermit(ctx, fence.projectID, fence.actorID, fence.workspaceID, write); permit != nil {
		if permit.taskID != taskID || fence.workspaceOwnerID != permit.workspaceOwnerID ||
			fence.hostID != permit.taskHostID || fence.resourceVersion == nil || *fence.resourceVersion != permit.resourceVersion {
			return fence, domain.NewError(domain.CodeConflict, "project Task Workspace fence changed")
		}
		return fence, nil
	}
	if _, _, err := lockProjectForActorInTx(ctx, tx, fence.actorID, fence.projectID, project.RoleMember, false); err != nil {
		return fence, err
	}
	if fence.workspaceID != "" {
		ownerID, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, fence.projectID, fence.actorID, fence.workspaceID, write)
		if err != nil {
			return fence, err
		}
		if ownerID != fence.workspaceOwnerID || fence.resourceVersion == nil || *fence.resourceVersion != version {
			return fence, domain.NewError(domain.CodeForbidden, "project Task Workspace authorization changed")
		}
		if err := verifyProjectTaskWorkspaceGrant(ctx, tx, taskID, fence.projectID, fence.actorID, ownerID, fence.workspaceID); err != nil {
			return fence, err
		}
		if fence.hostID != "" {
			if fence.actorID != ownerID {
				grant, err := lockProjectWorkspaceGrant(ctx, tx, fence.actorID, ownerID, fence.workspaceID)
				if err != nil || grant.hostID != fence.hostID {
					return fence, domain.NewError(domain.CodeForbidden, "project Workspace Host changed")
				}
			}
			if err := lockProjectWorkspaceHost(ctx, tx, fence.hostID, ownerID); err != nil {
				return fence, err
			}
		}
		if !write {
			var actualOwner, state string
			if err := tx.QueryRow(ctx, `SELECT owner_id,state FROM workspaces WHERE id=$1 AND deleted_at IS NULL FOR SHARE`,
				fence.workspaceID).Scan(&actualOwner, &state); err != nil || actualOwner != ownerID || state != "ready" {
				return fence, domain.NewError(domain.CodeForbidden, "project Task Workspace is unavailable")
			}
			if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, fence.workspaceID); err != nil {
				return fence, err
			}
		}
	}
	return fence, nil
}

func verifyProjectTaskWorkspaceFence(ctx context.Context, tx pgx.Tx, taskID string, fence projectTaskWorkspaceFence) error {
	var matches bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'')=$2 AND initiator_id=$3 AND COALESCE(workspace_id,'')=$4
 AND COALESCE(workspace_owner_id,'')=$5 AND workspace_project_resource_version IS NOT DISTINCT FROM $6::bigint
 AND COALESCE(host_id,'')=$7
FROM tasks WHERE id=$1`, taskID, fence.projectID, fence.actorID, fence.workspaceID, fence.workspaceOwnerID, fence.resourceVersion, fence.hostID).Scan(&matches)
	if err != nil || !matches {
		return domain.NewError(domain.CodeConflict, "project Task Workspace fence changed")
	}
	return nil
}

func (s *Store) ValidateProjectTaskWorkspaceAccess(ctx context.Context, value task.Task, workspaceID string, write bool) (string, error) {
	if ctx == nil || !project.ValidID(value.ProjectID) || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.InitiatorID) ||
		!domain.ValidWorkspaceID(workspaceID) {
		return "", domain.InvalidArgument("workspace", "project Task Workspace identity is invalid")
	}
	var ownerID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		owner, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID, value.InitiatorID, workspaceID, write)
		if err != nil {
			return err
		}
		current, err := lockAuthorizedAgentTask(ctx, tx, deferredTargetFence(value))
		if err != nil || !matchesDeferredTaskAgent(current, value) {
			return domain.NewError(domain.CodeUnauthenticated, "project Task Workspace authorization is unavailable")
		}
		var boundWorkspace, boundOwner string
		var boundVersion *int64
		var revisionCurrent bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(workspace_id,''),COALESCE(workspace_owner_id,''),
workspace_project_resource_version,current_revision=desired_revision FROM tasks WHERE id=$1`, value.ID).
			Scan(&boundWorkspace, &boundOwner, &boundVersion, &revisionCurrent); err != nil {
			return fmt.Errorf("read project Task Workspace binding: %w", err)
		}
		if !revisionCurrent || boundWorkspace != "" && (boundWorkspace != workspaceID || boundOwner != owner || boundVersion == nil || *boundVersion != version) {
			return domain.NewError(domain.CodeConflict, "project Task Workspace binding changed")
		}
		if boundWorkspace != "" {
			if err := verifyProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, owner, workspaceID); err != nil {
				return err
			}
		}
		selection, found, err := loadDeferredTaskTargetInTx(ctx, tx, value)
		if err != nil || (!found && boundWorkspace == "") || (found && selection.target.WorkspaceID != workspaceID) {
			return domain.NewError(domain.CodeForbidden, "project Task Workspace was not selected")
		}
		if found {
			if err := verifyDeferredTargetSources(ctx, tx, value, selection, false, true); err != nil {
				return err
			}
		}
		ownerID = owner
		return nil
	})
	return ownerID, err
}

// WithProjectTaskWorkspaceAccess protects a read or the complete publish
// window against membership/resource/grant revocation. For owner-only writes
// it deliberately does not hold a Workspace row lock: the nested publish
// transaction must acquire that row exclusively to commit its revision.
func (s *Store) WithProjectTaskWorkspaceAccess(ctx context.Context, value task.Task, write bool, operation func(context.Context) error) error {
	if ctx == nil || operation == nil || !project.ValidID(value.ProjectID) || !domain.ValidTaskID(value.ID) ||
		!domain.ValidWorkspaceID(value.WorkspaceID) || !domain.ValidUserID(value.InitiatorID) ||
		value.WorkspaceProjectResourceVersion == nil || value.WorkspaceOwnerID == "" {
		return domain.InvalidArgument("workspace", "project Task Workspace access is invalid")
	}
	if permit := projectWorkspacePermit(ctx, value.ProjectID, value.InitiatorID, value.WorkspaceID, write); permit != nil {
		if permit.taskID != value.ID || permit.taskOwnerID != value.OwnerID || permit.workspaceOwnerID != value.WorkspaceOwnerID ||
			permit.taskHostID != value.HostID ||
			permit.resourceVersion != *value.WorkspaceProjectResourceVersion || permit.leaseOwner != value.Lease.Owner ||
			permit.leaseVersion != value.Lease.Version || permit.agentID != value.AgentInstanceID ||
			permit.agentGeneration != value.AgentGeneration || permit.agentSessionID != value.AgentSessionID ||
			!reflect.DeepEqual(permit.authorization, value.Authorization) {
			return domain.NewError(domain.CodeForbidden, "project Task Workspace permit changed")
		}
		return operation(ctx)
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceMutationFence(ctx, tx, value.WorkspaceID, false); err != nil {
			return err
		}
		ownerID, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID, value.InitiatorID, value.WorkspaceID, write)
		if err != nil {
			return err
		}
		if _, err := requireOwnerExecutionEnabled(ctx, tx, value.InitiatorID, nil); err != nil {
			return err
		}
		if ownerID != value.WorkspaceOwnerID || version != *value.WorkspaceProjectResourceVersion || write && ownerID != value.InitiatorID {
			return domain.NewError(domain.CodeForbidden, "project Workspace authorization changed")
		}
		if err := verifyProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, ownerID, value.WorkspaceID); err != nil {
			return err
		}
		var protectedHostID string
		if value.InitiatorID != ownerID {
			grant, err := lockProjectWorkspaceGrant(ctx, tx, value.InitiatorID, ownerID, value.WorkspaceID)
			if err != nil {
				return err
			}
			protectedHostID = grant.hostID
		}
		if value.HostID != "" {
			if protectedHostID != "" && protectedHostID != value.HostID {
				return domain.NewError(domain.CodeForbidden, "project Workspace Host changed")
			}
			if err := lockProjectWorkspaceHost(ctx, tx, value.HostID, ownerID); err != nil {
				return err
			}
			protectedHostID = value.HostID
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.ID, OwnerID: value.OwnerID})
		if err != nil {
			return domain.NewError(domain.CodeUnauthenticated, "project Task is unavailable")
		}
		if !reflect.DeepEqual(rowTaskDomain(row).Authorization, value.Authorization) {
			return domain.NewError(domain.CodeUnauthenticated, "project Task authorization changed")
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, row)
		if err != nil || !authorized {
			return domain.NewError(domain.CodeUnauthenticated, "project Task authorization is unavailable")
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks t JOIN task_agent_instances agent
ON agent.task_id=t.id AND agent.owner_id=t.owner_id AND agent.agent_instance_id=t.agent_instance_id
 AND agent.agent_generation=t.agent_generation AND agent.agent_session_id=t.agent_session_id
JOIN service_identity instance ON instance.singleton=true AND instance.instance_id=agent.service_instance_id
WHERE t.id=$1 AND t.owner_id=$2 AND t.initiator_id=$3 AND t.project_id=$4 AND t.workspace_id=$5
 AND t.workspace_owner_id=$6 AND t.workspace_project_resource_version=$7 AND COALESCE(t.host_id,'')=$15 AND t.status='running'
 AND t.lease_owner=$8 AND t.lease_version=$9 AND t.lease_expires_at>clock_timestamp()
 AND t.auth_version=$10 AND t.current_revision=t.desired_revision
 AND ((agent.state='active' AND agent.lease_expires_at>clock_timestamp()) OR
   ($14::boolean AND agent.state='stopped' AND (NOT agent.guardian_task_stop_ack_required OR agent.guardian_stop_acknowledged_at IS NOT NULL)
    AND ((agent.stop_receipt IS NOT NULL AND agent.stop_receipt_digest=sha256(agent.stop_receipt)) IS TRUE OR
         (agent.guardian_stop_attestation IS NOT NULL AND agent.guardian_stop_attestation_digest=sha256(agent.guardian_stop_attestation)) IS TRUE)))
 AND agent.lease_owner=$8 AND agent.lease_version=$9 AND agent.auth_version=$10
 AND agent.agent_instance_id=$11 AND agent.agent_generation=$12 AND agent.agent_session_id=$13)`,
			value.ID, value.OwnerID, value.InitiatorID, value.ProjectID, value.WorkspaceID, ownerID, version,
			value.Lease.Owner, value.Lease.Version, value.Authorization.Version, value.AgentInstanceID,
			value.AgentGeneration, value.AgentSessionID, write, value.HostID).Scan(&valid); err != nil || !valid {
			return domain.NewError(domain.CodeUnauthenticated, "project Task Workspace lease is unavailable")
		}
		permit := &workspaceMutationPermit{workspaceID: value.WorkspaceID, workspaceOwnerID: ownerID,
			projectID: value.ProjectID, actorID: value.InitiatorID, taskID: value.ID, resourceVersion: version,
			taskOwnerID: value.OwnerID, leaseOwner: value.Lease.Owner, leaseVersion: value.Lease.Version,
			grantHostID: protectedHostID, taskHostID: value.HostID,
			write: write, authorization: rowTaskDomain(row).Authorization, agentID: value.AgentInstanceID,
			agentGeneration: value.AgentGeneration, agentSessionID: value.AgentSessionID}
		permit.active.Store(true)
		defer permit.active.Store(false)
		guardedCtx := context.WithValue(ctx, workspaceMutationPermitKey{}, permit)
		if row.AuthKind != "schedule_task" {
			var authorizationDeadline time.Time
			if err := tx.QueryRow(ctx, `SELECT LEAST(COALESCE(t.auth_expires_at,'infinity'::timestamptz),token.expires_at,
COALESCE(installation.expires_at,'infinity'::timestamptz))
FROM tasks t JOIN tokens token ON token.id=t.auth_token_id AND token.owner_id=t.initiator_id
LEFT JOIN client_installations installation ON installation.id=t.auth_client_installation_id AND installation.owner_id=t.initiator_id
WHERE t.id=$1 AND t.owner_id=$2`, value.ID, value.OwnerID).Scan(&authorizationDeadline); err != nil {
				return domain.NewError(domain.CodeUnauthenticated, "project Task authorization deadline is unavailable")
			}
			var cancel context.CancelFunc
			guardedCtx, cancel = context.WithDeadline(guardedCtx, authorizationDeadline)
			defer cancel()
		}
		if err := operation(guardedCtx); err != nil {
			return err
		}
		return guardedCtx.Err()
	})
}

func (s *Store) WithProjectTaskWorkspacePublish(ctx context.Context, command app.TaskWorkspacePublishCommand, operation func(context.Context) error) error {
	if ctx == nil || operation == nil {
		return domain.InvalidArgument("workspace", "Workspace publish context is invalid")
	}
	row, err := sqlcgen.New(s.pool).GetTask(ctx, sqlcgen.GetTaskParams{ID: command.TaskID, OwnerID: command.OwnerID})
	if err != nil {
		return domain.NewError(domain.CodeNotFound, "task not found")
	}
	value := rowTaskDomain(row)
	if value.ProjectID == "" {
		if command.ProjectID != "" {
			return domain.NewError(domain.CodeForbidden, "project Workspace publish identity changed")
		}
		return operation(ctx)
	}
	if command.ProjectID != value.ProjectID || command.InitiatorID != value.InitiatorID ||
		command.WorkspaceOwnerID != value.WorkspaceOwnerID || command.WorkspaceID != value.WorkspaceID ||
		command.LeaseOwner != value.Lease.Owner || command.LeaseVersion != value.Lease.Version {
		return domain.NewError(domain.CodeForbidden, "project Workspace publish identity changed")
	}
	if permit := projectWorkspacePermit(ctx, value.ProjectID, value.InitiatorID, value.WorkspaceID, true); permit != nil && permit.taskID == value.ID {
		return operation(ctx)
	}
	return s.WithProjectTaskWorkspaceAccess(ctx, value, true, operation)
}
