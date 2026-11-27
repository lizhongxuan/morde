package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type adminObservabilityLogsReaderStub struct {
	page  app.AdminObservabilityLogPage
	err   error
	query app.AdminObservabilityLogQuery
	calls int
}

func (s *adminObservabilityLogsReaderStub) ListAdminObservabilityLogs(_ context.Context, query app.AdminObservabilityLogQuery) (app.AdminObservabilityLogPage, error) {
	s.query, s.calls = query, s.calls+1
	return s.page, s.err
}

type adminObservabilityLogHealthStub struct {
	health app.AdminObservabilityLogHealth
}

func (s adminObservabilityLogHealthStub) Health() app.AdminObservabilityLogHealth { return s.health }

func adminObservabilityLogsRouter(t *testing.T, role identity.Role, reader app.AdminObservabilityLogReader,
	health AdminObservabilityLogHealthProvider, auditWriter *adminAuditWriterStub) *gin.Engine {
	t.Helper()
	services := newServices()
	services.verifier.role = role
	deps := services.deps()
	deps.Observability = AdminObservabilityDeps{Logs: reader, LogHealth: health}
	deps.AdminAudit = auditWriter
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestAdminObservabilityTraceLogsAreAdminOnlyAuditedAndHaveIndependentCoverage(t *testing.T) {
	traceID := "0123456789abcdef0123456789abcdef"
	reader := &adminObservabilityLogsReaderStub{page: app.AdminObservabilityLogPage{
		Items: []app.AdminObservabilityLog{{Time: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
			Severity: "error", Service: "morde-portal", Message: "task failed", TraceID: traceID}},
		Coverage: app.AdminObservabilitySource{Status: "available"},
	}}
	health := adminObservabilityLogHealthStub{health: app.AdminObservabilityLogHealth{Status: "partial", ReasonCode: "log_queue_dropped", DroppedRecords: 2}}
	auditWriter := &adminAuditWriterStub{}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	user := adminObservabilityLogsRouter(t, identity.RoleUser, reader, health, auditWriter)
	denied := requestWithCookie(user, http.MethodGet, "/api/admin/observability/traces/"+traceID+"/logs", cookie, "")
	if denied.Code != http.StatusForbidden || reader.calls != 0 || len(auditWriter.entries) != 0 {
		t.Fatalf("non-admin logs = %d %s calls=%d audits=%d", denied.Code, denied.Body, reader.calls, len(auditWriter.entries))
	}

	admin := adminObservabilityLogsRouter(t, identity.RoleAdmin, reader, health, auditWriter)
	path := "/api/admin/observability/traces/" + traceID + "/logs?request_id=" + url.QueryEscape("req-1") + "&task_id=task-1&limit=10"
	response := requestWithCookie(admin, http.MethodGet, path, cookie, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		reader.calls != 1 || len(auditWriter.entries) != 1 || auditWriter.entries[0].Action != audit.ActionObservabilityTraceRead {
		t.Fatalf("admin logs = %d %s cache=%q calls=%d audits=%+v", response.Code, response.Body,
			response.Header().Get("Cache-Control"), reader.calls, auditWriter.entries)
	}
	if reader.query.TraceID != traceID || reader.query.RequestID != "req-1" || reader.query.TaskID != "task-1" || reader.query.Limit != 10 {
		t.Fatalf("log query was not forwarded: %+v", reader.query)
	}
	if reader.page.Coverage.Status != "available" {
		t.Fatalf("test setup coverage unexpectedly changed: %+v", reader.page.Coverage)
	}
	var body app.AdminObservabilityLogPage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Coverage.Status != "partial" || body.Coverage.Reason == nil || !strings.Contains(*body.Coverage.Reason, "队列") || len(body.Items) != 1 {
		t.Fatalf("log queue loss was not visible in coverage: %+v", body)
	}
}

func TestAdminObservabilityTraceLogsReportReadFailureWithoutHidingPage(t *testing.T) {
	traceID := "0123456789abcdef0123456789abcdef"
	reader := &adminObservabilityLogsReaderStub{err: errors.New("database connection details")}
	auditWriter := &adminAuditWriterStub{}
	admin := adminObservabilityLogsRouter(t, identity.RoleAdmin, reader, nil, auditWriter)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(admin, http.MethodGet, "/api/admin/observability/traces/"+traceID+"/logs", cookie, "")
	if response.Code != http.StatusOK || len(auditWriter.entries) != 1 {
		t.Fatalf("independent log failure response = %d %s audits=%d", response.Code, response.Body, len(auditWriter.entries))
	}
	if !strings.Contains(response.Body.String(), `"coverage":{"status":"unavailable"`) ||
		!strings.Contains(response.Body.String(), "correlated logs could not be queried") || strings.Contains(response.Body.String(), "database connection details") {
		t.Fatalf("log failure leaked internals or lost coverage state: %s", response.Body)
	}
}

func TestMergeCorrelatedLogHealthPreservesUnavailableSource(t *testing.T) {
	provider := adminObservabilityLogHealthStub{health: app.AdminObservabilityLogHealth{Status: "partial", ReasonCode: "log_queue_dropped"}}
	failed := app.AdminObservabilitySource{Status: "unavailable", Reason: stringPointer("query failed")}
	merged := mergeCorrelatedLogHealth(failed, provider)
	if merged.Status != "unavailable" || merged.Reason == nil || *merged.Reason != "query failed" {
		t.Fatalf("writer health masked a query failure: %+v", merged)
	}
}
