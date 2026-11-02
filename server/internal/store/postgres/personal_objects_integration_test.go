package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

func TestPostgresGetPersonalObjectAndRechecksAuthorization(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	insertTestPersonalObject(t, store, owner.ID, false)

	got, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeProfile, personal.ProfileObjectID)
	var body struct {
		Name   string `json:"name"`
		Avatar string `json:"avatar"`
	}
	decodeErr := json.Unmarshal(got.Body, &body)
	if err != nil || got.ObjectType != personal.ObjectTypeProfile || got.ObjectID != personal.ProfileObjectID ||
		got.Revision != 4 || decodeErr != nil || body.Name != "Muse" || body.Avatar != "dragon" {
		t.Fatalf("GetPersonalObject() = %+v, read error %v, decode error %v", got, err, decodeErr)
	}

	missingOwner := createTestUser(t, store, 3, 4000, 8192)
	missingPrincipal := createPersonalReadSessionPrincipal(t, store, missingOwner.ID)
	if _, err := store.GetPersonalObject(ctx, missingPrincipal, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing profile error = %v, want not_found", err)
	}

	deletedOwner := createTestUser(t, store, 3, 4000, 8192)
	deletedPrincipal := createPersonalReadSessionPrincipal(t, store, deletedOwner.ID)
	insertTestPersonalObject(t, store, deletedOwner.ID, true)
	if _, err := store.GetPersonalObject(ctx, deletedPrincipal, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted profile error = %v, want not_found", err)
	}

	if err := store.RevokeToken(ctx, principal.TokenID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked session error = %v, want unauthenticated", err)
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
	if got, err := store.GetPersonalObject(ctx, clientPrincipal, personal.ObjectTypeProfile, personal.ProfileObjectID); err != nil || got.Revision != 4 {
		t.Fatalf("client profile read = %+v, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp() WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPersonalObject(ctx, clientPrincipal, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked installation error = %v, want unauthenticated", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET status='disabled' WHERE id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPersonalObject(ctx, clientPrincipal, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("disabled account error = %v, want unauthenticated", err)
	}
}

func createPersonalReadSessionPrincipal(t *testing.T, store *Store, ownerID string) identity.Principal {
	t.Helper()
	tokenID := testSessionTokenID(ownerID)
	_, secretHash, err := identity.NewCredential(identity.KindSession, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(context.Background(), identity.Token{
		ID: tokenID, OwnerID: ownerID, Kind: identity.KindSession, SecretHash: secretHash,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: tokenID}
}

func insertTestPersonalObject(t *testing.T, store *Store, ownerID string, deleted bool) {
	t.Helper()
	ctx := context.Background()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO account_change_counters (service_instance_id, owner_id, last_sequence)
VALUES ($1::uuid,$2,1)`, instanceID, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO account_object_versions
(service_instance_id, owner_id, object_type, object_id, revision, is_deleted)
VALUES ($1::uuid,$2,$3,$4,4,$5)`, instanceID, ownerID, personal.ObjectTypeProfile, personal.ProfileObjectID, deleted); err != nil {
		t.Fatal(err)
	}
	var body any = `{"name":"Muse","avatar":"dragon"}`
	if deleted {
		body = nil
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO personal_objects
(service_instance_id, owner_id, object_type, object_id, body)
VALUES ($1::uuid,$2,$3,$4,$5::jsonb)`, instanceID, ownerID, personal.ObjectTypeProfile, personal.ProfileObjectID, body); err != nil {
		t.Fatal(err)
	}
}
