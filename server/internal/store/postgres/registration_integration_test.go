package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestRegisteredUserWaitsForApprovalBeforeLogin(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 10, LoginWindow: time.Minute,
	}, credentialDefaultHostNoop{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.EnsureInitialAdmin(ctx, store); err != nil {
		t.Fatalf("initialize admin: %v", err)
	}
	registered, err := users.Register(ctx, "new-member", "new-password-123", "192.0.2.1")
	if err != nil || registered.Role != identity.RoleUser || registered.Status != identity.StatusPending {
		t.Fatalf("registered user = %#v, %v", registered, err)
	}
	if _, err := credentials.Login(ctx, "new-member", "wrong-password", "192.0.2.1"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("wrong password error = %v, want unauthenticated", err)
	}
	if _, err := credentials.Login(ctx, "new-member", "new-password-123", "192.0.2.1"); domain.CodeOf(err) != domain.CodeAccountPending {
		t.Fatalf("pending user login error = %v, want account_pending", err)
	}

	admin, err := store.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	adminLogin, err := credentials.Login(ctx, "admin", "admin", "127.0.0.1")
	if err != nil {
		t.Fatalf("initial admin login: %v", err)
	}
	adminPrincipal, err := credentials.VerifySession(ctx, adminLogin.Credential)
	if err != nil || !adminPrincipal.MustChangePassword {
		t.Fatalf("initial admin principal = %#v, %v", adminPrincipal, err)
	}
	if err := credentials.ChangePassword(ctx, adminPrincipal, "admin", "new-admin-password"); err != nil {
		t.Fatalf("initial admin password change: %v", err)
	}
	adminLogin, err = credentials.Login(ctx, "admin", "new-admin-password", "192.0.2.1")
	if err != nil {
		t.Fatalf("admin login after password change: %v", err)
	}
	adminPrincipal, err = credentials.VerifySession(ctx, adminLogin.Credential)
	if err != nil || !adminPrincipal.Role.IsAdmin() || adminPrincipal.MustChangePassword {
		t.Fatalf("admin principal after password change = %#v, %v", adminPrincipal, err)
	}

	approved := identity.StatusApproved
	if _, err := users.Update(ctx, adminPrincipal, registered.ID, app.UpdateUserCommand{
		ExpectedStatus: identity.StatusPending, Status: &approved,
	}); err != nil {
		t.Fatalf("approve registered account: %v", err)
	}
	login, err := credentials.Login(ctx, "new-member", "new-password-123", "192.0.2.1")
	if err != nil || login.User.ID != registered.ID || login.User.Role != identity.RoleUser || login.Credential == "" {
		t.Fatalf("login after approval = %#v, %v", login.User, err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil || principal.Role != identity.RoleUser {
		t.Fatalf("registered session = %#v, %v", principal, err)
	}
	role := identity.RoleAdmin
	if _, err := users.Update(ctx, principal, registered.ID, app.UpdateUserCommand{
		ExpectedStatus: identity.StatusApproved, Role: &role,
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("ordinary user admin promotion error = %v, want forbidden", err)
	}
	admin, err = store.GetUserByID(ctx, admin.ID)
	if err != nil || admin.PasswordHash == "initial-hash" || admin.MustChangePassword {
		t.Fatalf("admin password state = %#v, %v", admin, err)
	}
}

func TestUserReviewMigrationRejectsExistingAccountsWithoutEnabledAdmin(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 45); err != nil {
		t.Fatalf("roll schema back to before account review: %v", err)
	}
	userID, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, password_hash, role, quota_hosts, quota_cpu_milli, quota_mem_mib, disabled)
VALUES ($1, 'legacy-member', 'member-hash', 'user', 3, 4000, 8192, false)`, userID); err != nil {
		t.Fatalf("seed pre-review user without admin: %v", err)
	}
	if _, err := provider.UpTo(ctx, 46); err == nil {
		t.Fatal("migration unexpectedly accepted existing accounts without an enabled admin")
	}
	var latest int64
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&latest); err != nil || latest != 45 {
		t.Fatalf("schema version after rejected migration = %d, %v; want 45", latest, err)
	}
}
