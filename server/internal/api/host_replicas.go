package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type hostReplicaBindingsResponse struct {
	RuntimeGeneration int64                        `json:"runtime_generation"`
	Items             []hostReplicaBindingResponse `json:"items"`
}

type hostReplicaBindingResponse struct {
	WorkspaceID     string                     `json:"workspace_id"`
	SyncCredential  string                     `json:"sync_credential,omitempty"`
	Directory       string                     `json:"directory"`
	State           workspace.BindingState     `json:"state"`
	BindingVersion  int64                      `json:"binding_version"`
	DesiredRevision int64                      `json:"desired_revision"`
	AppliedRevision int64                      `json:"applied_revision"`
	SyncState       workspace.BindingSyncState `json:"sync_state"`
	SyncConflicts   []string                   `json:"sync_conflicts"`
}

type hostReplicaAppliedRequest struct {
	SessionID       string `json:"session_id"`
	AppliedRevision int64  `json:"applied_revision"`
}

type hostReplicaCheckpointRequest struct {
	CheckpointRevision int64 `json:"checkpoint_revision"`
}

type hostReplicaSyncStatusRequest struct {
	State     workspace.BindingSyncState `json:"state"`
	Conflicts []string                   `json:"conflicts"`
}

func (h *api) registerHostReplicaRoutes(engine *gin.Engine) {
	base := "/api/host-replicas/:host_id/generations/:generation"
	group := engine.Group(base, h.authenticateHostReplica)
	group.GET("/workspace-bindings", h.listHostReplicaBindings)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/sync-status", h.authenticateHostReplicaBinding, h.reportHostReplicaWorkspaceSyncStatus)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/applied", h.authenticateHostReplicaBinding, h.recordHostReplicaAppliedRevision)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/activate", h.authenticateHostReplicaBinding, h.activateHostReplicaBinding)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/detached", h.authenticateHostReplicaBinding, h.completeHostReplicaUnbinding)
}

func (h *api) authenticateHostReplica(c *gin.Context) {
	service := h.deps.HostReplicas
	if service == nil {
		serviceUnavailable(c)
		return
	}
	hostID, ok := safeIDParam(c, "host_id", "host_id", domain.ValidHostID)
	if !ok {
		return
	}
	generation, ok := pathInt64(c, "generation")
	if !ok {
		return
	}
	values := c.Request.Header.Values("Authorization")
	if len(values) != 1 {
		unauthenticated(c)
		return
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		unauthenticated(c)
		return
	}
	principal, err := service.Authenticate(c.Request.Context(), hostID, generation, parts[1])
	if err != nil {
		writeError(c, err)
		return
	}
	c.Set(principalKey, principal)
	c.Next()
}

func (h *api) authenticateHostReplicaBinding(c *gin.Context) {
	service := h.deps.HostReplicas
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	workspaceID, ok := safeIDParam(c, "workspace_id", "workspace_id", domain.ValidWorkspaceID)
	if !ok {
		return
	}
	bindingVersion, ok := pathInt64(c, "binding_version")
	if !ok {
		return
	}
	principal, err := service.ScopeWorkspace(c.Request.Context(), principal, workspaceID, bindingVersion)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Set(principalKey, principal)
	c.Next()
}

func (h *api) listHostReplicaBindings(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	items, err := h.deps.HostReplicas.ListBindings(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := hostReplicaBindingsResponse{
		RuntimeGeneration: principal.HostGeneration,
		Items:             make([]hostReplicaBindingResponse, 0, len(items)),
	}
	for _, binding := range items {
		if !domain.ValidWorkspaceID(binding.Workspace.ID) || binding.HostID != principal.HostScope || binding.OwnerID != principal.UserID ||
			!domain.ValidWorkspaceID(binding.Directory) || !validWorkspaceBindingState(binding.State) || binding.BindingVersion < 1 ||
			binding.DesiredRevision < 0 || binding.AppliedRevision < 0 ||
			workspace.ValidateBindingSyncStatus(binding.SyncState, binding.SyncConflicts) != nil {
			writeError(c, domain.NewError(domain.CodeInternal, "Host workspace binding is inconsistent"))
			return
		}
		item := hostReplicaBindingResponse{
			WorkspaceID: binding.Workspace.ID, Directory: binding.Directory, State: binding.State,
			BindingVersion: binding.BindingVersion, DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
			SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
		}
		if binding.State == workspace.BindingPreparing || binding.State == workspace.BindingActive || binding.State == workspace.BindingUnbinding {
			item.SyncCredential, err = h.deps.HostReplicas.CredentialForBinding(c.Request.Context(), principal, binding.Workspace.ID, binding.BindingVersion)
			if err != nil {
				writeError(c, err)
				return
			}
		}
		response.Items = append(response.Items, item)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) reportHostReplicaWorkspaceSyncStatus(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	var request hostReplicaSyncStatusRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || workspace.ValidateBindingSyncStatus(request.State, request.Conflicts) != nil {
		writeError(c, domain.InvalidArgument("sync_status", "workspace sync status is invalid"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("sync_status", "request body must contain exactly one value"))
		return
	}
	binding, err := h.deps.HostReplicas.ReportWorkspaceSyncStatus(c.Request.Context(), principal, request.State, request.Conflicts)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostReplicaBindingResponse{
		WorkspaceID: binding.Workspace.ID, Directory: binding.Directory, State: binding.State,
		BindingVersion: binding.BindingVersion, DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
	})
}

func (h *api) recordHostReplicaAppliedRevision(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	var request hostReplicaAppliedRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || !workspace.ValidSessionID(request.SessionID) || request.AppliedRevision < 0 {
		writeError(c, domain.InvalidArgument("applied_revision", "applied workspace revision receipt is invalid"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("applied_revision", "request body must contain exactly one value"))
		return
	}
	if h.deps.WorkspaceSync == nil {
		serviceUnavailable(c)
		return
	}
	session, err := h.deps.WorkspaceSync.GetSyncSession(c.Request.Context(), principal, principal.ReplicaWorkspaceID, request.SessionID)
	if err != nil {
		writeError(c, err)
		return
	}
	validDirection := session.Direction == workspace.SyncDownload || session.Direction == workspace.SyncBidirectional
	validPolicy := session.Policy == workspace.SyncFillMissing || session.Policy == workspace.SyncLive
	if session.State != workspace.SyncCompleted || !validDirection || !validPolicy ||
		session.TargetRevision != request.AppliedRevision || len(session.Conflicts) != 0 {
		writeError(c, domain.NewError(domain.CodeWorkspaceConflict, "workspace revision was not fully applied by a completed download session"))
		return
	}
	binding, err := h.deps.HostReplicas.RecordAppliedRevision(c.Request.Context(), principal, principal.ReplicaWorkspaceID,
		principal.BindingVersion, request.SessionID, request.AppliedRevision)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostReplicaBindingResponse{
		WorkspaceID: binding.Workspace.ID, Directory: binding.Directory, State: binding.State,
		BindingVersion: binding.BindingVersion, DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
	})
}

func (h *api) activateHostReplicaBinding(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	binding, err := h.deps.HostReplicas.ActivateBinding(c.Request.Context(), principal, principal.ReplicaWorkspaceID, principal.BindingVersion)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostReplicaBindingResponse{
		WorkspaceID: binding.Workspace.ID, Directory: binding.Directory, State: binding.State,
		BindingVersion: binding.BindingVersion, DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
	})
}

func (h *api) completeHostReplicaUnbinding(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	var request hostReplicaCheckpointRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.CheckpointRevision < 0 {
		writeError(c, domain.InvalidArgument("checkpoint_revision", "Host workspace checkpoint is invalid"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("checkpoint_revision", "request body must contain exactly one value"))
		return
	}
	binding, err := h.deps.HostReplicas.CompleteUnbinding(c.Request.Context(), principal,
		principal.ReplicaWorkspaceID, principal.BindingVersion, request.CheckpointRevision)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostReplicaBindingResponse{
		WorkspaceID: binding.Workspace.ID, Directory: binding.Directory, State: binding.State,
		BindingVersion: binding.BindingVersion, DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
	})
}

func pathInt64(c *gin.Context, field string) (int64, bool) {
	value, err := strconv.ParseInt(c.Param(field), 10, 64)
	if err != nil || value < 1 {
		writeError(c, domain.InvalidArgument(field, "path value is invalid"))
		return 0, false
	}
	return value, true
}
