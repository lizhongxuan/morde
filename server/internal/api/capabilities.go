package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type capabilitiesResponse struct {
	ServiceInstanceID      string   `json:"service_instance_id"`
	APIVersion             string   `json:"api_version"`
	HubVersion             *string  `json:"hub_version"`
	EventVersion           *string  `json:"event_version"`
	MinimumClientVersion   *string  `json:"minimum_client_version"`
	SupportedFeatures      []string `json:"supported_features"`
	DefaultMaxRequestBytes int64    `json:"default_max_request_bytes"`
}

func (h *api) capabilities(c *gin.Context) {
	// A missing identity cannot safely partition client data across servers.
	if !domain.ValidServiceInstanceID(h.options.ServiceInstanceID) {
		serviceUnavailable(c)
		return
	}
	features := []string{"password_login"}
	_, clientVerifier := h.deps.Verifier.(interface {
		VerifyClient(context.Context, string) (identity.Principal, error)
	})
	_, deviceVerifier := h.deps.Verifier.(interface {
		VerifyDevice(context.Context, string) (identity.Principal, error)
	})
	var hubVersion *string
	if h.deps.ClientLogin != nil && clientVerifier {
		features = append(features, "ordered_client_login", "client_installations")
	}
	if h.deps.Hub != nil && clientVerifier {
		version := "1"
		hubVersion = &version
	}
	if h.deps.ModelConnections != nil && clientVerifier {
		features = append(features, "available_models")
	}
	if hubVersion != nil {
		features = append(features, "hub")
	}
	if h.deps.PersonalData != nil && h.deps.PersonalSync != nil && clientVerifier {
		features = append(features, "personal_sync")
	}
	if syncService, ok := h.deps.WorkspaceSync.(interface{ ProjectUploadEnabled() bool }); ok &&
		syncService.ProjectUploadEnabled() && clientVerifier {
		features = append(features, "workspace_sync", "project_upload")
	}
	if recoveryService, ok := h.deps.WorkspaceSync.(WorkspaceSyncCheckpointRecoveryService); ok &&
		recoveryService.CheckpointRecoveryEnabled() && clientVerifier && deviceVerifier {
		features = append(features, "workspace_sync_checkpoint_recovery")
	}
	if h.deps.ModelGateway != nil && clientVerifier {
		features = append(features, "model_gateway")
	}
	taskAPIAvailable := h.deps.Tasks != nil && h.deps.Conversations != nil
	if taskAPIAvailable {
		features = append(features, "task_api")
	}
	conversationAutoRoutingAvailable := h.deps.ConversationAutoRoutingReady && taskAPIAvailable &&
		h.deps.ProjectConversations != nil && h.deps.ClientLogin != nil && clientVerifier &&
		h.deps.ModelConnections != nil && h.deps.ModelGateway != nil
	if conversationAutoRoutingAvailable {
		features = append(features, "conversation_auto_routing_v1")
	}
	if h.deps.ProjectWorkItems != nil {
		features = append(features, "project_work_items_v2")
	}
	if derivatives, ok := h.deps.ProjectConversationAttachments.(interface {
		projectConversationDerivativeService
		DerivativesReady() bool
	}); ok && derivatives.DerivativesReady() && h.deps.ProjectConversations != nil && clientVerifier {
		features = append(features, "project_attachment_derivatives_v1")
	}
	cloudTasksAvailable := h.deps.ExecutionControl != nil && taskAPIAvailable
	if cloudTasksAvailable {
		features = append(features, "cloud_tasks")
	}
	if h.deps.TaskMedia != nil && h.deps.TaskMedia.GenerationReady() && clientVerifier && cloudTasksAvailable && h.deps.WorkspaceSync != nil &&
		h.deps.ModelConnections != nil && h.deps.ModelGateway != nil {
		features = append(features, "task_media_generation")
	}
	if h.deps.ProjectBudgetExecutionReady && cloudTasksAvailable && h.deps.Projects != nil &&
		h.deps.ProjectContent != nil && h.deps.ProjectConversations != nil && h.deps.ProjectWorkItems != nil &&
		h.deps.ProjectBudget != nil && h.deps.ModelConnections != nil && h.deps.ModelGateway != nil {
		features = append(features, "project_budget_execution")
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, capabilitiesResponse{
		ServiceInstanceID: h.options.ServiceInstanceID,
		APIVersion:        "1", HubVersion: hubVersion, SupportedFeatures: features,
		DefaultMaxRequestBytes: h.options.APIMaxBodyBytes,
	})
}
