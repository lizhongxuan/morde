package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type taskArtifactContentStub struct {
	*taskServiceStub
	content    app.TaskArtifactContent
	principal  identity.Principal
	taskID     string
	artifactID string
	calls      int
	err        error
}

func (s *taskArtifactContentStub) OpenTaskArtifactContent(_ context.Context, principal identity.Principal, taskID, artifactID string) (app.TaskArtifactContent, error) {
	s.calls++
	s.principal, s.taskID, s.artifactID = principal, taskID, artifactID
	return s.content, s.err
}

func taskArtifactAPIRouter(service TaskArtifactContentEndpointService, principals ...identity.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
		if len(principals) > 0 {
			principal = principals[0]
		}
		c.Set(principalKey, principal)
		c.Next()
	})
	(&api{}).registerTaskArtifactContent(engine.Group("/api"), service)
	return engine
}

func TestGetTaskArtifactContentStreamsVerifiedAttachment(t *testing.T) {
	const digest = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	service := &taskArtifactContentStub{content: app.TaskArtifactContent{
		Filename: "设计方案.md", MediaType: "text/markdown; charset=utf-8", SizeBytes: 11,
		SHA256: digest, Body: readSeekCloser{Reader: bytes.NewReader([]byte("hello world"))},
	}}
	response := httptest.NewRecorder()
	taskArtifactAPIRouter(service).ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/tasks/q1234567/artifacts/a1234567/content", nil))
	if response.Code != http.StatusOK || response.Body.String() != "hello world" || service.calls != 1 ||
		service.principal.Kind != identity.KindSession || service.taskID != "q1234567" || service.artifactID != "a1234567" ||
		response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" ||
		response.Header().Get("X-Content-SHA256") != digest || response.Header().Get("Content-Disposition") != "attachment; filename*=utf-8''%E8%AE%BE%E8%AE%A1%E6%96%B9%E6%A1%88.md" ||
		response.Header().Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Fatalf("artifact response=%d headers=%v body=%q calls=%d", response.Code, response.Header(), response.Body.String(), service.calls)
	}
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func TestGetTaskArtifactContentRejectsUnsafeMetadataAndRequestShape(t *testing.T) {
	valid := app.TaskArtifactContent{Filename: "report.md", MediaType: "text/markdown", SizeBytes: 1,
		SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", Body: readSeekCloser{bytes.NewReader([]byte("x"))}}
	for _, test := range []struct {
		name     string
		path     string
		content  app.TaskArtifactContent
		wantCode int
	}{
		{name: "query", path: "/api/tasks/q1234567/artifacts/a1234567/content?path=secret", content: valid, wantCode: http.StatusBadRequest},
		{name: "body", path: "/api/tasks/q1234567/artifacts/a1234567/content", content: valid, wantCode: http.StatusBadRequest},
		{name: "bad digest", path: "/api/tasks/q1234567/artifacts/a1234567/content", content: app.TaskArtifactContent{Filename: "x", MediaType: "text/plain", SizeBytes: 1, SHA256: "invalid", Body: readSeekCloser{bytes.NewReader([]byte("x"))}}, wantCode: http.StatusInternalServerError},
		{name: "bad filename", path: "/api/tasks/q1234567/artifacts/a1234567/content", content: app.TaskArtifactContent{Filename: "\r\nX-Evil: yes", MediaType: "text/plain", SizeBytes: 1, SHA256: valid.SHA256, Body: readSeekCloser{bytes.NewReader([]byte("x"))}}, wantCode: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &taskArtifactContentStub{content: test.content}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.name == "body" {
				request.Body = io.NopCloser(strings.NewReader("{}"))
				request.ContentLength = 2
			}
			taskArtifactAPIRouter(service).ServeHTTP(response, request)
			if response.Code != test.wantCode || service.calls != 0 && (test.name == "query" || test.name == "body") {
				t.Fatalf("response=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
			}
			if response.Header().Get("X-Evil") != "" {
				t.Fatalf("filename injected response header: %v", response.Header())
			}
		})
	}
	service := &taskArtifactContentStub{content: valid}
	response := httptest.NewRecorder()
	taskArtifactAPIRouter(service, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey}).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/artifacts/a1234567/content", nil))
	if response.Code != http.StatusUnauthorized || service.calls != 0 {
		t.Fatalf("non-session artifact request response=%d calls=%d", response.Code, service.calls)
	}
	service = &taskArtifactContentStub{err: domain.NewError(domain.CodeNotFound, "task artifact not found")}
	response = httptest.NewRecorder()
	taskArtifactAPIRouter(service).ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/tasks/q1234567/artifacts/a1234567/content", nil))
	if response.Code != http.StatusNotFound || service.calls != 1 {
		t.Fatalf("missing artifact response=%d calls=%d", response.Code, service.calls)
	}
}
