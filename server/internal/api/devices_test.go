package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type deviceServiceStub struct {
	items      []app.DeviceInfo
	listErr    error
	renameErr  error
	revokeErr  error
	principal  identity.Principal
	renameID   string
	renameName string
	revokeID   string
	revokeIP   string
}

func (s *deviceServiceStub) List(_ context.Context, principal identity.Principal) ([]app.DeviceInfo, error) {
	s.principal = principal
	return s.items, s.listErr
}

func (s *deviceServiceStub) Rename(_ context.Context, principal identity.Principal, id, name string) error {
	s.principal, s.renameID, s.renameName = principal, id, name
	return s.renameErr
}

func (s *deviceServiceStub) Revoke(_ context.Context, principal identity.Principal, id, ip string) error {
	s.principal, s.revokeID, s.revokeIP = principal, id, ip
	return s.revokeErr
}

func deviceAPIRouter(principal *identity.Principal, service deviceService) *gin.Engine {
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Set(principalKey, *principal)
			c.Next()
		})
	}
	registerDeviceRoutes(router.Group("/api"), service)
	return router
}

func TestListDevicesReturnsInstallationPresenceAndEmptyArray(t *testing.T) {
	name := "Work Mac"
	created := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	service := &deviceServiceStub{items: []app.DeviceInfo{{
		ID: "i1234567", DisplayName: name, Platform: "desktop", ExpiresAt: created.Add(time.Hour),
		CreatedAt: created, Online: true,
	}}}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	response := httptest.NewRecorder()
	deviceAPIRouter(&principal, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if response.Code != http.StatusOK || service.principal.UserID != principal.UserID {
		t.Fatalf("GET /devices status=%d body=%s principal=%#v", response.Code, response.Body, service.principal)
	}
	var body struct {
		Items []deviceInfoResponse `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Items) != 1 || body.Items[0].ID != "i1234567" || !body.Items[0].Online || body.Items[0].DisplayName != name {
		t.Fatalf("GET /devices body = %s, decoded %#v, err %v", response.Body, body, err)
	}

	service.items = nil
	response = httptest.NewRecorder()
	deviceAPIRouter(&principal, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty device list status=%d body=%s", response.Code, response.Body)
	}
}

func TestDeviceRoutesRequirePrincipalAndReturnServiceErrors(t *testing.T) {
	service := &deviceServiceStub{}
	response := httptest.NewRecorder()
	deviceAPIRouter(nil, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /devices status=%d body=%s", response.Code, response.Body)
	}

	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	service.listErr = domain.NewError(domain.CodeForbidden, "not allowed")
	response = httptest.NewRecorder()
	deviceAPIRouter(&principal, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("service error status=%d body=%s", response.Code, response.Body)
	}
}

func TestRenameDeviceValidatesBodyAndReturnsNoContent(t *testing.T) {
	service := &deviceServiceStub{}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	request := httptest.NewRequest(http.MethodPatch, "/api/devices/i1234567", strings.NewReader(`{"display_name":"Travel Mac"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	deviceAPIRouter(&principal, service).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || service.principal.UserID != principal.UserID || service.renameID != "i1234567" || service.renameName != "Travel Mac" {
		t.Fatalf("PATCH /devices status=%d body=%s service=%#v", response.Code, response.Body, service)
	}

	for _, body := range []string{`{"display_name":"x","owner_id":"u7654321"}`, `not json`} {
		request = httptest.NewRequest(http.MethodPatch, "/api/devices/i1234567", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response = httptest.NewRecorder()
		deviceAPIRouter(&principal, service).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("PATCH invalid body=%q status=%d body=%s", body, response.Code, response.Body)
		}
	}
	request = httptest.NewRequest(http.MethodPatch, "/api/devices/bad", strings.NewReader(`{"display_name":"x"}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	deviceAPIRouter(&principal, service).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("PATCH invalid id status=%d body=%s", response.Code, response.Body)
	}

	service.renameErr = errors.New("database unavailable")
	request = httptest.NewRequest(http.MethodPatch, "/api/devices/i1234567", strings.NewReader(`{"display_name":"Travel Mac"}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	deviceAPIRouter(&principal, service).ServeHTTP(response, request)
	if response.Code < http.StatusInternalServerError {
		t.Fatalf("PATCH service error status=%d body=%s", response.Code, response.Body)
	}
}

func TestDeleteDeviceRevokesUsingRequestIdentityAndIP(t *testing.T) {
	service := &deviceServiceStub{}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	request := httptest.NewRequest(http.MethodDelete, "/api/devices/i1234567", nil)
	request.RemoteAddr = "192.0.2.15:1234"
	response := httptest.NewRecorder()
	deviceAPIRouter(&principal, service).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || service.principal.UserID != principal.UserID || service.revokeID != "i1234567" || service.revokeIP != "192.0.2.15" {
		t.Fatalf("DELETE /devices status=%d body=%s service=%#v", response.Code, response.Body, service)
	}
}
