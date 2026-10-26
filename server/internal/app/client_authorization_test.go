package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"slices"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type clientAuthorizationFake struct {
	ClientAuthorizationStore
	byID          map[string]identity.ClientAuthorization
	installations map[string]identity.ClientInstallation
	clientToken   identity.Token
	syncToken     *identity.Token
}

func (f *clientAuthorizationFake) CreateClientAuthorization(_ context.Context, value identity.ClientAuthorization) error {
	f.byID[value.ID] = value
	return nil
}
func (f *clientAuthorizationFake) GetClientAuthorization(_ context.Context, id string) (identity.ClientAuthorization, error) {
	v, ok := f.byID[id]
	if !ok {
		return identity.ClientAuthorization{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	return v, nil
}
func (f *clientAuthorizationFake) GetClientAuthorizationByCode(_ context.Context, hash []byte) (identity.ClientAuthorization, error) {
	for _, v := range f.byID {
		if subtle.ConstantTimeCompare(v.UserCodeHash, hash) == 1 {
			return v, nil
		}
	}
	return identity.ClientAuthorization{}, domain.NewError(domain.CodeNotFound, "not found")
}
func (f *clientAuthorizationFake) ApproveClientAuthorization(_ context.Context, hash []byte, owner string, grants []string, newID, _ string, expiresAt time.Time) (identity.ClientInstallation, error) {
	v, err := f.GetClientAuthorizationByCode(context.Background(), hash)
	if err != nil || v.State != "pending" {
		return identity.ClientInstallation{}, domain.NewError(domain.CodeConflict, "not pending")
	}
	id := newID
	if v.RenewalInstallationID != "" {
		id = v.RenewalInstallationID
	}
	installation := identity.ClientInstallation{ID: id, OwnerID: owner, DisplayName: v.DisplayName, Platform: v.Platform, WorkspaceIDs: append([]string{}, grants...), ExpiresAt: expiresAt, CreatedAt: time.Now()}
	f.installations[id] = installation
	v.State = "approved"
	v.OwnerID = owner
	v.InstallationID = id
	v.WorkspaceIDs = append([]string{}, grants...)
	f.byID[v.ID] = v
	return installation, nil
}
func (f *clientAuthorizationFake) DenyClientAuthorization(_ context.Context, hash []byte, _, _ string) error {
	v, err := f.GetClientAuthorizationByCode(context.Background(), hash)
	if err != nil || v.State != "pending" {
		return domain.NewError(domain.CodeConflict, "not pending")
	}
	v.State = "denied"
	f.byID[v.ID] = v
	return nil
}
func (f *clientAuthorizationFake) ClaimClientAuthorization(_ context.Context, id string, hash []byte, client identity.Token, sync *identity.Token) (identity.ClientInstallation, error) {
	v := f.byID[id]
	if v.State != "approved" || subtle.ConstantTimeCompare(v.SecretHash, hash) != 1 {
		return identity.ClientInstallation{}, identity.ErrInvalidCredential
	}
	v.State = "claimed"
	f.byID[id] = v
	f.clientToken = client
	f.syncToken = sync
	return f.installations[v.InstallationID], nil
}
func (f *clientAuthorizationFake) ListClientInstallations(_ context.Context, owner string) ([]identity.ClientInstallation, error) {
	result := make([]identity.ClientInstallation, 0)
	for _, v := range f.installations {
		if v.OwnerID == owner {
			result = append(result, v)
		}
	}
	return result, nil
}
func (f *clientAuthorizationFake) GetClientInstallation(_ context.Context, id string) (identity.ClientInstallation, error) {
	return f.installations[id], nil
}
func (f *clientAuthorizationFake) RevokeClientInstallation(_ context.Context, id, owner, _ string) error {
	v, ok := f.installations[id]
	if !ok || v.OwnerID != owner {
		return domain.NewError(domain.CodeNotFound, "not found")
	}
	now := time.Now()
	v.RevokedAt = &now
	f.installations[id] = v
	return nil
}

func TestClientAuthorizationOneTimeDesktopAndMobile(t *testing.T) {
	store := &clientAuthorizationFake{byID: map[string]identity.ClientAuthorization{}, installations: map[string]identity.ClientInstallation{}}
	workspaces := &credentialWorkspaces{values: map[string]workspace.Workspace{"w1234567": {ID: "w1234567", OwnerID: "u1234567", State: workspace.StateReady}}}
	service, err := NewClientAuthorizationService(store, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	session := identity.Principal{Kind: identity.KindSession, UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser}
	ctx := context.Background()
	for _, tc := range []struct {
		platform string
		sync     bool
		grants   []string
	}{{"desktop", true, []string{"w1234567"}}, {"android", false, []string{}}} {
		started, err := service.Start(ctx, tc.platform, "test", tc.sync, "", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if state, err := service.Poll(ctx, started.DeviceCode, "127.0.0.1"); err != nil || state.State != "authorization_pending" {
			t.Fatalf("pending = %#v %v", state, err)
		}
		approved, err := service.Approve(ctx, session, started.UserCode, tc.grants, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		listed, err := service.List(ctx, session)
		if err != nil || len(listed) == 0 {
			t.Fatalf("list approved installation = %#v %v", listed, err)
		}
		matched := false
		for _, item := range listed {
			if item.ID == approved.ID {
				matched = item.ExpiresAt.Equal(approved.ExpiresAt) && slices.Equal(item.WorkspaceIDs, approved.WorkspaceIDs)
			}
		}
		if !matched {
			t.Fatalf("approved installation differs from list: %#v %#v", approved, listed)
		}
		claimed, err := service.Poll(ctx, started.DeviceCode, "127.0.0.1")
		if err != nil || claimed.State != "approved" || claimed.ClientCredential == "" || (claimed.SyncCredential != "") != tc.sync ||
			!claimed.ClientExpiresAt.Equal(approved.ExpiresAt) || (tc.sync && !claimed.SyncExpiresAt.Equal(approved.ExpiresAt)) {
			t.Fatalf("claim = %#v %v", claimed, err)
		}
		if _, err := service.Poll(ctx, started.DeviceCode, "127.0.0.1"); !errors.Is(err, identity.ErrInvalidCredential) {
			t.Fatalf("replayed claim: %v", err)
		}
		if store.clientToken.Kind != identity.KindClient || store.clientToken.ClientInstallationID != claimed.InstallationID || (store.syncToken != nil) != tc.sync {
			t.Fatalf("tokens = %#v %#v", store.clientToken, store.syncToken)
		}
	}
}

func TestClientAuthorizationRejectsMobileSyncAndForeignGrants(t *testing.T) {
	store := &clientAuthorizationFake{byID: map[string]identity.ClientAuthorization{}, installations: map[string]identity.ClientInstallation{}}
	workspaces := &credentialWorkspaces{values: map[string]workspace.Workspace{"w1234567": {ID: "w1234567", OwnerID: "u7654321", State: workspace.StateReady}}}
	service, _ := NewClientAuthorizationService(store, workspaces)
	ctx := context.Background()
	if _, err := service.Start(ctx, "ios", "phone", true, "", "127.0.0.1"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("mobile sync = %v", err)
	}
	started, err := service.Start(ctx, "desktop", "desk", true, "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	session := identity.Principal{Kind: identity.KindSession, UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser}
	if _, err := service.Approve(ctx, session, started.UserCode, []string{"w1234567"}, "127.0.0.1"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("foreign grant = %v", err)
	}
	if _, err := service.Approve(ctx, session, started.UserCode, []string{}, "127.0.0.1"); err != nil {
		t.Fatalf("empty grants: %v", err)
	}
}

func TestClientAuthorizationDenyExpiryAndSessionGate(t *testing.T) {
	store := &clientAuthorizationFake{byID: map[string]identity.ClientAuthorization{}, installations: map[string]identity.ClientInstallation{}}
	service, err := NewClientAuthorizationService(store, &credentialWorkspaces{values: map[string]workspace.Workspace{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started, err := service.Start(ctx, "ios", "phone", false, "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	client := identity.Principal{Kind: identity.KindClient, UserID: "u1234567", ClientInstallationID: "i1234567"}
	if _, err := service.Inspect(ctx, client, started.UserCode, "127.0.0.1"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("client inspected pairing: %v", err)
	}
	session := identity.Principal{Kind: identity.KindSession, UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleUser}
	if err := service.Deny(ctx, session, started.UserCode, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if state, err := service.Poll(ctx, started.DeviceCode, "127.0.0.1"); err != nil || state.State != "denied" {
		t.Fatalf("denied poll = %#v %v", state, err)
	}
	if _, err := service.Approve(ctx, session, started.UserCode, []string{}, "127.0.0.1"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("approved denied code: %v", err)
	}
	expired, err := service.Start(ctx, "android", "tablet", false, "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, id, _, err := identity.ParseCredential(expired.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	pairing := store.byID[id]
	pairing.ExpiresAt = time.Now().Add(-time.Second)
	store.byID[id] = pairing
	if state, err := service.Poll(ctx, expired.DeviceCode, "127.0.0.1"); err != nil || state.State != "expired" {
		t.Fatalf("expired poll = %#v %v", state, err)
	}
}
