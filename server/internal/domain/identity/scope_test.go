package identity

import "testing"

func TestAPIKeyPrincipalRequiresExplicitScopeAndHost(t *testing.T) {
	key := Principal{Kind: KindAPIKey, Scopes: []Scope{ScopeComputerObserve}, HostIDs: []string{"h1234567"}}
	if !key.CanAccessHost("h1234567", ScopeComputerObserve) {
		t.Fatal("explicit matching scope and Host should be allowed")
	}
	for _, principal := range []Principal{
		{Kind: KindAPIKey, Scopes: []Scope{ScopeComputerObserve}},
		{Kind: KindAPIKey, HostIDs: []string{"h1234567"}},
		{Kind: KindAPIKey, Scopes: []Scope{"*"}, HostIDs: []string{"h1234567"}},
	} {
		if principal.CanAccessHost("h1234567", ScopeComputerObserve) {
			t.Fatalf("principal %#v gained access without explicit scope and Host", principal)
		}
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
	for _, scope := range []Scope{"", "*", "host:*", "unknown"} {
		if ValidScope(scope) {
			t.Errorf("ValidScope(%q) = true", scope)
		}
	}
}

func TestScheduleScopesStayExplicit(t *testing.T) {
	key := Principal{Kind: KindAPIKey, HostIDs: []string{"h1234567"}, Scopes: []Scope{ScopeScheduleManage}}
	if !key.CanAccessHost("h1234567", ScopeScheduleManage) {
		t.Fatal("schedule:manage must allow a schedule operation for an explicitly permitted Host")
	}
	if key.CanAccessHost("h1234567", ScopeTaskSubmit) || key.CanAccessHost("h1234567", ScopeScheduleRead) {
		t.Fatal("schedule:manage must not imply task:submit or schedule:read")
	}
}

func TestDeviceCredentialIsLimitedToGrantedWorkspaces(t *testing.T) {
	device := Principal{
		Kind: KindDevice, WorkspaceIDs: []string{"w1234567"}, Scopes: []Scope{ScopeWorkspaceSync},
	}
	if !device.AllowsWorkspace("w1234567") {
		t.Fatal("device should access an explicitly granted workspace")
	}
	if device.AllowsWorkspace("w7654321") || device.HasScope(ScopeHostRead) {
		t.Fatal("device gained access outside its workspace synchronization grant")
	}
	apiKey := Principal{Kind: KindAPIKey, Scopes: []Scope{ScopeWorkspaceSync}}
	if apiKey.HasScope(ScopeWorkspaceSync) {
		t.Fatal("API keys must not inherit the device-only workspace scope")
	}
}
