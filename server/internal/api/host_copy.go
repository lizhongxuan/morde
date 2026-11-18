package api

import (
	"context"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type HostCopyEndpointService interface {
	Copy(context.Context, identity.Principal, string, app.HostCopyCommand, string) error
}

type hostCopyRequest struct {
	Name                    string `json:"name"`
	ExpectedCurrentRevision *int64 `json:"expected_current_revision"`
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
		err := service.Copy(c.Request.Context(), principal, hostID, app.HostCopyCommand{
			Name: request.Name, ExpectedCurrentRevision: *request.ExpectedCurrentRevision,
		}, key)
		if err == nil {
			err = domain.NewError(domain.CodeWorkspaceConflict, "independent Host copies are unavailable because cloud computers share the owner's Agent Workspace")
		}
		writeError(c, err)
	}
}
