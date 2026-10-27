package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type updateAdminHostResourcesRequest struct {
	CPUMilli        int   `json:"cpu_milli"`
	MemMiB          int   `json:"mem_mib"`
	ExpectedVersion int64 `json:"expected_version"`
}

type adminHostResourcesResponse struct {
	HostID      string            `json:"host_id"`
	State       string            `json:"state"`
	Resources   HostResourcesView `json:"resources"`
	Version     int64             `json:"version"`
	UpdateState string            `json:"update_state"`
}

func (h *api) getAdminHostResources(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	if h.deps.AdminHostResources == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.AdminHostResources.Get(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostResourcesView(value))
}

func (h *api) updateAdminHostResources(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	if h.deps.AdminHostResources == nil {
		serviceUnavailable(c)
		return
	}
	var request updateAdminHostResourcesRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	value, err := h.deps.AdminHostResources.Update(c.Request.Context(), principal, id,
		app.HostResources{CPUMilli: request.CPUMilli, MemMiB: request.MemMiB}, request.ExpectedVersion)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, hostResourcesView(value))
}

func hostResourcesView(value app.HostResourceSnapshot) adminHostResourcesResponse {
	return adminHostResourcesResponse{HostID: value.HostID, State: string(value.State),
		Resources: HostResourcesView{CPUMilli: value.Resources.CPUMilli, MemMiB: value.Resources.MemMiB}, Version: value.Version, UpdateState: value.UpdateState}
}
