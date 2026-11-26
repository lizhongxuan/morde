package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"morde/server/internal/domain/identity"
)

type pcDeliveryObservationVerifier struct {
	verifierStub
	principal identity.Principal
}

func (v pcDeliveryObservationVerifier) VerifySession(context.Context, string) (identity.Principal, error) {
	return v.principal, nil
}

type pcDeliveryObservationHTTPStub struct {
	principal identity.Principal
	taskID    string
	messageID string
	revision  int64
	calls     int
	err       error
}

func (s *pcDeliveryObservationHTTPStub) ObservePCDelivery(_ context.Context, principal identity.Principal,
	taskID, messageID string, revision int64) error {
	s.principal, s.taskID, s.messageID, s.revision = principal, taskID, messageID, revision
	s.calls++
	return s.err
}

func pcDeliveryObservationRouter(t *testing.T, principal identity.Principal, service *pcDeliveryObservationHTTPStub) http.Handler {
	t.Helper()
	services := newServices()
	deps := services.deps()
	deps.Verifier = pcDeliveryObservationVerifier{principal: principal}
	deps.TaskPCDeliveryObservations = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestPCDeliveryObservationRequiresDesktopSessionAndPublishedReplyIdentity(t *testing.T) {
	principal := identity.Principal{UserID: testUserID, Role: identity.RoleUser, Kind: identity.KindSession,
		TokenID: testTokenID, ClientInstallationID: "i1234567"}
	service := &pcDeliveryObservationHTTPStub{}
	router := pcDeliveryObservationRouter(t, principal, service)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	path := "/api/tasks/q1234567/pc-delivery-observations"
	response := requestWithCookieBody(router, http.MethodPost, path,
		`{"message_id":"m1234567","task_revision":4}`, cookie, "https://portal.example.test")
	if response.Code != http.StatusNoContent || response.Header().Get("Cache-Control") != "no-store" || service.calls != 1 ||
		service.principal.UserID != principal.UserID || service.principal.TokenID != principal.TokenID ||
		service.principal.ClientInstallationID != principal.ClientInstallationID || service.principal.Kind != principal.Kind ||
		service.principal.Role != principal.Role || service.taskID != "q1234567" || service.messageID != "m1234567" || service.revision != 4 {
		t.Fatalf("PC observation status=%d headers=%v calls=%d principal=%+v task=%q message=%q revision=%d body=%s",
			response.Code, response.Header(), service.calls, service.principal, service.taskID, service.messageID, service.revision, response.Body)
	}

	invalid := []struct {
		name string
		path string
		body string
		want int
	}{
		{name: "unexpected query", path: path + "?extra=1", body: `{"message_id":"m1234567","task_revision":4}`, want: http.StatusBadRequest},
		{name: "unknown field", path: path, body: `{"message_id":"m1234567","task_revision":4,"content":"must not be accepted"}`, want: http.StatusBadRequest},
		{name: "negative revision", path: path, body: `{"message_id":"m1234567","task_revision":-1}`, want: http.StatusBadRequest},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			before := service.calls
			response := requestWithCookieBody(router, http.MethodPost, test.path, test.body, cookie, "https://portal.example.test")
			if response.Code != test.want || service.calls != before {
				t.Fatalf("invalid PC observation status=%d calls=%d (before %d), body=%s", response.Code, service.calls, before, response.Body)
			}
		})
	}

	withoutInstallation := principal
	withoutInstallation.ClientInstallationID = ""
	missingInstallationRouter := pcDeliveryObservationRouter(t, withoutInstallation, &pcDeliveryObservationHTTPStub{})
	missingInstallation := requestWithCookieBody(missingInstallationRouter, http.MethodPost, path,
		`{"message_id":"m1234567","task_revision":4}`, cookie, "https://portal.example.test")
	if missingInstallation.Code != http.StatusUnauthorized {
		t.Fatalf("session without desktop installation status=%d body=%s, want 401", missingInstallation.Code, missingInstallation.Body)
	}

	service.err = errors.New("delivery observation unavailable")
	failed := requestWithCookieBody(router, http.MethodPost, path,
		`{"message_id":"m1234567","task_revision":4}`, cookie, "https://portal.example.test")
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), service.err.Error()) {
		t.Fatalf("service failure was incorrectly acknowledged: status=%d headers=%v", failed.Code, failed.Header())
	}
}
