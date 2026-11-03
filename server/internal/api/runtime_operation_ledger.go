package api

import (
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

const runtimeOperationLedgerBodyLimit = (8 << 20) + (64 << 10)

// registerRuntimeOperationLedgerRoutes must be called with the Host Replica
// authentication group; these endpoints are not valid as public API routes.
func (h *api) registerRuntimeOperationLedgerRoutes(group *gin.RouterGroup) {
	group.GET("/operation-ledger/:namespace", h.listRuntimeOperationLedger)
	group.POST("/operation-ledger/:namespace", h.appendRuntimeOperationLedger)
}

func (h *api) listRuntimeOperationLedger(c *gin.Context) {
	service := h.deps.RuntimeOperationLedger
	if service == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	after, err := uniqueUintQuery(c, "after_sequence", 0)
	if err != nil {
		writeError(c, domain.InvalidArgument("after_sequence", "operation ledger cursor is invalid"))
		return
	}
	limit, err := uniqueInt32Query(c, "limit", app.RuntimeOperationLedgerPageLimit)
	if err != nil || limit < 1 || limit > app.RuntimeOperationLedgerPageLimit {
		writeError(c, domain.InvalidArgument("limit", "operation ledger page size is invalid"))
		return
	}
	page, err := service.ListRuntimeOperationEvents(c.Request.Context(), principal, c.Param("namespace"), after, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h *api) appendRuntimeOperationLedger(c *gin.Context) {
	service := h.deps.RuntimeOperationLedger
	if service == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(c, domain.InvalidArgument("body", "content type must be application/json"))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, runtimeOperationLedgerBodyLimit)
	var event app.RuntimeOperationLedgerEvent
	if !decodeStrictJSON(c, &event) {
		return
	}
	acknowledged, err := service.AppendRuntimeOperationEvent(c.Request.Context(), principal, c.Param("namespace"), event)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, acknowledged)
}

func uniqueUintQuery(c *gin.Context, key string, fallback uint64) (uint64, error) {
	values, exists := c.Request.URL.Query()[key]
	if !exists {
		return fallback, nil
	}
	if len(values) != 1 {
		return 0, errors.New("query must appear once")
	}
	value, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func uniqueInt32Query(c *gin.Context, key string, fallback int32) (int32, error) {
	value, err := uniqueUintQuery(c, key, uint64(fallback))
	if err != nil || value > uint64(^uint32(0)>>1) {
		return 0, errors.New("query is outside the supported range")
	}
	return int32(value), nil
}
