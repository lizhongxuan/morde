package postgres

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	sitedomain "morde/server/internal/domain/site"
)

func TestPostgresInvitationsCaptureAttributionWithoutRewardsOrReviewBypass(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	inviter := createTestUser(t, store, 3, 4000, 8192)
	adminOps, err := app.NewAdminOperationsService(store)
	if err != nil {
		t.Fatal(err)
	}
	code, err := adminOps.InviteCode(ctx, identity.Principal{UserID: inviter.ID, TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleUser})
	if err != nil || len(code) != 32 {
		t.Fatalf("get inviter code = %q, %v", code, err)
	}
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := users.Register(ctx, "referred-user", "a-long-password", "192.0.2.22", code)
	if err != nil || registered.Status != identity.StatusPending || registered.Role != identity.RoleUser {
		t.Fatalf("referred registration = %+v, %v", registered, err)
	}
	var inviterID, inviteeID, storedCode string
	if err := store.pool.QueryRow(ctx, `SELECT inviter_id,invitee_id,invite_code FROM user_invites WHERE invitee_id=$1`, registered.ID).
		Scan(&inviterID, &inviteeID, &storedCode); err != nil || inviterID != inviter.ID || inviteeID != registered.ID || storedCode != code {
		t.Fatalf("stored invitation = %s/%s/%s err=%v", inviterID, inviteeID, storedCode, err)
	}
	var budgets, ledger int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_account_budgets WHERE owner_id=$1`, registered.ID).Scan(&budgets); err != nil || budgets != 0 {
		t.Fatalf("new-user model budgets=%d err=%v", budgets, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1`, registered.ID).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("new-user model ledger rows=%d err=%v", ledger, err)
	}
	reservation := app.ModelCallReservation{Source: app.ModelCallClient, OwnerID: registered.ID, ModelID: "p1234567",
		ProviderModel: "test-model", CallID: "no-default-budget-01", RequestDigest: make([]byte, 32), ReservedTokens: 1}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("unconfigured model budget error = %v", err)
	}
	admin := personalQuotaAdminPrincipal(t, store)
	invites, err := adminOps.ListInvites(ctx, admin, app.AdminInviteFilter{Limit: 25})
	if err != nil || invites.Total != 1 || invites.TotalInviters != 1 || len(invites.Items) != 1 ||
		invites.Items[0].InviteeStatus != identity.StatusPending || invites.Items[0].InviterInvitees != 1 {
		t.Fatalf("admin invitation history = %+v, %v", invites, err)
	}
	if _, err := users.Register(ctx, "invalid-referral", "a-long-password", "192.0.2.23", strings.Repeat("0", 32)); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unknown invitation code error = %v", err)
	}
	var invalidCount int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE name='invalid-referral'`).Scan(&invalidCount); err != nil || invalidCount != 0 {
		t.Fatalf("invalid code created user count=%d err=%v", invalidCount, err)
	}
}

func TestPostgresAdminNudgesUseCASAndAuditAtomically(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	service, err := app.NewAdminOperationsService(store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := service.NudgePolicy(ctx, admin)
	if err != nil || initial.Enabled || initial.Version != 1 || initial.URL != "" || initial.UpdatedAt != nil {
		t.Fatalf("default nudge = %+v, %v", initial, err)
	}
	updated, err := service.UpdateNudgePolicy(ctx, admin, app.UpdateAdminNudgeCommand{ExpectedVersion: 1, Enabled: true,
		URL: "https://github.com/morde/example", TextZH: "支持 Morde", IP: "192.0.2.33"})
	if err != nil || !updated.Enabled || updated.Version != 2 || updated.UpdatedAt == nil {
		t.Fatalf("nudge update = %+v, %v", updated, err)
	}
	var auditActor, auditKind, auditAction, auditIP string
	if err := store.pool.QueryRow(ctx, `SELECT actor_id,actor_kind,action,ip FROM audit_logs WHERE action=$1 AND target='admin_nudge_policy' ORDER BY id DESC LIMIT 1`,
		string(audit.ActionAdminNudgePolicyUpdate)).Scan(&auditActor, &auditKind, &auditAction, &auditIP); err != nil || auditActor != admin.UserID ||
		auditKind != string(audit.ActorSession) || auditAction != string(audit.ActionAdminNudgePolicyUpdate) || auditIP != "192.0.2.33" {
		t.Fatalf("nudge audit = %s/%s/%s/%s err=%v", auditActor, auditKind, auditAction, auditIP, err)
	}
	if _, err := service.UpdateNudgePolicy(ctx, admin, app.UpdateAdminNudgeCommand{ExpectedVersion: 1, Enabled: false}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale nudge update = %v, want conflict", err)
	}
	public, err := service.PublicNudgePolicy(ctx)
	if err != nil || !public.Enabled || public.URL != updated.URL || public.TextZH != updated.TextZH {
		t.Fatalf("public nudge = %+v, %v", public, err)
	}
}

func TestPostgresAdminSiteStatisticsUseLocalSiteAndReleaseRows(t *testing.T) {
	store, _, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	created, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	release, err := store.CreateCandidateRelease(ctx, newTestCandidate(t, created.ID, workspaceID), 1<<20, testSiteOwnerMaxBytes)
	if err != nil || release.State != sitedomain.ReleaseCandidate {
		t.Fatalf("create local site release = %+v, %v", release, err)
	}
	service, err := app.NewAdminOperationsService(store)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ListSites(ctx, personalQuotaAdminPrincipal(t, store), 25, 0)
	if err != nil || page.Total != 1 || page.Summary.Sites != 1 || page.Summary.ActiveSites != 1 || page.Summary.Releases != 1 ||
		page.Summary.HealthyReleases != 0 || len(page.Items) != 1 || page.Items[0].ID != string(created.ID) ||
		page.Items[0].ReleaseCount != 1 || page.Items[0].HealthyReleaseCount != 0 || page.Items[0].LatestReleaseAt == nil {
		t.Fatalf("local site admin page = %+v, %v", page, err)
	}
}

func TestPostgresAdminNudgeAuditFailureRollsBackPolicyChange(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	admin := personalQuotaAdminPrincipal(t, store)
	service, err := app.NewAdminOperationsService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE audit_logs ADD CONSTRAINT test_fail_admin_nudge_audit
CHECK (action <> 'admin.nudge.policy.update')`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateNudgePolicy(ctx, admin, app.UpdateAdminNudgeCommand{ExpectedVersion: 1, Enabled: true,
		URL: "https://github.com/morde/example", TextEN: "Support Morde", IP: "192.0.2.34"}); err == nil {
		t.Fatal("nudge policy update succeeded despite audit insert failure")
	}
	value, err := service.NudgePolicy(ctx, admin)
	if err != nil || value.Enabled || value.Version != 1 || value.URL != "" || value.UpdatedAt != nil {
		t.Fatalf("nudge after failed audit = %+v, %v", value, err)
	}
}
