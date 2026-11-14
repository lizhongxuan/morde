package api

import (
	"net/http"
	"testing"
)

func TestCodexClientCredentialReadAllowlist(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/codex/sessions"},
		{http.MethodGet, "/api/codex/sessions/:id/events"},
		{http.MethodGet, "/api/codex/sessions/:id/events/stream"},
		{http.MethodPost, "/api/codex/sessions/:id/commands"},
	} {
		if !clientCredentialRoute(route.method, route.path) {
			t.Errorf("clientCredentialRoute(%q, %q) = false", route.method, route.path)
		}
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodDelete, "/api/codex/sessions/:id"},
	} {
		if clientCredentialRoute(route.method, route.path) {
			t.Errorf("clientCredentialRoute(%q, %q) = true", route.method, route.path)
		}
	}
}
