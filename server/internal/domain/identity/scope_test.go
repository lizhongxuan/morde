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

func TestScheduledTaskExecutionScopesDependOnHostBinding(t *testing.T) {
	hostBound := ScheduleTaskExecutionScopes()
	hostless := HostlessScheduleTaskExecutionScopes()
	goalFollowup := GoalFollowupScheduleTaskExecutionScopes()
	if !IsScheduleTaskExecutionScopeSet(hostBound) || !IsHostBoundScheduleTaskExecutionScopeSet(hostBound) ||
		IsHostlessScheduleTaskExecutionScopeSet(hostBound) {
		t.Fatalf("host-bound scheduled Task scopes were not recognized: %v", hostBound)
	}
	if !IsScheduleTaskExecutionScopeSet(hostless) || !IsHostlessScheduleTaskExecutionScopeSet(hostless) ||
		IsHostBoundScheduleTaskExecutionScopeSet(hostless) {
		t.Fatalf("hostless scheduled Task scopes were not recognized: %v", hostless)
	}
	if !IsScheduleTaskExecutionScopeSet(goalFollowup) || !IsGoalFollowupScheduleTaskExecutionScopeSet(goalFollowup) ||
		!IsHostlessScheduleTaskExecutionScopeSet(goalFollowup) || IsHostBoundScheduleTaskExecutionScopeSet(goalFollowup) ||
		ValidScope(ScopeGoalFollowupReadOnly) {
		t.Fatalf("read-only Goal follow-up Task scopes were not recognized as internal-only: %v", goalFollowup)
	}
	for _, scope := range hostless {
		if scope == ScopeComputerObserve || scope == ScopeComputerOperate {
			t.Fatalf("hostless scheduled Task has computer scope %q", scope)
		}
	}
	principal := Principal{Kind: KindScheduleTask, Scopes: hostless}
	if !principal.HasScope(ScopeTaskRead) || !principal.HasScope(ScopeTaskSubmit) || !principal.HasScope(ScopeTaskControl) ||
		principal.HasScope(ScopeComputerObserve) || principal.HasScope(ScopeComputerOperate) {
		t.Fatalf("hostless scheduled Task principal scope checks are wrong: %v", principal.Scopes)
	}
	for _, invalid := range [][]Scope{
		append(append([]Scope(nil), hostless...), ScopeComputerObserve),
		append(append([]Scope(nil), goalFollowup...), ScopeComputerObserve),
		{ScopeTaskRead, ScopeTaskSubmit, ScopeTaskRead},
		{ScopeTaskRead, ScopeTaskSubmit},
	} {
		if IsScheduleTaskExecutionScopeSet(invalid) {
			t.Fatalf("invalid scheduled Task scope set was accepted: %v", invalid)
		}
	}
}
