package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type controlServiceStub struct {
	status       app.ControlStatus
	statusErr    error
	statusCalls  int
	statusHostID string
	statusUserID string
	result       app.ControlActionResult
	actErr       error
	actCalls     int
	actHostID    string
	actPrincipal identity.Principal
	commands     []app.ControlActionCommand
}

func (s *controlServiceStub) Status(_ context.Context, principal identity.Principal, hostID string) (app.ControlStatus, error) {
	s.statusCalls++
	s.statusHostID, s.statusUserID = hostID, principal.UserID
	return s.status, s.statusErr
}

func (s *controlServiceStub) Act(_ context.Context, principal identity.Principal, hostID string, command app.ControlActionCommand) (app.ControlActionResult, error) {
	s.actCalls++
	s.actHostID, s.actPrincipal = hostID, principal
	s.commands = append(s.commands, command)
	return s.result, s.actErr
}

func controlRouter(t *testing.T, service ControlService) http.Handler {
	t.Helper()
	services := newServices()
	deps := services.deps()
	deps.Control = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestControlStatusRequiresAuthenticationAndValidHostID(t *testing.T) {
	service := &controlServiceStub{status: app.ControlStatus{HostID: testHostID, ControlState: host.ControlAI}}
	router := controlRouter(t, service)

	assertError(t, request(router, http.MethodGet, "/api/hosts/"+testHostID+"/control", "", "", ""), http.StatusUnauthorized, "unauthenticated")
	assertError(t, request(router, http.MethodGet, "/api/hosts/bad/control", "", "Bearer "+apiKeyCredential(), ""), http.StatusBadRequest, "invalid_argument")
	if service.statusCalls != 0 {
		t.Fatalf("service status calls = %d; want no calls for rejected requests", service.statusCalls)
	}
}

func TestGetControlReturnsPublicTransitionSummary(t *testing.T) {
	now := time.Date(2026, time.October, 21, 1, 2, 3, 0, time.UTC)
	service := &controlServiceStub{status: app.ControlStatus{
		HostID: testHostID, ControlState: host.ControlTakingOver, ControlVersion: 1,
		ActiveTransition: &control.Transition{
			HostID: testHostID, OwnerID: testUserID, RequestID: "handoff-42", Action: control.ActionTakeOver,
			Phase: control.PhaseDraining, FailureReason: "operations_pending", ExpectedControlVersion: 0, BlockedControlVersion: 1, TargetControlVersion: 2,
			TaskIDs: []string{"q1234567"}, DrainedReceipt: json.RawMessage(`{"private":"receipt"}`),
			CreatedAt: now, UpdatedAt: now,
		},
	}}
	response := request(controlRouter(t, service), http.MethodGet, "/api/hosts/"+testHostID+"/control", "", "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusOK || service.statusCalls != 1 || service.statusHostID != testHostID || service.statusUserID != testUserID {
		t.Fatalf("GET control = %d calls=%d host=%q user=%q body=%s", response.Code, service.statusCalls, service.statusHostID, service.statusUserID, response.Body)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"host_id", "control_state", "control_version", "active_transition"} {
		if len(body[field]) == 0 {
			t.Errorf("response is missing %q: %s", field, response.Body)
		}
	}
	if strings.Contains(response.Body.String(), "q1234567") || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "drained_receipt") {
		t.Fatalf("control response leaked internal task or receipt data: %s", response.Body)
	}
	if !strings.Contains(response.Body.String(), `"failure_reason":"operations_pending"`) {
		t.Fatalf("control response omitted the safe failure reason: %s", response.Body)
	}
}

func TestControlActionValidatesRequestAndReturnsPendingAsAccepted(t *testing.T) {
	service := &controlServiceStub{result: app.ControlActionResult{
		Status: app.ControlStatus{
			HostID: testHostID, ControlState: host.ControlTakingOver, ControlVersion: 1,
			ActiveTransition: &control.Transition{
				HostID: testHostID, RequestID: "takeover-001", Action: control.ActionTakeOver,
				Phase: control.PhaseDraining, ExpectedControlVersion: 0, BlockedControlVersion: 1, TargetControlVersion: 2,
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			},
		},
		PendingOperationIDs: []string{"o1234567"},
	}}
	router := controlRouter(t, service)
	for _, body := range []string{
		`{"action":"take_over","request_id":"takeover-001"}`,
		`{"action":"unknown","request_id":"takeover-001","expected_control_version":0}`,
		`{"action":"take_over","request_id":"bad id","expected_control_version":0}`,
		`{"action":"take_over","request_id":"takeover-001","expected_control_version":-1}`,
		`{"action":"take_over","request_id":"takeover-001","expected_control_version":0,"unexpected":true}`,
	} {
		assertError(t, request(router, http.MethodPost, "/api/hosts/"+testHostID+"/control/actions", body, "Bearer "+apiKeyCredential(), ""), http.StatusBadRequest, "invalid_argument")
	}
	if service.actCalls != 0 {
		t.Fatalf("invalid action service calls = %d; want 0", service.actCalls)
	}

	const body = `{"action":"take_over","request_id":"takeover-001","expected_control_version":0}`
	for i := 0; i < 2; i++ {
		response := request(router, http.MethodPost, "/api/hosts/"+testHostID+"/control/actions", body, "Bearer "+apiKeyCredential(), "")
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status = %d body=%s; want 202", i+1, response.Code, response.Body)
		}
		var decoded controlActionResponse
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.HostID != testHostID || decoded.ControlState != host.ControlTakingOver || len(decoded.PendingOperationIDs) != 1 || decoded.PendingOperationIDs[0] != "o1234567" {
			t.Fatalf("attempt %d response = %#v", i+1, decoded)
		}
	}
	if service.actCalls != 2 || service.actHostID != testHostID || service.actPrincipal.UserID != testUserID || len(service.commands) != 2 {
		t.Fatalf("act calls=%d host=%q principal=%#v commands=%#v", service.actCalls, service.actHostID, service.actPrincipal, service.commands)
	}
	for _, command := range service.commands {
		if command.Action != control.ActionTakeOver || command.RequestID != "takeover-001" || command.ExpectedControlVersion != 0 {
			t.Fatalf("command = %#v", command)
		}
	}
}

func TestControlActionReturnsOKWhenNoOperationsRemain(t *testing.T) {
	service := &controlServiceStub{result: app.ControlActionResult{
		Status: app.ControlStatus{HostID: testHostID, ControlState: host.ControlUser, ControlVersion: 2},
	}}
	response := request(controlRouter(t, service), http.MethodPost, "/api/hosts/"+testHostID+"/control/actions",
		`{"action":"take_over","request_id":"takeover-002","expected_control_version":0}`, "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s; want 200", response.Code, response.Body)
	}
	var decoded controlActionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PendingOperationIDs == nil || len(decoded.PendingOperationIDs) != 0 || decoded.ActiveTransition != nil || decoded.ControlVersion != 2 {
		t.Fatalf("response = %#v", decoded)
	}
}
