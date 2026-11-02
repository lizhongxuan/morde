package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type clientScheduleGrantStoreFake struct {
	ClientLoginStore
	issuedPrincipal  identity.Principal
	token            identity.Token
	issueErr         error
	revokedPrincipal identity.Principal
	revokeCalls      int
	revokeErr        error
}

func (s *clientScheduleGrantStoreFake) IssueClientScheduleGrant(_ context.Context, principal identity.Principal, token identity.Token) error {
	s.issuedPrincipal, s.token = principal, token
	return s.issueErr
}

func (s *clientScheduleGrantStoreFake) RevokeClientScheduleGrants(_ context.Context, principal identity.Principal) error {
	s.revokedPrincipal = principal
	s.revokeCalls++
	return s.revokeErr
}

func TestIssueScheduleGrantCreatesSeparateScopedClientCredential(t *testing.T) {
	store := &clientScheduleGrantStoreFake{}
	service := &ClientLoginService{store: store}
	principal := clientScheduleGrantTestPrincipal()
	before := time.Now().UTC()

	result, err := service.IssueScheduleGrant(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	kind, tokenID, secret, err := identity.ParseCredential(result.Credential)
	if err != nil || kind != identity.KindClient || tokenID != store.token.ID || !identity.VerifySecret(secret, store.token.SecretHash) {
		t.Fatalf("issued schedule grant credential = kind:%s id:%s err:%v", kind, tokenID, err)
	}
	wantScopes := []identity.Scope{identity.ScopeScheduleRead, identity.ScopeScheduleManage}
	if !reflect.DeepEqual(store.issuedPrincipal, principal) || store.token.OwnerID != principal.UserID ||
		store.token.ClientInstallationID != principal.ClientInstallationID || store.token.Kind != identity.KindClient ||
		store.token.Name != "iOS cloud schedule grant" || !identity.IsScheduleGrantScopeSet(store.token.Scopes) ||
		result.ExpiresAt.Before(before.Add(11*time.Hour)) || result.ExpiresAt.After(before.Add(13*time.Hour)) ||
		!result.ExpiresAt.Equal(store.token.ExpiresAt) || len(wantScopes) != len(store.token.Scopes) {
		t.Fatalf("schedule grant token/result mismatch: token=%#v result=%#v", store.token, result)
	}
}

func TestScheduleGrantRequiresRegularClientAndMapsInactiveInstallation(t *testing.T) {
	serviceStore := &clientScheduleGrantStoreFake{}
	service := &ClientLoginService{store: serviceStore}
	principal := clientScheduleGrantTestPrincipal()
	invalid := []identity.Principal{
		{UserID: principal.UserID, TokenID: principal.TokenID, ClientInstallationID: principal.ClientInstallationID, Kind: identity.KindSession},
		func() identity.Principal {
			value := principal
			value.Scopes = []identity.Scope{identity.ScopeScheduleManage}
			return value
		}(),
		func() identity.Principal { value := principal; value.HostIDs = []string{"h1234567"}; return value }(),
		func() identity.Principal { value := principal; value.WorkspaceIDs = []string{"w1234567"}; return value }(),
	}
	for index, value := range invalid {
		if _, err := service.IssueScheduleGrant(context.Background(), value); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("invalid issuer %d error = %v, want forbidden", index, err)
		}
		if err := service.RevokeScheduleGrant(context.Background(), value); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("invalid revoker %d error = %v, want forbidden", index, err)
		}
	}
	if serviceStore.token.ID != "" || serviceStore.revokeCalls != 0 {
		t.Fatalf("invalid principals reached grant store: issued=%#v revoke calls=%d", serviceStore.token, serviceStore.revokeCalls)
	}

	serviceStore.issueErr = identity.ErrInvalidCredential
	if _, err := service.IssueScheduleGrant(context.Background(), principal); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("inactive installation issue error = %v, want unauthenticated", err)
	}
	serviceStore.issueErr = nil
	serviceStore.revokeErr = errors.New("store unavailable")
	if err := service.RevokeScheduleGrant(context.Background(), principal); err == nil || serviceStore.revokeCalls != 1 || !reflect.DeepEqual(serviceStore.revokedPrincipal, principal) {
		t.Fatalf("revoke result error=%v calls=%d principal=%#v", err, serviceStore.revokeCalls, serviceStore.revokedPrincipal)
	}
}

func clientScheduleGrantTestPrincipal() identity.Principal {
	return identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
}
