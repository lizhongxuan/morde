package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestHostSharingPersistenceAndTicketActor(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	owner := createTestUser(t, store, 2, 10_000, 20_000)
	member := createTestUser(t, store, 2, 10_000, 20_000)
	created := createTestHost(t, ctx, store, owner.ID, capacity, nil)
	grant := host.Member{
		HostID: created.ID, OwnerID: owner.ID, UserID: member.ID,
		Permissions: []host.Permission{host.PermissionDesktopView},
	}
	if value, err := store.UpsertHostMember(ctx, grant); err != nil || value.UserID != member.ID || value.RevokedAt != nil {
		t.Fatalf("upsert host member = %#v, %v", value, err)
	}
	access, err := store.GetHostActorAccess(ctx, created.ID, member.ID)
	if err != nil || access.OwnerID != owner.ID || access.Disabled || len(access.Permissions) != 1 || access.Permissions[0] != host.PermissionDesktopView {
		t.Fatalf("member access = %#v, %v", access, err)
	}
	shared, err := store.ListSharedHosts(ctx, member.ID)
	if err != nil || len(shared) != 1 || shared[0].HostID != created.ID {
		t.Fatalf("shared Hosts = %#v, %v", shared, err)
	}
	if err := store.RevokeHostMember(ctx, created.ID, owner.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	access, err = store.GetHostActorAccess(ctx, created.ID, member.ID)
	if err != nil || len(access.Permissions) != 0 {
		t.Fatalf("access after revoke = %#v, %v", access, err)
	}
	shared, err = store.ListSharedHosts(ctx, member.ID)
	if err != nil || len(shared) != 0 {
		t.Fatalf("shared Hosts after revoke = %#v, %v", shared, err)
	}
	if restored, err := store.UpsertHostMember(ctx, grant); err != nil || restored.RevokedAt != nil {
		t.Fatalf("restore host grant = %#v, %v", restored, err)
	}

	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateToken(ctx, identity.Token{
		ID: tokenID, OwnerID: owner.ID, ActorUserID: member.ID, Kind: identity.KindTicket,
		HostID: created.ID, SecretHash: []byte("ticket-secret-hash"), ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("create member ticket: %v", err)
	}
	ticket, err := store.ConsumeTicket(ctx, tokenID)
	if err != nil || ticket.UserID != owner.ID || ticket.ActorUserID != member.ID || ticket.HostID != created.ID {
		t.Fatalf("consumed member ticket = %#v, %v", ticket, err)
	}

	member.Disabled = true
	if _, err := store.UpdateUser(ctx, "", member); err != nil {
		t.Fatal(err)
	}
	access, err = store.GetHostActorAccess(ctx, created.ID, member.ID)
	if err != nil || !access.Disabled {
		t.Fatalf("disabled user access = %#v, %v", access, err)
	}
}
