package postgres

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type projectWorkspaceSelectionKey struct{}

type projectWorkspaceSelectionPermit struct {
	tx                                         pgx.Tx
	taskID, ownerID, projectID, actorID        string
	workspaceID, hostID                        string
	agentID, sessionID, leaseOwner             string
	agentGeneration, leaseVersion, authVersion int64
	active                                     atomic.Bool
}

// withProjectWorkspaceSelectionTx only reuses a live, exact selection
// transaction. Personal bindings continue to use their ordinary transaction.
func (s *Store) withProjectWorkspaceSelectionTx(ctx context.Context, value task.Task, workspaceID, hostID string,
	fn func(pgx.Tx) error) error {
	if value.ProjectID == "" || workspaceID == "" {
		return s.WithTx(ctx, fn)
	}
	permit, _ := ctx.Value(projectWorkspaceSelectionKey{}).(*projectWorkspaceSelectionPermit)
	if permit == nil || !permit.active.Load() || permit.taskID != value.ID || permit.ownerID != value.OwnerID ||
		permit.projectID != value.ProjectID || permit.actorID != value.InitiatorID || permit.workspaceID != workspaceID ||
		permit.hostID != hostID || permit.agentID != value.AgentInstanceID || permit.sessionID != value.AgentSessionID ||
		permit.agentGeneration != value.AgentGeneration || permit.leaseOwner != value.Lease.Owner ||
		permit.leaseVersion != value.Lease.Version || permit.authVersion != value.Authorization.Version {
		return domain.NewError(domain.CodeForbidden, "project Workspace selection is unavailable")
	}
	return fn(permit.tx)
}

// WithProjectTaskWorkspaceSelection serializes the first manifest read and
// binding against project membership, Workspace grant and transfer changes.
// The callback may only bind the exact Task/Workspace/Host in this transaction.
func (s *Store) WithProjectTaskWorkspaceSelection(ctx context.Context, value task.Task, target app.DeferredTaskTarget,
	write bool, operation func(context.Context, string) error) error {
	if ctx == nil || operation == nil || !project.ValidID(value.ProjectID) || !domain.ValidTaskID(value.ID) ||
		!domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.InitiatorID) ||
		!domain.ValidWorkspaceID(target.WorkspaceID) || target.Unavailable ||
		(target.HostID != "" && !domain.ValidHostID(target.HostID)) {
		return domain.InvalidArgument("workspace", "project Workspace selection is invalid")
	}
	if permit, _ := ctx.Value(projectWorkspaceSelectionKey{}).(*projectWorkspaceSelectionPermit); permit != nil && permit.active.Load() {
		return domain.NewError(domain.CodeConflict, "project Workspace selection is already active")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceMutationFence(ctx, tx, target.WorkspaceID, false); err != nil {
			return err
		}
		ownerID, version, err := lockProjectTaskWorkspaceAccess(ctx, tx, value.ProjectID, value.InitiatorID, target.WorkspaceID, write)
		if err != nil {
			return err
		}
		if target.HostID != "" {
			if ownerID != value.InitiatorID {
				grant, err := lockProjectWorkspaceGrant(ctx, tx, value.InitiatorID, ownerID, target.WorkspaceID)
				if err != nil || grant.hostID != target.HostID {
					return domain.NewError(domain.CodeForbidden, "project Workspace is not bound to the selected Host")
				}
			}
			if err := lockProjectWorkspaceHost(ctx, tx, target.HostID, ownerID); err != nil {
				return err
			}
			if err := lockTaskTargetHost(ctx, tx, target.HostID, value.InitiatorID); err != nil {
				return err
			}
		}
		fence := deferredTargetFence(value)
		current, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil || !matchesDeferredTaskAgent(current, value) || current.HostID != value.HostID {
			return domain.NewError(domain.CodeUnauthenticated, "project Workspace Task authorization is unavailable")
		}
		if err := lockActiveAgentLease(ctx, tx, fence); err != nil {
			return err
		}
		var agentInstanceCurrent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_instances a JOIN service_identity i
ON i.singleton=true AND i.instance_id=a.service_instance_id
WHERE a.task_id=$1 AND a.owner_id=$2 AND a.agent_instance_id=$3 AND a.agent_generation=$4)`,
			value.ID, value.OwnerID, value.AgentInstanceID, value.AgentGeneration).Scan(&agentInstanceCurrent); err != nil || !agentInstanceCurrent {
			return domain.NewError(domain.CodeUnauthenticated, "project Workspace Agent instance is unavailable")
		}
		selection, found, err := loadDeferredTaskTargetInTx(ctx, tx, value)
		if err != nil || !found || selection.target.Unavailable || selection.target.WorkspaceID != target.WorkspaceID ||
			(target.HostID != "" && (selection.target.HostID != target.HostID || selection.target.ApplicationID != target.ApplicationID)) {
			return domain.NewError(domain.CodeForbidden, "project Workspace was not selected")
		}
		if err := verifyDeferredTargetSources(ctx, tx, value, selection, target.HostID != "", true); err != nil {
			return err
		}
		var currentWorkspace, currentOwner string
		var currentVersion *int64
		var revisionCurrent bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(workspace_id,''),COALESCE(workspace_owner_id,''),
workspace_project_resource_version,current_revision=desired_revision FROM tasks WHERE id=$1`, value.ID).
			Scan(&currentWorkspace, &currentOwner, &currentVersion, &revisionCurrent); err != nil || !revisionCurrent {
			return domain.NewError(domain.CodeConflict, "project Workspace Task revision changed")
		}
		if currentWorkspace != "" {
			if currentWorkspace != target.WorkspaceID || currentOwner != ownerID || currentVersion == nil || *currentVersion != version {
				return domain.NewError(domain.CodeConflict, "project Workspace binding changed")
			}
			if err := verifyProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, ownerID, target.WorkspaceID); err != nil {
				return err
			}
		}
		permit := &projectWorkspaceSelectionPermit{tx: tx, taskID: value.ID, ownerID: value.OwnerID, projectID: value.ProjectID,
			actorID: value.InitiatorID, workspaceID: target.WorkspaceID, hostID: target.HostID,
			agentID: value.AgentInstanceID, sessionID: value.AgentSessionID, agentGeneration: value.AgentGeneration,
			leaseOwner: value.Lease.Owner, leaseVersion: value.Lease.Version, authVersion: value.Authorization.Version}
		permit.active.Store(true)
		defer permit.active.Store(false)
		guardedCtx := context.WithValue(ctx, projectWorkspaceSelectionKey{}, permit)
		var deadline time.Time
		if err := tx.QueryRow(ctx, `SELECT LEAST(COALESCE(t.auth_expires_at,'infinity'::timestamptz),token.expires_at,
COALESCE(installation.expires_at,'infinity'::timestamptz))
FROM tasks t JOIN tokens token ON token.id=t.auth_token_id AND token.owner_id=t.initiator_id
LEFT JOIN client_installations installation ON installation.id=t.auth_client_installation_id AND installation.owner_id=t.initiator_id
WHERE t.id=$1 AND t.owner_id=$2`, value.ID, value.OwnerID).Scan(&deadline); err != nil {
			return domain.NewError(domain.CodeUnauthenticated, "project Task authorization deadline is unavailable")
		}
		guardedCtx, cancel := context.WithDeadline(guardedCtx, deadline)
		defer cancel()
		if err := operation(guardedCtx, ownerID); err != nil {
			return err
		}
		if err := guardedCtx.Err(); err != nil {
			return err
		}
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3
 AND project_id=$4 AND workspace_id=$5 AND workspace_owner_id=$6 AND workspace_project_resource_version=$7
 AND ($8='' OR COALESCE(host_id,'')=$8) AND status='running' AND lease_owner=$9 AND lease_version=$10
 AND lease_expires_at>clock_timestamp() AND auth_version=$11 AND current_revision=desired_revision)`,
			value.ID, value.OwnerID, value.InitiatorID, value.ProjectID, target.WorkspaceID, ownerID, version, target.HostID,
			value.Lease.Owner, value.Lease.Version, value.Authorization.Version).Scan(&bound); err != nil || !bound {
			return domain.NewError(domain.CodeConflict, "project Workspace was not safely bound")
		}
		return verifyProjectTaskWorkspaceGrant(ctx, tx, value.ID, value.ProjectID, value.InitiatorID, ownerID, target.WorkspaceID)
	})
}

var _ app.ProjectTaskWorkspaceSelection = (*Store)(nil)
