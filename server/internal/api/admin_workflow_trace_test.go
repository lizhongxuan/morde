package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain/identity"
	"morde/server/internal/evaluation"
)

type workflowTraceHTTPStub struct {
	taskID  string
	ownerID string
	calls   int
	err     error
	trace   evaluation.WorkflowTrace
}

func (s *workflowTraceHTTPStub) GetTaskWorkflowTrace(_ context.Context, taskID, ownerID string) (evaluation.WorkflowTrace, error) {
	s.taskID, s.ownerID, s.calls = taskID, ownerID, s.calls+1
	return s.trace, s.err
}

func workflowTraceAdminRouter(t *testing.T, reader WorkflowTraceReader, role identity.Role) *gin.Engine {
	t.Helper()
	services := newServices()
	services.verifier.role = role
	deps := services.deps()
	deps.WorkflowTraces = reader
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestAdminWorkflowTraceIsAdminOnlyAndNoStore(t *testing.T) {
	reader := &workflowTraceHTTPStub{trace: evaluation.WorkflowTrace{
		Version: 1, TraceID: "trace_0123456789abcdef", DataClass: evaluation.WorkflowDataLiveObserved,
		CapturedAt: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), Coverage: evaluation.WorkflowCoverageUnknown,
		CoverageSourceIDs: []string{}, Sources: []evaluation.WorkflowSource{}, Nodes: []evaluation.WorkflowNode{},
		Edges: []evaluation.WorkflowEdge{}, Gaps: []evaluation.WorkflowGap{},
	}}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	path := "/api/admin/users/u1234567/tasks/q1234567/workflow-trace"

	userRouter := workflowTraceAdminRouter(t, reader, identity.RoleUser)
	denied := requestWithCookie(userRouter, http.MethodGet, path, cookie, "")
	if denied.Code != http.StatusForbidden || reader.calls != 0 {
		t.Fatalf("non-admin workflow trace = %d %s calls=%d", denied.Code, denied.Body, reader.calls)
	}

	adminRouter := workflowTraceAdminRouter(t, reader, identity.RoleAdmin)
	response := requestWithCookie(adminRouter, http.MethodGet, path, cookie, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || reader.calls != 1 ||
		reader.taskID != "q1234567" || reader.ownerID != "u1234567" || !containsAny(response.Body.String(), "trace_0123456789abcdef") {
		t.Fatalf("admin workflow trace = %d %s cache=%q calls=%d owner=%q task=%q", response.Code, response.Body,
			response.Header().Get("Cache-Control"), reader.calls, reader.ownerID, reader.taskID)
	}
}
