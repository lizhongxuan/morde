package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type startClientAuthorizationRequest struct {
	Platform              string `json:"platform"`
	DisplayName           string `json:"display_name"`
	WantsWorkspaceSync    bool   `json:"wants_workspace_sync"`
	RenewalInstallationID string `json:"renewal_installation_id"`
}

type pollClientAuthorizationRequest struct {
	DeviceCode string `json:"device_code"`
}
type inspectClientAuthorizationRequest struct {
	UserCode string `json:"user_code"`
}
type denyClientAuthorizationRequest struct {
	UserCode string `json:"user_code"`
}

type startClientAuthorizationResponse struct {
	DeviceCode      string    `json:"device_code"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	ExpiresAt       time.Time `json:"expires_at"`
	IntervalSeconds int       `json:"interval_seconds"`
}

type pollClientAuthorizationResponse struct {
	State            string     `json:"state"`
	ExpiresAt        time.Time  `json:"expires_at"`
	InstallationID   string     `json:"installation_id,omitempty"`
	ClientCredential string     `json:"client_credential,omitempty"`
	ClientExpiresAt  *time.Time `json:"client_expires_at,omitempty"`
	SyncCredential   string     `json:"sync_credential,omitempty"`
	SyncExpiresAt    *time.Time `json:"sync_expires_at,omitempty"`
	WorkspaceIDs     []string   `json:"workspace_ids,omitempty"`
}

type inspectClientAuthorizationResponse struct {
	Platform              string    `json:"platform"`
	DisplayName           string    `json:"display_name"`
	WantsWorkspaceSync    bool      `json:"wants_workspace_sync"`
	RenewalInstallationID string    `json:"renewal_installation_id,omitempty"`
	ExpiresAt             time.Time `json:"expires_at"`
}

type approveClientAuthorizationRequest struct {
	UserCode     string   `json:"user_code"`
	WorkspaceIDs []string `json:"workspace_ids"`
}

type clientInfoResponse struct {
	ID           string     `json:"id"`
	Platform     string     `json:"platform"`
	DisplayName  string     `json:"display_name"`
	WorkspaceIDs []string   `json:"workspace_ids"`
	ExpiresAt    time.Time  `json:"expires_at"`
	Revoked      bool       `json:"revoked"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at"`
}

func (h *api) registerClientAuthorizations(group *gin.RouterGroup) {
	group.POST("/client-authorizations/inspect", h.inspectClientAuthorization)
	group.POST("/client-authorizations/approve", h.approveClientAuthorization)
	group.POST("/client-authorizations/deny", h.denyClientAuthorization)
	group.GET("/clients", h.listClients)
	group.DELETE("/clients/:id", h.revokeClient)
}

func (h *api) createClientAuthorization(c *gin.Context) {
	if h.deps.ClientAuthorizations == nil {
		serviceUnavailable(c)
		return
	}
	var request startClientAuthorizationRequest
	if !decodeJSON(c, &request) {
		return
	}
	result, err := h.deps.ClientAuthorizations.Start(c.Request.Context(), request.Platform, request.DisplayName, request.WantsWorkspaceSync, request.RenewalInstallationID, c.ClientIP())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, startClientAuthorizationResponse{DeviceCode: result.DeviceCode, UserCode: result.UserCode,
		VerificationURI: "/client-authorize", ExpiresAt: result.ExpiresAt, IntervalSeconds: result.Interval})
}

func (h *api) pollClientAuthorization(c *gin.Context) {
	if h.deps.ClientAuthorizations == nil {
		serviceUnavailable(c)
		return
	}
	var request pollClientAuthorizationRequest
	if !decodeJSON(c, &request) {
		return
	}
	result, err := h.deps.ClientAuthorizations.Poll(c.Request.Context(), request.DeviceCode, c.ClientIP())
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCredential) {
			writeError(c, domain.NewError(domain.CodeUnauthenticated, "client authorization is invalid"))
		} else {
			writeError(c, err)
		}
		return
	}
	response := pollClientAuthorizationResponse{State: result.State, ExpiresAt: result.ExpiresAt}
	if result.State == "approved" {
		response.InstallationID = result.InstallationID
		response.ClientCredential = result.ClientCredential
		response.ClientExpiresAt = &result.ClientExpiresAt
		response.WorkspaceIDs = append([]string{}, result.WorkspaceIDs...)
		if result.SyncCredential != "" {
			response.SyncCredential = result.SyncCredential
			response.SyncExpiresAt = &result.SyncExpiresAt
		}
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) inspectClientAuthorization(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.ClientAuthorizations == nil {
		serviceUnavailable(c)
		return
	}
	var request inspectClientAuthorizationRequest
	if !decodeJSON(c, &request) {
		return
	}
	result, err := h.deps.ClientAuthorizations.Inspect(c.Request.Context(), principal, request.UserCode, c.ClientIP())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, inspectClientAuthorizationResponse{Platform: result.Platform, DisplayName: result.DisplayName,
		WantsWorkspaceSync: result.WantsWorkspaceSync, RenewalInstallationID: result.RenewalInstallationID, ExpiresAt: result.ExpiresAt})
}

func (h *api) approveClientAuthorization(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.ClientAuthorizations == nil {
		serviceUnavailable(c)
		return
	}
	var request approveClientAuthorizationRequest
	if !decodeJSON(c, &request) {
		return
	}
	if request.WorkspaceIDs == nil {
		writeError(c, domain.InvalidArgument("workspace_ids", "workspace_ids must be an explicit array"))
		return
	}
	result, err := h.deps.ClientAuthorizations.Approve(c.Request.Context(), principal, request.UserCode, request.WorkspaceIDs, c.ClientIP())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toClientInfoResponse(result))
}

func (h *api) denyClientAuthorization(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.ClientAuthorizations == nil {
		serviceUnavailable(c)
		return
	}
	var request denyClientAuthorizationRequest
	if !decodeJSON(c, &request) {
		return
	}
	if err := h.deps.ClientAuthorizations.Deny(c.Request.Context(), principal, request.UserCode, c.ClientIP()); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *api) listClients(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.ClientAuthorizations == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.ClientAuthorizations.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := make([]clientInfoResponse, 0, len(items))
	for _, item := range items {
		response = append(response, toClientInfoResponse(item))
	}
	c.JSON(http.StatusOK, gin.H{"items": response})
}

func (h *api) revokeClient(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidClientInstallationID)
	if !ok {
		return
	}
	if h.deps.ClientAuthorizations == nil {
		serviceUnavailable(c)
		return
	}
	if err := h.deps.ClientAuthorizations.Revoke(c.Request.Context(), principal, id, c.ClientIP()); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func toClientInfoResponse(value app.ClientInfo) clientInfoResponse {
	return clientInfoResponse{ID: value.ID, Platform: value.Platform, DisplayName: value.DisplayName,
		WorkspaceIDs: append([]string{}, value.WorkspaceIDs...), ExpiresAt: value.ExpiresAt,
		Revoked: value.Revoked, CreatedAt: value.CreatedAt, LastUsedAt: value.LastUsedAt}
}
