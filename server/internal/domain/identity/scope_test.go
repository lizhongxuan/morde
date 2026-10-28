package identity

import "testing"

func TestAPIKeyPrincipalUsesCurrentAccountHostAuthorization(t *testing.T) {
	key := Principal{Kind: KindAPIKey}
	if !key.CanAccessHost("h1234567", ScopeComputerObserve) {
		t.Fatal("valid host candidate should pass scope check before owner/share authorization")
	}
	if key.CanAccessHost("bad", ScopeComputerObserve) || key.CanAccessHost("h1234567", ScopeTaskSubmit) {
		t.Fatal("invalid host or retired task capability was allowed")
	}
	if (Principal{Kind: KindSession}).CanAccessHost("h1234567", ScopeComputerObserve) != true {
		t.Fatal("session should retain its authenticated user authorization")
	}
}

func TestValidScope(t *testing.T) {
	for _, scope := range apiKeyScopes {
		if !ValidScope(scope) {
			t.Errorf("ValidScope(%q) = false", scope)
		}
	}
	if !ValidScope(ScopeMCPControl) || !(Principal{Kind: KindSession}).HasScope(ScopeMCPControl) {
		t.Fatal("session MCP scope marker should be valid but does not grant connector access by itself")
	}
	if (Principal{Kind: KindAPIKey, Scopes: []Scope{ScopeMCPControl}}).HasScope(ScopeMCPControl) ||
		(Principal{Kind: KindClient}).HasScope(ScopeMCPControl) {
		t.Fatal("MCP control scope marker must not grant API key or client credentials")
	}
	for _, scope := range []Scope{"", "*", "host:*", "unknown"} {
		if ValidScope(scope) {
			t.Errorf("ValidScope(%q) = true", scope)
		}
	}
}

func TestRetiredTaskAndScheduleScopesAreUnavailableToAPIKeys(t *testing.T) {
	key := Principal{Kind: KindAPIKey}
	for _, scope := range []Scope{ScopeTaskRead, ScopeTaskSubmit, ScopeTaskControl, ScopeScheduleRead, ScopeScheduleManage} {
		if key.CanAccessHost("h1234567", scope) {
			t.Fatalf("retired scope %q was allowed", scope)
		}
	}
}

func TestDeviceCredentialUsesDynamicWorkspaceAuthorization(t *testing.T) {
	device := Principal{
		Kind: KindDevice, ClientInstallationID: "i1234567", Scopes: []Scope{ScopeWorkspaceSync},
	}
	if !device.AllowsWorkspace("w1234567") || !device.AllowsWorkspace("w7654321") {
		t.Fatal("device should allow candidate workspaces for an owner and ready check")
	}
	if device.AllowsWorkspace("bad") || device.HasScope(ScopeHostRead) {
		t.Fatal("device gained access outside its workspace synchronization scope")
	}
	device.ClientInstallationID = ""
	if device.AllowsWorkspace("w1234567") {
		t.Fatal("unbound device accessed workspace")
	}
	apiKey := Principal{Kind: KindAPIKey, Scopes: []Scope{ScopeWorkspaceSync}}
	if apiKey.HasScope(ScopeWorkspaceSync) {
		t.Fatal("API keys must not inherit the device-only workspace scope")
	}
}
