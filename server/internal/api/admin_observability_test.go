package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type adminObservabilityReaderStub struct {
	overview app.AdminObservabilityOverview
	sessions app.AdminObservabilityTraceSessions
	calls    int
}

func (s *adminObservabilityReaderStub) GetAdminObservabilityOverview(context.Context) (app.AdminObservabilityOverview, error) {
	s.calls++
	return s.overview, nil
}

func (*adminObservabilityReaderStub) ListAdminObservabilityTraces(context.Context, app.AdminObservabilityTraceQuery) (app.AdminObservabilityTracePage, error) {
	return app.AdminObservabilityTracePage{Items: []app.AdminObservabilityTraceSummary{}}, nil
}

func (*adminObservabilityReaderStub) GetAdminObservabilityTrace(context.Context, string) (app.AdminObservabilityTraceDetail, error) {
	return app.AdminObservabilityTraceDetail{}, nil
}

func (s *adminObservabilityReaderStub) GetAdminObservabilityTraceSessions(context.Context, string) (app.AdminObservabilityTraceSessions, error) {
	s.calls++
	return s.sessions, nil
}

type adminObservabilityCheckpointStub struct {
	page      app.AgentCheckpointAdminPage
	taskID    string
	ownerID   string
	sessionID string
	from      int64
}

func (s *adminObservabilityCheckpointStub) ReadAdminPage(_ context.Context, taskID, ownerID, sessionID string, from int64) (app.AgentCheckpointAdminPage, error) {
	s.taskID, s.ownerID, s.sessionID, s.from = taskID, ownerID, sessionID, from
	return s.page, nil
}

func adminObservabilityRouter(t *testing.T, role identity.Role, reader *adminObservabilityReaderStub,
	checkpoints AdminAgentCheckpointHistoryReader, audit *adminAuditWriterStub) *gin.Engine {
	t.Helper()
	services := newServices()
	services.verifier.role = role
	deps := services.deps()
	deps.Observability = AdminObservabilityDeps{Reader: reader, AgentCheckpoints: checkpoints}
	deps.AdminAudit = audit
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestAdminObservabilityOverviewIsAdminOnlyAndKeepsSourcesIndependent(t *testing.T) {
	reader := &adminObservabilityReaderStub{overview: app.AdminObservabilityOverview{
		SpansSource:  app.AdminObservabilitySource{Status: "unavailable", Reason: stringPointer("span store failed")},
		EventsSource: app.AdminObservabilitySource{Status: "available"},
	}}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	user := adminObservabilityRouter(t, identity.RoleUser, reader, nil, &adminAuditWriterStub{})
	denied := requestWithCookie(user, http.MethodGet, "/api/admin/observability/overview", cookie, "")
	if denied.Code != http.StatusForbidden || reader.calls != 0 {
		t.Fatalf("non-admin overview = %d %s calls=%d", denied.Code, denied.Body, reader.calls)
	}

	admin := adminObservabilityRouter(t, identity.RoleAdmin, reader, nil, &adminAuditWriterStub{})
	response := requestWithCookie(admin, http.MethodGet, "/api/admin/observability/overview", cookie, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || reader.calls != 1 {
		t.Fatalf("admin overview = %d %s cache=%q calls=%d", response.Code, response.Body,
			response.Header().Get("Cache-Control"), reader.calls)
	}
	var body adminObservabilityOverviewResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Sources.Spans.Status != "unavailable" || body.Sources.DSHEvents.Status != "available" {
		t.Fatalf("overview sources were not independent: %+v", body.Sources)
	}
}

func TestAdminObservabilityEventsReadCommittedPageAndRedactCredentials(t *testing.T) {
	traceID := "0123456789abcdef0123456789abcdef"
	ownerID, taskID, sessionID := "u1234567", "q1234567", "root_session"
	reader := &adminObservabilityReaderStub{sessions: app.AdminObservabilityTraceSessions{
		TaskID: &taskID, OwnerID: &ownerID, Sessions: []app.AdminObservabilityAgentSession{{SessionID: sessionID, Role: "root"}},
		Source: app.AdminObservabilitySource{Status: "available"},
	}}
	checkpoint := &adminObservabilityCheckpointStub{page: app.AgentCheckpointAdminPage{
		Coverage: app.AgentCheckpointCoverageComplete, Watermark: 3, NextFrom: 2,
		Payload: json.RawMessage(`{"events":[{"seq":0,"time":1795726800000,"type":"tool_call","data":{"token":"secret-value","inputTokens":12,"message":"safe"}},{"seq":1,"time":1795726801000,"type":"tool_result","data":{"ok":true}}]}`),
	}}
	audit := &adminAuditWriterStub{}
	admin := adminObservabilityRouter(t, identity.RoleAdmin, reader, checkpoint, audit)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	path := "/api/admin/observability/traces/" + traceID + "/events?agent_session_id=" + url.QueryEscape(sessionID) + "&limit=1"
	response := requestWithCookie(admin, http.MethodGet, path, cookie, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		checkpoint.taskID != taskID || checkpoint.ownerID != ownerID || checkpoint.sessionID != sessionID || checkpoint.from != 0 || len(audit.entries) != 1 {
		t.Fatalf("admin DSH event page = %d %s cache=%q checkpoint=%+v audits=%d", response.Code, response.Body,
			response.Header().Get("Cache-Control"), checkpoint, len(audit.entries))
	}
	var body adminObservabilityTraceEventsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].Sequence != 0 || body.Coverage.Status != "available" ||
		body.NextSequence == nil || *body.NextSequence != 1 {
		t.Fatalf("event page did not preserve event coverage/pagination: %+v", body)
	}
	var data map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body.Events[0].Data)))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data["token"] != "[已隐藏凭据]" || data["inputTokens"].(json.Number).String() != "12" || data["message"] != "safe" {
		t.Fatalf("event data was not safely projected: %s", body.Events[0].Data)
	}
}

func TestCheckpointPageCoverageMapsEmptyAndCompleteStates(t *testing.T) {
	complete := checkpointPageCoverage(app.AgentCheckpointCoverageComplete, "", app.AdminObservabilitySource{})
	if complete.Status != "available" {
		t.Fatalf("complete checkpoint was mapped to %q", complete.Status)
	}
	empty := checkpointPageCoverage(app.AgentCheckpointCoverageEmpty, "", app.AdminObservabilitySource{})
	if empty.Status != "partial" || empty.Reason == nil {
		t.Fatalf("empty checkpoint did not explain the gap: %+v", empty)
	}
}

func TestMergeExporterHealthExplainsSpanExportFailures(t *testing.T) {
	for _, testCase := range []struct {
		reasonCode string
		wantReason string
	}{
		{reasonCode: "span_export_failed", wantReason: "最近一次 span 批次导出失败，历史记录仍可检索"},
		{reasonCode: "export_timeout", wantReason: "span 批次导出超时，部分记录可能延迟或缺失"},
		{reasonCode: "export_cancelled", wantReason: "span 批次导出已取消，部分记录可能缺失"},
		{reasonCode: "invalid_span_name", wantReason: "span 名称不符合允许格式，所在批次未写入；可查看 Portal 诊断日志"},
		{reasonCode: "invalid_span_identity", wantReason: "span 标识或时间无效，所在批次未写入；可查看 Portal 诊断日志"},
	} {
		source := mergeExporterHealth(app.AdminObservabilitySource{Status: "available"}, app.AdminObservabilityExportHealth{
			Status: "unavailable", ReasonCode: testCase.reasonCode,
		})
		if source.Status != "partial" || source.Reason == nil || *source.Reason != testCase.wantReason {
			t.Fatalf("reason %q produced source %+v, want %q", testCase.reasonCode, source, testCase.wantReason)
		}
	}
}
