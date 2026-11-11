package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskFileFetchQueryStub struct {
	*taskServiceStub
	value     app.TaskFileFetch
	principal identity.Principal
	taskID    string
	fetchID   string
	calls     int
	err       error
}

func (s *taskFileFetchQueryStub) GetTaskFileFetch(_ context.Context, principal identity.Principal, taskID, fetchID string) (app.TaskFileFetch, error) {
	s.calls++
	s.principal, s.taskID, s.fetchID = principal, taskID, fetchID
	return s.value, s.err
}

func TestGetTaskFileFetchReturnsSafeOwnerSessionProjection(t *testing.T) {
	now := time.Now().UTC()
	service := &taskFileFetchQueryStub{
		taskServiceStub: &taskServiceStub{},
		value: app.TaskFileFetch{
			ID: "o1234567", OwnerID: "u1234567", TaskID: "q1234567", OperationKey: "agent:test",
			SourceInstallationID: "i1234567", SourceDeviceName: "My desktop", TargetGrantVersion: 4,
			TaskAuthorizationVersion: 3, TaskRevision: 8, RelativePath: "reports/result.pdf",
			Status: task.FileFetchTransferring, ContentState: task.FileFetchContentNone,
			ParseStatus: task.FileFetchParsePending, CreatedAt: now, UpdatedAt: now,
		},
	}
	response := httptest.NewRecorder()
	taskAPIRouter(service, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/tasks/q1234567/file-fetches/o1234567", nil))
	if response.Code != http.StatusOK || service.calls != 1 || service.principal.Kind != identity.KindSession ||
		service.taskID != "q1234567" || service.fetchID != "o1234567" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET file fetch status=%d calls=%d principal=%+v headers=%v body=%s", response.Code, service.calls,
			service.principal, response.Header(), response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["fetch_id"] != "o1234567" || body["task_revision"] != float64(8) ||
		body["status"] != string(task.FileFetchTransferring) || body["relative_path"] != "reports/result.pdf" ||
		body["content_state"] != string(task.FileFetchContentNone) || body["parse_status"] != string(task.FileFetchParsePending) || body["parse_error"] != nil {
		t.Fatalf("file fetch status projection = %#v", body)
	}
	for _, forbidden := range []string{"ticket", "ticket_hash", "target_grant_version", "task_authorization_version"} {
		if _, exists := body[forbidden]; exists {
			t.Fatalf("file fetch status leaked %q: %#v", forbidden, body)
		}
	}
}

type taskFileFetchTransferStub struct {
	TaskFileFetchEndpointService
	manifestCalls int
	chunkCalls    int
	commitCalls   int
}

func (s *taskFileFetchTransferStub) ManifestTaskFileFetch(_ context.Context, taskID, fetchID, ticket string,
	request app.TaskFileFetchManifestRequest) (app.TaskFileFetchManifestReceipt, error) {
	s.manifestCalls++
	return app.TaskFileFetchManifestReceipt{FetchID: fetchID, SourceVersion: request.SourceVersion,
		Status: string(task.FileFetchTransferring), ConfirmedSequences: []int32{}}, nil
}

func (s *taskFileFetchTransferStub) PutTaskFileFetchChunk(_ context.Context, taskID, fetchID, ticket string,
	sequence int32, digest string, body io.Reader) (app.TaskFileFetchChunkReceipt, error) {
	s.chunkCalls++
	return app.TaskFileFetchChunkReceipt{FetchID: fetchID, Sequence: sequence, ConfirmedBytes: 1}, nil
}

func (s *taskFileFetchTransferStub) CommitTaskFileFetch(_ context.Context, taskID, fetchID, ticket string,
	request app.TaskFileFetchCommitRequest) (app.TaskFileFetchCommitReceipt, error) {
	s.commitCalls++
	return app.TaskFileFetchCommitReceipt{FetchID: fetchID, TaskRevision: 1, TargetGrantVersion: 1, Status: string(task.FileFetchCompleted),
		SourceVersion: request.SourceVersion, SourceSHA256: request.SourceSHA256, SizeBytes: request.SizeBytes,
		ContentID: "o7654321", ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil
}

func TestTaskFileFetchTransferRoutesRequireTicketAndStrictRequestShape(t *testing.T) {
	service := &taskFileFetchTransferStub{}
	engine := gin.New()
	(&api{}).registerTaskFileFetchTransferRoutes(engine, service)

	validManifest := `{"source_version":"` + strings.Repeat("a", 64) + `","source_sha256":"` + strings.Repeat("b", 64) + `","size_bytes":1,"media_type":"text/plain"}`
	for _, test := range []struct {
		name        string
		url         string
		body        string
		contentType string
		authorize   []string
		wantCode    int
	}{
		{name: "missing ticket", url: "/api/tasks/q1234567/file-fetches/o1234567/manifest", body: validManifest, contentType: "application/json", wantCode: http.StatusNotFound},
		{name: "duplicate authorization headers", url: "/api/tasks/q1234567/file-fetches/o1234567/manifest", body: validManifest, contentType: "application/json", authorize: []string{"Bearer first", "Bearer second"}, wantCode: http.StatusNotFound},
		{name: "wrong media type", url: "/api/tasks/q1234567/file-fetches/o1234567/manifest", body: validManifest, contentType: "text/plain", authorize: []string{"Bearer fixture"}, wantCode: http.StatusBadRequest},
		{name: "query string", url: "/api/tasks/q1234567/file-fetches/o1234567/manifest?ticket=secret", body: validManifest, contentType: "application/json", authorize: []string{"Bearer fixture"}, wantCode: http.StatusBadRequest},
		{name: "unknown JSON field", url: "/api/tasks/q1234567/file-fetches/o1234567/manifest", body: strings.TrimSuffix(validManifest, "}") + `,"ticket":"secret"}`, contentType: "application/json", authorize: []string{"Bearer fixture"}, wantCode: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.url, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			for _, value := range test.authorize {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != test.wantCode {
				t.Fatalf("request status=%d body=%s; want %d", response.Code, response.Body.String(), test.wantCode)
			}
			if strings.Contains(response.Body.String(), "secret") {
				t.Fatal("error response echoed rejected ticket material")
			}
		})
	}
	if service.manifestCalls != 0 {
		t.Fatalf("invalid manifest requests reached service %d times", service.manifestCalls)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/file-fetches/o1234567/manifest", strings.NewReader(validManifest))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer fixture")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || service.manifestCalls != 1 {
		t.Fatalf("valid manifest status=%d calls=%d headers=%v body=%s", response.Code, service.manifestCalls, response.Header(), response.Body.String())
	}
}

func TestGetTaskFileFetchRequiresPortalSessionAndCanonicalQuery(t *testing.T) {
	service := &taskFileFetchQueryStub{taskServiceStub: &taskServiceStub{}}
	for name, test := range map[string]struct {
		principal identity.Principal
		url       string
		wantCode  int
	}{
		"client credential": {principal: identity.Principal{UserID: "u1234567", Kind: identity.KindClient, Role: identity.RoleUser}, url: "/api/tasks/q1234567/file-fetches/o1234567", wantCode: http.StatusUnauthorized},
		"query string":      {principal: identity.Principal{UserID: "u1234567", Kind: identity.KindSession, Role: identity.RoleUser}, url: "/api/tasks/q1234567/file-fetches/o1234567?ticket=secret", wantCode: http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(func(c *gin.Context) {
				c.Set(principalKey, test.principal)
				c.Next()
			})
			handler := &api{options: Options{ServiceInstanceID: "11111111-1111-4111-8111-111111111111"}}
			handler.registerTasks(engine.Group("/api"), service)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.url, nil))
			if response.Code != test.wantCode {
				t.Fatalf("GET file fetch status=%d body=%s; want %d", response.Code, response.Body.String(), test.wantCode)
			}
			if service.calls != 0 {
				t.Fatalf("unauthorized or non-canonical request reached query service %d times", service.calls)
			}
			if strings.Contains(response.Body.String(), "secret") {
				t.Fatal("error response echoed the rejected ticket query")
			}
		})
	}
}

func TestGetTaskFileFetchHidesMissingRecords(t *testing.T) {
	service := &taskFileFetchQueryStub{
		taskServiceStub: &taskServiceStub{},
		err:             domain.NewError(domain.CodeNotFound, "file fetch not found"),
	}
	response := httptest.NewRecorder()
	taskAPIRouter(service, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/tasks/q1234567/file-fetches/o7654321", nil))
	if response.Code != http.StatusNotFound || service.calls != 1 {
		t.Fatalf("missing Task fetch status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}
}
