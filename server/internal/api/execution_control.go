package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
)

type ExecutionControlService interface {
	Status(context.Context, identity.Principal) (execution.Control, error)
	StopAll(context.Context, identity.Principal, int64, string) (execution.Control, error)
	Enable(context.Context, identity.Principal, int64, string) (execution.Control, error)
}

type executionControlResponse struct {
	State            execution.State `json:"state"`
	Epoch            int64           `json:"epoch"`
	PendingTaskCount int64           `json:"pending_task_count"`
}

type executionControlActionRequest struct {
	ExpectedEpoch int64 `json:"expected_epoch"`
}

func (h *api) registerExecutionControl(group *gin.RouterGroup, service ExecutionControlService) {
	if service == nil {
		return
	}
	group.GET("/execution", h.getExecutionControl(service))
	group.POST("/execution/stop-all", h.stopAllExecution(service))
	group.POST("/execution/enable", h.enableExecution(service))
}

func (h *api) getExecutionControl(service ExecutionControlService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		value, err := service.Status(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, executionControlView(value))
	}
}

func (h *api) stopAllExecution(service ExecutionControlService) gin.HandlerFunc {
	return h.executionControlAction(service, "stop_all")
}

func (h *api) enableExecution(service ExecutionControlService) gin.HandlerFunc {
	return h.executionControlAction(service, "enable")
}

func (h *api) executionControlAction(service ExecutionControlService, action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request executionControlActionRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		if request.ExpectedEpoch < 1 {
			writeError(c, domain.InvalidArgument("expected_epoch", "expected_epoch must be a positive integer"))
			return
		}
		var value execution.Control
		var err error
		switch action {
		case "stop_all":
			value, err = service.StopAll(c.Request.Context(), principal, request.ExpectedEpoch, c.GetHeader("Idempotency-Key"))
		case "enable":
			value, err = service.Enable(c.Request.Context(), principal, request.ExpectedEpoch, c.GetHeader("Idempotency-Key"))
		}
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, executionControlView(value))
	}
}

func executionControlView(value execution.Control) executionControlResponse {
	return executionControlResponse{State: value.State, Epoch: value.Epoch, PendingTaskCount: value.PendingTaskCount}
}
