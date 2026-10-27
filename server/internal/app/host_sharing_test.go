package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

const (
	sharingOwnerID  = "u1234567"
	sharingMemberID = "u7654321"
	sharingHostID   = "h1234567"
)

type sharingHostStore struct {
	HostStore
	value host.Host
}

func (s sharingHostStore) GetHost(context.Context, string) (host.Host, error) { return s.value, nil }

type sharingUserStore struct {
	UserStore
	user identity.User
}

func (s sharingUserStore) GetUserByID(context.Context, string) (identity.User, error) {
	return s.user, nil
}

type sharingStore struct {
	HostSharingStore
	access  host.ActorAccess
	member  host.Member
	revoked bool
}

func (s *sharingStore) UpsertHostMember(_ context.Context, member host.Member) (host.Member, error) {
	s.member = member
	return member, nil
}

func (s *sharingStore) ListHostMembers(context.Context, string, string) ([]host.Member, error) {
	return []host.Member{s.member}, nil
}

func (s *sharingStore) RevokeHostMember(context.Context, string, string, string) error {
	s.revoked = true
	return nil
}

func (s *sharingStore) ListSharedHosts(context.Context, string) ([]host.SharedHost, error) {
	return nil, nil
}

func (s *sharingStore) GetHostActorAccess(context.Context, string, string) (host.ActorAccess, error) {
	if s.revoked {
		return host.ActorAccess{OwnerID: sharingOwnerID}, nil
	}
	return s.access, nil
}

type sharingTokenStore struct {
	TokenStore
	value identity.Token
	calls int
}

func (s *sharingTokenStore) CreateToken(_ context.Context, token identity.Token) (identity.Token, error) {
	s.value = token
	s.calls++
	return token, nil
}

type sharingAudit struct{ entries []audit.Entry }

func (s *sharingAudit) Write(_ context.Context, entry audit.Entry) error {
	s.entries = append(s.entries, entry)
	return nil
}

func newSharingService(t *testing.T, memberPermissions []host.Permission) (HostSharingService, *sharingStore, *sharingTokenStore, *sharingAudit) {
	t.Helper()
	store := &sharingStore{access: host.ActorAccess{OwnerID: sharingOwnerID, Permissions: memberPermissions}}
	tokens := &sharingTokenStore{}
	auditor := &sharingAudit{}
	service, err := NewHostSharingService(
		sharingHostStore{value: host.Host{ID: sharingHostID, OwnerID: sharingOwnerID, Name: "demo", State: host.StateRunning}},
		sharingUserStore{user: identity.User{ID: sharingMemberID, Status: identity.StatusApproved}}, store, tokens, auditor,
		"https", "hosts.example.test", time.Minute, zap.NewNop(), func() {},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, tokens, auditor
}

func sharingSession(userID string) identity.Principal {
	return identity.Principal{UserID: userID, TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleUser}
}

func TestHostSharingGrantIsOwnerManagedAndNormalizesPermissions(t *testing.T) {
	service, store, _, auditor := newSharingService(t, []host.Permission{host.PermissionDesktopView})
	member, err := service.Invite(context.Background(), sharingSession(sharingOwnerID), sharingHostID, sharingMemberID,
		[]host.Permission{host.PermissionTerminalControl, host.PermissionDesktopControl, host.PermissionDesktopControl, host.PermissionMCPControl})
	want := []host.Permission{host.PermissionDesktopView, host.PermissionDesktopControl, host.PermissionTerminalControl, host.PermissionMCPControl}
	if err != nil || member.UserID != sharingMemberID || len(store.member.Permissions) != len(want) {
		t.Fatalf("Invite() = %#v, %v", member, err)
	}
	for i := range want {
		if store.member.Permissions[i] != want[i] || member.Permissions[i] != want[i] {
			t.Fatalf("Invite() permissions = %#v, want %#v", member.Permissions, want)
		}
	}
	if len(auditor.entries) != 1 || auditor.entries[0].ActorID != sharingOwnerID || auditor.entries[0].Action != audit.ActionHostMemberInvite {
		t.Fatalf("invite audit = %#v", auditor.entries)
	}
	mcpMember, err := service.Invite(context.Background(), sharingSession(sharingOwnerID), sharingHostID, sharingMemberID, []host.Permission{host.PermissionMCPControl})
	if err != nil || len(mcpMember.Permissions) != 1 || mcpMember.Permissions[0] != host.PermissionMCPControl {
		t.Fatalf("Invite(MCP control) = %#v, %v", mcpMember, err)
	}
	if _, err := service.Invite(context.Background(), sharingSession(sharingOwnerID), sharingHostID, sharingMemberID, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("Invite(empty permissions) error = %v, want invalid_argument", err)
	}
	if _, err := service.Invite(context.Background(), sharingSession(sharingMemberID), sharingHostID, sharingOwnerID, []host.Permission{host.PermissionDesktopView}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("member attempted re-sharing: %v", err)
	}
}

func TestHostSharingAuthorizationIsDynamicAndPermissionScoped(t *testing.T) {
	service, store, _, _ := newSharingService(t, []host.Permission{host.PermissionDesktopView})
	owner, err := service.Authorize(context.Background(), sharingHostID, sharingOwnerID, host.PermissionMCPControl)
	if err != nil || !owner {
		t.Fatalf("owner authorization = %t, %v", owner, err)
	}
	member, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionDesktopView)
	if err != nil || member {
		t.Fatalf("member desktop view authorization = %t, %v", member, err)
	}
	store.access.Permissions = []host.Permission{host.PermissionDesktopControl}
	if owner, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionDesktopView); err != nil || owner {
		t.Fatalf("desktop control did not imply view = %t, %v", owner, err)
	}
	if owner, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionDesktopControl); err != nil || owner {
		t.Fatalf("member desktop control authorization = %t, %v", owner, err)
	}
	store.access.Permissions = []host.Permission{host.PermissionMCPControl}
	if owner, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionMCPControl); err != nil || owner {
		t.Fatalf("member MCP control authorization = %t, %v", owner, err)
	}
	if _, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionTerminalControl); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("member terminal authorization = %v, want not found", err)
	}
	store.revoked = true
	if _, err := service.Authorize(context.Background(), sharingHostID, sharingMemberID, host.PermissionDesktopView); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("authorization after revocation = %v, want not found", err)
	}
}

func TestHostObservationTicketKeepsMemberActorSeparateFromOwner(t *testing.T) {
	service, _, tokens, auditor := newSharingService(t, []host.Permission{host.PermissionDesktopView})
	info, err := service.IssueObservationTicket(context.Background(), sharingSession(sharingMemberID), sharingHostID)
	if err != nil || info.URL == "" || !info.ExpiresAt.After(time.Now()) {
		t.Fatalf("IssueObservationTicket() = %#v, %v", info, err)
	}
	if tokens.value.OwnerID != sharingOwnerID || tokens.value.ActorUserID != sharingMemberID || tokens.value.HostID != sharingHostID {
		t.Fatalf("observation ticket owner/actor/host = %q/%q/%q", tokens.value.OwnerID, tokens.value.ActorUserID, tokens.value.HostID)
	}
	if len(auditor.entries) != 1 || auditor.entries[0].ActorID != sharingMemberID {
		t.Fatalf("observation audit = %#v", auditor.entries)
	}
	if _, err := service.IssueObservationTicket(context.Background(), sharingSession(sharingOwnerID), sharingHostID); err != nil {
		t.Fatalf("owner observation ticket = %v", err)
	}
	if tokens.value.OwnerID != sharingOwnerID || tokens.value.ActorUserID != "" {
		t.Fatalf("owner ticket owner/actor = %q/%q, want owner with implicit actor", tokens.value.OwnerID, tokens.value.ActorUserID)
	}
}

func TestClientSharedHostTicketsAreObservationOnlyAndAudited(t *testing.T) {
	service, _, tokens, auditor := newSharingService(t, []host.Permission{
		host.PermissionDesktopView, host.PermissionTerminalControl,
	})
	principal := sharingSession(sharingMemberID)
	principal.Kind = identity.KindClient
	principal.ClientInstallationID = "i1234567"

	info, err := service.IssueObservationTicket(context.Background(), principal, sharingHostID)
	if err != nil || info.URL == "" || !info.ExpiresAt.After(time.Now()) {
		t.Fatalf("IssueObservationTicket(Client) = %#v, %v", info, err)
	}
	if tokens.calls != 1 || tokens.value.OwnerID != sharingOwnerID || tokens.value.ActorUserID != sharingMemberID ||
		tokens.value.HostID != sharingHostID || tokens.value.ClientInstallationID != principal.ClientInstallationID {
		t.Fatalf("client observation ticket calls/value = %d / %#v", tokens.calls, tokens.value)
	}
	if len(auditor.entries) != 1 || auditor.entries[0].ActorID != sharingMemberID ||
		auditor.entries[0].ActorKind != audit.ActorClient || auditor.entries[0].Action != audit.ActionHostEnter ||
		auditor.entries[0].Target != sharingHostID || auditor.entries[0].Detail["mode"] != "desktop_observe" {
		t.Fatalf("client observation audit = %#v", auditor.entries)
	}

	if _, err := service.IssueTerminalTicket(context.Background(), principal, sharingHostID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("IssueTerminalTicket(Client with terminal grant) = %v, want forbidden", err)
	}
	if tokens.calls != 1 || len(auditor.entries) != 1 {
		t.Fatalf("denied terminal ticket changed token/audit count: %d / %d", tokens.calls, len(auditor.entries))
	}
}

func TestInstalledWebSharedHostTerminalTicketIsInstallationBound(t *testing.T) {
	service, _, tokens, _ := newSharingService(t, []host.Permission{host.PermissionTerminalControl})
	principal := sharingSession(sharingMemberID)
	principal.ClientInstallationID = "i1234567"
	if _, err := service.IssueTerminalTicket(context.Background(), principal, sharingHostID); err != nil {
		t.Fatal(err)
	}
	if tokens.value.ClientInstallationID != principal.ClientInstallationID || tokens.value.ActorUserID != sharingMemberID {
		t.Fatalf("installed Web shared Host ticket = %#v", tokens.value)
	}
}

func TestHostTerminalTicketIsScopedAndRequiresTerminalPermission(t *testing.T) {
	service, _, tokens, auditor := newSharingService(t, []host.Permission{host.PermissionTerminalControl})
	info, err := service.IssueTerminalTicket(context.Background(), sharingSession(sharingMemberID), sharingHostID)
	if err != nil || !strings.Contains(info.URL, "next=%2F__ch%2Fterminal%2F") {
		t.Fatalf("IssueTerminalTicket() = %#v, %v", info, err)
	}
	if tokens.value.OwnerID != sharingOwnerID || tokens.value.ActorUserID != sharingMemberID || tokens.value.HostID != sharingHostID {
		t.Fatalf("terminal ticket owner/actor/host = %q/%q/%q", tokens.value.OwnerID, tokens.value.ActorUserID, tokens.value.HostID)
	}
	if len(auditor.entries) != 1 || auditor.entries[0].Detail["mode"] != "terminal_control" {
		t.Fatalf("terminal entry audit = %#v", auditor.entries)
	}
	if _, err := service.IssueObservationTicket(context.Background(), sharingSession(sharingMemberID), sharingHostID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("terminal-only member observation ticket = %v, want not found", err)
	}
}
