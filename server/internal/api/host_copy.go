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

type HostCopyEndpointService interface {
	Copy(context.Context, identity.Principal, string, app.HostCopyCommand, string) (app.HostCopyResult, error)
}

type hostCopyRequest struct {
	Name                    string `json:"name"`
	ExpectedCurrentRevision *int64 `json:"expected_current_revision"`
}

type hostCopyResponse struct {
	SourceHostID      string                  `json:"source_host_id"`
	SourceWorkspaceID string                  `json:"source_workspace_id"`
	Host              hostResponse            `json:"host"`
	Workspace         app.DeviceWorkspaceInfo `json:"workspace"`
	SourceRevision    int64                   `json:"source_revision"`
	State             workspace.CopyState     `json:"state"`
	Replayed          bool                    `json:"replayed"`
}

func (h *api) registerHostCopies(group *gin.RouterGroup, service HostCopyEndpointService) {
	group.POST("/hosts/:id/copies", h.copyHost(service))
}

func (h *api) copyHost(service HostCopyEndpointService) gin.HandlerFunc {
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
		hostID, ok := safeID(c, "id", domain.ValidHostID)
		if !ok {
			return
		}
		key, ok := workspaceSyncIdempotencyKey(c)
		if !ok {
			return
		}
		var request hostCopyRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedCurrentRevision == nil || *request.ExpectedCurrentRevision < 0 {
			writeError(c, domain.InvalidArgument("expected_current_revision", "a non-negative expected current revision is required"))
			return
		}
		result, err := service.Copy(c.Request.Context(), principal, hostID, app.HostCopyCommand{
			Name: request.Name, ExpectedCurrentRevision: *request.ExpectedCurrentRevision,
		}, key)
		if err != nil {
			writeError(c, err)
			return
		}
		view, err := h.hostView(result.Host)
		if err != nil {
			writeError(c, err)
			return
		}
		if result.SourceHostID != hostID || !domain.ValidWorkspaceID(result.SourceWorkspaceID) ||
			!domain.ValidWorkspaceID(result.Workspace.ID) || result.Workspace.State != workspace.StateReady ||
			result.Workspace.Revision != 1 || result.SourceRevision != *request.ExpectedCurrentRevision || result.State != workspace.CopyCompleted {
			writeError(c, domain.NewError(domain.CodeInternal, "Host copy result is invalid"))
			return
		}
		response := hostCopyResponse{
			SourceHostID: result.SourceHostID, SourceWorkspaceID: result.SourceWorkspaceID, Host: view,
			Workspace: result.Workspace, SourceRevision: result.SourceRevision, State: result.State, Replayed: result.Replayed,
		}
		status := http.StatusCreated
		if result.Replayed {
			status = http.StatusOK
		}
		c.JSON(status, response)
	}
}
