package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type goalRoutesServiceStub struct {
	principal    identity.Principal
	page         app.PageRequest
	id           string
	body         json.RawMessage
	baseRevision int64
	mutationID   string
	listResult   app.GoalPage
	object       personal.Object
	receipt      personal.MutationReceipt
	listCalls    int
	getCalls     int
	createCalls  int
	patchCalls   int
	deleteCalls  int
	err          error
}

func (s *goalRoutesServiceStub) List(_ context.Context, principal identity.Principal, page app.PageRequest) (app.GoalPage, error) {
	s.listCalls++
	s.principal, s.page = principal, page
	return s.listResult, s.err
}

func (s *goalRoutesServiceStub) Get(_ context.Context, principal identity.Principal, id string) (personal.Object, error) {
	s.getCalls++
	s.principal, s.id = principal, id
	return s.object, s.err
}

func (s *goalRoutesServiceStub) Create(_ context.Context, principal identity.Principal, id string, body json.RawMessage, mutationID string) (personal.MutationReceipt, error) {
	s.createCalls++
	s.principal, s.id, s.body, s.mutationID = principal, id, append(json.RawMessage(nil), body...), mutationID
	return s.receipt, s.err
}

func (s *goalRoutesServiceStub) Patch(_ context.Context, principal identity.Principal, id string, baseRevision int64, body json.RawMessage, mutationID string) (personal.MutationReceipt, error) {
	s.patchCalls++
	s.principal, s.id, s.baseRevision, s.body, s.mutationID = principal, id, baseRevision, append(json.RawMessage(nil), body...), mutationID
	return s.receipt, s.err
}

func (s *goalRoutesServiceStub) Delete(_ context.Context, principal identity.Principal, id string, baseRevision int64, mutationID string) (personal.MutationReceipt, error) {
	s.deleteCalls++
	s.principal, s.id, s.baseRevision, s.mutationID = principal, id, baseRevision, mutationID
	return s.receipt, s.err
}

func goalAPIRouter(principal *identity.Principal, service goalRoutesService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	registerGoalRoutes(router.Group("/api"), service)
	return router
}

func goalAPIRequest(router http.Handler, method, path, body, idempotencyKey string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestGoalRoutesListReturnsEmptyArrayAndForwardsPageAndPrincipal(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &goalRoutesServiceStub{listResult: app.GoalPage{Items: []personal.Object{}, NextCursor: "next-page"}}
	response := goalAPIRequest(goalAPIRouter(&principal, service), http.MethodGet, "/api/goals?limit=7&cursor=before", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[]`) || !strings.Contains(response.Body.String(), `"next_cursor":"next-page"`) {
		t.Fatalf("list goals = %d %s", response.Code, response.Body)
	}
	if service.listCalls != 1 || !reflect.DeepEqual(service.principal, principal) || service.page != (app.PageRequest{Limit: 7, Cursor: "before"}) {
		t.Fatalf("list arguments principal=%+v page=%+v calls=%d", service.principal, service.page, service.listCalls)
	}
	badLimit := goalAPIRequest(goalAPIRouter(&principal, service), http.MethodGet, "/api/goals?limit=101", "", "")
	if badLimit.Code != http.StatusBadRequest || service.listCalls != 1 {
		t.Fatalf("bad limit = %d %s; calls=%d", badLimit.Code, badLimit.Body, service.listCalls)
	}
}

func TestGoalRoutesUseOpaqueIDOwnerPrincipalAndIdempotencyForMutations(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient, TokenID: "t1234567", ClientInstallationID: "i1234567"}
	service := &goalRoutesServiceStub{
		object:  personal.Object{ObjectType: personal.ObjectTypeGoal, ObjectID: "g_1", Revision: 2, Body: json.RawMessage(`{"title":"Learn Go","status":"active","steps":[]}`)},
		receipt: personal.MutationReceipt{ObjectType: personal.ObjectTypeGoal, ObjectID: "g_1", ResultRevision: 3, AccountChangeSequence: 9, Replay: true},
	}
	router := goalAPIRouter(&principal, service)

	created := goalAPIRequest(router, http.MethodPost, "/api/goals", `{"id":"g_1","body":{"title":"Learn Go","status":"active","steps":[]}}`, "goal-create-1")
	if created.Code != http.StatusCreated || service.createCalls != 1 || !reflect.DeepEqual(service.principal, principal) || service.id != "g_1" ||
		service.mutationID != "goal-create-1" || !strings.Contains(created.Body.String(), `"replay":true`) || strings.Contains(created.Body.String(), `"body"`) {
		t.Fatalf("create = %d %s; principal=%+v id=%q mutation=%q calls=%d", created.Code, created.Body, service.principal, service.id, service.mutationID, service.createCalls)
	}
	if string(service.body) != `{"title":"Learn Go","status":"active","steps":[]}` {
		t.Fatalf("create body = %s", service.body)
	}

	got := goalAPIRequest(router, http.MethodGet, "/api/goals/g_1", "", "")
	if got.Code != http.StatusOK || service.getCalls != 1 || service.id != "g_1" || !reflect.DeepEqual(service.principal, principal) ||
		!strings.Contains(got.Body.String(), `"object_type":"goal"`) || !strings.Contains(got.Body.String(), `"steps":[]`) {
		t.Fatalf("get = %d %s; id=%q principal=%+v calls=%d", got.Code, got.Body, service.id, service.principal, service.getCalls)
	}

	patched := goalAPIRequest(router, http.MethodPatch, "/api/goals/g_1", `{"base_revision":2,"body":{"title":"Learn Go","status":"active","steps":[]}}`, "goal-patch-1")
	if patched.Code != http.StatusOK || service.patchCalls != 1 || !reflect.DeepEqual(service.principal, principal) || service.id != "g_1" ||
		service.baseRevision != 2 || service.mutationID != "goal-patch-1" || string(service.body) != `{"title":"Learn Go","status":"active","steps":[]}` {
		t.Fatalf("patch = %d %s; principal=%+v id=%q revision=%d key=%q body=%s calls=%d", patched.Code, patched.Body, service.principal, service.id, service.baseRevision, service.mutationID, service.body, service.patchCalls)
	}

	deleted := goalAPIRequest(router, http.MethodDelete, "/api/goals/g_1", `{"base_revision":3}`, "goal-delete-1")
	if deleted.Code != http.StatusOK || service.deleteCalls != 1 || !reflect.DeepEqual(service.principal, principal) || service.id != "g_1" ||
		service.baseRevision != 3 || service.mutationID != "goal-delete-1" {
		t.Fatalf("delete = %d %s; principal=%+v id=%q revision=%d key=%q calls=%d", deleted.Code, deleted.Body, service.principal, service.id, service.baseRevision, service.mutationID, service.deleteCalls)
	}
}

func TestGoalRoutesRequirePrincipalAndMutationPreconditions(t *testing.T) {
	service := &goalRoutesServiceStub{}
	if response := goalAPIRequest(goalAPIRouter(nil, service), http.MethodGet, "/api/goals", "", ""); response.Code != http.StatusUnauthorized || service.listCalls != 0 {
		t.Fatalf("missing principal = %d %s calls=%d", response.Code, response.Body, service.listCalls)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := goalAPIRouter(&principal, service)
	missingCreateKey := goalAPIRequest(router, http.MethodPost, "/api/goals", `{"id":"g_1","body":{"title":"Learn Go","status":"active","steps":[]}}`, "")
	missingPatchRevision := goalAPIRequest(router, http.MethodPatch, "/api/goals/g_1", `{"body":{"title":"Learn Go","status":"active","steps":[]}}`, "goal-patch-1")
	missingDeleteRevision := goalAPIRequest(router, http.MethodDelete, "/api/goals/g_1", `{}`, "goal-delete-1")
	missingPatchKey := goalAPIRequest(router, http.MethodPatch, "/api/goals/g_1", `{"base_revision":0,"body":{"title":"Learn Go","status":"active","steps":[]}}`, "")
	if missingCreateKey.Code != http.StatusBadRequest || missingPatchRevision.Code != http.StatusBadRequest ||
		missingDeleteRevision.Code != http.StatusBadRequest || missingPatchKey.Code != http.StatusBadRequest ||
		service.createCalls+service.patchCalls+service.deleteCalls != 0 {
		t.Fatalf("precondition responses = create:%d patch-revision:%d delete-revision:%d patch-key:%d calls=%d", missingCreateKey.Code, missingPatchRevision.Code, missingDeleteRevision.Code, missingPatchKey.Code, service.createCalls+service.patchCalls+service.deleteCalls)
	}
}
