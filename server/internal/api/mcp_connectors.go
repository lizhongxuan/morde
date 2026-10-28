package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type mcpConnectorResponse struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	EndpointURL       string    `json:"endpoint_url"`
	Enabled           bool      `json:"enabled"`
	HasCredential     bool      `json:"has_credential"`
	CredentialVersion int64     `json:"credential_version"`
	Revision          int64     `json:"revision"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type mcpConnectorListResponse struct {
	Items []mcpConnectorResponse `json:"items"`
}

type createMCPConnectorRequest struct {
	Name        string          `json:"name"`
	EndpointURL string          `json:"endpoint_url"`
	Credential  json.RawMessage `json:"credential"`
}

type updateMCPConnectorRequest struct {
	Name             string `json:"name"`
	EndpointURL      string `json:"endpoint_url"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type rotateMCPConnectorCredentialRequest struct {
	Credential       json.RawMessage `json:"credential"`
	ExpectedRevision int64           `json:"expected_revision"`
}

type setMCPConnectorEnabledRequest struct {
	Enabled          *bool `json:"enabled"`
	ExpectedRevision int64 `json:"expected_revision"`
}

type beginMCPConnectorOAuthRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	ClientID         string `json:"client_id"`
	ClientSecret     string `json:"client_secret"`
}

type mcpConnectorOAuthStartResponse struct {
	Kind             string `json:"kind"`
	AuthorizationURL string `json:"authorization_url,omitempty"`
	DeveloperURL     string `json:"developer_url,omitempty"`
	RedirectURI      string `json:"redirect_uri,omitempty"`
}

func (h *api) registerMCPConnectors(group *gin.RouterGroup) {
	group.GET("/mcp/connectors", h.listMCPConnectors)
	group.POST("/mcp/connectors", h.createMCPConnector)
	group.GET("/mcp/connectors/:id", h.getMCPConnector)
	group.PUT("/mcp/connectors/:id", h.updateMCPConnector)
	group.PUT("/mcp/connectors/:id/credential", h.rotateMCPConnectorCredential)
	group.PUT("/mcp/connectors/:id/enabled", h.setMCPConnectorEnabled)
	group.POST("/mcp/connectors/:id/oauth/authorize", h.beginMCPConnectorOAuth)
	group.DELETE("/mcp/connectors/:id", h.deleteMCPConnector)
}

func (h *api) beginMCPConnectorOAuth(c *gin.Context) {
	principal, id, _, ok := h.mcpConnectorRequest(c)
	if !ok {
		return
	}
	oauth, ok := h.mcpConnectorOAuthManager(c)
	if !ok {
		return
	}
	var request beginMCPConnectorOAuthRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	result, err := oauth.BeginOAuth(c.Request.Context(), principal, id, request.ExpectedRevision, request.ClientID, request.ClientSecret)
	if err != nil {
		writeError(c, err)
		return
	}
	if result.Kind != "authorize" && result.Kind != "client_required" ||
		result.Kind == "authorize" && (result.AuthorizationURL == "" || result.RedirectURI == "") ||
		result.Kind == "client_required" && (result.DeveloperURL == "" || result.RedirectURI == "") {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	c.JSON(http.StatusOK, mcpConnectorOAuthStartResponse{
		Kind: result.Kind, AuthorizationURL: result.AuthorizationURL, DeveloperURL: result.DeveloperURL, RedirectURI: result.RedirectURI,
	})
}

func (h *api) mcpConnectorOAuthCallback(c *gin.Context) {
	oauth, ok := h.mcpConnectorOAuthManager(c)
	if !ok {
		c.Data(http.StatusServiceUnavailable, "text/html; charset=utf-8", []byte("<!doctype html><title>Morde</title><p>Authorization is unavailable. Return to Morde and try again.</p>"))
		return
	}
	err := oauth.CompleteOAuth(c.Request.Context(), c.Query("state"), c.Query("code"), c.Query("error"), c.Query("iss"))
	status := http.StatusOK
	message := "Authorization complete. You can close this page and return to Morde."
	if err != nil {
		status = http.StatusBadRequest
		message = "Authorization was not completed. Return to Morde and try again."
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	c.Data(status, "text/html; charset=utf-8", []byte("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>Morde</title></head><body><p>"+message+"</p></body></html>"))
}

func (h *api) listMCPConnectors(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	manager, ok := h.mcpConnectorManager(c)
	if !ok {
		return
	}
	items, err := manager.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := mcpConnectorListResponse{Items: make([]mcpConnectorResponse, 0, len(items))}
	for _, item := range items {
		value, err := toMCPConnectorResponse(item)
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, value)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) createMCPConnector(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	var request createMCPConnectorRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	manager, ok := h.mcpConnectorManager(c)
	if !ok {
		return
	}
	item, err := manager.Create(c.Request.Context(), principal, app.CreateMCPConnectorCommand{
		Name: request.Name, EndpointURL: request.EndpointURL, CredentialJSON: request.Credential,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toMCPConnectorResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, response)
}

func (h *api) getMCPConnector(c *gin.Context) {
	principal, id, manager, ok := h.mcpConnectorRequest(c)
	if !ok {
		return
	}
	item, err := manager.Get(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toMCPConnectorResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) updateMCPConnector(c *gin.Context) {
	principal, id, manager, ok := h.mcpConnectorRequest(c)
	if !ok {
		return
	}
	var request updateMCPConnectorRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	item, err := manager.Update(c.Request.Context(), principal, id, app.UpdateMCPConnectorCommand{
		Name: request.Name, EndpointURL: request.EndpointURL, ExpectedRevision: request.ExpectedRevision,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toMCPConnectorResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) rotateMCPConnectorCredential(c *gin.Context) {
	principal, id, manager, ok := h.mcpConnectorRequest(c)
	if !ok {
		return
	}
	var request rotateMCPConnectorCredentialRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	item, err := manager.RotateCredential(c.Request.Context(), principal, id, app.RotateMCPConnectorCredentialCommand{
		CredentialJSON: request.Credential, ExpectedRevision: request.ExpectedRevision,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toMCPConnectorResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) setMCPConnectorEnabled(c *gin.Context) {
	principal, id, manager, ok := h.mcpConnectorRequest(c)
	if !ok {
		return
	}
	var request setMCPConnectorEnabledRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.Enabled == nil {
		writeError(c, domain.InvalidArgument("enabled", "enabled must be provided"))
		return
	}
	item, err := manager.SetEnabled(c.Request.Context(), principal, id, *request.Enabled, app.MCPConnectorRevisionCommand{
		ExpectedRevision: request.ExpectedRevision,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toMCPConnectorResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) deleteMCPConnector(c *gin.Context) {
	principal, id, manager, ok := h.mcpConnectorRequest(c)
	if !ok {
		return
	}
	query := c.Request.URL.Query()
	for key := range query {
		if key != "expected_revision" {
			writeError(c, domain.InvalidArgument("expected_revision", "only expected_revision is accepted"))
			return
		}
	}
	values := query["expected_revision"]
	if len(values) != 1 {
		writeError(c, domain.InvalidArgument("expected_revision", "a positive connector revision is required"))
		return
	}
	revision, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || revision < 1 {
		writeError(c, domain.InvalidArgument("expected_revision", "a positive connector revision is required"))
		return
	}
	if err := manager.Delete(c.Request.Context(), principal, id, app.MCPConnectorRevisionCommand{ExpectedRevision: revision}); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *api) mcpConnectorRequest(c *gin.Context) (identity.Principal, string, MCPConnectorManager, bool) {
	principal, ok := modelSession(c)
	if !ok {
		return identity.Principal{}, "", nil, false
	}
	id, ok := safeID(c, "id", domain.ValidMCPConnectorID)
	if !ok {
		return identity.Principal{}, "", nil, false
	}
	manager, ok := h.mcpConnectorManager(c)
	if !ok {
		return identity.Principal{}, "", nil, false
	}
	return principal, id, manager, true
}

func (h *api) mcpConnectorManager(c *gin.Context) (MCPConnectorManager, bool) {
	if h.deps.MCPConnectors == nil {
		serviceUnavailable(c)
		return nil, false
	}
	return h.deps.MCPConnectors, true
}

func (h *api) mcpConnectorOAuthManager(c *gin.Context) (MCPConnectorOAuthManager, bool) {
	manager, ok := h.deps.MCPConnectors.(MCPConnectorOAuthManager)
	if !ok {
		serviceUnavailable(c)
		return nil, false
	}
	return manager, true
}

func toMCPConnectorResponse(item app.MCPConnectorView) (mcpConnectorResponse, error) {
	if !domain.ValidMCPConnectorID(item.ID) || item.Name == "" || item.EndpointURL == "" || item.Revision < 1 || item.CredentialVersion < 0 ||
		(item.HasCredential != (item.CredentialVersion > 0)) || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return mcpConnectorResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return mcpConnectorResponse{
		ID: item.ID, Name: item.Name, EndpointURL: item.EndpointURL, Enabled: item.Enabled,
		HasCredential: item.HasCredential, CredentialVersion: item.CredentialVersion, Revision: item.Revision,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}
