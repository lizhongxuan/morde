package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
)

type recoverAdminHostRequest struct {
	RuntimeGeneration int64 `json:"runtime_generation"`
	ConfirmRetry      bool  `json:"confirm_retry"`
}

type adminHostRecoveryResponse struct {
	HostID            string `json:"host_id"`
	RuntimeGeneration int64  `json:"runtime_generation"`
	Action            string `json:"action"`
	SandboxCount      int    `json:"sandbox_count"`
}

func (h *api) recoverAdminHost(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	if h.deps.AdminHostRecovery == nil {
		serviceUnavailable(c)
		return
	}
	var request recoverAdminHostRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.RuntimeGeneration <= 0 {
		writeError(c, domain.InvalidArgument("runtime_generation", "runtime_generation must be positive"))
		return
	}
	result, err := h.deps.AdminHostRecovery.RecoverUnknownRestore(c.Request.Context(), principal, id,
		request.RuntimeGeneration, request.ConfirmRetry)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, adminHostRecoveryResponse{
		HostID: result.HostID, RuntimeGeneration: result.RuntimeGeneration,
		Action: result.Action, SandboxCount: result.SandboxCount,
	})
}
