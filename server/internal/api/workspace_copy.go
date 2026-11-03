package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type WorkspaceCopyEndpointService interface {
	Copy(context.Context, identity.Principal, string, app.WorkspaceCopyCommand, string) (app.WorkspaceCopyResult, error)
}

type workspaceCopyRequest struct {
	Name                    string `json:"name"`
	ExpectedCurrentRevision *int64 `json:"expected_current_revision"`
}

func (h *api) registerWorkspaceCopies(group *gin.RouterGroup, service WorkspaceCopyEndpointService) {
	group.POST("/workspaces/:id/copies", h.copyWorkspace(service))
}

func (h *api) copyWorkspace(service WorkspaceCopyEndpointService) gin.HandlerFunc {
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
		var request workspaceCopyRequest
		if !decodeWorkspaceSyncJSON(c, &request) {
			return
		}
		if request.ExpectedCurrentRevision == nil || *request.ExpectedCurrentRevision < 0 {
			writeError(c, domain.InvalidArgument("expected_current_revision", "a non-negative expected current revision is required"))
			return
		}
		result, err := service.Copy(c.Request.Context(), principal, workspaceID, app.WorkspaceCopyCommand{
			Name: request.Name, ExpectedCurrentRevision: *request.ExpectedCurrentRevision,
		}, key)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if result.Replayed {
			status = http.StatusOK
		}
		c.JSON(status, result)
	}
}
