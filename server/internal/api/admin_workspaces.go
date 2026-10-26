package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type AdminWorkspaceService interface {
	List(context.Context, identity.Principal, app.PageRequest) (workspace.AdminPage, error)
	Get(context.Context, identity.Principal, string) (workspace.AdminSummary, error)
}

type adminWorkspacePageResponse struct {
	Items      []adminWorkspaceResponse `json:"items"`
	NextCursor *string                  `json:"next_cursor"`
}

type adminWorkspaceResponse struct {
	ID         string                          `json:"id"`
	OwnerID    string                          `json:"owner_id"`
	OwnerName  string                          `json:"owner_name"`
	Name       string                          `json:"name"`
	State      workspace.State                 `json:"state"`
	Revision   int64                           `json:"revision"`
	UsedBytes  int64                           `json:"used_bytes"`
	QuotaBytes int64                           `json:"quota_bytes"`
	CreatedAt  time.Time                       `json:"created_at"`
	UpdatedAt  time.Time                       `json:"updated_at"`
	Bindings   []adminWorkspaceBindingResponse `json:"bindings"`
}

type adminWorkspaceBindingResponse struct {
	HostID          string                     `json:"host_id"`
	HostName        string                     `json:"host_name"`
	State           workspace.BindingState     `json:"state"`
	SyncState       workspace.BindingSyncState `json:"sync_state"`
	DesiredRevision int64                      `json:"desired_revision"`
	AppliedRevision int64                      `json:"applied_revision"`
}

func (h *api) listAdminWorkspaces(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminWorkspaces == nil {
		serviceUnavailable(c)
		return
	}
	page, ok := requestPage(c)
	if !ok {
		return
	}
	result, err := h.deps.AdminWorkspaces.List(c.Request.Context(), principal, page)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminWorkspacePageResponse{Items: make([]adminWorkspaceResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
	for _, item := range result.Items {
		view, err := toAdminWorkspaceResponse(item)
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, view)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminWorkspace(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminWorkspaces == nil {
		serviceUnavailable(c)
		return
	}
	item, err := h.deps.AdminWorkspaces.Get(c.Request.Context(), principal, c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toAdminWorkspaceResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func toAdminWorkspaceResponse(item workspace.AdminSummary) (adminWorkspaceResponse, error) {
	if !domain.ValidWorkspaceID(item.ID) || !domain.ValidUserID(item.OwnerID) || item.OwnerName == "" ||
		(item.State != workspace.StateReady && item.State != workspace.StateDeleting) || item.Revision < 0 || item.UsedBytes < 0 || item.QuotaBytes < 0 {
		return adminWorkspaceResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	response := adminWorkspaceResponse{
		ID: item.ID, OwnerID: item.OwnerID, OwnerName: item.OwnerName, Name: item.Name,
		State: item.State, Revision: item.Revision, UsedBytes: item.UsedBytes,
		QuotaBytes: item.QuotaBytes, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		Bindings: make([]adminWorkspaceBindingResponse, 0, len(item.Bindings)),
	}
	for _, binding := range item.Bindings {
		if !domain.ValidHostID(binding.HostID) || !validAdminBindingState(binding.State) || !validAdminBindingSyncState(binding.SyncState) ||
			binding.DesiredRevision < 0 || binding.AppliedRevision < 0 {
			return adminWorkspaceResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.Bindings = append(response.Bindings, adminWorkspaceBindingResponse{
			HostID: binding.HostID, HostName: binding.HostName, State: binding.State, SyncState: binding.SyncState,
			DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		})
	}
	return response, nil
}

func validAdminBindingState(value workspace.BindingState) bool {
	switch value {
	case workspace.BindingPreparing, workspace.BindingActive, workspace.BindingUnbinding, workspace.BindingDetached, workspace.BindingFailed:
		return true
	default:
		return false
	}
}

func validAdminBindingSyncState(value workspace.BindingSyncState) bool {
	return value == workspace.BindingSyncHealthy || value == workspace.BindingSyncConflict
}
