package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type WorkspaceDiffService interface {
	GetDiff(context.Context, identity.Principal, string, int64, int64) (app.WorkspaceDiff, error)
	Restore(context.Context, identity.Principal, string, app.WorkspaceRestoreCommand, string) (app.WorkspaceRestoreResult, error)
}

func (h *api) registerWorkspaceDiff(group *gin.RouterGroup, service WorkspaceDiffService) {
	group.GET("/workspaces/:id/diff", h.getWorkspaceDiff(service))
	group.POST("/workspaces/:id/restore", h.restoreWorkspace(service))
}

func (h *api) getWorkspaceDiff(service WorkspaceDiffService) gin.HandlerFunc {
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
		fromRevision, err := optionalWorkspaceSyncInt(c, "from_revision", -1, 0, int64(^uint64(0)>>1))
		if err != nil || fromRevision < 0 {
			if err == nil {
				err = domain.InvalidArgument("from_revision", "query parameter is required")
			}
			writeError(c, err)
			return
		}
		toRevision, err := optionalWorkspaceSyncInt(c, "to_revision", -1, 0, int64(^uint64(0)>>1))
		if err != nil || toRevision < 0 {
			if err == nil {
				err = domain.InvalidArgument("to_revision", "query parameter is required")
			}
			writeError(c, err)
			return
		}
		diff, err := service.GetDiff(c.Request.Context(), principal, workspaceID, fromRevision, toRevision)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, diff)
	}
}

type workspaceRestoreRequest struct {
	TargetRevision          int64    `json:"target_revision"`
	ExpectedCurrentRevision int64    `json:"expected_current_revision"`
	Paths                   []string `json:"paths"`
}

func (h *api) restoreWorkspace(service WorkspaceDiffService) gin.HandlerFunc {
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
		key, ok := workspaceSyncIdempotencyKey(c)
		if !ok {
			return
		}
		var request workspaceRestoreRequest
		if !decodeWorkspaceSyncJSON(c, &request) {
			return
		}
		result, err := service.Restore(c.Request.Context(), principal, workspaceID, app.WorkspaceRestoreCommand{
			TargetRevision: request.TargetRevision, ExpectedCurrentRevision: request.ExpectedCurrentRevision, Paths: request.Paths,
		}, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}
