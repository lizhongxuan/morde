package mcp

import (
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestAuthorizeToolUsesExplicitScopesAndHostGrants(t *testing.T) {
	key := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeHostRead, identity.ScopeHostCreate, identity.ScopeComputerObserve, identity.ScopeComputerOperate, identity.ScopeHostManage, identity.ScopeHostDeleteData},
	}
	tests := []struct {
		name    string
		key     identity.Principal
		tool    string
		args    map[string]any
		wantErr domain.ErrorCode
	}{
		{name: "authorized host screenshot", key: key, tool: "browser_screenshot", args: map[string]any{"host_id": "h1234567"}},
		{name: "host create needs no preexisting host grant", key: key, tool: "host_create"},
		{name: "empty host allow list is no authority", key: identity.Principal{Kind: identity.KindAPIKey, Scopes: []identity.Scope{identity.ScopeComputerObserve}}, tool: "browser_screenshot", args: map[string]any{"host_id": "h1234567"}, wantErr: domain.CodeNotFound},
		{name: "scope does not imply another scope", key: identity.Principal{Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeComputerObserve}}, tool: "browser_click", args: map[string]any{"host_id": "h1234567"}, wantErr: domain.CodeForbidden},
		{name: "host read requires host read scope", key: identity.Principal{Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeComputerObserve}}, tool: "host_get", args: map[string]any{"host_id": "h1234567"}, wantErr: domain.CodeForbidden},
		{name: "host delete retains independent workspace data", key: identity.Principal{Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeHostManage}}, tool: "host_delete", args: map[string]any{"host_id": "h1234567"}},
		{name: "unknown tool is denied", key: key, tool: "host_shell", wantErr: domain.CodeForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := authorizeTool(test.key, test.tool, test.args)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("authorizeTool() = %v; want success", err)
				}
				return
			}
			if got := domain.CodeOf(err); got != test.wantErr {
				t.Fatalf("authorizeTool() error code = %q, err=%v; want %q", got, err, test.wantErr)
			}
		})
	}
}
