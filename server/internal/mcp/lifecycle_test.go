package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func TestLifecycleAndPrivatePreviewTools(t *testing.T) {
	hosts := &hostServiceStub{value: host.Host{ID: "h1234567", OwnerID: "user_test", State: host.StateRunning}}
	service := &service{deps: Deps{PreviewPortMin: 1024, PreviewPortDeny: []int{8080}, Hosts: hosts, Domains: config.Domains{Scheme: "https", HostSuffix: "hosts.example.test"}}}
	principal := identity.Principal{
		UserID: "user_test", Role: identity.RoleUser, Kind: identity.KindAPIKey,
	}
	ctx := context.Background()

	for _, name := range []string{"host_list", "host_get", "host_delete", "host_renew"} {
		args := map[string]any{"host_id": "h1234567"}
		if name == "host_list" {
			args = map[string]any{}
		}
		value, err := service.lifecycle(ctx, principal, name, args)
		if err != nil || value == nil {
			t.Fatalf("%s value=%+v err=%v", name, value, err)
		}
	}
	projected := service.hostOutput(host.Host{Workspaces: []workspace.Binding{{
		ID: "b1234567", Workspace: workspace.Workspace{ID: "w1234567", Name: "code", State: workspace.StateReady, Revision: 3, UsedBytes: 12, QuotaBytes: 1024},
		Directory: "w1234567", State: workspace.BindingPreparing, BindingVersion: 2, DesiredRevision: 3, AppliedRevision: 1,
	}}})
	if len(projected.Workspaces) != 1 || projected.Workspaces[0].Workspace.ID != "w1234567" ||
		projected.Workspaces[0].State != workspace.BindingPreparing || projected.Workspaces[0].BindingVersion != 2 || projected.Workspaces[0].DesiredRevision != 3 {
		t.Fatalf("workspace binding projection = %+v", projected.Workspaces)
	}
	if _, err := service.lifecycle(ctx, principal, "unknown", nil); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("unknown lifecycle error = %v", err)
	}

	value, err := service.preview(ctx, principal, map[string]any{"host_id": "h1234567", "port": json.Number("3000")})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) != `{"url":"https://3000-h1234567.hosts.example.test"}` {
		t.Fatalf("private preview = %s, %v", encoded, err)
	}
	for _, port := range []string{"80", "8080", "5900", "6080", "8091", "8100", "9222", "65536"} {
		if _, err := service.preview(ctx, principal, map[string]any{"host_id": "h1234567", "port": json.Number(port)}); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("denied preview port %s error = %v", port, err)
		}
	}
	hosts.getErr = domain.NewError(domain.CodeNotFound, "not found")
	if _, err := service.preview(ctx, principal, map[string]any{"host_id": "h1234567", "port": json.Number("3000")}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unauthorized preview error = %v", err)
	}
}
