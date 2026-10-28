package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
)

type executionControlServiceStub struct {
	status       execution.Control
	stopResult   execution.Control
	enableResult execution.Control
	principal    identity.Principal
	key          string
	epoch        int64
	action       string
	calls        int
}

func (s *executionControlServiceStub) Status(_ context.Context, principal identity.Principal) (execution.Control, error) {
	s.principal, s.action = principal, "status"
	s.calls++
	return s.status, nil
}

func (s *executionControlServiceStub) StopAll(_ context.Context, principal identity.Principal, epoch int64, key string) (execution.Control, error) {
	s.principal, s.epoch, s.key, s.action = principal, epoch, key, "stop_all"
	s.calls++
	return s.stopResult, nil
}

func (s *executionControlServiceStub) Enable(_ context.Context, principal identity.Principal, epoch int64, key string) (execution.Control, error) {
	s.principal, s.epoch, s.key, s.action = principal, epoch, key, "enable"
	s.calls++
	return s.enableResult, nil
}

func TestExecutionControlRoutesUseSessionAndExpectedEpoch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	service := &executionControlServiceStub{
		status:       execution.Control{OwnerID: testUserID, State: execution.StateEnabled, Epoch: 4, Runtime: execution.Runtime{State: execution.RuntimeStateNotStarted}, UpdatedAt: now},
		stopResult:   execution.Control{OwnerID: testUserID, State: execution.StateStopping, Epoch: 5, PendingTaskCount: 2, Runtime: execution.Runtime{State: execution.RuntimeStateNotStarted}, UpdatedAt: now},
		enableResult: execution.Control{OwnerID: testUserID, State: execution.StateEnabled, Epoch: 6, Runtime: execution.Runtime{State: execution.RuntimeStateNotStarted}, UpdatedAt: now},
	}
	services := newServices()
	services.executionControl = service
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	response := requestWithCookie(router, http.MethodGet, "/api/execution", cookie, "")
	var status executionControlResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.State != execution.StateEnabled || status.Epoch != 4 ||
		status.Runtime.State != execution.RuntimeStateNotStarted {
		t.Fatalf("GET /api/execution = %d %s", response.Code, response.Body)
	}
	for _, privateField := range []string{"nonce_sha256", "lease_owner", "control_version", "guardian_stop_attestation"} {
		if strings.Contains(response.Body.String(), privateField) {
			t.Fatalf("GET /api/execution exposed internal Runtime field %q: %s", privateField, response.Body)
		}
	}
	if service.principal.Kind != identity.KindSession || service.action != "status" {
		t.Fatalf("status call principal/action = %#v/%q", service.principal, service.action)
	}

	response = requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/execution/stop-all", `{"expected_epoch":4}`, cookie,
		"https://portal.example.test", "Idempotency-Key", "stop-control-0001")
	if response.Code != http.StatusAccepted || service.action != "stop_all" || service.epoch != 4 || service.key != "stop-control-0001" || service.principal.Kind != identity.KindSession {
		t.Fatalf("POST stop-all = %d %s; call=%#v", response.Code, response.Body, service)
	}

	response = requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/execution/enable", `{"expected_epoch":5}`, cookie,
		"https://portal.example.test", "Idempotency-Key", "enable-control-0001")
	if response.Code != http.StatusOK || service.action != "enable" || service.epoch != 5 || service.key != "enable-control-0001" {
		t.Fatalf("POST enable = %d %s; call=%#v", response.Code, response.Body, service)
	}
}

func TestExecutionControlRejectsInvalidRuntimeSummary(t *testing.T) {
	_, err := toExecutionControlResponse(execution.Control{OwnerID: testUserID, State: execution.StateEnabled, Epoch: 1,
		Runtime: execution.Runtime{State: "unknown"}, UpdatedAt: time.Now().UTC()}, testUserID)
	if err == nil {
		t.Fatal("execution response accepted an unknown Runtime state")
	}
}

func TestExecutionControlMutationRejectsInvalidRequestBeforeCallingService(t *testing.T) {
	service := &executionControlServiceStub{stopResult: execution.Control{
		OwnerID: testUserID, State: execution.StateDisabled, Epoch: 5, Runtime: execution.Runtime{State: execution.RuntimeStateNotStarted}, UpdatedAt: time.Now().UTC(),
	}}
	services := newServices()
	services.executionControl = service
	router := newRouter(t, services, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	for _, request := range []struct {
		body string
		key  string
	}{
		{body: `{}`, key: "stop-control-0001"},
		{body: `{"expected_epoch":0}`, key: "stop-control-0002"},
		{body: `{"expected_epoch":1,"unknown":true}`, key: "stop-control-0003"},
	} {
		response := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/execution/stop-all", request.body, cookie,
			"https://portal.example.test", "Idempotency-Key", request.key)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid request %s / %q = %d %s", request.body, request.key, response.Code, response.Body)
		}
	}
	missingKey := requestWithCookieBody(router, http.MethodPost, "/api/execution/stop-all", `{"expected_epoch":1}`, cookie,
		"https://portal.example.test")
	if missingKey.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key = %d %s", missingKey.Code, missingKey.Body)
	}
	if service.calls != 0 {
		t.Fatalf("invalid requests reached service %d times", service.calls)
	}
}
