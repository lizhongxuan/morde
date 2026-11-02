package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalDataServiceStub struct {
	principal  identity.Principal
	objectType string
	objectID   string
	object     personal.Object
	command    app.PersonalMutationCommand
	receipt    personal.MutationReceipt
	getCalls   int
	putCalls   int
	err        error
}

func (s *personalDataServiceStub) GetPersonalObject(_ context.Context, principal identity.Principal, objectType, objectID string) (personal.Object, error) {
	s.getCalls++
	s.principal, s.objectType, s.objectID = principal, objectType, objectID
	return s.object, s.err
}

func TestPersonalDataRoutesForwardAllowlistedContentTypeAndID(t *testing.T) {
	service := &personalDataServiceStub{receipt: personal.MutationReceipt{
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", ResultRevision: 1, AccountChangeSequence: 1,
	}}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := personalDataAPIRouter(&principal, service)
	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/personal-data/memory/global", nil))
	if get.Code != http.StatusOK || service.objectType != personal.ObjectTypeMemory || service.objectID != "global" {
		t.Fatalf("memory GET = %d %s, forwarded %s/%s", get.Code, get.Body, service.objectType, service.objectID)
	}

	put := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/personal-data/memory/global",
		strings.NewReader(`{"base_revision":0,"body":{"content":"A user-selected memory."}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "memory-write-1")
	router.ServeHTTP(put, request)
	if put.Code != http.StatusOK || service.command.ObjectType != personal.ObjectTypeMemory ||
		service.command.ObjectID != "global" || string(service.command.Body) != `{"content":"A user-selected memory."}` {
		t.Fatalf("memory PUT = %d %s, command %+v", put.Code, put.Body, service.command)
	}
}

func (s *personalDataServiceStub) ApplyPersonalMutation(_ context.Context, principal identity.Principal, command app.PersonalMutationCommand) (personal.MutationReceipt, error) {
	s.putCalls++
	s.principal, s.command = principal, command
	return s.receipt, s.err
}

func personalDataAPIRouter(principal *identity.Principal, service personalDataService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	registerPersonalDataRoutes(router.Group("/api"), service)
	return router
}

func TestPersonalDataGetReturnsOwnerScopedObject(t *testing.T) {
	service := &personalDataServiceStub{object: personal.Object{
		ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, Revision: 4,
		Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`),
	}}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	response := httptest.NewRecorder()
	personalDataAPIRouter(&principal, service).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/personal-data/profile/default", nil))
	var object personalObjectResponse
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil || response.Code != http.StatusOK ||
		service.getCalls != 1 || service.principal.UserID != principal.UserID || object.Revision != 4 ||
		string(object.Body) != `{"name":"Muse","avatar":"dragon"}` {
		t.Fatalf("status=%d object=%+v calls=%d body=%s err=%v", response.Code, object, service.getCalls, response.Body, err)
	}
}

func TestPersonalDataPutUsesBaseRevisionAndIdempotencyKey(t *testing.T) {
	service := &personalDataServiceStub{receipt: personal.MutationReceipt{
		ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID,
		ResultRevision: 5, AccountChangeSequence: 19,
	}}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	request := httptest.NewRequest(http.MethodPut, "/api/personal-data/profile/default",
		strings.NewReader(`{"base_revision":4,"body":{"name":"Muse","avatar":"dragon"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "profile-write-001")
	response := httptest.NewRecorder()
	personalDataAPIRouter(&principal, service).ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.putCalls != 1 || service.command.MutationID != "profile-write-001" ||
		service.command.BaseRevision != 4 || string(service.command.Body) != `{"name":"Muse","avatar":"dragon"}` {
		t.Fatalf("status=%d command=%+v calls=%d body=%s", response.Code, service.command, service.putCalls, response.Body)
	}
}

func TestPersonalDataMutationRequiresIdempotencyKey(t *testing.T) {
	service := &personalDataServiceStub{}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	request := httptest.NewRequest(http.MethodPut, "/api/personal-data/profile/default",
		strings.NewReader(`{"base_revision":0,"body":{"name":"Muse","avatar":"dragon"}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	personalDataAPIRouter(&principal, service).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.putCalls != 0 {
		t.Fatalf("missing idempotency key status=%d calls=%d body=%s", response.Code, service.putCalls, response.Body)
	}
}

func TestPersonalDataAndDeviceRoutesAreRegisteredForClientCredentials(t *testing.T) {
	services := newServices()
	personalData := &personalDataServiceStub{object: personal.Object{
		ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, Revision: 4,
		Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`),
	}, receipt: personal.MutationReceipt{
		ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID,
		ResultRevision: 5, AccountChangeSequence: 12,
	}}
	deps := services.deps()
	deps.PersonalData, deps.Devices = personalData, &deviceServiceStub{}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	authorization := "Bearer " + credential

	response := request(router, http.MethodGet, "/api/personal-data/profile/default", "", authorization, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":4`) || personalData.getCalls != 1 {
		t.Fatalf("GET personal data = %d %s calls=%d", response.Code, response.Body, personalData.getCalls)
	}
	response = requestWithHeader(router, http.MethodPut, "/api/personal-data/profile/default",
		`{"base_revision":4,"body":{"name":"Updated","avatar":"dragon"}}`, authorization, "", "Idempotency-Key", "profile-change-001")
	if response.Code != http.StatusOK || personalData.putCalls != 1 || personalData.command.MutationID != "profile-change-001" ||
		personalData.command.BaseRevision != 4 || personalData.principal.Kind != identity.KindClient ||
		!strings.Contains(response.Body.String(), `"account_change_sequence":12`) || strings.Contains(response.Body.String(), `"body"`) {
		t.Fatalf("PUT personal data = %d %s command=%+v principal=%+v", response.Code, response.Body, personalData.command, personalData.principal)
	}
	response = requestWithHeader(router, http.MethodDelete, "/api/personal-data/profile/default",
		`{"base_revision":5}`, authorization, "", "Idempotency-Key", "profile-delete-001")
	if response.Code != http.StatusOK || !personalData.command.Delete || personalData.command.BaseRevision != 5 ||
		personalData.command.Body != nil || personalData.command.MutationID != "profile-delete-001" {
		t.Fatalf("DELETE personal data = %d %s command=%+v", response.Code, response.Body, personalData.command)
	}
	response = request(router, http.MethodGet, "/api/devices", "", authorization, "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET devices = %d %s", response.Code, response.Body)
	}
	largeBody := `{"base_revision":0,"body":{"name":"` + strings.Repeat("x", personal.MaxMutationBodyBytes+4096) + `","avatar":"dragon"}}`
	previousCalls := personalData.putCalls
	response = requestWithHeader(router, http.MethodPut, "/api/personal-data/profile/default", largeBody,
		authorization, "", "Idempotency-Key", "profile-change-large")
	if response.Code != http.StatusRequestEntityTooLarge || personalData.putCalls != previousCalls {
		t.Fatalf("oversized personal data body = %d %s calls=%d, want 413 without service call", response.Code, response.Body, personalData.putCalls)
	}
}

func TestPersonalRevisionConflictExposesOnlyRevisionDetails(t *testing.T) {
	router := gin.New()
	router.GET("/conflict", func(c *gin.Context) {
		err := domain.NewError(domain.CodeConflict, "untrusted message")
		err.Details["reason"] = "personal_object_revision_changed"
		err.Details["base_revision"] = int64(2)
		err.Details["current_revision"] = int64(3)
		err.Details["deleted"] = false
		err.Details["body"] = "private profile content"
		writeError(c, err)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/conflict", nil))
	var result struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if response.Code != http.StatusConflict || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("personal conflict = %d %s", response.Code, response.Body)
	}
	if result.Error.Details["base_revision"] != float64(2) ||
		result.Error.Details["current_revision"] != float64(3) || result.Error.Details["deleted"] != false {
		t.Fatalf("personal conflict details = %#v", result.Error.Details)
	}
	if len(result.Error.Details) != 3 || strings.Contains(response.Body.String(), "private profile content") {
		t.Fatalf("personal conflict exposed profile content: %s", response.Body)
	}
}
