package postgres

import (
	"context"
	"sync"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestAdminUserUpdateProtectsSelfAndLastAdministrator(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	admin := createTestAdmin(t, store)
	member := createTestUser(t, store, 3, 20_000, 40_000)

	selfDisabled := admin
	selfDisabled.Disabled = true
	if _, err := store.UpdateUser(ctx, admin.ID, selfDisabled); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("self-disable error = %v, want forbidden", err)
	}
	selfDemoted := admin
	selfDemoted.Role = identity.RoleUser
	if _, err := store.UpdateUser(ctx, admin.ID, selfDemoted); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("self-demotion error = %v, want forbidden", err)
	}

	lastAdminDemoted := admin
	lastAdminDemoted.Role = identity.RoleUser
	if _, err := store.UpdateUser(ctx, member.ID, lastAdminDemoted); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("last-admin demotion error = %v, want invalid state", err)
	}

	secondAdmin := createTestAdmin(t, store)
	admin.Disabled = true
	if _, err := store.UpdateUser(ctx, secondAdmin.ID, admin); err != nil {
		t.Fatalf("disable one of two administrators: %v", err)
	}
	secondAdmin.Role = identity.RoleUser
	if _, err := store.UpdateUser(ctx, member.ID, secondAdmin); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("demote remaining administrator error = %v, want invalid state", err)
	}
}

func TestAdminUserUpdateSerializesConcurrentLastAdminChanges(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	first := createTestAdmin(t, store)
	second := createTestAdmin(t, store)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, attempt := range []struct {
		actor string
		user  identity.User
	}{
		{actor: first.ID, user: second},
		{actor: second.ID, user: first},
	} {
		attempt := attempt
		attempt.user.Disabled = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.UpdateUser(ctx, attempt.actor, attempt.user)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded, rejected := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if domain.CodeOf(err) == domain.CodeInvalidState {
			rejected++
		} else {
			t.Fatalf("concurrent admin change error = %v", err)
		}
	}
	var activeAdmins int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled=false`).Scan(&activeAdmins); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || rejected != 1 || activeAdmins != 1 {
		t.Fatalf("concurrent changes: succeeded=%d rejected=%d active_admins=%d", succeeded, rejected, activeAdmins)
	}
}

func createTestAdmin(t *testing.T, store *Store) identity.User {
	t.Helper()
	user := createTestUser(t, store, 3, 20_000, 40_000)
	user.Role = identity.RoleAdmin
	updated, err := store.UpdateUser(context.Background(), "", user)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
