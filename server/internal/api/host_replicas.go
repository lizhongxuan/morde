package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type hostReplicaBindingsResponse struct {
	RuntimeGeneration int64                        `json:"runtime_generation"`
	Items             []hostReplicaBindingResponse `json:"items"`
}

type hostReplicaBindingResponse struct {
	WorkspaceID               string                                      `json:"workspace_id"`
	SyncCredential            string                                      `json:"sync_credential,omitempty"`
	Directory                 string                                      `json:"directory"`
	State                     workspace.BindingState                      `json:"state"`
	BindingVersion            int64                                       `json:"binding_version"`
	DesiredRevision           int64                                       `json:"desired_revision"`
	AppliedRevision           int64                                       `json:"applied_revision"`
	SyncState                 workspace.BindingSyncState                  `json:"sync_state"`
	SyncConflicts             []string                                    `json:"sync_conflicts"`
	ProjectUploadApplications []workspace.PendingProjectUploadApplication `json:"project_upload_applications"`
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

type hostProjectUploadApplicationRecorder interface {
	RecordProjectUploadApplication(context.Context, identity.Principal, string, int64, string,
		workspace.HostProjectUploadApplicationReport) (workspace.SyncHostApplication, error)
}

type hostTaskWorkspaceViewDeletionsResponse struct {
	RuntimeGeneration int64                               `json:"runtime_generation"`
	Items             []hostTaskWorkspaceViewDeletionItem `json:"items"`
}

type hostTaskWorkspaceViewDeletionItem struct {
	RequestID string `json:"request_id"`
	TaskID    string `json:"task_id"`
}

type HostTaskWorkspaceViewDeletionService interface {
	ListPendingWorkspaceViewDeletions(context.Context, identity.Principal) ([]app.TaskWorkspaceViewDeletion, error)
	CompleteWorkspaceViewDeletion(context.Context, identity.Principal, string, string) (app.TaskWorkspaceViewDeletion, error)
}

func (h *api) registerHostReplicaRoutes(engine *gin.Engine) {
	base := "/api/host-replicas/:host_id/generations/:generation"
	group := engine.Group(base, h.authenticateHostReplica)
	h.registerRuntimeOperationLedgerRoutes(group)
	group.GET("/workspace-bindings", h.listHostReplicaBindings)
	group.GET("/task-workspace-view-deletions", h.listTaskWorkspaceViewDeletions)
	group.POST("/tasks/:task_id/workspace-view-deletions/:request_id/applied", h.completeTaskWorkspaceViewDeletion)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/sync-status", h.authenticateHostReplicaBinding, h.reportHostReplicaWorkspaceSyncStatus)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/applied", h.authenticateHostReplicaBinding, h.recordHostReplicaAppliedRevision)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/project-uploads/:session_id/applied", h.authenticateHostReplicaBinding, h.recordHostProjectUploadApplication)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/activate", h.authenticateHostReplicaBinding, h.activateHostReplicaBinding)
	group.POST("/workspaces/:workspace_id/bindings/:binding_version/detached", h.authenticateHostReplicaBinding, h.completeHostReplicaUnbinding)
}

func (h *api) listTaskWorkspaceViewDeletions(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	service, ok := h.deps.Tasks.(HostTaskWorkspaceViewDeletionService)
	if !ok {
		serviceUnavailable(c)
		return
	}
	items, err := service.ListPendingWorkspaceViewDeletions(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := hostTaskWorkspaceViewDeletionsResponse{
		RuntimeGeneration: principal.HostGeneration,
		Items:             make([]hostTaskWorkspaceViewDeletionItem, 0, len(items)),
	}
	for _, item := range items {
		if !domain.ValidTaskWorkspaceViewDeletionID(item.ID) || !domain.ValidTaskID(item.TaskID) || item.HostID != principal.HostScope || item.Status != "pending" {
			writeError(c, domain.NewError(domain.CodeInternal, "task Workspace view deletion is inconsistent"))
			return
		}
		response.Items = append(response.Items, hostTaskWorkspaceViewDeletionItem{RequestID: item.ID, TaskID: item.TaskID})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) completeTaskWorkspaceViewDeletion(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	taskID, ok := safeIDParam(c, "task_id", "task_id", domain.ValidTaskID)
	if !ok {
		return
	}
	requestID, ok := safeIDParam(c, "request_id", "request_id", domain.ValidTaskWorkspaceViewDeletionID)
	if !ok {
		return
	}
	var request struct{}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(c, domain.InvalidArgument("request", "request body must be an empty JSON object"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("request", "request body must contain exactly one value"))
		return
	}
	service, ok := h.deps.Tasks.(HostTaskWorkspaceViewDeletionService)
	if !ok {
		serviceUnavailable(c)
		return
	}
	deletion, err := service.CompleteWorkspaceViewDeletion(c.Request.Context(), principal, taskID, requestID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostTaskWorkspaceViewDeletionItem{RequestID: deletion.ID, TaskID: deletion.TaskID})
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
		if len(binding.ProjectUploadApplications) > 1000 {
			writeError(c, domain.NewError(domain.CodeInternal, "Host project upload application queue is inconsistent"))
			return
		}
		seenProjectApplications := make(map[string]struct{}, len(binding.ProjectUploadApplications))
		for _, application := range binding.ProjectUploadApplications {
			projectName := strings.TrimPrefix(application.ProjectPath, "project/")
			if !workspace.ValidSessionID(application.SessionID) || !workspace.ValidProjectSourceBindingID(application.SourceBindingID) ||
				application.ProjectPath == projectName || strings.Contains(projectName, "/") ||
				workspace.ValidateSyncProjectFolderName(projectName) != nil ||
				workspace.ValidateRelativePath(application.ProjectPath) != nil || application.TargetRevision < 0 ||
				!workspace.ValidProjectManifestSHA256(application.ManifestSHA256) {
				writeError(c, domain.NewError(domain.CodeInternal, "Host project upload application queue is inconsistent"))
				return
			}
			if _, exists := seenProjectApplications[application.SessionID]; exists {
				writeError(c, domain.NewError(domain.CodeInternal, "Host project upload application queue contains duplicates"))
				return
			}
			seenProjectApplications[application.SessionID] = struct{}{}
		}
		item := hostReplicaBindingResponse{
			WorkspaceID: binding.Workspace.ID, Directory: binding.Directory, State: binding.State,
			BindingVersion: binding.BindingVersion, DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
			SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
			ProjectUploadApplications: append([]workspace.PendingProjectUploadApplication{}, binding.ProjectUploadApplications...),
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

func (h *api) recordHostProjectUploadApplication(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	sessionID, ok := safeIDParam(c, "session_id", "session_id", workspace.ValidSessionID)
	if !ok {
		return
	}
	var report workspace.HostProjectUploadApplicationReport
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil || workspace.ValidateHostProjectUploadApplicationReport(report) != nil {
		writeError(c, domain.InvalidArgument("project_upload_application", "Host project upload application report is invalid"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("project_upload_application", "request body must contain exactly one value"))
		return
	}
	service, ok := h.deps.HostReplicas.(hostProjectUploadApplicationRecorder)
	if !ok {
		serviceUnavailable(c)
		return
	}
	application, err := service.RecordProjectUploadApplication(c.Request.Context(), principal,
		principal.ReplicaWorkspaceID, principal.BindingVersion, sessionID, report)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, application)
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
