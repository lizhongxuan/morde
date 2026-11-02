package app

import (
	"context"
	"net/url"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestWorkspaceFilePermissionIsIndependentAndRevocationIsDynamic(t *testing.T) {
	service, store, _, _ := newSharingService(t, []host.Permission{host.PermissionWorkspaceFileRead})
	owner, err := service.Authorize(context.Background(), sharingHostID, sharingOwnerID, host.PermissionWorkspaceFileRead)
	if err != nil || !owner {
		t.Fatalf("owner workspace file authorization = %t, %v", owner, err)
	}
	member, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionWorkspaceFileRead)
	if err != nil || member {
		t.Fatalf("member workspace file authorization = %t, %v", member, err)
	}
	for _, permission := range []host.Permission{
		host.PermissionDesktopView, host.PermissionDesktopControl, host.PermissionTerminalControl, host.PermissionMCPControl,
	} {
		if _, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, permission); domain.CodeOf(err) != domain.CodeNotFound {
			t.Errorf("workspace file grant implied %q: %v", permission, err)
		}
	}
	store.access.Permissions = []host.Permission{host.PermissionDesktopView, host.PermissionDesktopControl}
	if _, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionWorkspaceFileRead); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("desktop grants implied Workspace file access: %v", err)
	}
	store.access.Permissions = []host.Permission{host.PermissionWorkspaceFileRead}
	store.revoked = true
	if _, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionWorkspaceFileRead); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Workspace file access after revoke = %v, want not found", err)
	}
}

func TestInviteAcceptsWorkspaceFilePermissionWithoutImpliedAccess(t *testing.T) {
	service, store, _, _ := newSharingService(t, nil)
	member, err := service.Invite(context.Background(), sharingSession(sharingOwnerID), sharingHostID, sharingMemberID,
		[]host.Permission{host.PermissionWorkspaceFileRead})
	if err != nil {
		t.Fatal(err)
	}
	if len(member.Permissions) != 1 || member.Permissions[0] != host.PermissionWorkspaceFileRead ||
		len(store.member.Permissions) != 1 || store.member.Permissions[0] != host.PermissionWorkspaceFileRead {
		t.Fatalf("workspace-only invite permissions = %#v stored=%#v", member.Permissions, store.member.Permissions)
	}
}

func TestWorkspaceTicketRequiresWorkspaceGrantAndPinsWorkspaceEntry(t *testing.T) {
	service, store, tokens, audit := newSharingService(t, []host.Permission{host.PermissionWorkspaceFileRead})
	info, err := service.IssueWorkspaceTicket(context.Background(), sharingSession(sharingMemberID), sharingHostID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(info.URL)
	if err != nil || parsed.Path != "/__ch/auth" || parsed.Query().Get("next") != "/__ch/wb/?tab=workspace" {
		t.Fatalf("Workspace ticket URL = %q, err=%v", info.URL, err)
	}
	if tokens.value.OwnerID != sharingOwnerID || tokens.value.ActorUserID != sharingMemberID || tokens.value.HostID != sharingHostID {
		t.Fatalf("ticket owner/actor/host = %q/%q/%q", tokens.value.OwnerID, tokens.value.ActorUserID, tokens.value.HostID)
	}
	if len(audit.entries) != 1 || audit.entries[0].Detail["mode"] != "workspace_file_read" {
		t.Fatalf("Workspace entry audit = %#v", audit.entries)
	}

	store.revoked = true
	if _, err := service.IssueWorkspaceTicket(context.Background(), sharingSession(sharingMemberID), sharingHostID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Workspace ticket after revoke = %v, want not found", err)
	}
	if tokens.calls != 1 {
		t.Fatalf("ticket was issued after revoke: calls=%d", tokens.calls)
	}
}

func TestClientCannotRequestWorkspaceTicket(t *testing.T) {
	service, _, tokens, _ := newSharingService(t, []host.Permission{host.PermissionWorkspaceFileRead})
	principal := sharingSession(sharingMemberID)
	principal.Kind = identity.KindClient
	principal.ClientInstallationID = "i1234567"
	if _, err := service.IssueWorkspaceTicket(context.Background(), principal, sharingHostID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("client Workspace ticket = %v, want forbidden", err)
	}
	if tokens.calls != 0 {
		t.Fatalf("client Workspace ticket created credentials: calls=%d", tokens.calls)
	}
}
