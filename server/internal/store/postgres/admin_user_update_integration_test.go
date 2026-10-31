package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

func TestUserUpdateUsesExpectedStatusAndRevokesCredentials(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	admin := createTestSuperAdmin(t, store)
	member := createTestUser(t, store, 3, 20_000, 40_000)
	tokenIDs := createTestCredentials(t, store, member.ID)

	member.Status = identity.StatusDisabled
	updated, err := store.UpdateUser(ctx, admin.ID, identity.StatusApproved, member)
	if err != nil || updated.Status != identity.StatusDisabled {
		t.Fatalf("disable member = %#v, %v", updated, err)
	}
	if _, err := store.UpdateUser(ctx, admin.ID, identity.StatusApproved, member); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale account status error = %v, want conflict", err)
	}
	for _, id := range tokenIDs {
		var revoked bool
		if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM tokens WHERE id=$1`, id).Scan(&revoked); err != nil || !revoked {
			t.Fatalf("credential %s revoked = %t, %v", id, revoked, err)
		}
	}
	users, err := store.ListUsersByStatus(ctx, statusPointer(identity.StatusDisabled))
	if err != nil || len(users) != 1 || users[0].ID != member.ID {
		t.Fatalf("disabled user list = %#v, %v", users, err)
	}

	member.Status = identity.StatusApproved
	if _, err := store.UpdateUser(ctx, admin.ID, identity.StatusDisabled, member); err != nil {
		t.Fatalf("restore member: %v", err)
	}
	member.Role = identity.RoleAdmin
	memberTokenIDs := createTestCredentials(t, store, member.ID)
	if _, err := store.UpdateUser(ctx, admin.ID, identity.StatusApproved, member); err != nil {
		t.Fatalf("promote approved member: %v", err)
	}
	for _, id := range memberTokenIDs {
		var revoked bool
		if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM tokens WHERE id=$1`, id).Scan(&revoked); err != nil || !revoked {
			t.Fatalf("credential %s revoked after role change = %t, %v", id, revoked, err)
		}
	}

	admin.Status = identity.StatusDisabled
	if _, err := store.UpdateUser(ctx, member.ID, identity.StatusApproved, admin); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("ordinary administrator changing super admin status error = %v, want forbidden", err)
	}
	admin.Status = identity.StatusApproved
	admin.Role = identity.RoleAdmin
	if _, err := store.UpdateUser(ctx, admin.ID, identity.StatusApproved, admin); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("super admin demotion error = %v, want forbidden", err)
	}
}

func TestQuotaUpdateKeepsCredentialsAndSuperAdminPasswordIsNotReset(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	admin := createTestSuperAdmin(t, store)
	member := createTestUser(t, store, 3, 20_000, 40_000)
	tokenIDs := createTestCredentials(t, store, member.ID)

	member.QuotaHosts++
	if _, err := store.UpdateUser(ctx, admin.ID, identity.StatusApproved, member); err != nil {
		t.Fatalf("quota update: %v", err)
	}
	for _, id := range tokenIDs {
		var revoked bool
		if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM tokens WHERE id=$1`, id).Scan(&revoked); err != nil || revoked {
			t.Fatalf("credential %s revoked by quota update = %t, %v", id, revoked, err)
		}
	}

	adminTokenIDs := createTestCredentials(t, store, admin.ID)
	admin.PasswordHash = "changed-hash"
	admin.MustChangePassword = false
	updated, err := store.UpdateUser(ctx, admin.ID, identity.StatusApproved, admin)
	if err != nil || updated.PasswordHash != "changed-hash" || updated.MustChangePassword {
		t.Fatalf("super admin password change = %#v, %v", updated, err)
	}
	for _, id := range adminTokenIDs {
		var revoked bool
		if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM tokens WHERE id=$1`, id).Scan(&revoked); err != nil || !revoked {
			t.Fatalf("super admin credential %s revoked = %t, %v", id, revoked, err)
		}
	}
	newCandidate := initialAdminUser(t, "new-initial-hash")
	if err := store.EnsureInitialAdmin(ctx, newCandidate); err != nil {
		t.Fatalf("idempotent initial admin check: %v", err)
	}
	stored, err := store.GetUserByName(ctx, "admin")
	if err != nil || stored.PasswordHash != "changed-hash" || stored.MustChangePassword {
		t.Fatalf("initialization overwrote changed password = %#v, %v", stored, err)
	}
}

func TestUserUpdateAndAuditAreAtomic(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	admin := createTestSuperAdmin(t, store)
	member, err := store.CreateUser(ctx, pendingTestUser(t))
	if err != nil {
		t.Fatal(err)
	}
	tokenIDs := createTestCredentials(t, store, member.ID)
	member.Status = identity.StatusApproved
	_, err = store.UpdateUserWithAudit(ctx, admin.ID, identity.StatusPending, member, audit.Entry{
		ActorID: admin.ID, ActorKind: audit.ActorKind("invalid"), Action: audit.ActionUserUpdate,
		Target: member.ID, Detail: map[string]any{"status": identity.StatusApproved},
	}, nil)
	if err == nil {
		t.Fatal("invalid audit entry unexpectedly committed user update")
	}
	stored, err := store.GetUserByID(ctx, member.ID)
	if err != nil || stored.Status != identity.StatusPending {
		t.Fatalf("user status after audit failure = %#v, %v", stored, err)
	}
	for _, id := range tokenIDs {
		var revoked bool
		if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM tokens WHERE id=$1`, id).Scan(&revoked); err != nil || revoked {
			t.Fatalf("credential %s revoked after rolled-back update = %t, %v", id, revoked, err)
		}
	}
}

func TestEnsureInitialAdminSerializesFirstCreationAndRejectsNonEmptyStore(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	first, second := initialAdminUser(t, "first-hash"), initialAdminUser(t, "second-hash")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, value := range []identity.User{first, second} {
		value := value
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- store.EnsureInitialAdmin(ctx, value)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent initial admin creation: %v", err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='super_admin'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("super admin count = %d, %v", count, err)
	}

	other, err := store.CreateUser(ctx, pendingTestUser(t))
	if err != nil {
		t.Fatal(err)
	}
	otherCandidate := initialAdminUser(t, "must-not-be-created")
	if err := store.EnsureInitialAdmin(ctx, otherCandidate); err != nil {
		t.Fatalf("existing super admin should make initialization idempotent: %v", err)
	}
	if _, err := store.GetUserByID(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureInitialAdminFailsClosedWhenUsersExistWithoutAdmin(t *testing.T) {
	store, _ := newTemporaryStore(t)
	if _, err := store.CreateUser(context.Background(), pendingTestUser(t)); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInitialAdmin(context.Background(), initialAdminUser(t, "hash")); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("non-empty account table without admin error = %v, want invalid state", err)
	}
}

func createTestSuperAdmin(t *testing.T, store *Store) identity.User {
	t.Helper()
	value := initialAdminUser(t, "initial-hash")
	if err := store.EnsureInitialAdmin(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetUserByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func initialAdminUser(t *testing.T, passwordHash string) identity.User {
	t.Helper()
	id, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	return identity.User{
		ID: id, Name: "admin", PasswordHash: passwordHash, Role: identity.RoleSuperAdmin,
		Status: identity.StatusApproved, MustChangePassword: true,
		QuotaHosts: 3, QuotaCPUMilli: 4000, QuotaMemMiB: 8192,
	}
}

func pendingTestUser(t *testing.T) identity.User {
	t.Helper()
	id, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	return identity.User{
		ID: id, Name: "user-" + id, PasswordHash: "hash", Role: identity.RoleUser,
		Status: identity.StatusPending, QuotaHosts: 3, QuotaCPUMilli: 4000, QuotaMemMiB: 8192,
		DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048,
	}
}

func createTestCredentials(t *testing.T, store *Store, ownerID string) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, 4)
	installationID, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id, owner_id, display_name, platform, expires_at)
VALUES ($1, $2, 'test client', 'desktop', now()+interval '1 day')`, installationID, ownerID); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []identity.Kind{identity.KindSession, identity.KindAPIKey, identity.KindDevice} {
		id, err := domain.NewTokenID()
		if err != nil {
			t.Fatal(err)
		}
		token := identity.Token{ID: id, OwnerID: ownerID, Kind: kind, SecretHash: []byte("secret-hash"), ExpiresAt: time.Now().Add(time.Hour)}
		if kind == identity.KindDevice {
			token.Scopes = []identity.Scope{identity.ScopeWorkspaceSync}
			token.ClientInstallationID = installationID
		}
		if _, err := store.CreateToken(ctx, token); err != nil {
			t.Fatalf("create %s token: %v", kind, err)
		}
		ids = append(ids, id)
	}
	clientTokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateToken(ctx, identity.Token{
		ID: clientTokenID, OwnerID: ownerID, ClientInstallationID: installationID,
		Kind: identity.KindClient, SecretHash: []byte("client-secret-hash"), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create client token: %v", err)
	}
	ids = append(ids, clientTokenID)
	return ids
}

func statusPointer(value identity.AccountStatus) *identity.AccountStatus { return &value }
