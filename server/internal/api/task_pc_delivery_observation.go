package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type TaskPCDeliveryObservationService interface {
	ObservePCDelivery(context.Context, identity.Principal, string, string, int64) error
}

type taskPCDeliveryObservationRequest struct {
	MessageID    string `json:"message_id"`
	TaskRevision int64  `json:"task_revision"`
}

func (h *api) registerTaskPCDeliveryObservations(group *gin.RouterGroup, service TaskPCDeliveryObservationService) {
	if service != nil {
		group.POST("/tasks/:id/pc-delivery-observations", h.observePCDelivery(service))
	}
}

func (h *api) observePCDelivery(service TaskPCDeliveryObservationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" {
			writeError(c, domain.InvalidArgument("query", "unexpected query parameters"))
			return
		}
		principal, ok := principalFrom(c)
		if !ok || principal.Kind != identity.KindSession || principal.Role != identity.RoleUser ||
			!domain.ValidClientInstallationID(principal.ClientInstallationID) {
			unauthenticated(c)
			return
		}
		taskID, ok := safeID(c, "id", domain.ValidTaskID)
		if !ok {
			return
		}
		var request taskPCDeliveryObservationRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		if !domain.ValidMessageID(request.MessageID) || request.TaskRevision < 0 {
			writeError(c, domain.InvalidArgument("delivery_observation", "message and task revision are required"))
			return
		}
		if err := service.ObservePCDelivery(c.Request.Context(), principal, taskID, request.MessageID, request.TaskRevision); err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Status(http.StatusNoContent)
	}
}
