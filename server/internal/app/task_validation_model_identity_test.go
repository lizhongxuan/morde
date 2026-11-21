package app

import "testing"

func TestResolveTaskValidationModelIdentityAcceptsCatalogEntries(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		configured string
		reported   string
		wantFamily string
	}{
		{name: "openai luna", baseURL: "https://api.openai.com/v1", configured: "gpt-6-luna", reported: "gpt-6-luna", wantFamily: "openai-gpt6"},
		{name: "openai astra same family", baseURL: "https://api.openai.com/v1", configured: "gpt-6.2-astra", reported: "gpt-6.2-astra", wantFamily: "openai-gpt6"},
		{name: "openai sol same family", baseURL: "https://api.openai.com/v1", configured: "gpt-6.2-sol", reported: "gpt-6.2-sol", wantFamily: "openai-gpt6"},
		{name: "deepseek flash", baseURL: "https://api.deepseek.com", configured: "deepseek-flash", reported: "deepseek-flash", wantFamily: "deepseek-v4"},
		{name: "deepseek pro", baseURL: "https://api.deepseek.com", configured: "deepseek-v4-pro", reported: "deepseek-v4-pro", wantFamily: "deepseek-v4"},
		{name: "zai glm", baseURL: "https://api.z.ai/api/paas/v4", configured: "glm-4.6", reported: "glm-4.6", wantFamily: "zai-glm"},
		{name: "normalized official route", baseURL: "HTTPS://API.OPENAI.COM/v1/", configured: "gpt-6.1-terra", reported: "gpt-6.1-terra", wantFamily: "openai-gpt6"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, ok := ResolveTaskValidationModelIdentity(test.baseURL, test.configured, test.reported)
			if !ok {
				t.Fatal("expected catalog identity")
			}
			if identity.Family != test.wantFamily {
				t.Fatalf("family = %q, want %q", identity.Family, test.wantFamily)
			}
			if identity.CatalogVersion != taskValidationModelIdentityCatalogVersion {
				t.Fatalf("catalog version = %q, want %q", identity.CatalogVersion, taskValidationModelIdentityCatalogVersion)
			}
		})
	}
}

func TestResolveTaskValidationModelIdentityRejectsUnknownOrUntrustedValues(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		configured string
		reported   string
	}{
		{name: "empty endpoint", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "empty configured model", baseURL: "https://api.openai.com/v1", reported: "gpt-6-luna"},
		{name: "empty reported model", baseURL: "https://api.openai.com/v1", configured: "gpt-6-luna"},
		{name: "mismatched response model", baseURL: "https://api.openai.com/v1", configured: "gpt-6-luna", reported: "gpt-6.2-astra"},
		{name: "unknown configured model", baseURL: "https://api.openai.com/v1", configured: "gpt-6-future", reported: "gpt-6-future"},
		{name: "unknown reported model", baseURL: "https://api.openai.com/v1", configured: "gpt-6-luna", reported: "gpt-6-future"},
		{name: "dated configured alias", baseURL: "https://api.openai.com/v1", configured: "gpt-6-luna-2026-11-20", reported: "gpt-6-luna-2026-11-20"},
		{name: "dated reported alias", baseURL: "https://api.openai.com/v1", configured: "gpt-6-luna", reported: "gpt-6-luna-2026-11-20"},
		{name: "provider alias", baseURL: "https://api.deepseek.com", configured: "deepseek-v4", reported: "deepseek-v4"},
		{name: "unverified zai model", baseURL: "https://api.z.ai/api/paas/v4", configured: "glm-5.1", reported: "glm-5.1"},
		{name: "HTTP endpoint", baseURL: "http://api.openai.com/v1", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "compatible endpoint", baseURL: "https://proxy.example/v1", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "provider subpath", baseURL: "https://api.openai.com/v1/proxy", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "noncanonical port", baseURL: "https://api.openai.com:443/v1", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "userinfo", baseURL: "https://user:pass@api.openai.com/v1", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "query", baseURL: "https://api.openai.com/v1?token=x", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "fragment", baseURL: "https://api.openai.com/v1#fragment", configured: "gpt-6-luna", reported: "gpt-6-luna"},
		{name: "configured surrounding whitespace", baseURL: "https://api.openai.com/v1", configured: " gpt-6-luna", reported: "gpt-6-luna"},
		{name: "reported surrounding whitespace", baseURL: "https://api.openai.com/v1", configured: "gpt-6-luna", reported: "gpt-6-luna "},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if identity, ok := ResolveTaskValidationModelIdentity(test.baseURL, test.configured, test.reported); ok {
				t.Fatalf("unexpected identity: %+v", identity)
			}
		})
	}
}
