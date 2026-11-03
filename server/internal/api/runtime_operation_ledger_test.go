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
	"morde/server/internal/domain/identity"
)

type runtimeOperationLedgerStub struct {
	principal identity.Principal
	namespace string
	after     uint64
	limit     int32
	event     app.RuntimeOperationLedgerEvent
	listCalls int
}

func (s *runtimeOperationLedgerStub) ListRuntimeOperationEvents(_ context.Context, principal identity.Principal, namespace string, after uint64, limit int32) (app.RuntimeOperationLedgerPage, error) {
	s.principal, s.namespace, s.after, s.limit = principal, namespace, after, limit
	s.listCalls++
	return app.RuntimeOperationLedgerPage{Items: []app.RuntimeOperationLedgerEvent{{Version: 1, Sequence: 1, Kind: "runtime_state", Data: json.RawMessage(`{"mode":"blocked"}`), Hash: strings.Repeat("a", 64)}}}, nil
}

func TestHostReplicaRuntimeOperationLedgerRouteUsesReplicaAuthentication(t *testing.T) {
	services := newServices()
	services.hostReplica = &hostReplicaServiceStub{principal: identity.Principal{
		UserID: testUserID, Kind: identity.KindHostReplica, HostScope: testHostID, HostGeneration: 9,
	}}
	ledger := &runtimeOperationLedgerStub{}
	deps := services.deps()
	deps.RuntimeOperationLedger = ledger
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/host-replicas/" + testHostID + "/generations/9/operation-ledger/runtime_core"
	response := request(router, http.MethodGet, path, "", "Bearer replica-credential", "")
	if response.Code != http.StatusOK || ledger.listCalls != 1 || ledger.principal.Kind != identity.KindHostReplica ||
		ledger.principal.HostScope != testHostID || ledger.principal.HostGeneration != 9 {
		t.Fatalf("authenticated ledger read = %d %s, store = %#v", response.Code, response.Body, ledger)
	}
	response = request(router, http.MethodGet, path, "", "", "")
	if response.Code != http.StatusUnauthorized || ledger.listCalls != 1 {
		t.Fatalf("unauthenticated ledger read = %d %s, store calls=%d", response.Code, response.Body, ledger.listCalls)
	}
}

func (s *runtimeOperationLedgerStub) AppendRuntimeOperationEvent(_ context.Context, principal identity.Principal, namespace string, event app.RuntimeOperationLedgerEvent) (app.RuntimeOperationLedgerEvent, error) {
	s.principal, s.namespace, s.event = principal, namespace, event
	return event, nil
}

func TestRuntimeOperationLedgerRoutesRequirePrincipalAndPassBoundedPageValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &runtimeOperationLedgerStub{}
	handler := &api{deps: Deps{RuntimeOperationLedger: service}}
	engine := gin.New()
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindHostReplica, HostScope: "h1234567", HostGeneration: 9}
	group := engine.Group("/api/host-replicas/:host_id/generations/:generation", func(c *gin.Context) {
		c.Set(principalKey, principal)
		c.Next()
	})
	handler.registerRuntimeOperationLedgerRoutes(group)

	request := httptest.NewRequest(http.MethodGet, "/api/host-replicas/h1234567/generations/9/operation-ledger/runtime_core?after_sequence=5&limit=17", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET operation ledger status=%d body=%s", response.Code, response.Body.String())
	}
	if service.principal.UserID != principal.UserID || service.principal.Kind != principal.Kind || service.principal.HostScope != principal.HostScope ||
		service.principal.HostGeneration != principal.HostGeneration || service.namespace != "runtime_core" || service.after != 5 || service.limit != 17 {
		t.Fatalf("GET operation ledger service args = %#v", service)
	}

	body := `{"version":1,"sequence":1,"kind":"runtime_state","data":{"mode":"blocked"},"previous_hash":"","hash":"` + strings.Repeat("b", 64) + `"}`
	request = httptest.NewRequest(http.MethodPost, "/api/host-replicas/h1234567/generations/9/operation-ledger/runtime_core", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST operation ledger status=%d body=%s", response.Code, response.Body.String())
	}
	if service.principal.UserID != principal.UserID || service.principal.Kind != principal.Kind || service.principal.HostScope != principal.HostScope ||
		service.principal.HostGeneration != principal.HostGeneration || service.namespace != "runtime_core" || service.event.Sequence != 1 || service.event.Kind != "runtime_state" {
		t.Fatalf("POST operation ledger service args = %#v", service)
	}
}

func TestRuntimeOperationLedgerRoutesRejectRepeatedOrOutOfRangeQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &runtimeOperationLedgerStub{}
	handler := &api{deps: Deps{RuntimeOperationLedger: service}}
	engine := gin.New()
	group := engine.Group("/api/host-replicas/:host_id/generations/:generation", func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Kind: identity.KindHostReplica, HostScope: "h1234567", HostGeneration: 1})
		c.Next()
	})
	handler.registerRuntimeOperationLedgerRoutes(group)
	for _, path := range []string{
		"/api/host-replicas/h1234567/generations/1/operation-ledger/runtime_core?limit=1&limit=2",
		"/api/host-replicas/h1234567/generations/1/operation-ledger/runtime_core?limit=129",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}
