package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type workspaceFileOperationService interface {
	ApplyFileOperation(context.Context, identity.Principal, string, app.WorkspaceFileOperationCommand, string) (app.WorkspaceFileOperationResult, error)
}

type workspaceFileOperationRequest struct {
	Action                  app.WorkspaceFileOperationAction `json:"action"`
	SourcePath              string                           `json:"source_path"`
	TargetPath              string                           `json:"target_path"`
	ExpectedCurrentRevision *int64                           `json:"expected_current_revision"`
	WriteEnabled            bool                             `json:"write_enabled"`
}

func (h *api) registerWorkspaceFileOperations(group *gin.RouterGroup, service workspaceFileOperationService) {
	group.POST("/workspaces/:id/file-operations", func(c *gin.Context) {
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
		var request workspaceFileOperationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		idempotencyKey, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if request.ExpectedCurrentRevision == nil || *request.ExpectedCurrentRevision < 0 {
			writeError(c, domain.InvalidArgument("expected_current_revision", "expected Workspace revision is required"))
			return
		}
		result, err := service.ApplyFileOperation(c.Request.Context(), principal, workspaceID, app.WorkspaceFileOperationCommand{
			Action: request.Action, SourcePath: request.SourcePath, TargetPath: request.TargetPath,
			ExpectedCurrentRevision: *request.ExpectedCurrentRevision, WriteEnabled: request.WriteEnabled,
		}, idempotencyKey)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
}
