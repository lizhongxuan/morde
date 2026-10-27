package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type clientLoginRequest struct {
	Name                  string `json:"name"`
	Password              string `json:"password"`
	Platform              string `json:"platform"`
	DisplayName           string `json:"display_name"`
	RenewalInstallationID string `json:"renewal_installation_id"`
}

type clientLoginResponse struct {
	InstallationID       string     `json:"installation_id"`
	ClientCredential     string     `json:"client_credential"`
	ClientExpiresAt      time.Time  `json:"client_expires_at"`
	SyncCredential       string     `json:"sync_credential,omitempty"`
	SyncExpiresAt        *time.Time `json:"sync_expires_at,omitempty"`
	WebSessionCredential string     `json:"web_session_credential,omitempty"`
	WebSessionExpiresAt  *time.Time `json:"web_session_expires_at,omitempty"`
	User                 UserView   `json:"user"`
}

type clientInfoResponse struct {
	ID          string     `json:"id"`
	Platform    string     `json:"platform"`
	DisplayName string     `json:"display_name"`
	ExpiresAt   time.Time  `json:"expires_at"`
	Revoked     bool       `json:"revoked"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

func (h *api) registerClients(group *gin.RouterGroup) {
	group.GET("/clients", h.listClients)
	group.DELETE("/clients/:id", h.revokeClient)
}

func (h *api) clientLogin(c *gin.Context) {
	if h.deps.ClientLogin == nil {
		serviceUnavailable(c)
		return
	}
	var request clientLoginRequest
	if !decodeJSON(c, &request) {
		return
	}
	if strings.TrimSpace(request.Name) == "" || request.Password == "" {
		writeError(c, domain.InvalidArgument("name", "name and password are required"))
		return
	}
	result, err := h.deps.ClientLogin.Login(c.Request.Context(), request.Name, request.Password, request.Platform,
		request.DisplayName, request.RenewalInstallationID, c.GetHeader("Idempotency-Key"), c.ClientIP())
	if err != nil {
		writeError(c, err)
		return
	}
	response := clientLoginResponse{
		InstallationID: result.InstallationID, ClientCredential: result.ClientCredential, ClientExpiresAt: result.ClientExpiresAt,
		SyncCredential: result.SyncCredential, WebSessionCredential: result.WebSessionCredential, User: userSummaryView(result.User),
	}
	if result.SyncCredential != "" {
		response.SyncExpiresAt = &result.SyncExpiresAt
	}
	if result.WebSessionCredential != "" {
		response.WebSessionExpiresAt = &result.WebSessionExpiresAt
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) clientWebSession(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok || principal.Kind != identity.KindClient {
		unauthenticated(c)
		return
	}
	if h.deps.ClientLogin == nil {
		serviceUnavailable(c)
		return
	}
	result, err := h.deps.ClientLogin.WebSession(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"web_session_credential": result.Credential, "web_session_expires_at": result.ExpiresAt})
}

func (h *api) clientLogout(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok || principal.Kind != identity.KindClient {
		unauthenticated(c)
		return
	}
	if h.deps.ClientLogin == nil {
		serviceUnavailable(c)
		return
	}
	if err := h.deps.ClientLogin.Logout(c.Request.Context(), principal, c.ClientIP()); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *api) listClients(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.ClientLogin == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.ClientLogin.List(c.Request.Context(), principal)
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
	if h.deps.ClientLogin == nil {
		serviceUnavailable(c)
		return
	}
	if err := h.deps.ClientLogin.Revoke(c.Request.Context(), principal, id, c.ClientIP()); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func toClientInfoResponse(value app.ClientInfo) clientInfoResponse {
	return clientInfoResponse{ID: value.ID, Platform: value.Platform, DisplayName: value.DisplayName,
		ExpiresAt: value.ExpiresAt, Revoked: value.Revoked, CreatedAt: value.CreatedAt, LastUsedAt: value.LastUsedAt}
}

func userSummaryView(value app.UserSummary) UserView {
	return UserView{ID: value.ID, Name: value.Name, Role: value.Role, Status: value.Status, MustChangePassword: value.MustChangePassword,
		Quota: TotalsView{Hosts: value.Quota.Hosts, CPUMilli: value.Quota.CPUMilli, MemMiB: value.Quota.MemMiB},
		Usage: TotalsView{Hosts: value.Usage.Hosts, CPUMilli: value.Usage.CPUMilli, MemMiB: value.Usage.MemMiB}}
}
