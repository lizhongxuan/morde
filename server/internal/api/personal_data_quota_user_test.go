package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type personalDataQuotaUserServiceStub struct {
	principal identity.Principal
	quota     app.PersonalDataQuota
	calls     int
	err       error
}

func (s *personalDataQuotaUserServiceStub) GetPersonalDataQuota(_ context.Context, principal identity.Principal) (app.PersonalDataQuota, error) {
	s.calls++
	s.principal = principal
	return s.quota, s.err
}

func personalDataQuotaUserRouter(principal *identity.Principal, service personalDataQuotaUserService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	RegisterPersonalDataQuotaRoute(router.Group("/api/personal-data"), service)
	return router
}

func personalDataQuotaUserRequest(router http.Handler, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	return response
}

func TestPersonalDataQuotaUserRouteReturnsOwnerQuotaAndRemainingBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name      string
		principal identity.Principal
		quota     app.PersonalDataQuota
		remaining int64
	}{
		{
			name:      "owner session",
			principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
			quota: app.PersonalDataQuota{OwnerID: "u1234567", UsedBytes: 20, ReservedBytes: 10, QuotaBytes: 100,
				MaxAttachmentBytes: 40, Version: 2},
			remaining: 70,
		},
		{
			name: "client credential owner",
			principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
				TokenID: "t1234567", ClientInstallationID: "i1234567"},
			quota: app.PersonalDataQuota{OwnerID: "u1234567", UsedBytes: 90, ReservedBytes: 20, QuotaBytes: 100,
				MaxAttachmentBytes: 40, Version: 3},
			remaining: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &personalDataQuotaUserServiceStub{quota: test.quota}
			router := personalDataQuotaUserRouter(&test.principal, service)
			response := personalDataQuotaUserRequest(router, "/api/personal-data/quota?owner_id=u7654321")
			var got personalDataQuotaUserResponse
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil {
				t.Fatalf("GET quota = %d %s", response.Code, response.Body)
			}
			if service.calls != 1 || !reflect.DeepEqual(service.principal, test.principal) || got.OwnerID != test.principal.UserID ||
				got.UsedBytes != test.quota.UsedBytes || got.ReservedBytes != test.quota.ReservedBytes || got.QuotaBytes != test.quota.QuotaBytes ||
				got.MaxAttachmentBytes != test.quota.MaxAttachmentBytes || got.RemainingBytes != test.remaining || got.Version != test.quota.Version {
				t.Fatalf("response=%+v service=%+v, want owner=%s remaining=%d", got, service, test.principal.UserID, test.remaining)
			}
		})
	}
}

func TestPersonalDataQuotaUserRouteRejectsMissingPrincipalAndInvalidState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &personalDataQuotaUserServiceStub{quota: app.PersonalDataQuota{
		OwnerID: "u1234567", QuotaBytes: 100, MaxAttachmentBytes: 40, Version: 1,
	}}
	missingPrincipal := personalDataQuotaUserRequest(personalDataQuotaUserRouter(nil, service), "/api/personal-data/quota")
	if missingPrincipal.Code != http.StatusUnauthorized || service.calls != 0 {
		t.Fatalf("missing principal = %d %s calls=%d", missingPrincipal.Code, missingPrincipal.Body, service.calls)
	}

	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service.quota.OwnerID = "u7654321"
	invalid := personalDataQuotaUserRequest(personalDataQuotaUserRouter(&principal, service), "/api/personal-data/quota")
	if invalid.Code != http.StatusInternalServerError || service.calls != 1 {
		t.Fatalf("cross-account service receipt = %d %s calls=%d", invalid.Code, invalid.Body, service.calls)
	}
	service.quota = app.PersonalDataQuota{OwnerID: principal.UserID, UsedBytes: -1, QuotaBytes: 100, MaxAttachmentBytes: 40, Version: 1}
	badState := personalDataQuotaUserRequest(personalDataQuotaUserRouter(&principal, service), "/api/personal-data/quota")
	if badState.Code != http.StatusInternalServerError || service.calls != 2 {
		t.Fatalf("invalid quota state = %d %s calls=%d", badState.Code, badState.Body, service.calls)
	}
}

func TestPersonalDataQuotaUserRouteHandlesServiceErrorsAndUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &personalDataQuotaUserServiceStub{err: domain.NewError(domain.CodeForbidden, "access denied")}
	response := personalDataQuotaUserRequest(personalDataQuotaUserRouter(&principal, service), "/api/personal-data/quota")
	if response.Code != http.StatusForbidden || service.calls != 1 {
		t.Fatalf("service error = %d %s calls=%d", response.Code, response.Body, service.calls)
	}
	unavailable := personalDataQuotaUserRequest(personalDataQuotaUserRouter(&principal, nil), "/api/personal-data/quota")
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable service = %d %s", unavailable.Code, unavailable.Body)
	}
}

func TestPersonalDataQuotaUserAllowsClientCredentialRoute(t *testing.T) {
	if !clientCredentialRoute(http.MethodGet, "/api/personal-data/quota") {
		t.Fatal("client credential cannot reach personal-data quota route")
	}
	if clientCredentialRoute(http.MethodPost, "/api/personal-data/quota") {
		t.Fatal("client credential unexpectedly has a write route for personal-data quota")
	}
}
