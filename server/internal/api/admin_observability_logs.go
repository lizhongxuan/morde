package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
)

func (h *api) listAdminObservabilityTraceLogs(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	response := app.AdminObservabilityLogPage{Items: []app.AdminObservabilityLog{}}
	if h.deps.Observability.Logs == nil {
		response.Coverage = unavailableObservabilitySource("correlated log reader is not configured")
		c.JSON(http.StatusOK, response)
		return
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(c, domain.InvalidArgument("limit", "limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	query := app.AdminObservabilityLogQuery{TraceID: c.Param("trace_id"), RequestID: c.Query("request_id"),
		TaskID: c.Query("task_id"), Limit: limit, Cursor: c.Query("cursor")}
	if len(query.RequestID) > 255 || len(query.TaskID) > 128 || len(query.Cursor) > 512 {
		writeError(c, domain.InvalidArgument("observability_log_query", "observability log filter exceeds its limit"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	page, err := h.deps.Observability.Logs.ListAdminObservabilityLogs(ctx, query)
	if err != nil {
		page = app.AdminObservabilityLogPage{Items: []app.AdminObservabilityLog{},
			Coverage: unavailableObservabilitySource("correlated logs could not be queried")}
	}
	page.Coverage = mergeCorrelatedLogHealth(page.Coverage, h.deps.Observability.LogHealth)
	if !h.writeAdminObservabilityAudit(c, principal, audit.ActionObservabilityTraceRead, query.TraceID,
		map[string]any{"trace_id": query.TraceID, "request_id": query.RequestID, "task_id": query.TaskID,
			"log_count": len(page.Items), "cursor_present": query.Cursor != ""}) {
		return
	}
	c.JSON(http.StatusOK, page)
}

func mergeCorrelatedLogHealth(source app.AdminObservabilitySource, provider AdminObservabilityLogHealthProvider) app.AdminObservabilitySource {
	if provider == nil {
		return source
	}
	health := provider.Health()
	if health.Status == "unavailable" {
		return app.AdminObservabilitySource{Status: "unavailable", Reason: observabilityStringPointer("correlated log writer is unavailable"), UpdatedAt: health.LastSuccessAt}
	}
	if source.Status == "unavailable" {
		return source
	}
	if health.Status == "partial" {
		reason := "部分关联日志可能缺失"
		switch health.ReasonCode {
		case "log_queue_dropped":
			reason = "部分日志因有界队列达到上限而未入库"
		case "log_write_failed":
			reason = "部分日志批次写入失败"
		}
		if source.Reason != nil && *source.Reason != "" {
			reason = *source.Reason + "；" + reason
		}
		return app.AdminObservabilitySource{Status: "partial", Reason: &reason, UpdatedAt: health.LastSuccessAt}
	}
	return source
}
