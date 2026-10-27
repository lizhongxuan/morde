package mcp

import (
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestAuthorizeToolUsesCurrentAPIKeyAbilities(t *testing.T) {
	key := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
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
		{name: "key can operate without stored scope grants", key: key, tool: "browser_click", args: map[string]any{"host_id": "h1234567"}},
		{name: "key can read without stored host grants", key: key, tool: "host_get", args: map[string]any{"host_id": "h1234567"}},
		{name: "host delete retains independent workspace data", key: key, tool: "host_delete", args: map[string]any{"host_id": "h1234567"}},
		{name: "malformed host id has no authority", key: key, tool: "host_get", args: map[string]any{"host_id": "host_test"}, wantErr: domain.CodeNotFound},
		{name: "session is not an API key", key: identity.Principal{Kind: identity.KindSession}, tool: "host_get", args: map[string]any{"host_id": "h1234567"}, wantErr: domain.CodeUnauthenticated},
		{name: "retired task tool is denied", key: key, tool: "task_get", args: map[string]any{"host_id": "h1234567"}, wantErr: domain.CodeForbidden},
		{name: "retired schedule tool is denied", key: key, tool: "schedule_list", args: map[string]any{"host_id": "h1234567"}, wantErr: domain.CodeForbidden},
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
