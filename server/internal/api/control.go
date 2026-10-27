package api

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

// ControlService is the user-facing Host handoff workflow.
type ControlService interface {
	Status(context.Context, identity.Principal, string) (app.ControlStatus, error)
	Act(context.Context, identity.Principal, string, app.ControlActionCommand) (app.ControlActionResult, error)
}

type controlStatusResponse struct {
	HostID           string                     `json:"host_id"`
	ControlState     host.ControlState          `json:"control_state"`
	ControlVersion   int64                      `json:"control_version"`
	ActiveTransition *controlTransitionResponse `json:"active_transition"`
}

type controlActionResponse struct {
	HostID              string                     `json:"host_id"`
	ControlState        host.ControlState          `json:"control_state"`
	ControlVersion      int64                      `json:"control_version"`
	ActiveTransition    *controlTransitionResponse `json:"active_transition"`
	PendingOperationIDs []string                   `json:"pending_operation_ids"`
}

// The public transition intentionally omits task snapshots and runtime receipts.
type controlTransitionResponse struct {
	RequestID              string         `json:"request_id"`
	Action                 control.Action `json:"action"`
	Phase                  control.Phase  `json:"phase"`
	FailureReason          string         `json:"failure_reason"`
	ExpectedControlVersion int64          `json:"expected_control_version"`
	BlockedControlVersion  int64          `json:"blocked_control_version"`
	TargetControlVersion   int64          `json:"target_control_version"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

type controlActionRequest struct {
	Action                 control.Action `json:"action"`
	RequestID              string         `json:"request_id"`
	ExpectedControlVersion *int64         `json:"expected_control_version"`
}

func (h *api) registerControl(group *gin.RouterGroup, service ControlService) {
	if service == nil {
		return
	}
	group.GET("/hosts/:id/control", h.getControl(service))
	group.POST("/hosts/:id/control/actions", h.actOnControl(service))
}

func (h *api) getControl(service ControlService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		hostID, ok := safeID(c, "id", domain.ValidHostID)
		if !ok {
			return
		}
		status, err := service.Status(c.Request.Context(), principal, hostID)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toControlStatusResponse(status)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) actOnControl(service ControlService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		hostID, ok := safeID(c, "id", domain.ValidHostID)
		if !ok {
			return
		}
		var request controlActionRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.Action != control.ActionTakeOver && request.Action != control.ActionRelease {
			writeError(c, domain.InvalidArgument("action", "control action is invalid"))
			return
		}
		if !control.ValidRequestID(request.RequestID) {
			writeError(c, domain.InvalidArgument("request_id", "request id is invalid"))
			return
		}
		if request.ExpectedControlVersion == nil || *request.ExpectedControlVersion < 0 || *request.ExpectedControlVersion > math.MaxInt64-2 {
			writeError(c, domain.InvalidArgument("expected_control_version", "control version is out of range"))
			return
		}

		result, err := service.Act(c.Request.Context(), principal, hostID, app.ControlActionCommand{
			Action: request.Action, RequestID: request.RequestID, ExpectedControlVersion: *request.ExpectedControlVersion,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toControlActionResponse(result)
		if err != nil {
			writeError(c, err)
			return
		}
		httpStatus := http.StatusOK
		if len(response.PendingOperationIDs) != 0 {
			httpStatus = http.StatusAccepted
		}
		c.JSON(httpStatus, response)
	}
}

func toControlStatusResponse(value app.ControlStatus) (controlStatusResponse, error) {
	if !domain.ValidHostID(value.HostID) || !validControlState(value.ControlState) || value.ControlVersion < 0 {
		return controlStatusResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	var transition *controlTransitionResponse
	if value.ActiveTransition != nil {
		converted, err := toControlTransitionResponse(*value.ActiveTransition, value.HostID)
		if err != nil {
			return controlStatusResponse{}, err
		}
		transition = &converted
	}
	return controlStatusResponse{
		HostID: value.HostID, ControlState: value.ControlState,
		ControlVersion: value.ControlVersion, ActiveTransition: transition,
	}, nil
}

func toControlActionResponse(value app.ControlActionResult) (controlActionResponse, error) {
	status, err := toControlStatusResponse(value.Status)
	if err != nil {
		return controlActionResponse{}, err
	}
	pending := make([]string, 0, len(value.PendingOperationIDs))
	seen := make(map[string]struct{}, len(value.PendingOperationIDs))
	for _, operationID := range value.PendingOperationIDs {
		if !domain.ValidOperationID(operationID) {
			return controlActionResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if _, ok := seen[operationID]; ok {
			return controlActionResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		seen[operationID] = struct{}{}
		pending = append(pending, operationID)
	}
	return controlActionResponse{
		HostID: status.HostID, ControlState: status.ControlState, ControlVersion: status.ControlVersion,
		ActiveTransition: status.ActiveTransition, PendingOperationIDs: pending,
	}, nil
}

func toControlTransitionResponse(value control.Transition, hostID string) (controlTransitionResponse, error) {
	if value.HostID != hostID || !control.ValidRequestID(value.RequestID) ||
		(value.Action != control.ActionTakeOver && value.Action != control.ActionRelease) || !control.ValidPhase(value.Phase) ||
		value.ExpectedControlVersion < 0 || value.ExpectedControlVersion > math.MaxInt64-2 ||
		value.BlockedControlVersion != value.ExpectedControlVersion+1 || value.TargetControlVersion != value.ExpectedControlVersion+2 ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return controlTransitionResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if !validPublicControlFailureReason(value.FailureReason) {
		return controlTransitionResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return controlTransitionResponse{
		RequestID: value.RequestID, Action: value.Action, Phase: value.Phase, FailureReason: value.FailureReason,
		ExpectedControlVersion: value.ExpectedControlVersion, BlockedControlVersion: value.BlockedControlVersion,
		TargetControlVersion: value.TargetControlVersion, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}, nil
}

func validPublicControlFailureReason(reason string) bool {
	switch reason {
	case "", "audit_unavailable", "task_snapshot_failed", "runtime_unavailable", "transition_persistence_failed",
		"operations_pending", "runtime_fence_mismatch", "runtime_quiesce_failed", "receipt_invalid",
		"runtime_control_failed", "invalid_action":
		return true
	default:
		return false
	}
}
