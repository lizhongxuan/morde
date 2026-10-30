package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type createWorkspaceSyncDeviceChallengeRequest struct {
	DeviceID       string `json:"device_id"`
	CertificateDER []byte `json:"certificate_der"`
}

type createWorkspaceSyncDeviceChallengeResponse struct {
	ID             string    `json:"id"`
	DeviceID       string    `json:"device_id"`
	Nonce          []byte    `json:"nonce"`
	SigningPayload []byte    `json:"signing_payload"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type registerWorkspaceSyncDeviceRequest struct {
	ChallengeID string `json:"challenge_id"`
	Signature   []byte `json:"signature"`
}

type workspaceSyncDeviceBindingResponse struct {
	ID                   string                           `json:"id"`
	WorkspaceID          string                           `json:"workspace_id"`
	ClientInstallationID string                           `json:"client_installation_id"`
	DeviceID             string                           `json:"device_id"`
	FolderID             string                           `json:"folder_id"`
	ServerDeviceID       string                           `json:"server_device_id,omitempty"`
	RelayAddress         string                           `json:"relay_address,omitempty"`
	State                workspace.SyncDeviceBindingState `json:"state"`
	CreatedAt            time.Time                        `json:"created_at"`
	RevokedAt            *time.Time                       `json:"revoked_at,omitempty"`
}

func (h *api) registerWorkspaceSyncDeviceRoutes(group *gin.RouterGroup, service WorkspaceSyncDeviceService) {
	group.POST("/workspaces/:id/sync-device-challenges", h.createWorkspaceSyncDeviceChallenge(service))
	group.POST("/workspaces/:id/sync-devices", h.registerWorkspaceSyncDevice(service))
	group.GET("/workspaces/:id/sync-devices", h.listWorkspaceSyncDevices(service))
	group.DELETE("/workspaces/:id/sync-devices/:binding_id", h.revokeWorkspaceSyncDevice(service))
}

func (h *api) listWorkspaceSyncDevices(service WorkspaceSyncDeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		items, err := service.List(c.Request.Context(), principal, workspaceID)
		if err != nil {
			writeError(c, err)
			return
		}
		response := struct {
			Items []workspaceSyncDeviceBindingResponse `json:"items"`
		}{Items: make([]workspaceSyncDeviceBindingResponse, 0, len(items))}
		for _, item := range items {
			response.Items = append(response.Items, workspaceSyncDeviceBindingResponse{
				ID: item.ID, WorkspaceID: item.WorkspaceID, ClientInstallationID: item.ClientInstallationID,
				DeviceID: item.DeviceID, FolderID: item.FolderID, State: item.State,
				CreatedAt: item.CreatedAt, RevokedAt: item.RevokedAt,
			})
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) createWorkspaceSyncDeviceChallenge(service WorkspaceSyncDeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		var request createWorkspaceSyncDeviceChallengeRequest
		if !decodeWorkspaceSyncJSON(c, &request) {
			return
		}
		challenge, err := service.CreateChallenge(c.Request.Context(), principal, workspaceID, request.DeviceID, request.CertificateDER)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, createWorkspaceSyncDeviceChallengeResponse{
			ID: challenge.ID, DeviceID: challenge.DeviceID, Nonce: challenge.Nonce,
			SigningPayload: challenge.SigningPayload, ExpiresAt: challenge.ExpiresAt,
		})
	}
}

func (h *api) registerWorkspaceSyncDevice(service WorkspaceSyncDeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		idempotencyKey, ok := workspaceSyncIdempotencyKey(c)
		if !ok {
			return
		}
		var request registerWorkspaceSyncDeviceRequest
		if !decodeWorkspaceSyncJSON(c, &request) {
			return
		}
		registration, err := service.Register(c.Request.Context(), principal, workspaceID, request.ChallengeID, idempotencyKey, request.Signature)
		if err != nil {
			writeError(c, err)
			return
		}
		binding := registration.Binding
		c.JSON(http.StatusOK, workspaceSyncDeviceBindingResponse{
			ID: binding.ID, WorkspaceID: binding.WorkspaceID, ClientInstallationID: binding.ClientInstallationID,
			DeviceID: binding.DeviceID, FolderID: binding.FolderID, ServerDeviceID: registration.Node.DeviceID,
			RelayAddress: registration.Node.RelayAddress, State: binding.State,
			CreatedAt: binding.CreatedAt, RevokedAt: binding.RevokedAt,
		})
	}
}

func (h *api) revokeWorkspaceSyncDevice(service WorkspaceSyncDeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		bindingID := c.Param("binding_id")
		if !domain.ValidWorkspaceSyncDeviceBindingID(bindingID) {
			writeError(c, domain.InvalidArgument("binding_id", "invalid workspace sync device binding id"))
			return
		}
		if err := service.Revoke(c.Request.Context(), principal, workspaceID, bindingID); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
