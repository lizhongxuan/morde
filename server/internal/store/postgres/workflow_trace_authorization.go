package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/evaluation"
)

type workflowTraceCurrentAuthorizationSnapshot struct {
	Available           bool                                  `json:"available"`
	CapturedAt          time.Time                             `json:"captured_at"`
	Task                workflowTraceAuthorizationTask        `json:"task"`
	Owner               *workflowTraceAuthorizationAccount    `json:"owner"`
	Initiator           *workflowTraceAuthorizationAccount    `json:"initiator"`
	Credential          *workflowTraceAuthorizationCredential `json:"credential"`
	Installation        *workflowTraceAuthorizationInstall    `json:"client_installation"`
	ExecutionControl    *workflowTraceAuthorizationControl    `json:"execution_control"`
	Host                *workflowTraceAuthorizationHost       `json:"host"`
	HostMemberGrant     *workflowTraceAuthorizationGrant      `json:"host_member_grant"`
	Project             *workflowTraceAuthorizationProject    `json:"project"`
	ProjectMembership   *workflowTraceAuthorizationMember     `json:"project_membership"`
	ProjectResources    []workflowTraceAuthorizationResource  `json:"project_resources"`
	WorkspaceBindings   []workflowTraceAuthorizationBinding   `json:"workspace_bindings"`
	WorkspaceGrantFence *workflowTraceWorkspaceGrantFence     `json:"workspace_grant_fence"`
	WorkspaceFenceGrant *workflowTraceAuthorizationGrant      `json:"workspace_fence_host_member_grant"`
	AgentHostFences     []workflowTraceAgentHostFence         `json:"agent_host_fences"`
}

type workflowTraceAuthorizationTask struct {
	ID                                string   `json:"id"`
	OwnerID                           string   `json:"owner_id"`
	InitiatorID                       string   `json:"initiator_id"`
	HostID                            string   `json:"host_id"`
	ProjectID                         string   `json:"project_id"`
	AuthorizationKind                 string   `json:"authorization_kind"`
	AuthorizationTokenID              string   `json:"authorization_token_id"`
	AuthorizationScopes               []string `json:"authorization_scopes"`
	AuthorizationClientInstallationID string   `json:"authorization_client_installation_id"`
	AuthorizationVersion              int64    `json:"authorization_version"`
	ExecutionEpoch                    int64    `json:"execution_epoch"`
	AgentGeneration                   int64    `json:"agent_generation"`
	WorkspaceID                       string   `json:"workspace_id"`
	WorkspaceOwnerID                  string   `json:"workspace_owner_id"`
	WorkspaceProjectResourceVersion   *int64   `json:"workspace_project_resource_version"`
}

type workflowTraceAuthorizationAccount struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type workflowTraceAuthorizationCredential struct {
	ID                   string    `json:"id"`
	OwnerID              string    `json:"owner_id"`
	Kind                 string    `json:"kind"`
	Scopes               []string  `json:"scopes"`
	ClientInstallationID string    `json:"client_installation_id"`
	ExpiresAt            time.Time `json:"expires_at"`
	Revoked              bool      `json:"revoked"`
}

type workflowTraceAuthorizationInstall struct {
	ID        string     `json:"id"`
	OwnerID   string     `json:"owner_id"`
	Platform  string     `json:"platform"`
	ExpiresAt *time.Time `json:"expires_at"`
	Revoked   bool       `json:"revoked"`
	Active    bool       `json:"active"`
}

type workflowTraceAuthorizationControl struct {
	State            string `json:"state"`
	Epoch            int64  `json:"epoch"`
	EpochMatchesTask bool   `json:"epoch_matches_task"`
}

type workflowTraceAuthorizationHost struct {
	ID                string `json:"id"`
	OwnerID           string `json:"owner_id"`
	State             string `json:"state"`
	ControlState      string `json:"control_state"`
	RuntimeGeneration int64  `json:"runtime_generation"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	ControlVersion    int64  `json:"control_version"`
	Deleted           bool   `json:"deleted"`
}

type workflowTraceAuthorizationGrant struct {
	HostID       string    `json:"host_id"`
	OwnerID      string    `json:"owner_id"`
	UserID       string    `json:"user_id"`
	Permissions  []string  `json:"permissions"`
	GrantVersion string    `json:"grant_version"`
	CreatedAt    time.Time `json:"created_at"`
	Revoked      bool      `json:"revoked"`
}

type workflowTraceAuthorizationProject struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Version int64  `json:"version"`
}

type workflowTraceAuthorizationMember struct {
	Role    string `json:"role"`
	Version int64  `json:"version"`
}

type workflowTraceAuthorizationResource struct {
	Kind            string `json:"kind"`
	ResourceID      string `json:"resource_id"`
	ResourceOwnerID string `json:"resource_owner_id"`
	Version         int64  `json:"version"`
	Active          bool   `json:"active"`
}

type workflowTraceAuthorizationBinding struct {
	HostID         string `json:"host_id"`
	WorkspaceID    string `json:"workspace_id"`
	OwnerID        string `json:"owner_id"`
	State          string `json:"state"`
	BindingVersion int64  `json:"binding_version"`
}

type workflowTraceWorkspaceGrantFence struct {
	ProjectID        string `json:"project_id"`
	WorkspaceID      string `json:"workspace_id"`
	WorkspaceOwnerID string `json:"workspace_owner_id"`
	ActorID          string `json:"actor_id"`
	HostID           string `json:"host_id"`
	BindingVersion   int64  `json:"binding_version"`
	GrantVersion     string `json:"grant_version"`
	GrantFingerprint string `json:"grant_fingerprint"`
}

type workflowTraceAgentHostFence struct {
	Generation        int64  `json:"generation"`
	HostID            string `json:"host_id"`
	HostOwnerID       string `json:"host_owner_id"`
	HostActorID       string `json:"host_actor_id"`
	HostPermission    string `json:"host_permission"`
	AuthorizationKind string `json:"authorization_kind"`
	RuntimeGeneration int64  `json:"runtime_generation"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	ControlVersion    int64  `json:"control_version"`
	GrantFingerprint  string `json:"grant_fingerprint"`
}

func addWorkflowTraceCurrentAuthorization(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder,
	taskRow workflowTraceTask) error {
	if _, err := tx.Exec(ctx, `SAVEPOINT workflow_trace_authorization_snapshot`); err != nil {
		return fmt.Errorf("save workflow authorization snapshot point: %w", err)
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
 'captured_at',statement_timestamp(),
 'task', jsonb_build_object(
   'id',t.id,'owner_id',t.owner_id,'initiator_id',t.initiator_id,'host_id',COALESCE(t.host_id,''),'project_id',COALESCE(t.project_id,''),
   'authorization_kind',t.auth_kind,'authorization_token_id',t.auth_token_id,'authorization_scopes',t.auth_scopes,
   'authorization_client_installation_id',COALESCE(t.auth_client_installation_id,''),'authorization_version',t.auth_version,
   'execution_epoch',t.execution_epoch,'agent_generation',t.agent_generation,'workspace_id',COALESCE(t.workspace_id,''),
   'workspace_owner_id',COALESCE(t.workspace_owner_id,''),'workspace_project_resource_version',t.workspace_project_resource_version),
	 'owner', (SELECT jsonb_build_object('id',u.id,'status',u.status) FROM users u WHERE u.id=t.owner_id),
	 'initiator', (SELECT jsonb_build_object('id',u.id,'status',u.status) FROM users u WHERE u.id=t.initiator_id),
 'credential', (SELECT jsonb_build_object('id',token.id,'owner_id',token.owner_id,'kind',token.kind,'scopes',token.scopes,
   'client_installation_id',COALESCE(token.client_installation_id,''),'expires_at',token.expires_at,'revoked',token.revoked_at IS NOT NULL)
   FROM tokens token WHERE token.id=t.auth_token_id),
 'client_installation', (SELECT jsonb_build_object('id',installation.id,'owner_id',installation.owner_id,'platform',installation.platform,
   'expires_at',installation.expires_at,'revoked',installation.revoked_at IS NOT NULL,
   'active',installation.revoked_at IS NULL AND (installation.expires_at IS NULL OR installation.expires_at>statement_timestamp()))
   FROM client_installations installation WHERE installation.id=t.auth_client_installation_id AND installation.owner_id=t.owner_id),
 'execution_control', (SELECT jsonb_build_object('state',control.state,'epoch',control.epoch,
   'epoch_matches_task',control.epoch=t.execution_epoch) FROM owner_execution_controls control WHERE control.owner_id=t.initiator_id),
 'host', (SELECT jsonb_build_object('id',host.id,'owner_id',host.owner_id,'state',host.state,'control_state',host.control_state,
   'runtime_generation',host.runtime_generation,'runtime_instance_id',COALESCE(host.runtime_instance_id,''),
   'control_version',host.control_version,'deleted',host.deleted_at IS NOT NULL) FROM hosts host WHERE host.id=t.host_id),
 'host_member_grant', (SELECT jsonb_build_object('host_id',member.host_id,'owner_id',member.owner_id,'user_id',member.user_id,
   'permissions',member.permissions,'grant_version',member.grant_version::text,'created_at',member.created_at,'revoked',member.revoked_at IS NOT NULL)
   FROM host_members member WHERE member.host_id=t.host_id AND member.user_id=t.initiator_id),
 'project', (SELECT jsonb_build_object('id',project.id,'owner_id',project.owner_id,'version',project.version)
   FROM projects project WHERE project.id=t.project_id),
 'project_membership', (SELECT jsonb_build_object('role',membership.role,'version',membership.version)
   FROM project_members membership WHERE membership.project_id=t.project_id AND membership.user_id=t.initiator_id),
 'project_resources', COALESCE((SELECT jsonb_agg(jsonb_build_object('kind',resource.kind,'resource_id',resource.resource_id,
   'resource_owner_id',resource.resource_owner_id,'version',resource.version,'active',resource.active) ORDER BY resource.kind,resource.resource_id)
   FROM project_resources resource WHERE resource.project_id=t.project_id
     AND resource.resource_id IN (COALESCE(t.host_id,''),COALESCE(t.workspace_id,''))), '[]'::jsonb),
 'workspace_bindings', COALESCE((SELECT jsonb_agg(jsonb_build_object('host_id',binding.host_id,'workspace_id',binding.workspace_id,
   'owner_id',binding.owner_id,'state',binding.state,'binding_version',binding.binding_version) ORDER BY binding.host_id)
   FROM workspace_bindings binding WHERE binding.workspace_id=t.workspace_id AND binding.owner_id=t.workspace_owner_id
     AND binding.host_id IN (COALESCE(t.host_id,''),COALESCE((SELECT fence.host_id FROM task_workspace_grant_fences fence WHERE fence.task_id=t.id),''))), '[]'::jsonb),
 'workspace_grant_fence', (SELECT jsonb_build_object('project_id',fence.project_id,'workspace_id',fence.workspace_id,
   'workspace_owner_id',fence.workspace_owner_id,'actor_id',fence.actor_id,'host_id',fence.host_id,'binding_version',fence.binding_version,
   'grant_version',fence.grant_version::text,'grant_fingerprint',fence.grant_fingerprint)
   FROM task_workspace_grant_fences fence WHERE fence.task_id=t.id),
 'workspace_fence_host_member_grant', (SELECT jsonb_build_object('host_id',member.host_id,'owner_id',member.owner_id,'user_id',member.user_id,
   'permissions',member.permissions,'grant_version',member.grant_version::text,'created_at',member.created_at,'revoked',member.revoked_at IS NOT NULL)
   FROM task_workspace_grant_fences fence JOIN host_members member ON member.host_id=fence.host_id AND member.user_id=fence.actor_id
     AND member.owner_id=fence.workspace_owner_id WHERE fence.task_id=t.id),
 'agent_host_fences', COALESCE((SELECT jsonb_agg(jsonb_build_object('generation',agent.agent_generation,
   'host_id',COALESCE(agent.host_id,''),'host_owner_id',COALESCE(agent.host_owner_id,''),'host_actor_id',COALESCE(agent.host_actor_id,''),
   'host_permission',COALESCE(agent.host_permission,''),'authorization_kind',COALESCE(agent.host_authorization_kind,''),
   'runtime_generation',COALESCE(agent.host_runtime_generation,0),'runtime_instance_id',COALESCE(agent.host_runtime_instance_id,''),
   'control_version',COALESCE(agent.host_control_version,0),'grant_fingerprint',COALESCE(agent.host_grant_fingerprint,'')) ORDER BY agent.agent_generation)
   FROM (SELECT * FROM task_agent_instances WHERE task_id=t.id ORDER BY agent_generation DESC LIMIT 16) agent), '[]'::jsonb)
)
FROM tasks t WHERE t.id=$1 AND t.owner_id=$2`, taskRow.ID, taskRow.OwnerID).Scan(&raw)
	if err != nil {
		if rollbackErr := rollbackWorkflowTraceAuthorizationSavepoint(ctx, tx); rollbackErr != nil {
			return rollbackErr
		}
		snapshot := workflowTraceCurrentAuthorizationSnapshot{Available: false}
		return addWorkflowTraceAuthorizationUnavailable(builder, taskRow, snapshot)
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT workflow_trace_authorization_snapshot`); err != nil {
		return fmt.Errorf("release workflow authorization snapshot point: %w", err)
	}
	var snapshot workflowTraceCurrentAuthorizationSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return addWorkflowTraceAuthorizationUnavailable(builder, taskRow, workflowTraceCurrentAuthorizationSnapshot{Available: false})
	}
	snapshot.Available = true
	return addWorkflowTraceAuthorizationSnapshot(builder, taskRow, snapshot)
}

func rollbackWorkflowTraceAuthorizationSavepoint(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT workflow_trace_authorization_snapshot`); err != nil {
		return fmt.Errorf("rollback workflow authorization snapshot point: %w", err)
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT workflow_trace_authorization_snapshot`); err != nil {
		return fmt.Errorf("release unavailable workflow authorization snapshot point: %w", err)
	}
	return nil
}

func addWorkflowTraceAuthorizationUnavailable(builder *workflowTraceBuilder, taskRow workflowTraceTask,
	snapshot workflowTraceCurrentAuthorizationSnapshot) error {
	snapshot.CapturedAt = builder.trace.CapturedAt
	sourceID, err := builder.addSource(evaluation.WorkflowSourceAuthorizationSnapshot,
		"db://tasks/"+taskRow.ID+"/authorization/current", snapshot)
	if err != nil {
		return err
	}
	builder.addNode(evaluation.WorkflowNodeAuth, evaluation.WorkflowNodeUnknown, taskRow.ID+":auth:current", sourceID)
	builder.addGap(evaluation.WorkflowStageAuthorization, "current identity, credentials and Host/Project authorization sources are unavailable",
		evaluation.WorkflowGapUnknown, nil, sourceID)
	return nil
}

func addWorkflowTraceAuthorizationSnapshot(builder *workflowTraceBuilder, taskRow workflowTraceTask,
	snapshot workflowTraceCurrentAuthorizationSnapshot) error {
	sourceID, err := builder.addSource(evaluation.WorkflowSourceAuthorizationSnapshot,
		"db://tasks/"+taskRow.ID+"/authorization/current", snapshot)
	if err != nil {
		return err
	}
	builder.addNode(evaluation.WorkflowNodeAuth, evaluation.WorkflowNodeUnknown, taskRow.ID+":auth:current", sourceID)
	if !workflowTraceCredentialSnapshotMatches(snapshot) {
		builder.addGap(evaluation.WorkflowStageAuthorization, "current credential or installation is not an exact active match to the Task snapshot",
			evaluation.WorkflowGapUnknown, nil, sourceID)
	}
	if snapshot.Owner == nil || snapshot.Owner.Status != "approved" {
		builder.addGap(evaluation.WorkflowStageAuthorization, "current Task owner identity is unavailable or not approved",
			evaluation.WorkflowGapUnknown, nil, sourceID)
	}
	if snapshot.ExecutionControl == nil || snapshot.ExecutionControl.State != "enabled" || !snapshot.ExecutionControl.EpochMatchesTask {
		builder.addGap(evaluation.WorkflowStageAuthorization, "current account execution control does not exactly match the Task execution epoch",
			evaluation.WorkflowGapUnknown, nil, sourceID)
	}
	if snapshot.Task.HostID != "" && !workflowTraceHostFenceMatches(snapshot) {
		builder.addGap(evaluation.WorkflowStageAuthorization, "current Host runtime/control/grant does not exactly match a persisted Task Agent Host fence",
			evaluation.WorkflowGapUnknown, nil, sourceID)
	}
	if snapshot.Task.ProjectID != "" {
		if snapshot.ProjectMembership == nil {
			builder.addGap(evaluation.WorkflowStageAuthorization, "current Project membership is unavailable",
				evaluation.WorkflowGapUnknown, nil, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageAuthorization, "Project membership version was not pinned to this Task",
				evaluation.WorkflowGapUnknown, nil, sourceID)
		}
		if !workflowTraceProjectResourceMatches(snapshot, "host") {
			builder.addGap(evaluation.WorkflowStageAuthorization, "current Project Host resource is unavailable or inactive",
				evaluation.WorkflowGapUnknown, nil, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageAuthorization, "Project Host resource version was not pinned to this Task",
				evaluation.WorkflowGapUnknown, nil, sourceID)
		}
		if snapshot.Task.WorkspaceID != "" {
			if snapshot.Task.WorkspaceProjectResourceVersion == nil || !workflowTraceProjectResourceMatches(snapshot, "workspace") ||
				!workflowTraceProjectWorkspaceVersionMatches(snapshot) {
				builder.addGap(evaluation.WorkflowStageAuthorization, "current Project Workspace resource does not exactly match its Task version",
					evaluation.WorkflowGapUnknown, nil, sourceID)
			}
			if snapshot.Task.WorkspaceOwnerID != "" && snapshot.Task.WorkspaceOwnerID != snapshot.Task.InitiatorID &&
				!workflowTraceWorkspaceGrantMatches(snapshot) {
				builder.addGap(evaluation.WorkflowStageAuthorization, "current shared Workspace Host binding/grant does not exactly match its persisted Task fence",
					evaluation.WorkflowGapUnknown, nil, sourceID)
			}
		}
	}
	return nil
}

func workflowTraceCredentialSnapshotMatches(snapshot workflowTraceCurrentAuthorizationSnapshot) bool {
	task, credential := snapshot.Task, snapshot.Credential
	if credential == nil || snapshot.Initiator == nil || snapshot.Initiator.Status != "approved" || credential.ID != task.AuthorizationTokenID ||
		credential.OwnerID != task.InitiatorID || credential.Kind != task.AuthorizationKind || credential.Revoked ||
		!credential.ExpiresAt.After(snapshot.CapturedAt) || credential.ClientInstallationID != task.AuthorizationClientInstallationID ||
		!sameWorkflowAuthorizationScopes(credential.Scopes, task.AuthorizationScopes) {
		return false
	}
	if task.AuthorizationClientInstallationID == "" {
		return credential.Kind != "client"
	}
	return snapshot.Installation != nil && snapshot.Installation.ID == task.AuthorizationClientInstallationID &&
		snapshot.Installation.OwnerID == task.OwnerID && snapshot.Installation.Active && !snapshot.Installation.Revoked
}

func sameWorkflowAuthorizationScopes(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func workflowTraceHostFenceMatches(snapshot workflowTraceCurrentAuthorizationSnapshot) bool {
	if snapshot.Host == nil || snapshot.Task.AgentGeneration < 1 {
		return false
	}
	var fence *workflowTraceAgentHostFence
	for index := range snapshot.AgentHostFences {
		if snapshot.AgentHostFences[index].Generation == snapshot.Task.AgentGeneration {
			fence = &snapshot.AgentHostFences[index]
			break
		}
	}
	host := snapshot.Host
	if fence == nil || host.Deleted || host.State != "running" || host.ControlState != "ai" ||
		fence.HostID != host.ID || fence.HostID != snapshot.Task.HostID || fence.HostOwnerID != host.OwnerID ||
		fence.HostActorID != snapshot.Task.InitiatorID || fence.HostPermission != "desktop:control" ||
		fence.RuntimeGeneration != host.RuntimeGeneration || fence.RuntimeInstanceID != host.RuntimeInstanceID ||
		fence.ControlVersion != host.ControlVersion {
		return false
	}
	if fence.AuthorizationKind == "owner" {
		return snapshot.Task.InitiatorID == host.OwnerID && fence.GrantFingerprint == ""
	}
	if fence.AuthorizationKind != "member" || snapshot.HostMemberGrant == nil {
		return false
	}
	grant := snapshot.HostMemberGrant
	if grant.HostID != host.ID || grant.OwnerID != host.OwnerID || grant.UserID != snapshot.Task.InitiatorID ||
		grant.Revoked || grant.GrantVersion == "" || !containsWorkflowAuthorizationScope(grant.Permissions, "desktop:control") {
		return false
	}
	return fingerprintHostMemberGrant(grant.CreatedAt, grant.GrantVersion, grant.Permissions) == fence.GrantFingerprint
}

func containsWorkflowAuthorizationScope(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func workflowTraceProjectResourceMatches(snapshot workflowTraceCurrentAuthorizationSnapshot, kind string) bool {
	resourceID := ""
	resourceOwnerID := ""
	switch kind {
	case "host":
		resourceID = snapshot.Task.HostID
		if snapshot.Host != nil {
			resourceOwnerID = snapshot.Host.OwnerID
		}
	case "workspace":
		resourceID = snapshot.Task.WorkspaceID
		resourceOwnerID = snapshot.Task.WorkspaceOwnerID
	default:
		return false
	}
	for _, resource := range snapshot.ProjectResources {
		if resource.Kind == kind && resource.Active && resource.ResourceID == resourceID &&
			resource.ResourceOwnerID == resourceOwnerID && resourceOwnerID != "" {
			return true
		}
	}
	return false
}

func workflowTraceProjectWorkspaceVersionMatches(snapshot workflowTraceCurrentAuthorizationSnapshot) bool {
	for _, resource := range snapshot.ProjectResources {
		if resource.Kind == "workspace" && resource.ResourceID == snapshot.Task.WorkspaceID && resource.Active &&
			resource.Version == *snapshot.Task.WorkspaceProjectResourceVersion {
			return true
		}
	}
	return false
}

func workflowTraceWorkspaceGrantMatches(snapshot workflowTraceCurrentAuthorizationSnapshot) bool {
	fence, grant := snapshot.WorkspaceGrantFence, snapshot.WorkspaceFenceGrant
	task := snapshot.Task
	if fence == nil || grant == nil || task.ProjectID == "" || task.WorkspaceID == "" || task.WorkspaceOwnerID == "" ||
		fence.ProjectID != task.ProjectID || fence.WorkspaceID != task.WorkspaceID ||
		fence.WorkspaceOwnerID != task.WorkspaceOwnerID || fence.ActorID != task.InitiatorID ||
		grant.Revoked || grant.HostID != fence.HostID || grant.OwnerID != fence.WorkspaceOwnerID ||
		grant.UserID != fence.ActorID || grant.GrantVersion != fence.GrantVersion ||
		fingerprintHostMemberGrant(grant.CreatedAt, grant.GrantVersion, grant.Permissions) != fence.GrantFingerprint ||
		!containsWorkflowAuthorizationScope(grant.Permissions, "workspace:file:read") {
		return false
	}
	for _, binding := range snapshot.WorkspaceBindings {
		if binding.HostID == fence.HostID && binding.WorkspaceID == fence.WorkspaceID && binding.OwnerID == fence.WorkspaceOwnerID &&
			binding.State == "active" && binding.BindingVersion == fence.BindingVersion {
			return true
		}
	}
	return false
}
