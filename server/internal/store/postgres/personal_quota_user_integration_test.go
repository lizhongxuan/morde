package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestPostgresGetPersonalDataQuotaUserUsesOwnerDefaultsAndRechecksAuthorization(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	service, err := app.NewPersonalDataQuotaUserService(store)
	if err != nil {
		t.Fatal(err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("new user quota rows=%d err=%v, want no preexisting row", rows, err)
	}
	quota, err := service.GetPersonalDataQuota(ctx, principal)
	if err != nil || quota.OwnerID != owner.ID || quota.UsedBytes != 0 || quota.ReservedBytes != 0 ||
		quota.QuotaBytes != app.DefaultPersonalDataQuotaBytes || quota.MaxAttachmentBytes != app.DefaultPersonalDataMaxAttachmentBytes || quota.Version != 1 {
		t.Fatalf("default owner quota = %+v, err=%v", quota, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("read created a quota row: rows=%d err=%v", rows, err)
	}

	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO personal_data_quotas
(service_instance_id,owner_id,used_bytes,reserved_bytes,quota_bytes,max_attachment_bytes,version)
VALUES ($1::uuid,$2,120,30,1000,512,4)`, instanceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	quota, err = service.GetPersonalDataQuota(ctx, principal)
	if err != nil || quota.OwnerID != owner.ID || quota.UsedBytes != 120 || quota.ReservedBytes != 30 ||
		quota.QuotaBytes != 1000 || quota.MaxAttachmentBytes != 512 || quota.Version != 4 {
		t.Fatalf("stored owner quota = %+v, err=%v", quota, err)
	}

	other := createTestUser(t, store, 3, 4000, 8192)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, other.ID)
	otherPrincipal.UserID = owner.ID
	if _, err := service.GetPersonalDataQuota(ctx, otherPrincipal); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("mismatched user and session token error = %v, want unauthenticated", err)
	}

	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(ctx, clientToken); err != nil {
		t.Fatal(err)
	}
	clientPrincipal := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: clientToken.ID, ClientInstallationID: installationID,
	}
	quota, err = service.GetPersonalDataQuota(ctx, clientPrincipal)
	if err != nil || quota.OwnerID != owner.ID || quota.UsedBytes != 120 || quota.ReservedBytes != 30 {
		t.Fatalf("client quota = %+v, err=%v", quota, err)
	}
	if err := store.RevokeToken(ctx, clientToken.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetPersonalDataQuota(ctx, clientPrincipal); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked client credential error = %v, want unauthenticated", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET status='disabled' WHERE id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetPersonalDataQuota(ctx, principal); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("disabled account error = %v, want unauthenticated", err)
	}
}

func TestPersonalDataQuotaUserStoreRejectsEscalatedPrincipalsBeforeTransaction(t *testing.T) {
	store := &Store{}
	ownerID := "u1234567"
	tokenID := "t1234567"
	for _, principal := range []identity.Principal{
		{UserID: ownerID, Role: identity.RoleAdmin, Kind: identity.KindClient, TokenID: tokenID, ClientInstallationID: "i1234567"},
		{UserID: ownerID, Role: identity.RoleSuperAdmin, Kind: identity.KindClient, TokenID: tokenID, ClientInstallationID: "i1234567"},
		{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: tokenID, HostScope: "h1234567"},
		{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindClient, TokenID: tokenID, ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeTaskRead}},
		{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindHostCookie, TokenID: tokenID, HostScope: "h1234567"},
	} {
		if _, err := store.GetPersonalDataQuotaUser(context.Background(), principal); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("escalated principal %+v error = %v, want forbidden", principal, err)
		}
	}
}
