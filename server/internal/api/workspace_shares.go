package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type WorkspaceShareEndpointService interface {
	Create(context.Context, identity.Principal, string, int64, workspace.SharePermission) (app.WorkspaceShareCreated, error)
	Preview(context.Context, identity.Principal, string) (app.WorkspaceSharePreview, error)
	Accept(context.Context, identity.Principal, string) (app.WorkspaceShareAccepted, error)
}

type createWorkspaceShareRequest struct {
	ExpectedCurrentRevision *int64                    `json:"expected_current_revision"`
	Permission              workspace.SharePermission `json:"permission"`
}

type workspaceShareTokenRequest struct {
	Token string `json:"token"`
}

func (h *api) registerWorkspaceShares(group *gin.RouterGroup, service WorkspaceShareEndpointService) {
	group.POST("/workspaces/:id/shares", h.createWorkspaceShare(service))
	group.POST("/workspace-shares/preview", h.previewWorkspaceShare(service))
	group.POST("/workspace-shares/accept", h.acceptWorkspaceShare(service))
}

func (h *api) createWorkspaceShare(service WorkspaceShareEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
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
		var request createWorkspaceShareRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedCurrentRevision == nil || *request.ExpectedCurrentRevision < 0 ||
			(request.Permission != workspace.ShareView && request.Permission != workspace.ShareEdit) {
			writeError(c, domain.InvalidArgument("workspace_share", "Workspace share request is invalid"))
			return
		}
		result, err := service.Create(c.Request.Context(), principal, workspaceID, *request.ExpectedCurrentRevision, request.Permission)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	}
}

func (h *api) previewWorkspaceShare(service WorkspaceShareEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request workspaceShareTokenRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		result, err := service.Preview(c.Request.Context(), principal, request.Token)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func (h *api) acceptWorkspaceShare(service WorkspaceShareEndpointService) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request workspaceShareTokenRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		result, err := service.Accept(c.Request.Context(), principal, request.Token)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	}
}
