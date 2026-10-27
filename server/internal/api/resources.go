package api

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type templateListResponse struct {
	Items []templateResponse `json:"items"`
}

type templateResponse struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Description          string            `json:"description"`
	Isolation            string            `json:"isolation"`
	Resources            resourcesResponse `json:"resources"`
	WorkspaceSizeMiB     int               `json:"workspace_size_mib"`
	TTLSeconds           int64             `json:"ttl_seconds"`
	IdleTimeoutSeconds   int64             `json:"idle_timeout_seconds"`
	OnIdle               string            `json:"on_idle"`
	KeepWorkspaceDefault bool              `json:"keep_workspace_default"`
}

type workspaceListResponse struct {
	Items []workspaceResponse `json:"items"`
}

type deviceWorkspaceListResponse struct {
	Items []app.DeviceWorkspaceInfo `json:"items"`
}

type workspaceResponse struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Revision   int64           `json:"revision"`
	UsedBytes  int64           `json:"used_bytes"`
	QuotaBytes int64           `json:"quota_bytes"`
	State      workspace.State `json:"state"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type createWorkspaceRequest struct {
	Name string `json:"name"`
}

type tokenListResponse struct {
	Items []tokenResponse `json:"items"`
}

type tokenResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Revoked    bool       `json:"revoked"`
	CreatedAt  time.Time  `json:"created_at"`
}

type createAPIKeyRequest struct {
	Name       string `json:"name"`
	TTLSeconds *int64 `json:"ttl_seconds"`
}

func (h *api) registerResources(group *gin.RouterGroup) {
	group.GET("/templates", h.listTemplates)
	group.GET("/device/workspaces", h.listDeviceWorkspaces)
	group.GET("/workspaces", h.listWorkspaces)
	group.POST("/workspaces", h.createWorkspace)
	group.GET("/workspaces/:id", h.getWorkspace)
	group.DELETE("/workspaces/:id", h.deleteWorkspace)
	group.GET("/apikeys", h.listAPIKeys)
	group.POST("/apikeys", h.createAPIKey)
	group.DELETE("/apikeys/:id", h.deleteAPIKey)
}

func (h *api) listDeviceWorkspaces(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	if h.deps.WorkspaceOps == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.WorkspaceOps.ListDeviceWorkspaces(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := deviceWorkspaceListResponse{Items: make([]app.DeviceWorkspaceInfo, 0, len(items))}
	response.Items = append(response.Items, items...)
	c.JSON(http.StatusOK, response)
}

func (h *api) listTemplates(c *gin.Context) {
	if h.deps.Templates == nil {
		serviceUnavailable(c)
		return
	}
	records, err := h.deps.Templates.ListTemplates(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	response := templateListResponse{Items: make([]templateResponse, 0, len(records))}
	for _, item := range records {
		if !domain.ValidTemplateID(item.ID) || item.Name == "" || item.Isolation == "" || item.TTLSeconds <= 0 || item.IdleTimeoutSeconds <= 0 || item.OnIdle != "disabled" {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		response.Items = append(response.Items, templateResponse{
			ID: item.ID, Name: item.Name, Description: item.Description, Isolation: item.Isolation,
			Resources: resourcesResponse{
				CPUMilli: item.Resources.CPUMilli, MemMiB: item.Resources.MemMiB,
				CPULimitMilli: item.Resources.CPULimitMilli, MemLimitMiB: item.Resources.MemLimitMiB,
			},
			WorkspaceSizeMiB: item.WorkspaceSizeMiB, TTLSeconds: item.TTLSeconds,
			IdleTimeoutSeconds: item.IdleTimeoutSeconds, OnIdle: string(item.OnIdle),
			KeepWorkspaceDefault: item.KeepWorkspaceDefault,
		})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) listWorkspaces(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.WorkspaceOps == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.WorkspaceOps.ListWorkspaces(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := workspaceListResponse{Items: make([]workspaceResponse, 0, len(items))}
	for _, item := range items {
		view, err := toWorkspaceResponse(item)
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, view)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) createWorkspace(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.WorkspaceOps == nil {
		serviceUnavailable(c)
		return
	}
	var request createWorkspaceRequest
	if !decodeJSON(c, &request) {
		return
	}
	if strings.TrimSpace(request.Name) == "" || utf8.RuneCountInString(request.Name) > 120 {
		writeError(c, domain.InvalidArgument("name", "name must be between 1 and 120 characters"))
		return
	}
	keys := c.Request.Header.Values("Idempotency-Key")
	if len(keys) > 1 {
		writeError(c, domain.InvalidArgument("Idempotency-Key", "only one idempotency key may be supplied"))
		return
	}
	item, err := h.deps.WorkspaceOps.CreateWorkspace(c.Request.Context(), principal, app.CreateWorkspaceCommand{Name: request.Name}, c.GetHeader("Idempotency-Key"))
	if err != nil {
		writeError(c, err)
		return
	}
	view, err := toWorkspaceResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, view)
}

func (h *api) getWorkspace(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidWorkspaceID)
	if !ok {
		return
	}
	if h.deps.WorkspaceOps == nil {
		serviceUnavailable(c)
		return
	}
	item, err := h.deps.WorkspaceOps.GetWorkspace(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	view, err := toWorkspaceResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *api) deleteWorkspace(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidWorkspaceID)
	if !ok {
		return
	}
	if h.deps.WorkspaceOps == nil {
		serviceUnavailable(c)
		return
	}
	item, err := h.deps.WorkspaceOps.DeleteWorkspace(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	view, err := toWorkspaceResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, view)
}

func toWorkspaceResponse(item workspace.Workspace) (workspaceResponse, error) {
	if !domain.ValidWorkspaceID(item.ID) || strings.TrimSpace(item.Name) == "" || utf8.RuneCountInString(item.Name) > 120 ||
		!validWorkspaceState(item.State) || item.Revision < 0 || item.UsedBytes < 0 || item.QuotaBytes <= 0 || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return workspaceResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return workspaceResponse{
		ID: item.ID, Name: item.Name, Revision: item.Revision, UsedBytes: item.UsedBytes, QuotaBytes: item.QuotaBytes,
		State: item.State, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func validWorkspaceState(state workspace.State) bool {
	return state == workspace.StateReady || state == workspace.StateDeleting || state == workspace.StateDeleted
}

func (h *api) listAPIKeys(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.Tokens == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.Tokens.ListAPIKeys(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toTokenList(items, identity.KindAPIKey)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func toTokenList(items []identity.Token, kind identity.Kind) (tokenListResponse, error) {
	response := tokenListResponse{Items: make([]tokenResponse, 0, len(items))}
	for _, item := range items {
		if !domain.ValidTokenID(item.ID) || item.Kind != kind || (kind == identity.KindAPIKey && item.HostID != "") {
			return tokenListResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.Items = append(response.Items, tokenResponse{
			ID: item.ID, Name: item.Name,
			ExpiresAt: item.ExpiresAt, LastUsedAt: item.LastUsedAt, Revoked: item.RevokedAt != nil,
			CreatedAt: item.CreatedAt,
		})
	}
	return response, nil
}

func (h *api) createAPIKey(c *gin.Context) {
	principal, _ := principalFrom(c)
	if h.deps.Tokens == nil {
		serviceUnavailable(c)
		return
	}
	var request createAPIKeyRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if strings.TrimSpace(request.Name) == "" {
		writeError(c, domain.InvalidArgument("name", "name is required"))
		return
	}
	var ttl time.Duration
	if request.TTLSeconds != nil {
		if *request.TTLSeconds < 1 || *request.TTLSeconds > 31536000 {
			writeError(c, domain.InvalidArgument("ttl_seconds", "ttl_seconds is outside the allowed range"))
			return
		}
		ttl = time.Duration(*request.TTLSeconds) * time.Second
	}
	created, err := h.deps.Tokens.CreateAPIKey(c.Request.Context(), principal, request.Name, ttl)
	if err != nil {
		writeError(c, err)
		return
	}
	kind, tokenID, _, parseErr := identity.ParseCredential(created.Key)
	if !domain.ValidTokenID(created.ID) || parseErr != nil || kind != identity.KindAPIKey || tokenID != created.ID || !created.ExpiresAt.After(time.Now()) {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	c.JSON(http.StatusCreated, struct {
		ID        string    `json:"id"`
		Key       string    `json:"key"`
		ExpiresAt time.Time `json:"expires_at"`
	}{ID: created.ID, Key: created.Key, ExpiresAt: created.ExpiresAt})
}

func (h *api) deleteAPIKey(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidTokenID)
	if !ok {
		return
	}
	if h.deps.Tokens == nil {
		serviceUnavailable(c)
		return
	}
	if err := h.deps.Tokens.DeleteAPIKey(c.Request.Context(), principal, id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
