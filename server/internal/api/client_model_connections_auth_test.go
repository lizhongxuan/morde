package api

import (
	"net/http"
	"testing"
)

func TestClientCredentialRouteAllowsOwnerModelConnectionOperations(t *testing.T) {
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/model-connections"},
		{http.MethodGet, "/api/model-connections/:id"},
		{http.MethodGet, "/api/me/main-conversation"},
		{http.MethodGet, "/api/hosts/:id/control"},
		{http.MethodPost, "/api/me/main-conversation/ensure"},
		{http.MethodPost, "/api/hosts/:id/control/actions"},
		{http.MethodPost, "/api/hosts/:id/control/lease/renew"},
		{http.MethodPut, "/api/me/main-conversation"},
		{http.MethodPost, "/api/model-connections"},
		{http.MethodPost, "/api/model-connections/:id/check"},
		{http.MethodPut, "/api/model-connections/:id"},
		{http.MethodDelete, "/api/model-connections/:id"},
	} {
		if !clientCredentialRoute(route.method, route.path) {
			t.Errorf("clientCredentialRoute(%q, %q) = false", route.method, route.path)
		}
	}
	if clientCredentialRoute(http.MethodPut, "/api/admin/settings/model-default") {
		t.Fatal("client credential can update the administrator model default")
	}
}
