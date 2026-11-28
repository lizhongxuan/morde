package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type AdminObservabilityLinkWriter interface {
	LinkAcceptedMessageTrace(context.Context, string, string, string, string) error
}

type AdminObservabilityDeps struct {
	Reader           app.AdminObservabilityReader
	Logs             app.AdminObservabilityLogReader
	LinkWriter       AdminObservabilityLinkWriter
	ExportHealth     AdminObservabilityExportHealthProvider
	LogHealth        AdminObservabilityLogHealthProvider
	AgentCheckpoints AdminAgentCheckpointHistoryReader
}

type AdminObservabilityExportHealthProvider interface {
	Health() app.AdminObservabilityExportHealth
}

type AdminObservabilityLogHealthProvider interface {
	Health() app.AdminObservabilityLogHealth
}

type AdminAgentCheckpointHistoryReader interface {
	ReadAdminPage(context.Context, string, string, string, int64) (app.AgentCheckpointAdminPage, error)
}

type adminObservabilityOverviewResponse struct {
	Sources struct {
		Spans     app.AdminObservabilitySource `json:"spans"`
		Logs      app.AdminObservabilitySource `json:"logs"`
		DSHEvents app.AdminObservabilitySource `json:"dsh_events"`
	} `json:"sources"`
	Spans24h struct {
		RequestCount  int64   `json:"request_count"`
		ErrorCount    int64   `json:"error_count"`
		P95DurationMS float64 `json:"p95_duration_ms"`
	} `json:"spans_24h"`
}

type adminObservabilityTracePageResponse struct {
	Items      []app.AdminObservabilityTraceSummary `json:"items"`
	NextCursor *string                              `json:"next_cursor"`
	Sources    struct {
		Spans app.AdminObservabilitySource `json:"spans"`
	} `json:"sources"`
}

type adminObservabilityTraceEventsResponse struct {
	TaskID        *string                              `json:"task_id"`
	AgentSessions []app.AdminObservabilityAgentSession `json:"agent_sessions"`
	Events        []adminObservabilityDSHEvent         `json:"events"`
	NextSequence  *int64                               `json:"next_sequence"`
	Coverage      app.AdminObservabilitySource         `json:"coverage"`
}

type adminObservabilityDSHEvent struct {
	SessionID string          `json:"session_id"`
	Sequence  int64           `json:"sequence"`
	Time      time.Time       `json:"time"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
}

var adminObservabilitySessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (h *api) registerAdminObservability(group *gin.RouterGroup) {
	group.GET("/admin/observability/overview", h.getAdminObservabilityOverview)
	group.GET("/admin/observability/traces", h.listAdminObservabilityTraces)
	group.GET("/admin/observability/traces/:trace_id", h.getAdminObservabilityTrace)
	group.GET("/admin/observability/traces/:trace_id/events", h.listAdminObservabilityTraceEvents)
	group.GET("/admin/observability/traces/:trace_id/logs", h.listAdminObservabilityTraceLogs)
}

func (h *api) getAdminObservabilityOverview(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if _, ok := requireAdmin(c); !ok {
		return
	}
	response := adminObservabilityOverviewResponse{}
	if h.deps.Observability.Reader == nil {
		response.Sources.Spans = unavailableObservabilitySource("span reader is not configured")
		response.Sources.Logs = unavailableObservabilitySource("correlated log reader is not configured")
		response.Sources.DSHEvents = unavailableObservabilitySource("checkpoint reader is not configured")
		c.JSON(http.StatusOK, response)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	value, err := h.deps.Observability.Reader.GetAdminObservabilityOverview(ctx)
	if err != nil {
		writeError(c, err)
		return
	}
	response.Sources.Spans = value.SpansSource
	response.Sources.Logs = value.LogsSource
	response.Sources.DSHEvents = value.EventsSource
	response.Spans24h.RequestCount = value.RequestCount
	response.Spans24h.ErrorCount = value.ErrorCount
	response.Spans24h.P95DurationMS = value.P95DurationMS
	if h.deps.Observability.ExportHealth != nil {
		response.Sources.Spans = mergeExporterHealth(response.Sources.Spans, h.deps.Observability.ExportHealth.Health())
	}
	response.Sources.Logs = mergeCorrelatedLogHealth(response.Sources.Logs, h.deps.Observability.LogHealth)
	c.JSON(http.StatusOK, response)
}

func (h *api) listAdminObservabilityTraces(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.deps.Observability.Reader == nil {
		response := adminObservabilityTracePageResponse{Items: []app.AdminObservabilityTraceSummary{}}
		response.Sources.Spans = unavailableObservabilitySource("span reader is not configured")
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
	query := app.AdminObservabilityTraceQuery{
		TraceID: c.Query("trace_id"), RequestID: c.Query("request_id"), TaskID: c.Query("task_id"),
		Query: c.Query("q"), Limit: limit, Cursor: c.Query("cursor"),
	}
	if len(query.RequestID) > 255 || len(query.TaskID) > 128 || len(query.Query) > 256 || len(query.Cursor) > 512 {
		writeError(c, domain.InvalidArgument("observability_query", "observability filter exceeds its limit"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	page, err := h.deps.Observability.Reader.ListAdminObservabilityTraces(ctx, query)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminObservabilityTracePageResponse{Items: page.Items, NextCursor: page.NextCursor}
	response.Sources.Spans = availableObservabilitySource()
	if h.deps.Observability.ExportHealth != nil {
		response.Sources.Spans = mergeExporterHealth(response.Sources.Spans, h.deps.Observability.ExportHealth.Health())
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminObservabilityTrace(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.Observability.Reader == nil {
		serviceUnavailable(c)
		return
	}
	traceID := c.Param("trace_id")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	detail, err := h.deps.Observability.Reader.GetAdminObservabilityTrace(ctx, traceID)
	if err != nil {
		writeError(c, err)
		return
	}
	detail.Source = mergeExporterHealth(detail.Source, h.exportHealth())
	if !h.writeAdminObservabilityAudit(c, principal, audit.ActionObservabilityTraceRead, traceID,
		map[string]any{"trace_id": traceID, "span_count": len(detail.Spans)}) {
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (h *api) listAdminObservabilityTraceEvents(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	response := adminObservabilityTraceEventsResponse{AgentSessions: []app.AdminObservabilityAgentSession{}, Events: []adminObservabilityDSHEvent{}}
	if h.deps.Observability.Reader == nil {
		response.Coverage = unavailableObservabilitySource("span reader is not configured")
		c.JSON(http.StatusOK, response)
		return
	}
	traceID := c.Param("trace_id")
	fromSequence := int64(0)
	if raw := c.Query("from_sequence"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(c, domain.InvalidArgument("from_sequence", "from_sequence must be a non-negative integer"))
			return
		}
		fromSequence = parsed
	}
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(c, domain.InvalidArgument("limit", "limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	sessionID := c.Query("agent_session_id")
	if sessionID != "" && !adminObservabilitySessionIDPattern.MatchString(sessionID) {
		writeError(c, domain.InvalidArgument("agent_session_id", "Agent session ID is invalid"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	sessions, err := h.deps.Observability.Reader.GetAdminObservabilityTraceSessions(ctx, traceID)
	if err != nil {
		writeError(c, err)
		return
	}
	response.TaskID = sessions.TaskID
	response.AgentSessions = sessions.Sessions
	response.Coverage = sessions.Source
	if sessionID == "" {
		for _, session := range sessions.Sessions {
			if session.Role == "root" {
				sessionID = session.SessionID
				break
			}
		}
	}
	if sessionID != "" {
		found := false
		for _, session := range sessions.Sessions {
			if session.SessionID == sessionID {
				found = true
				break
			}
		}
		if !found {
			writeError(c, domain.NewError(domain.CodeNotFound, "Agent session is not linked to this trace"))
			return
		}
		if sessions.TaskID == nil || sessions.OwnerID == nil {
			writeError(c, domain.NewError(domain.CodeNotFound, "trace Task is unavailable"))
			return
		}
		if h.deps.Observability.AgentCheckpoints == nil {
			response.Coverage = unavailableObservabilitySource("DSH checkpoint reader is not configured")
		} else {
			page, pageErr := h.deps.Observability.AgentCheckpoints.ReadAdminPage(ctx, *sessions.TaskID, *sessions.OwnerID, sessionID, fromSequence)
			if pageErr != nil {
				response.Coverage = app.AdminObservabilitySource{Status: "partial", Reason: observabilityStringPointer("checkpoint page could not be read")}
			} else if page.Coverage == app.AgentCheckpointCoverageEmpty {
				response.Coverage = app.AdminObservabilitySource{Status: "partial", Reason: observabilityStringPointer("尚无已提交的 DSH checkpoint")}
			} else if len(page.Payload) == 0 {
				response.Coverage = checkpointPageCoverage(page.Coverage, page.Reason, sessions.Source)
				if response.Coverage.Status == "available" {
					response.Coverage.Status = "partial"
					response.Coverage.Reason = observabilityStringPointer("当前 checkpoint 未返回可展示事件")
				}
			} else {
				events, parseErr := parseAdminDSHCheckpointPage(sessionID, page.Payload, fromSequence, limit)
				if parseErr != nil {
					response.Coverage = app.AdminObservabilitySource{Status: "partial", Reason: observabilityStringPointer("checkpoint event data did not match its validated shape")}
				} else {
					response.Events = events
					response.Coverage = checkpointPageCoverage(page.Coverage, page.Reason, sessions.Source)
					if len(events) > 0 && (len(events) == limit || page.NextFrom <= page.Watermark || page.Coverage == app.AgentCheckpointCoveragePartial) {
						next := events[len(events)-1].Sequence + 1
						response.NextSequence = &next
					} else if len(events) == 0 && page.NextFrom <= page.Watermark {
						next := page.NextFrom
						response.NextSequence = &next
					}
				}
			}
		}
	}
	if len(sessions.Sessions) > 1 && c.Query("agent_session_id") == "" && response.Coverage.Status == "available" {
		response.Coverage.Status = "partial"
		response.Coverage.Reason = observabilityStringPointer("默认显示主 Agent；选择子 Agent 会话查看其独立事件序列")
	}
	if !h.writeAdminObservabilityAudit(c, principal, audit.ActionObservabilityAgentEventsRead, traceID,
		map[string]any{"trace_id": traceID, "task_id": valueOrEmpty(sessions.TaskID), "agent_session_id": sessionID,
			"from_sequence": fromSequence, "event_count": len(response.Events)}) {
		return
	}
	c.JSON(http.StatusOK, response)
}

func parseAdminDSHCheckpointPage(sessionID string, payload json.RawMessage, from int64, limit int) ([]adminObservabilityDSHEvent, error) {
	if len(payload) == 0 || len(payload) > 1<<20 {
		return nil, fmt.Errorf("checkpoint page size is invalid")
	}
	var body struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	events := make([]adminObservabilityDSHEvent, 0, min(limit, len(body.Events)))
	var previous int64 = -1
	for _, raw := range body.Events {
		var event struct {
			Sequence int64           `json:"seq"`
			TimeMS   int64           `json:"time"`
			Type     string          `json:"type"`
			Data     json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &event); err != nil || event.Sequence < 0 || event.Sequence <= previous || event.Type == "" ||
			len(event.Type) > 128 || event.TimeMS <= 0 || len(event.Data) == 0 {
			return nil, fmt.Errorf("checkpoint event shape is invalid")
		}
		previous = event.Sequence
		if event.Sequence < from || len(events) >= limit {
			continue
		}
		clean, err := redactAdminDiagnosticJSON(event.Data)
		if err != nil {
			return nil, err
		}
		events = append(events, adminObservabilityDSHEvent{SessionID: sessionID, Sequence: event.Sequence,
			Time: time.UnixMilli(event.TimeMS).UTC(), Type: event.Type, Data: clean})
	}
	return events, nil
}

func redactAdminDiagnosticJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	redacted := redactAdminDiagnosticValue("", value)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func redactAdminDiagnosticValue(key string, value any) any {
	if adminSensitiveDiagnosticKey(key) {
		return "[已隐藏凭据]"
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for childKey, child := range typed {
			result[childKey] = redactAdminDiagnosticValue(childKey, child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactAdminDiagnosticValue("", child)
		}
		return result
	case string:
		return app.SanitizeAdminAgentDiagnosticText(typed)
	default:
		return value
	}
}

func adminSensitiveDiagnosticKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
	switch normalized {
	case "inputtokens", "outputtokens", "prompttokens", "completiontokens", "reasoningtokens", "cachedinputtokens", "totaltokens":
		return false
	}
	for _, marker := range []string{"password", "passwd", "passphrase", "secret", "token", "apikey", "authorization", "credential", "privatekey", "accesskey", "refreshkey"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func (h *api) exportHealth() app.AdminObservabilityExportHealth {
	if h.deps.Observability.ExportHealth == nil {
		return app.AdminObservabilityExportHealth{Status: "unavailable", ReasonCode: "exporter_unconfigured"}
	}
	return h.deps.Observability.ExportHealth.Health()
}

func mergeExporterHealth(source app.AdminObservabilitySource, health app.AdminObservabilityExportHealth) app.AdminObservabilitySource {
	if health.Status == "healthy" {
		if source.Status == "disabled" {
			source.Status = "available"
		}
		return source
	}
	reason := "span exporter health is unavailable"
	switch health.ReasonCode {
	case "export_failed", "span_export_failed":
		reason = "最近一次 span 批次导出失败，历史记录仍可检索"
	case "export_timeout":
		reason = "span 批次导出超时，部分记录可能延迟或缺失"
	case "export_cancelled":
		reason = "span 批次导出已取消，部分记录可能缺失"
	case "invalid_span_name":
		reason = "span 名称不符合允许格式，所在批次未写入；可查看 Portal 诊断日志"
	case "invalid_span_identity":
		reason = "span 标识或时间无效，所在批次未写入；可查看 Portal 诊断日志"
	case "queue_full":
		reason = "span 队列发生溢出，部分记录可能缺失"
	case "exporter_shutdown":
		reason = "span exporter 正在关闭"
	case "exporter_unconfigured":
		reason = "span exporter 未配置"
	case "database_unavailable":
		reason = "span 存储当前不可用"
	}
	if source.Status != "unavailable" && source.Status != "disabled" {
		source.Status = "partial"
	} else if health.Status == "unavailable" {
		source.Status = "unavailable"
	}
	source.Reason = &reason
	if health.LastSuccessAt != nil && (source.UpdatedAt == nil || health.LastSuccessAt.After(*source.UpdatedAt)) {
		value := health.LastSuccessAt.UTC()
		source.UpdatedAt = &value
	}
	return source
}

func unavailableObservabilitySource(reason string) app.AdminObservabilitySource {
	return app.AdminObservabilitySource{Status: "unavailable", Reason: &reason}
}

func availableObservabilitySource() app.AdminObservabilitySource {
	return app.AdminObservabilitySource{Status: "available"}
}

func observabilityStringPointer(value string) *string { return &value }

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func checkpointPageCoverage(status, reason string, fallback app.AdminObservabilitySource) app.AdminObservabilitySource {
	switch status {
	case app.AgentCheckpointCoverageComplete:
		status = "available"
	case app.AgentCheckpointCoverageEmpty:
		status = "partial"
		if reason == "" {
			reason = "尚无已提交的 DSH checkpoint"
		}
	case app.AgentCheckpointCoveragePartial:
		status = "partial"
	}
	if status != "available" && status != "partial" && status != "unavailable" && status != "disabled" {
		status = "partial"
	}
	source := app.AdminObservabilitySource{Status: status}
	if reason != "" {
		source.Reason = &reason
	} else if status == "available" && fallback.Status == "partial" {
		source = fallback
	}
	return source
}

func (h *api) writeAdminObservabilityAudit(c *gin.Context, principal identity.Principal, action audit.Action, target string, detail map[string]any) bool {
	if h.deps.AdminAudit == nil {
		serviceUnavailable(c)
		return false
	}
	if err := h.deps.AdminAudit.Write(c.Request.Context(), audit.Entry{ActorID: principal.UserID, ActorKind: audit.ActorSession,
		Action: action, Target: target, Detail: detail, IP: c.ClientIP()}); err != nil {
		writeError(c, err)
		return false
	}
	return true
}
