package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type hostEmbedTicketTestStore struct {
	*sharingStore
	record   HostEmbedTicketRecord
	created  int
	consumed int
}

func (s *hostEmbedTicketTestStore) CreateHostEmbedTicket(_ context.Context, record HostEmbedTicketRecord) error {
	s.record = record
	s.created++
	return nil
}

func (s *hostEmbedTicketTestStore) ConsumeHostEmbedTicket(_ context.Context, hash []byte, hostID, instanceID, origin, nonce string) (HostEmbedTicketRecord, error) {
	s.consumed++
	if len(hash) != sha256.Size || string(hash) != string(s.record.TicketHash) || hostID != s.record.HostID ||
		instanceID != s.record.ServiceInstanceID || origin != s.record.PortalOrigin || nonce != s.record.EmbedNonce {
		return HostEmbedTicketRecord{}, errors.New("embed ticket binding mismatch")
	}
	return s.record, nil
}

type hostEmbedTicketAudit struct{}

func (hostEmbedTicketAudit) Write(context.Context, audit.Entry) error { return nil }

func hostEmbedTestService(t *testing.T, permissions []host.Permission) (*hostSharingService, *hostEmbedTicketTestStore) {
	t.Helper()
	store := &hostEmbedTicketTestStore{sharingStore: &sharingStore{access: host.ActorAccess{OwnerID: sharingOwnerID, Permissions: permissions}}}
	hosts := sharingHostStore{value: host.Host{
		ID: sharingHostID, OwnerID: sharingOwnerID, Name: "demo", State: host.StateRunning, RuntimeGeneration: 3,
	}}
	service, err := NewHostSharingService(hosts, sharingUserStore{user: identity.User{ID: sharingMemberID, Status: identity.StatusApproved}},
		store, &sharingTokenStore{}, hostEmbedTicketAudit{}, "https", "hosts.example.test", time.Hour, zap.NewNop(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	return service.(*hostSharingService), store
}

func TestHostEmbedTicketIssueBindsIdentityTargetAndHashOnlySecret(t *testing.T) {
	for _, test := range []struct {
		name        string
		principal   identity.Principal
		permissions []host.Permission
		target      string
		wantCeiling host.Permission
	}{
		{name: "owner browser control ceiling", principal: sharingSession(sharingOwnerID), target: HostEmbedTargetBrowser, wantCeiling: host.PermissionDesktopControl},
		{name: "member browser view ceiling", principal: sharingSession(sharingMemberID), permissions: []host.Permission{host.PermissionDesktopView}, target: HostEmbedTargetBrowser, wantCeiling: host.PermissionDesktopView},
		{name: "member browser control ceiling", principal: sharingSession(sharingMemberID), permissions: []host.Permission{host.PermissionDesktopControl}, target: HostEmbedTargetBrowser, wantCeiling: host.PermissionDesktopControl},
		{name: "member terminal requires a user control lease", principal: sharingSession(sharingMemberID), permissions: []host.Permission{host.PermissionTerminalControl, host.PermissionDesktopControl}, target: HostEmbedTargetTerminal, wantCeiling: host.PermissionTerminalControl},
		{name: "member workspace ceiling", principal: sharingSession(sharingMemberID), permissions: []host.Permission{host.PermissionWorkspaceFileRead}, target: HostEmbedTargetWorkspace, wantCeiling: host.PermissionWorkspaceFileRead},
		{name: "installed client identity is bound read only", principal: identity.Principal{UserID: sharingOwnerID, TokenID: "t1234567", Kind: identity.KindClient, ClientInstallationID: "i1234567"}, target: HostEmbedTargetBrowser, wantCeiling: host.PermissionDesktopView},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store := hostEmbedTestService(t, test.permissions)
			if test.principal.UserID == sharingOwnerID && test.principal.Kind == identity.KindSession {
				store.access.OwnerID = sharingOwnerID
			}
			result, err := service.IssueEmbedTicket(context.Background(), test.principal, HostEmbedTicketCommand{
				HostID: sharingHostID, ConversationID: "c1234567", Target: test.target,
				ServiceInstanceID: "11111111-1111-4111-8111-111111111111", PortalOrigin: "https://portal.example.test",
			})
			if err != nil {
				t.Fatalf("IssueEmbedTicket: %v", err)
			}
			if len(result.Ticket) != 43 || result.ExchangeURL != "https://h1234567.hosts.example.test/__ch/embed-auth" || result.ExpiresAt.After(time.Now().UTC().Add(time.Minute)) {
				t.Fatalf("issued ticket = %#v", result)
			}
			hash := sha256.Sum256([]byte(result.Ticket))
			if store.created != 1 || string(store.record.TicketHash) != string(hash[:]) || store.record.ActorID != test.principal.UserID ||
				store.record.PortalTokenID != test.principal.TokenID || store.record.ClientInstallationID != test.principal.ClientInstallationID ||
				store.record.ConversationID != "c1234567" || store.record.InitialTarget != test.target || len(store.record.TargetGrants) != 1 ||
				store.record.TargetGrants[0].Target != test.target || store.record.TargetGrants[0].PermissionCeiling != test.wantCeiling ||
				len(result.EmbedNonce) != 32 || store.record.EmbedNonce != result.EmbedNonce || len(result.Targets) != 1 || result.Targets[0] != test.target ||
				store.record.RuntimeGeneration != 3 || store.record.ServiceInstanceID != "11111111-1111-4111-8111-111111111111" ||
				store.record.PortalOrigin != "https://portal.example.test" {
				t.Fatalf("persisted ticket binding = %#v; created=%d", store.record, store.created)
			}
		})
	}
}

func TestHostEmbedTicketIssueIntersectsRequestedTargetsWithLiveGrants(t *testing.T) {
	service, store := hostEmbedTestService(t, []host.Permission{host.PermissionDesktopView, host.PermissionWorkspaceFileRead})
	result, err := service.IssueEmbedTicket(context.Background(), sharingSession(sharingMemberID), HostEmbedTicketCommand{
		HostID: sharingHostID, ConversationID: "c1234567", Target: HostEmbedTargetBrowser,
		Targets:           []string{HostEmbedTargetBrowser, HostEmbedTargetWorkspace, HostEmbedTargetFiles, HostEmbedTargetVSCode},
		ServiceInstanceID: "11111111-1111-4111-8111-111111111111", PortalOrigin: "https://portal.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Targets) != 2 || result.Targets[0] != HostEmbedTargetBrowser || result.Targets[1] != HostEmbedTargetWorkspace ||
		len(store.record.TargetGrants) != 2 || store.record.TargetGrants[1].PermissionCeiling != host.PermissionWorkspaceFileRead {
		t.Fatalf("authorized embed targets = %#v, record=%#v", result.Targets, store.record.TargetGrants)
	}
}

func TestHostEmbedTicketIssueRejectsMissingPermissionAndInvalidOrigin(t *testing.T) {
	service, store := hostEmbedTestService(t, []host.Permission{host.PermissionDesktopView})
	base := HostEmbedTicketCommand{HostID: sharingHostID, ConversationID: "c1234567", Target: HostEmbedTargetTerminal,
		ServiceInstanceID: "11111111-1111-4111-8111-111111111111", PortalOrigin: "https://portal.example.test"}
	if _, err := service.IssueEmbedTicket(context.Background(), sharingSession(sharingMemberID), base); err == nil || store.created != 0 {
		t.Fatalf("ticket without target permission = %v, creates=%d", err, store.created)
	}
	service, store = hostEmbedTestService(t, []host.Permission{host.PermissionTerminalControl})
	if _, err := service.IssueEmbedTicket(context.Background(), sharingSession(sharingMemberID), base); err == nil || store.created != 0 {
		t.Fatalf("terminal ticket without desktop-control permission = %v, creates=%d", err, store.created)
	}
	base.PortalOrigin = "https://portal.example.test/path"
	if _, err := service.IssueEmbedTicket(context.Background(), sharingSession(sharingOwnerID), base); err == nil || store.created != 0 {
		t.Fatalf("ticket with non-origin value = %v, creates=%d", err, store.created)
	}
}

func TestHostEmbedTicketConsumeHashesRawValueAndBindsExchangeContext(t *testing.T) {
	service, store := hostEmbedTestService(t, nil)
	result, err := service.IssueEmbedTicket(context.Background(), sharingSession(sharingOwnerID), HostEmbedTicketCommand{
		HostID: sharingHostID, ConversationID: "c1234567", Target: HostEmbedTargetBrowser,
		ServiceInstanceID: "11111111-1111-4111-8111-111111111111", PortalOrigin: "https://portal.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := service.ConsumeEmbedTicket(context.Background(), result.Ticket, result.EmbedNonce, sharingHostID,
		"11111111-1111-4111-8111-111111111111", "https://portal.example.test")
	if err != nil || consumed.HostID != sharingHostID || consumed.EmbedNonce != result.EmbedNonce || store.consumed != 1 {
		t.Fatalf("ConsumeEmbedTicket = %#v, %v; calls=%d", consumed, err, store.consumed)
	}
	if _, err := service.ConsumeEmbedTicket(context.Background(), result.Ticket, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", sharingHostID,
		"11111111-1111-4111-8111-111111111111", "https://portal.example.test"); err == nil {
		t.Fatal("consumed ticket with mismatched nonce")
	}
}
