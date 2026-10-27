package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestUserReviewMigrationPreservesAccountsAndFailsClosedOnAdminConflicts(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeProvider)
	if _, err := provider.DownTo(ctx, 45); err != nil {
		t.Fatalf("return to legacy user schema: %v", err)
	}

	for _, conflict := range []struct {
		name string
		role string
	}{{name: "Admin", role: "user"}, {name: "admin", role: "user"}} {
		id := legacyUserID(t)
		insertLegacyUser(t, ctx, pool, id, conflict.name, conflict.role, false, "legacy-hash")
		if _, err := provider.UpTo(ctx, 46); err == nil {
			t.Fatalf("migration accepted conflicting account %q", conflict.name)
		}
		var version int64
		if err := pool.QueryRow(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY version_id DESC LIMIT 1`).Scan(&version); err != nil || version != 45 {
			t.Fatalf("schema version after rejected conflict = %d, %v", version, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}

	adminID := legacyUserID(t)
	legacyAdminID := legacyUserID(t)
	memberID := legacyUserID(t)
	disabledID := legacyUserID(t)
	insertLegacyUser(t, ctx, pool, adminID, "admin", "admin", false, "keep-admin-hash")
	insertLegacyUser(t, ctx, pool, legacyAdminID, "legacy-admin", "admin", false, "keep-legacy-admin-hash")
	insertLegacyUser(t, ctx, pool, memberID, "member", "user", false, "keep-member-hash")
	insertLegacyUser(t, ctx, pool, disabledID, "disabled-member", "user", true, "keep-disabled-hash")
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tokens (id, owner_id, kind, secret_hash, expires_at)
VALUES ($1, $2, 'session', $3, $4)`, tokenID, memberID, []byte("hash"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("insert legacy session: %v", err)
	}
	if _, err := provider.UpTo(ctx, 46); err != nil {
		t.Fatalf("apply user review migration: %v", err)
	}

	checks := []struct {
		id, role, status string
		mustChange       bool
		passwordHash     string
	}{
		{adminID, string(identity.RoleSuperAdmin), string(identity.StatusApproved), true, "keep-admin-hash"},
		{legacyAdminID, string(identity.RoleAdmin), string(identity.StatusPending), false, "keep-legacy-admin-hash"},
		{memberID, string(identity.RoleUser), string(identity.StatusPending), false, "keep-member-hash"},
		{disabledID, string(identity.RoleUser), string(identity.StatusDisabled), false, "keep-disabled-hash"},
	}
	for _, check := range checks {
		var role, status, passwordHash string
		var mustChange bool
		err := pool.QueryRow(ctx, `SELECT role, status, must_change_password, password_hash FROM users WHERE id=$1`, check.id).
			Scan(&role, &status, &mustChange, &passwordHash)
		if err != nil || role != check.role || status != check.status ||
			mustChange != check.mustChange || passwordHash != check.passwordHash {
			t.Errorf("migrated user %s = role %q, status %q, must_change %t, password_hash %q, %v",
				check.id, role, status, mustChange, passwordHash, err)
		}
	}
	if _, err := provider.UpTo(ctx, 48); err != nil {
		t.Fatalf("restore schema needed by current startup query: %v", err)
	}
	if err := app.EnsureInitialAdmin(ctx, store); err != nil {
		t.Fatalf("startup failed to retain the fixed super administrator: %v", err)
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM tokens WHERE id=$1`, tokenID).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("legacy session revoked = %t, %v", revoked, err)
	}
	if _, err := provider.DownTo(ctx, 45); err != nil {
		t.Fatalf("roll back account review with existing users: %v", err)
	}
	var legacyRole string
	var legacyDisabled bool
	if err := pool.QueryRow(ctx, `SELECT role, disabled FROM users WHERE id=$1`, adminID).Scan(&legacyRole, &legacyDisabled); err != nil || legacyRole != "admin" || legacyDisabled {
		t.Fatalf("fixed administrator after rollback = role %q disabled %t, %v", legacyRole, legacyDisabled, err)
	}
}

func legacyUserID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertLegacyUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, name, role string, disabled bool, passwordHash string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, password_hash, role, disabled)
VALUES ($1, $2, $3, $4, $5)`, id, name, passwordHash, role, disabled); err != nil {
		t.Fatalf("insert legacy user %q: %v", name, err)
	}
}
