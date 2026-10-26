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

	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
)

type executionControlServiceStub struct {
	principal identity.Principal
	control   execution.Control
	epoch     int64
	key       string
	call      string
}

func (s *executionControlServiceStub) Status(_ context.Context, principal identity.Principal) (execution.Control, error) {
	s.principal, s.call = principal, "status"
	return s.control, nil
}

func (s *executionControlServiceStub) StopAll(_ context.Context, principal identity.Principal, epoch int64, key string) (execution.Control, error) {
	s.principal, s.call, s.epoch, s.key = principal, "stop", epoch, key
	return s.control, nil
}

func (s *executionControlServiceStub) Enable(_ context.Context, principal identity.Principal, epoch int64, key string) (execution.Control, error) {
	s.principal, s.call, s.epoch, s.key = principal, "enable", epoch, key
	return s.control, nil
}

func TestExecutionControlRoutesPassSessionAndReturnPublicShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &executionControlServiceStub{control: execution.Control{
		OwnerID: principal.UserID, State: execution.StateStopping, Epoch: 5, PendingTaskCount: 2, UpdatedAt: time.Now().UTC(),
	}}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(principalKey, principal); c.Next() })
	handler := &api{}
	handler.registerExecutionControl(engine.Group("/api"), service)

	get := httptest.NewRecorder()
	engine.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/execution", nil))
	var view map[string]json.RawMessage
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if get.Code != http.StatusOK || string(view["state"]) != `"stopping"` || string(view["epoch"]) != "5" || string(view["pending_task_count"]) != "2" {
		t.Fatalf("GET /api/execution = %d %s", get.Code, get.Body)
	}
	if view["owner_id"] != nil || view["updated_at"] != nil {
		t.Fatalf("internal execution fields leaked into API response: %s", get.Body)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/execution/stop-all", strings.NewReader(`{"expected_epoch":4}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "stop-control-key-01")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.call != "stop" || service.epoch != 4 || service.key != "stop-control-key-01" || service.principal.UserID != principal.UserID {
		t.Fatalf("POST stop-all = %d call=%s epoch=%d key=%q principal=%#v body=%s", response.Code, service.call, service.epoch, service.key, service.principal, response.Body)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/execution/enable", strings.NewReader(`{"expected_epoch":5,"unexpected":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "enable-control-key-01")
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.call != "stop" {
		t.Fatalf("unknown field was not rejected before service call: %d %s", response.Code, response.Body)
	}
}
