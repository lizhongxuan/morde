package app

import (
	"context"
	"reflect"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type personalDataQuotaUserStoreStub struct {
	quota     PersonalDataQuota
	principal identity.Principal
	calls     int
	err       error
}

func (s *personalDataQuotaUserStoreStub) GetPersonalDataQuotaUser(_ context.Context, principal identity.Principal) (PersonalDataQuota, error) {
	s.calls++
	s.principal = principal
	return s.quota, s.err
}

func TestPersonalDataQuotaUserReadsOnlyAuthenticatedOwner(t *testing.T) {
	store := &personalDataQuotaUserStoreStub{quota: PersonalDataQuota{
		OwnerID: "u1234567", UsedBytes: 12, ReservedBytes: 3, QuotaBytes: 100,
		MaxAttachmentBytes: 50, Version: 2,
	}}
	service, err := NewPersonalDataQuotaUserService(store)
	if err != nil {
		t.Fatal(err)
	}

	principals := []identity.Principal{
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient, TokenID: "t1234567", ClientInstallationID: "i1234567"},
	}
	for _, principal := range principals {
		got, err := service.GetPersonalDataQuota(context.Background(), principal)
		if err != nil || got != store.quota || !reflect.DeepEqual(store.principal, principal) {
			t.Fatalf("quota for %s = %+v, err=%v, forwarded principal=%+v", principal.Kind, got, err, store.principal)
		}
	}

	store.quota.OwnerID = "u7654321"
	if _, err := service.GetPersonalDataQuota(context.Background(), principals[0]); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("cross-account store receipt error = %v", err)
	}
}

func TestPersonalDataQuotaUserRejectsUnauthorizedPrincipals(t *testing.T) {
	store := &personalDataQuotaUserStoreStub{quota: PersonalDataQuota{
		OwnerID: "u1234567", QuotaBytes: 100, MaxAttachmentBytes: 50, Version: 1,
	}}
	service, err := NewPersonalDataQuotaUserService(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		principal identity.Principal
		wantCode  domain.ErrorCode
	}{
		{
			name: "host scoped session",
			principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession,
				TokenID: "t1234567", HostScope: "h1234567"},
			wantCode: domain.CodeForbidden,
		},
		{
			name: "host scoped workspace credential",
			principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession,
				TokenID: "t1234567", WorkspaceIDs: []string{"w1234567"}},
			wantCode: domain.CodeForbidden,
		},
		{
			name: "admin presented as client",
			principal: identity.Principal{UserID: "u1234567", Role: identity.RoleAdmin, Kind: identity.KindClient,
				TokenID: "t1234567", ClientInstallationID: "i1234567"},
			wantCode: domain.CodeForbidden,
		},
		{
			name: "client missing task submit scope",
			principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
				TokenID: "t1234567", ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeTaskRead}},
			wantCode: domain.CodeForbidden,
		},
		{
			name: "non session credential",
			principal: identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey,
				TokenID: "t1234567"},
			wantCode: domain.CodeForbidden,
		},
		{
			name:      "missing user identity",
			principal: identity.Principal{Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
			wantCode:  domain.CodeUnauthenticated,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := store.calls
			if _, err := service.GetPersonalDataQuota(context.Background(), test.principal); domain.CodeOf(err) != test.wantCode {
				t.Fatalf("unauthorized principal error = %v, want %s", err, test.wantCode)
			}
			if store.calls != before {
				t.Fatalf("unauthorized request reached store: calls %d -> %d", before, store.calls)
			}
		})
	}
}

func TestPersonalDataQuotaUserRejectsInvalidStoreState(t *testing.T) {
	valid := PersonalDataQuota{OwnerID: "u1234567", QuotaBytes: 100, MaxAttachmentBytes: 50, Version: 1}
	servicePrincipal := identity.Principal{UserID: valid.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	for _, test := range []struct {
		name  string
		quota PersonalDataQuota
	}{
		{name: "no owner", quota: PersonalDataQuota{QuotaBytes: 100, MaxAttachmentBytes: 50, Version: 1}},
		{name: "different owner", quota: PersonalDataQuota{OwnerID: "u7654321", QuotaBytes: 100, MaxAttachmentBytes: 50, Version: 1}},
		{name: "negative used bytes", quota: PersonalDataQuota{OwnerID: valid.OwnerID, UsedBytes: -1, QuotaBytes: 100, MaxAttachmentBytes: 50, Version: 1}},
		{name: "negative reserved bytes", quota: PersonalDataQuota{OwnerID: valid.OwnerID, ReservedBytes: -1, QuotaBytes: 100, MaxAttachmentBytes: 50, Version: 1}},
		{name: "zero quota", quota: PersonalDataQuota{OwnerID: valid.OwnerID, QuotaBytes: 0, MaxAttachmentBytes: 50, Version: 1}},
		{name: "invalid attachment limit", quota: PersonalDataQuota{OwnerID: valid.OwnerID, QuotaBytes: 100, MaxAttachmentBytes: 0, Version: 1}},
		{name: "attachment limit exceeds supported maximum", quota: PersonalDataQuota{OwnerID: valid.OwnerID, QuotaBytes: 100, MaxAttachmentBytes: 1 << 40, Version: 1}},
		{name: "zero version", quota: PersonalDataQuota{OwnerID: valid.OwnerID, QuotaBytes: 100, MaxAttachmentBytes: 50, Version: 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &personalDataQuotaUserStoreStub{quota: test.quota}
			service, err := NewPersonalDataQuotaUserService(store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.GetPersonalDataQuota(context.Background(), servicePrincipal); domain.CodeOf(err) != domain.CodeInternal {
				t.Fatalf("invalid store state error = %v", err)
			}
		})
	}
}

func TestNewPersonalDataQuotaUserServiceRequiresStore(t *testing.T) {
	if _, err := NewPersonalDataQuotaUserService(nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("missing store error = %v", err)
	}
}
