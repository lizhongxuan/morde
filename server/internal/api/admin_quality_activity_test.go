package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type adminQualityActivityServiceStub struct {
	quality   app.AdminQuality
	activity  app.AdminActivity
	places    app.AdminPlaces
	principal identity.Principal
	days      int
	calls     int
}

func (s *adminQualityActivityServiceStub) Quality(_ context.Context, principal identity.Principal, days int) (app.AdminQuality, error) {
	s.principal, s.days, s.calls = principal, days, s.calls+1
	return s.quality, nil
}

func (s *adminQualityActivityServiceStub) Activity(_ context.Context, principal identity.Principal, days int) (app.AdminActivity, error) {
	s.principal, s.days, s.calls = principal, days, s.calls+1
	return s.activity, nil
}

func (s *adminQualityActivityServiceStub) Places(_ context.Context, principal identity.Principal, days int) (app.AdminPlaces, error) {
	s.principal, s.days, s.calls = principal, days, s.calls+1
	return s.places, nil
}

func TestAdminQualityActivityRoutesAuthorizeValidateAndReturnAggregates(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	activity := &adminQualityActivityServiceStub{
		quality: app.AdminQuality{Days: 30, Available: false, UnavailableReason: "no consented sample source"},
		activity: app.AdminActivity{Days: 7, From: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
			ByDay:         []app.AdminActivityDay{{Date: "2026-11-02", HubOperations: 2}},
			HubOperations: []app.AdminHubOperationCount{{Action: "files", Status: "succeeded", Count: 2}}},
		places: app.AdminPlaces{Days: 90, Available: false, UnavailableReason: "no aggregated location source"},
	}
	deps := services.deps()
	deps.AdminQualityActivity = activity
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	quality := requestWithCookie(router, http.MethodGet, "/api/admin/quality?days=30", cookie, "")
	if quality.Code != http.StatusOK || quality.Body.String() != `{"days":30,"available":false,"unavailable_reason":"no consented sample source","sample_count":0,"failed_tasks":0,"failed_hub_operations":0,"unknown_hub_operations":0}` || activity.days != 30 {
		t.Fatalf("quality = %d %s days=%d", quality.Code, quality.Body, activity.days)
	}
	response := requestWithCookie(router, http.MethodGet, "/api/admin/activity?days=7", cookie, "")
	if response.Code != http.StatusOK || activity.calls != 2 || activity.principal.Role != identity.RoleAdmin {
		t.Fatalf("activity = %d %s calls=%d principal=%+v", response.Code, response.Body, activity.calls, activity.principal)
	}
	if body := response.Body.String(); body == "" || containsAny(body, `"payload"`, `"ip"`, `"owner_id"`, `"installation_id"`, `"request_hash"`) {
		t.Fatalf("activity response includes sensitive fields: %s", body)
	}
	places := requestWithCookie(router, http.MethodGet, "/api/admin/places?days=90", cookie, "")
	if places.Code != http.StatusOK || activity.days != 90 || !containsAny(places.Body.String(), "no aggregated location source") {
		t.Fatalf("places = %d %s days=%d", places.Code, places.Body, activity.days)
	}
	for _, path := range []string{"/api/admin/activity?days=8", "/api/admin/activity?days=7&days=30"} {
		bad := requestWithCookie(router, http.MethodGet, path, cookie, "")
		if bad.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d %s; want 400", path, bad.Code, bad.Body)
		}
	}
	if activity.calls != 3 {
		t.Fatalf("invalid requests reached service; calls=%d", activity.calls)
	}

	services.verifier.role = identity.RoleUser
	userRouter, err := New(services.deps(), validOptions())
	if err != nil {
		t.Fatal(err)
	}
	denied := requestWithCookie(userRouter, http.MethodGet, "/api/admin/activity", cookie, "")
	if denied.Code != http.StatusForbidden || activity.calls != 3 {
		t.Fatalf("non-admin activity = %d %s calls=%d", denied.Code, denied.Body, activity.calls)
	}
}

func containsAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
