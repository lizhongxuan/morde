package api

import (
	"context"
	"net/http"
	"time"

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
	OwnerID          string            `json:"owner_id"`
	State            execution.State   `json:"state"`
	Epoch            int64             `json:"epoch"`
	PendingTaskCount int64             `json:"pending_task_count"`
	Runtime          execution.Runtime `json:"runtime"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type executionControlMutationRequest struct {
	ExpectedEpoch *int64 `json:"expected_epoch"`
}

func (h *api) registerExecutionControl(group *gin.RouterGroup, service ExecutionControlService) {
	if service == nil {
		return
	}
	group.GET("/execution", h.getExecutionControl(service))
	group.POST("/execution/stop-all", h.mutateExecutionControl(service, "stop_all"))
	group.POST("/execution/enable", h.mutateExecutionControl(service, "enable"))
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
		response, err := toExecutionControlResponse(value, principal.UserID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) mutateExecutionControl(service ExecutionControlService, action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request executionControlMutationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedEpoch == nil || *request.ExpectedEpoch < 1 {
			writeError(c, domain.InvalidArgument("expected_epoch", "a positive expected epoch is required"))
			return
		}
		idempotencyKey, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		var value execution.Control
		var err error
		if action == "stop_all" {
			value, err = service.StopAll(c.Request.Context(), principal, *request.ExpectedEpoch, idempotencyKey)
		} else {
			value, err = service.Enable(c.Request.Context(), principal, *request.ExpectedEpoch, idempotencyKey)
		}
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toExecutionControlResponse(value, principal.UserID)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusOK
		if action == "stop_all" && value.State == execution.StateStopping {
			status = http.StatusAccepted
		}
		c.JSON(status, response)
	}
}

func toExecutionControlResponse(value execution.Control, ownerID string) (executionControlResponse, error) {
	if !domain.ValidUserID(ownerID) || value.OwnerID != ownerID || !value.State.Valid() || value.Epoch < 1 ||
		value.PendingTaskCount < 0 || !value.Runtime.Valid() || value.UpdatedAt.IsZero() {
		return executionControlResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return executionControlResponse{
		OwnerID: value.OwnerID, State: value.State, Epoch: value.Epoch,
		PendingTaskCount: value.PendingTaskCount, Runtime: value.Runtime, UpdatedAt: value.UpdatedAt,
	}, nil
}
