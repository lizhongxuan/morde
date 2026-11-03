package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceOwnershipTransferHTTPStub struct {
	value          workspace.OwnershipTransfer
	principal      identity.Principal
	workspaceID    string
	targetUserID   string
	transferID     string
	revision       int64
	idempotencyKey string
	requestCalls   int
	getCalls       int
	acceptCalls    int
	cancelCalls    int
	replayed       bool
}

func (s *workspaceOwnershipTransferHTTPStub) RequestWorkspaceTransfer(_ context.Context, principal identity.Principal,
	workspaceID, targetUserID string, revision int64, key string) (workspace.OwnershipTransfer, bool, error) {
	s.principal, s.workspaceID, s.targetUserID, s.revision, s.idempotencyKey = principal, workspaceID, targetUserID, revision, key
	s.requestCalls++
	return s.value, s.replayed, nil
}

func (s *workspaceOwnershipTransferHTTPStub) GetWorkspaceTransfer(_ context.Context, principal identity.Principal,
	workspaceID, transferID string) (workspace.OwnershipTransfer, error) {
	s.principal, s.workspaceID, s.transferID = principal, workspaceID, transferID
	s.getCalls++
	return s.value, nil
}

func (s *workspaceOwnershipTransferHTTPStub) AcceptWorkspaceTransfer(_ context.Context, principal identity.Principal,
	workspaceID, transferID, key string) (workspace.OwnershipTransfer, error) {
	s.principal, s.workspaceID, s.transferID, s.idempotencyKey = principal, workspaceID, transferID, key
	s.acceptCalls++
	return s.value, nil
}

func (s *workspaceOwnershipTransferHTTPStub) CancelWorkspaceTransfer(_ context.Context, principal identity.Principal,
	workspaceID, transferID, key string) (workspace.OwnershipTransfer, error) {
	s.principal, s.workspaceID, s.transferID, s.idempotencyKey = principal, workspaceID, transferID, key
	s.cancelCalls++
	return s.value, nil
}

func newWorkspaceOwnershipTransferHTTPRouter(service WorkspaceOwnershipTransferService) http.Handler {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession})
		c.Next()
	})
	h := &api{}
	h.registerWorkspaceOwnershipTransfers(router.Group("/api"), service)
	return router
}

func TestWorkspaceOwnershipTransferHTTPRoutesForwardSessionAndIdempotency(t *testing.T) {
	now := time.Now().UTC()
	service := &workspaceOwnershipTransferHTTPStub{value: workspace.OwnershipTransfer{
		ID: "r1234567", WorkspaceID: "w1234567", FromOwnerID: "u1234567", ToOwnerID: "u7654321",
		State: workspace.OwnershipTransferPending, ExpectedRevision: 4, Version: 1, CreatedAt: now, UpdatedAt: now,
	}}
	router := newWorkspaceOwnershipTransferHTTPRouter(service)

	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/ownership-transfers", strings.NewReader(`{"target_user_id":"u7654321","expected_revision":4}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "transfer-create-01")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.requestCalls != 1 || service.workspaceID != "w1234567" ||
		service.targetUserID != "u7654321" || service.revision != 4 || service.idempotencyKey != "transfer-create-01" ||
		service.principal.Kind != identity.KindSession {
		t.Fatalf("request transfer response = %d %s; service = %#v", response.Code, response.Body.String(), service)
	}
	var transfer workspace.OwnershipTransfer
	if err := json.Unmarshal(response.Body.Bytes(), &transfer); err != nil || transfer.ID != "r1234567" {
		t.Fatalf("request transfer response body = %#v, %v", transfer, err)
	}

	service.replayed = true
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/workspaces/w1234567/ownership-transfers",
		strings.NewReader(`{"target_user_id":"u7654321","expected_revision":4}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "transfer-create-01")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("replayed create response status = %d, want 200", response.Code)
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces/w1234567/ownership-transfers/r1234567", nil))
	if response.Code != http.StatusOK || service.getCalls != 1 || service.transferID != "r1234567" {
		t.Fatalf("get transfer response = %d %s, service = %#v", response.Code, response.Body.String(), service)
	}

	for _, test := range []struct {
		path      string
		wantCalls int
		callCount func() int
	}{
		{path: "/api/workspaces/w1234567/ownership-transfers/r1234567/accept", wantCalls: 1, callCount: func() int { return service.acceptCalls }},
		{path: "/api/workspaces/w1234567/ownership-transfers/r1234567/cancel", wantCalls: 1, callCount: func() int { return service.cancelCalls }},
	} {
		request = httptest.NewRequest(http.MethodPost, test.path, nil)
		request.Header.Set("Idempotency-Key", "transfer-action-01")
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK || test.callCount() != test.wantCalls || service.workspaceID != "w1234567" ||
			service.transferID != "r1234567" || service.idempotencyKey != "transfer-action-01" {
			t.Errorf("action %s response = %d %s, service = %#v", test.path, response.Code, response.Body.String(), service)
		}
	}
}

func TestWorkspaceOwnershipTransferHTTPRejectsMalformedRequests(t *testing.T) {
	service := &workspaceOwnershipTransferHTTPStub{value: workspace.OwnershipTransfer{
		ID: "r1234567", WorkspaceID: "w1234567", FromOwnerID: "u1234567", ToOwnerID: "u7654321",
		State: workspace.OwnershipTransferPending, ExpectedRevision: 4, Version: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	router := newWorkspaceOwnershipTransferHTTPRouter(service)
	for _, test := range []struct {
		path string
		body string
		key  bool
	}{
		{path: "/api/workspaces/w1234567/ownership-transfers", body: `{"target_user_id":"u7654321"}`, key: true},
		{path: "/api/workspaces/w1234567/ownership-transfers", body: `{"target_user_id":"u7654321","expected_revision":4,"unexpected":true}`, key: true},
		{path: "/api/workspaces/w1234567/ownership-transfers/r1234567/accept", body: `{"confirm":true}`, key: true},
		{path: "/api/workspaces/invalid/ownership-transfers/r1234567", key: false},
	} {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		if strings.Contains(test.path, "ownership-transfers/r1234567") && !strings.HasSuffix(test.path, "/accept") {
			request.Method = http.MethodGet
		}
		request.Header.Set("Content-Type", "application/json")
		if test.key {
			request.Header.Set("Idempotency-Key", "transfer-action-02")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("request %s returned %d %s, want 400", test.path, response.Code, response.Body.String())
		}
	}
	if service.requestCalls != 0 || service.acceptCalls != 0 || service.getCalls != 0 {
		t.Fatalf("malformed requests reached service: %#v", service)
	}
}

func TestWorkspaceOwnershipTransferRoutesAreMountedWithoutFencingWorkspaceRoutes(t *testing.T) {
	services := newServices()
	transferService := &workspaceOwnershipTransferHTTPStub{value: workspace.OwnershipTransfer{
		ID: "r1234567", WorkspaceID: testWorkspaceID, FromOwnerID: testUserID, ToOwnerID: "u7654321",
		State: workspace.OwnershipTransferPending, ExpectedRevision: 2, Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	deps := services.deps()
	deps.WorkspaceTransfers = transferService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	request := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/ownership-transfers",
		`{"target_user_id":"u7654321","expected_revision":2}`, cookie, "https://portal.example.test", "Idempotency-Key", "ownership-transfer-01")
	if request.Code != http.StatusCreated || transferService.requestCalls != 1 || transferService.principal.UserID != testUserID {
		t.Fatalf("create transfer = %d %s; service=%#v", request.Code, request.Body, transferService)
	}
	workspaces := requestWithCookie(router, http.MethodGet, "/api/workspaces", cookie, "")
	if workspaces.Code != http.StatusOK {
		t.Fatalf("workspace route after ownership transfer registration = %d %s", workspaces.Code, workspaces.Body)
	}
}
