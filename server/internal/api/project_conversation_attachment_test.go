package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type projectAttachmentAPIStub struct {
	calls            int
	scope            app.ProjectConversationAttachmentScope
	reference        app.ProjectConversationAttachmentReference
	size             int
	closed           bool
	derivatives      app.ProjectConversationDerivatives
	derivativesReady bool
}

func (s *projectAttachmentAPIStub) DerivativesReady() bool { return s.derivativesReady }

type projectAttachmentTestReader struct {
	io.Reader
	closed *bool
}

func (r projectAttachmentTestReader) Close() error { *r.closed = true; return nil }

func (s *projectAttachmentAPIStub) Initialize(_ context.Context, _ identity.Principal, scope app.ProjectConversationAttachmentScope, _ app.ProjectConversationAttachmentMetadata) (app.ProjectConversationAttachmentStatus, error) {
	s.calls++
	s.scope = scope
	return app.ProjectConversationAttachmentStatus{State: "uploading", ReceivedSequences: []int32{}}, nil
}

func (s *projectAttachmentAPIStub) Get(_ context.Context, _ identity.Principal, scope app.ProjectConversationAttachmentScope) (app.ProjectConversationAttachmentStatus, error) {
	s.calls++
	s.scope = scope
	return app.ProjectConversationAttachmentStatus{State: "uploading", ReceivedSequences: []int32{}}, nil
}

func (s *projectAttachmentAPIStub) PutChunk(_ context.Context, _ identity.Principal, scope app.ProjectConversationAttachmentScope, _ int32, _ string, body io.Reader, _ int64) (bool, error) {
	s.calls++
	s.scope = scope
	data, err := io.ReadAll(body)
	s.size = len(data)
	return false, err
}

func (s *projectAttachmentAPIStub) Commit(_ context.Context, _ identity.Principal, scope app.ProjectConversationAttachmentScope) (app.ProjectConversationAttachmentStatus, error) {
	s.calls++
	s.scope = scope
	return app.ProjectConversationAttachmentStatus{State: "committed", ReceivedSequences: []int32{}}, nil
}

func (s *projectAttachmentAPIStub) PutDerivatives(_ context.Context, _ identity.Principal, scope app.ProjectConversationAttachmentScope, value app.ProjectConversationDerivatives) (app.ProjectConversationAttachmentStatus, error) {
	s.calls++
	s.scope, s.derivatives = scope, value
	return app.ProjectConversationAttachmentStatus{State: "committed", DerivativeState: value.Outcome, ReceivedSequences: []int32{}}, nil
}

func (s *projectAttachmentAPIStub) Open(_ context.Context, _ identity.Principal, scope app.ProjectConversationAttachmentScope) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error) {
	s.calls++
	s.scope = scope
	return io.NopCloser(strings.NewReader("synthetic")), app.ProjectConversationAttachmentMetadata{MediaType: "text/plain", SizeBytes: 9, SHA256: strings.Repeat("a", 64)}, nil
}

func (s *projectAttachmentAPIStub) GetByID(_ context.Context, _ identity.Principal, reference app.ProjectConversationAttachmentReference) (app.ProjectConversationAttachmentMetadata, error) {
	s.calls++
	s.reference = reference
	return app.ProjectConversationAttachmentMetadata{AttachmentID: reference.AttachmentID, Name: "synthetic.txt",
		MediaType: "text/plain", SizeBytes: 9, SHA256: strings.Repeat("a", 64)}, nil
}

func (s *projectAttachmentAPIStub) OpenByID(_ context.Context, _ identity.Principal, reference app.ProjectConversationAttachmentReference) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error) {
	s.calls++
	s.reference = reference
	return projectAttachmentTestReader{Reader: strings.NewReader("synthetic"), closed: &s.closed},
		app.ProjectConversationAttachmentMetadata{AttachmentID: reference.AttachmentID, MediaType: "text/plain", SizeBytes: 9, SHA256: strings.Repeat("a", 64)}, nil
}

func TestProjectAttachmentRoutesFenceActorAndAllowLargeChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &projectAttachmentAPIStub{}
	router := gin.New()
	handler := &api{options: Options{APIMaxBodyBytes: 1 << 20}}
	group := router.Group("/api", handler.limitBody, func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Kind: identity.KindSession, TokenID: "t1234567"})
		c.Next()
	})
	RegisterProjectConversationAttachmentRoutes(group, service)
	base := "/api/projects/j1234567/conversations/c1234567/messages/m1234567/attachments/attachment-001"
	instanceID := "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, base}, {http.MethodGet, base}, {http.MethodPut, base + "/chunks/0"},
		{http.MethodPost, base + "/commit"}, {http.MethodPut, base + "/derivatives"}, {http.MethodGet, base + "/content"},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		request.Header.Set("X-Morde-Actor-ID", "u7654321")
		request.Header.Set("X-Morde-Service-Instance-ID", instanceID)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || service.calls != 0 {
			t.Fatalf("changed actor accepted for %s %s: status=%d calls=%d", tc.method, tc.path, response.Code, service.calls)
		}
	}
	body := bytes.Repeat([]byte("s"), (1<<20)+9)
	digest := sha256.Sum256(body)
	request := httptest.NewRequest(http.MethodPut, base+"/chunks/0", bytes.NewReader(body))
	request.Header.Set("X-Morde-Actor-ID", "u1234567")
	request.Header.Set("X-Morde-Service-Instance-ID", instanceID)
	request.Header.Set("X-Chunk-SHA256", hex.EncodeToString(digest[:]))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var receipt struct {
		Sequence int  `json:"sequence"`
		Replay   bool `json:"replay"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || service.size != len(body) ||
		service.scope.ProjectID != "j1234567" || service.scope.ConversationID != "c1234567" || service.scope.MessageID != "m1234567" ||
		service.scope.ClientAttachmentID != "attachment-001" || service.scope.ServiceInstanceID != instanceID {
		t.Fatalf("large project chunk response=%d size=%d scope=%+v", response.Code, service.size, service.scope)
	}
	missingInstance := httptest.NewRequest(http.MethodGet, base, nil)
	missingInstance.Header.Set("X-Morde-Actor-ID", "u1234567")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, missingInstance)
	if response.Code != http.StatusBadRequest || service.calls != 1 {
		t.Fatalf("missing service instance accepted: status=%d calls=%d", response.Code, service.calls)
	}
}

func TestProjectDerivativeRouteAcceptsBoundedSetAndRejectsOversize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &projectAttachmentAPIStub{}
	router := gin.New()
	handler := &api{options: Options{APIMaxBodyBytes: 1 << 20}}
	group := router.Group("/api", handler.limitBody, func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Kind: identity.KindSession, TokenID: "t1234567"})
		c.Next()
	})
	RegisterProjectConversationAttachmentRoutes(group, service)
	path := "/api/projects/j1234567/conversations/c1234567/messages/m1234567/attachments/attachment-001/derivatives"
	requestWithBody := func(body []byte) *http.Request {
		request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Morde-Actor-ID", "u1234567")
		request.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
		return request
	}
	value := app.ProjectConversationDerivatives{Outcome: "completed", Files: []app.ProjectConversationDerivativeFile{{
		Kind: "page_preview", Page: 1, Name: "synthetic.jpg", MediaType: "image/jpeg", SHA256: strings.Repeat("0", 64),
		Content: bytes.Repeat([]byte("synthetic"), 110000),
	}}}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, requestWithBody(body))
	if response.Code != http.StatusOK || service.calls != 1 || len(service.derivatives.Files) != 1 ||
		len(service.derivatives.Files[0].Content) != len(value.Files[0].Content) {
		t.Fatalf("bounded derivative request status=%d calls=%d", response.Code, service.calls)
	}
	oversize := append([]byte(`{"outcome":"skipped","files":[],"extra":"`), bytes.Repeat([]byte("x"), int(app.ProjectConversationDerivativeMaxJSONBytes))...)
	oversize = append(oversize, []byte(`"}`)...)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, requestWithBody(oversize))
	if response.Code != http.StatusRequestEntityTooLarge || service.calls != 1 {
		t.Fatalf("oversize derivative request status=%d calls=%d", response.Code, service.calls)
	}
}

func TestProjectAttachmentReadByIDRequiresActorAndClosesPrivateStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &projectAttachmentAPIStub{}
	router := gin.New()
	group := router.Group("/api", func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Kind: identity.KindSession, TokenID: "t1234567"})
		c.Next()
	})
	RegisterProjectConversationAttachmentRoutes(group, service)
	attachmentID := "pca_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	base := "/api/projects/j1234567/conversations/c1234567/messages/m1234567/attachments-by-id/" + attachmentID
	instanceID := "11111111-1111-4111-8111-111111111111"
	request := httptest.NewRequest(http.MethodGet, base, nil)
	request.Header.Set("X-Morde-Actor-ID", "u7654321")
	request.Header.Set("X-Morde-Service-Instance-ID", instanceID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || service.calls != 0 {
		t.Fatalf("changed actor accessed project metadata: status=%d calls=%d", response.Code, service.calls)
	}
	request = httptest.NewRequest(http.MethodGet, base, nil)
	request.Header.Set("X-Morde-Actor-ID", "u1234567")
	request.Header.Set("X-Morde-Service-Instance-ID", instanceID)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var metadata app.ProjectConversationAttachmentMetadata
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &metadata) != nil || metadata.AttachmentID != attachmentID ||
		service.reference.MessageID != "m1234567" || service.reference.ProjectID != "j1234567" || service.reference.ServiceInstanceID != instanceID {
		t.Fatalf("project metadata response=%d reference=%+v", response.Code, service.reference)
	}
	request = httptest.NewRequest(http.MethodGet, base+"/content", nil)
	request.Header.Set("X-Morde-Actor-ID", "u1234567")
	request.Header.Set("X-Morde-Service-Instance-ID", instanceID)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "synthetic" || !service.closed ||
		response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("project content response=%d closed=%t headers=%v", response.Code, service.closed, response.Header())
	}
}

type blockingProjectAttachmentReader struct {
	closed chan struct{}
	once   sync.Once
}

func (r *blockingProjectAttachmentReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, io.EOF
}

func (r *blockingProjectAttachmentReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type blockingProjectAttachmentService struct {
	*projectAttachmentAPIStub
	started chan struct{}
}

func (s *blockingProjectAttachmentService) OpenByID(_ context.Context, _ identity.Principal, _ app.ProjectConversationAttachmentReference) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error) {
	reader := &blockingProjectAttachmentReader{closed: make(chan struct{})}
	s.started <- struct{}{}
	return reader, app.ProjectConversationAttachmentMetadata{MediaType: "text/plain", SizeBytes: 1, SHA256: strings.Repeat("a", 64)}, nil
}

func TestProjectAttachmentReadLimitReleasesOnCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &blockingProjectAttachmentService{projectAttachmentAPIStub: &projectAttachmentAPIStub{}, started: make(chan struct{}, 8)}
	router := gin.New()
	group := router.Group("/api", func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Kind: identity.KindSession, TokenID: "t1234567"})
		c.Next()
	})
	RegisterProjectConversationAttachmentRoutes(group, service)
	path := "/api/projects/j1234567/conversations/c1234567/messages/m1234567/attachments-by-id/pca_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/content"
	start := func() (context.CancelFunc, <-chan int) {
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		request.Header.Set("X-Morde-Actor-ID", "u1234567")
		request.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
		done := make(chan int, 1)
		go func() {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			done <- response.Code
		}()
		return cancel, done
	}
	cancels := make([]context.CancelFunc, 0, 5)
	dones := make([]<-chan int, 0, 5)
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for range 4 {
		cancel, done := start()
		cancels, dones = append(cancels, cancel), append(dones, done)
		select {
		case <-service.started:
		case <-time.After(10 * time.Second):
			t.Fatal("project read did not start")
		}
	}
	blockedCancel, blockedDone := start()
	defer blockedCancel()
	select {
	case status := <-blockedDone:
		if status != http.StatusTooManyRequests {
			t.Fatalf("fifth concurrent read returned %d", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("excess concurrent read was not rejected")
	}
	cancels[0]()
	select {
	case <-dones[0]:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled read did not close its stream")
	}
	retryCancel, retryDone := start()
	cancels, dones = append(cancels, retryCancel), append(dones, retryDone)
	select {
	case <-service.started:
	case status := <-retryDone:
		t.Fatalf("released read slot was not reusable: status=%d", status)
	case <-time.After(10 * time.Second):
		t.Fatal("released read slot was not reusable")
	}
	for i := 1; i < len(cancels); i++ {
		cancels[i]()
		select {
		case <-dones[i]:
		case <-time.After(10 * time.Second):
			t.Fatal("cancelled read did not finish")
		}
	}
}

func TestProjectAttachmentReadLimitIsGloballyBounded(t *testing.T) {
	limiter := &projectAttachmentReadLimiter{byActor: make(map[string]int)}
	releases := make([]func(), 0, 32)
	for i := range 32 {
		release, ok := limiter.acquire(string(rune('a' + i)))
		if !ok {
			t.Fatalf("read %d was unexpectedly rejected", i)
		}
		releases = append(releases, release)
	}
	if _, ok := limiter.acquire("overflow"); ok {
		t.Fatal("global read limit was not enforced")
	}
	for _, release := range releases {
		release()
		release()
	}
	if limiter.active != 0 || len(limiter.byActor) != 0 {
		t.Fatal("read limit release leaked state")
	}
}
