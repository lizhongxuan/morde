package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type clientRevocationStore struct {
	ClientLoginStore
	events         *[]string
	err            error
	loginInstallID string
	authenticated  identity.User
}

func (s *clientRevocationStore) LoginClient(_ context.Context, installation identity.ClientInstallation, authenticated identity.User, _ string, _ int64, _ bool, _, _ []byte, _, _ string,
	issue func(string) (ClientIssuedTokens, error)) (identity.ClientInstallation, ClientIssuedTokens, error) {
	s.authenticated = authenticated
	if s.err != nil {
		return identity.ClientInstallation{}, ClientIssuedTokens{}, s.err
	}
	if s.loginInstallID != "" {
		installation.ID = s.loginInstallID
	}
	issued, err := issue(installation.ID)
	if err != nil {
		return identity.ClientInstallation{}, ClientIssuedTokens{}, err
	}
	*s.events = append(*s.events, "login-commit:"+installation.ID)
	return installation, issued, nil
}

func (s *clientRevocationStore) RevokeClientInstallation(ctx context.Context, installationID, ownerID, _ string, _ audit.ActorKind, disableRelay func(context.Context) error) error {
	if disableRelay != nil {
		if err := disableRelay(ctx); err != nil {
			return err
		}
	}
	if s.err != nil {
		return s.err
	}
	*s.events = append(*s.events, "commit:"+installationID)
	return nil
}

type clientRevocationWorkspaceSync struct {
	events       *[]string
	reconcileErr error
}

func (s *clientRevocationWorkspaceSync) DisableWorkspaceSyncNodeRelay(_ context.Context, ownerID string) error {
	*s.events = append(*s.events, "disable:"+ownerID)
	return nil
}

func (s *clientRevocationWorkspaceSync) ReconcileWorkspaceSyncNodeOwner(_ context.Context, ownerID string) error {
	*s.events = append(*s.events, "reconcile:"+ownerID)
	return s.reconcileErr
}

type clientRevocationHub struct{ events *[]string }

func (h *clientRevocationHub) CloseInstallation(installationID string) {
	*h.events = append(*h.events, "hub-close:"+installationID)
}

func (h *clientRevocationHub) CloseOwner(ownerID string) {
	*h.events = append(*h.events, "hub-owner-close:"+ownerID)
}

func TestClientInstallationRevocationClosesHubAfterCommitBeforeReconcile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(*ClientLoginService, identity.Principal) error
		kind   identity.Kind
	}{
		{name: "logout", kind: identity.KindClient, revoke: func(s *ClientLoginService, p identity.Principal) error {
			return s.Logout(context.Background(), p, "127.0.0.1")
		}},
		{name: "web session logout", kind: identity.KindSession, revoke: func(s *ClientLoginService, p identity.Principal) error {
			return s.LogoutWebSession(context.Background(), p, "127.0.0.1")
		}},
		{name: "revoke", kind: identity.KindClient, revoke: func(s *ClientLoginService, p identity.Principal) error {
			return s.Revoke(context.Background(), p, p.ClientInstallationID, "127.0.0.1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			syncController := &clientRevocationWorkspaceSync{events: &events, reconcileErr: errors.New("reconcile failed")}
			service := &ClientLoginService{
				store: &clientRevocationStore{events: &events}, workspaceSyncReconciler: syncController,
				hub: &clientRevocationHub{events: &events},
			}
			principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: tc.kind,
				TokenID: "t1234567", ClientInstallationID: "i1234567"}

			err := tc.revoke(service, principal)
			if domain.CodeOf(err) != domain.CodeUpstreamError {
				t.Fatalf("revocation error = %v, want workspace reconciliation error", err)
			}
			if got, want := strings.Join(events, ","), "disable:u1234567,commit:i1234567,hub-close:i1234567,reconcile:u1234567"; got != want {
				t.Fatalf("revocation events = %q, want %q", got, want)
			}
		})
	}
}

func TestClientInstallationRevocationFailureDoesNotCloseHub(t *testing.T) {
	events := []string{}
	service := &ClientLoginService{
		store:                   &clientRevocationStore{events: &events, err: errors.New("commit failed")},
		workspaceSyncReconciler: &clientRevocationWorkspaceSync{events: &events},
		hub:                     &clientRevocationHub{events: &events},
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567"}
	if err := service.Logout(context.Background(), principal, "127.0.0.1"); err == nil {
		t.Fatal("Logout() ignored installation revocation failure")
	}
	if got, want := strings.Join(events, ","), "disable:u1234567"; got != want {
		t.Fatalf("revocation events = %q, want %q", got, want)
	}
}

func TestClientAccountPrincipalRequiresUserInstallation(t *testing.T) {
	client := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
	if err := validateClientAccountPrincipal(client); err != nil {
		t.Fatalf("valid client principal rejected: %v", err)
	}
	for name, principal := range map[string]identity.Principal{
		"admin client":    {UserID: client.UserID, Role: identity.RoleAdmin, Kind: client.Kind, TokenID: client.TokenID, ClientInstallationID: client.ClientInstallationID},
		"no installation": {UserID: client.UserID, Role: client.Role, Kind: client.Kind, TokenID: client.TokenID},
		"no token":        {UserID: client.UserID, Role: client.Role, Kind: client.Kind, ClientInstallationID: client.ClientInstallationID},
		"API key":         {UserID: client.UserID, Role: client.Role, Kind: identity.KindAPIKey, TokenID: client.TokenID},
		"scoped client":   {UserID: client.UserID, Role: client.Role, Kind: client.Kind, TokenID: client.TokenID, ClientInstallationID: client.ClientInstallationID, WorkspaceIDs: []string{"w1234567"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateClientAccountPrincipal(principal); err == nil {
				t.Fatal("invalid client principal was accepted")
			}
		})
	}
	portal := identity.Principal{UserID: client.UserID, Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: client.TokenID}
	if err := validateClientAccountPrincipal(portal); err != nil {
		t.Fatalf("valid portal session rejected: %v", err)
	}
}

func TestClientLoginClosesInstallationAfterCredentialCommit(t *testing.T) {
	credentials, users, _, _, _, _ := newCredentialFixture(t)
	events := []string{}
	store := &clientRevocationStore{events: &events, loginInstallID: "i1234567"}
	hub := &clientRevocationHub{events: &events}
	service, err := NewClientLoginService(store, credentials, nil, hub)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Login(context.Background(), "alice", "correct horse", "desktop", "Desk", "", "login-key-01", "127.0.0.1",
		ClientLoginAttempt{InstanceID: "123e4567-e89b-12d3-a456-426614174000", Generation: 1})
	if err != nil || result.InstallationID != "i1234567" {
		t.Fatalf("Login() = %#v, %v", result, err)
	}
	if got, want := strings.Join(events, ","), "login-commit:i1234567,hub-close:i1234567"; got != want {
		t.Fatalf("login events = %q, want %q", got, want)
	}
	expectedUser := users.values["u1234567"]
	if store.authenticated.ID != expectedUser.ID || store.authenticated.PasswordHash != expectedUser.PasswordHash ||
		store.authenticated.Role != expectedUser.Role || store.authenticated.Status != expectedUser.Status ||
		store.authenticated.MustChangePassword != expectedUser.MustChangePassword {
		t.Fatalf("LoginClient() identity snapshot = %#v, want %#v", store.authenticated, expectedUser)
	}
}
