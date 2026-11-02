package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

func TestPostgresScheduleGrantRenewalMovesOnlyLiveSchedules(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	principalA := createIOSScheduleGrantPrincipal(t, ctx, store, pool, owner.ID)
	principalB := createIOSScheduleGrantPrincipal(t, ctx, store, pool, owner.ID)

	grantA1 := newScheduleGrantTokenForTest(t, owner.ID, principalA.ClientInstallationID)
	grantB1 := newScheduleGrantTokenForTest(t, owner.ID, principalB.ClientInstallationID)
	if err := store.IssueClientScheduleGrant(ctx, principalA, grantA1); err != nil {
		t.Fatal(err)
	}
	if err := store.IssueClientScheduleGrant(ctx, principalB, grantB1); err != nil {
		t.Fatal(err)
	}
	activeA, err := createClientAuthorizedSchedule(t, ctx, store, owner.ID, hostValue.ID, grantA1, schedule.StatusEnabled)
	if err != nil {
		t.Fatal(err)
	}
	disabledA, err := createClientAuthorizedSchedule(t, ctx, store, owner.ID, hostValue.ID, grantA1, schedule.StatusDisabled)
	if err != nil {
		t.Fatal(err)
	}
	activeB, err := createClientAuthorizedSchedule(t, ctx, store, owner.ID, hostValue.ID, grantB1, schedule.StatusEnabled)
	if err != nil {
		t.Fatal(err)
	}

	grantA2 := newScheduleGrantTokenForTest(t, owner.ID, principalA.ClientInstallationID)
	if err := store.IssueClientScheduleGrant(ctx, principalA, grantA2); err != nil {
		t.Fatal(err)
	}
	activeARenewed := mustLoadSchedule(t, ctx, store, owner.ID, activeA.ID)
	disabledARenewed := mustLoadSchedule(t, ctx, store, owner.ID, disabledA.ID)
	activeBRenewed := mustLoadSchedule(t, ctx, store, owner.ID, activeB.ID)
	if activeARenewed.Authorization.TokenID != grantA2.ID || activeARenewed.Version != activeA.Version || activeARenewed.Status != schedule.StatusEnabled {
		t.Fatalf("active schedule after grant renewal = %#v", activeARenewed)
	}
	if disabledARenewed.Authorization.TokenID != grantA2.ID || disabledARenewed.Version != disabledA.Version || disabledARenewed.Status != schedule.StatusDisabled {
		t.Fatalf("disabled schedule after grant renewal = %#v", disabledARenewed)
	}
	if activeBRenewed.Authorization.TokenID != grantB1.ID || activeBRenewed.Status != schedule.StatusEnabled {
		t.Fatalf("other installation schedule changed during renewal = %#v", activeBRenewed)
	}
	if scheduleAuthorizationValidForTest(t, ctx, store, owner.ID, activeARenewed.Authorization) != true ||
		scheduleAuthorizationValidForTest(t, ctx, store, owner.ID, activeBRenewed.Authorization) != true {
		t.Fatal("renewed or other-installation schedule authorization is invalid")
	}
	if !scheduleTokenRevokedForTest(t, ctx, store, grantA1.ID) || scheduleTokenRevokedForTest(t, ctx, store, grantA2.ID) ||
		scheduleTokenRevokedForTest(t, ctx, store, grantB1.ID) {
		t.Fatal("grant rotation did not revoke only the prior grant for installation A")
	}
	assertRegularClientTokenActive(t, ctx, store, principalA.TokenID)
	assertRegularClientTokenActive(t, ctx, store, principalB.TokenID)

	if err := store.RevokeClientScheduleGrants(ctx, principalA); err != nil {
		t.Fatal(err)
	}
	grantA3 := newScheduleGrantTokenForTest(t, owner.ID, principalA.ClientInstallationID)
	if err := store.IssueClientScheduleGrant(ctx, principalA, grantA3); err != nil {
		t.Fatal(err)
	}
	activeAAfterReissue := mustLoadSchedule(t, ctx, store, owner.ID, activeA.ID)
	if activeAAfterReissue.Authorization.TokenID != grantA2.ID || activeAAfterReissue.Status != schedule.StatusEnabled ||
		scheduleAuthorizationValidForTest(t, ctx, store, owner.ID, activeAAfterReissue.Authorization) {
		t.Fatalf("explicitly revoked schedule was implicitly reauthorized: %#v", activeAAfterReissue)
	}
	activeBAfterReissue := mustLoadSchedule(t, ctx, store, owner.ID, activeB.ID)
	if activeBAfterReissue.Authorization.TokenID != grantB1.ID ||
		!scheduleAuthorizationValidForTest(t, ctx, store, owner.ID, activeBAfterReissue.Authorization) {
		t.Fatalf("other installation lost authorization after revoke/reissue: %#v", activeBAfterReissue)
	}

	expiredA, err := createClientAuthorizedSchedule(t, ctx, store, owner.ID, hostValue.ID, grantA3, schedule.StatusEnabled)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tokens SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, grantA3.ID); err != nil {
		t.Fatal(err)
	}
	grantA4 := newScheduleGrantTokenForTest(t, owner.ID, principalA.ClientInstallationID)
	if err := store.IssueClientScheduleGrant(ctx, principalA, grantA4); err != nil {
		t.Fatal(err)
	}
	expiredAfterRenewal := mustLoadSchedule(t, ctx, store, owner.ID, expiredA.ID)
	if expiredAfterRenewal.Authorization.TokenID != grantA3.ID || expiredAfterRenewal.Status != schedule.StatusEnabled ||
		scheduleAuthorizationValidForTest(t, ctx, store, owner.ID, expiredAfterRenewal.Authorization) {
		t.Fatalf("expired schedule authorization was implicitly renewed: %#v", expiredAfterRenewal)
	}
	if !scheduleAuthorizationValidForTest(t, ctx, store, owner.ID,
		mustLoadSchedule(t, ctx, store, owner.ID, activeBAfterReissue.ID).Authorization) {
		t.Fatal("installation A grant operations affected installation B")
	}
}

func TestPostgresRevokeScheduleGrantsIsInstallationScopedAndAudited(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	principalA := createIOSScheduleGrantPrincipal(t, ctx, store, pool, owner.ID)
	principalB := createIOSScheduleGrantPrincipal(t, ctx, store, pool, owner.ID)
	grantA := newScheduleGrantTokenForTest(t, owner.ID, principalA.ClientInstallationID)
	grantB := newScheduleGrantTokenForTest(t, owner.ID, principalB.ClientInstallationID)
	if err := store.IssueClientScheduleGrant(ctx, principalA, grantA); err != nil {
		t.Fatal(err)
	}
	if err := store.IssueClientScheduleGrant(ctx, principalB, grantB); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeClientScheduleGrants(ctx, principalA); err != nil {
		t.Fatal(err)
	}
	if !scheduleTokenRevokedForTest(t, ctx, store, grantA.ID) || scheduleTokenRevokedForTest(t, ctx, store, grantB.ID) {
		t.Fatal("schedule grant revoke did not stay within the active installation")
	}
	assertRegularClientTokenActive(t, ctx, store, principalA.TokenID)
	assertRegularClientTokenActive(t, ctx, store, principalB.TokenID)
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_id=$1 AND action=$2 AND target=$3`,
		owner.ID, "client.schedule_grant", principalA.ClientInstallationID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("grant issue/revoke audit rows = %d, want 2, err=%v", auditCount, err)
	}
}

func createIOSScheduleGrantPrincipal(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, ownerID string) identity.Principal {
	t.Helper()
	installationID, err := domain.NewClientInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO client_installations(id,owner_id,display_name,platform,expires_at)
VALUES($1,$2,'test iOS','ios',clock_timestamp()+interval '30 days')`, installationID, ownerID); err != nil {
		t.Fatal(err)
	}
	clientToken := clientTestToken(t, ownerID, installationID, identity.KindClient, nil)
	created, err := store.CreateToken(ctx, clientToken)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: created.ID, ClientInstallationID: installationID}
}

func newScheduleGrantTokenForTest(t *testing.T, ownerID, installationID string) identity.Token {
	t.Helper()
	id, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	_, secretHash, err := identity.NewCredential(identity.KindClient, id)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Token{
		ID: id, OwnerID: ownerID, ClientInstallationID: installationID, Kind: identity.KindClient,
		Name: "iOS cloud schedule grant", Scopes: []identity.Scope{identity.ScopeScheduleRead, identity.ScopeScheduleManage},
		SecretHash: secretHash, ExpiresAt: time.Now().UTC().Add(12 * time.Hour),
	}
}

func createClientAuthorizedSchedule(t *testing.T, ctx context.Context, store *Store, ownerID, hostID string, grant identity.Token, status schedule.Status) (schedule.Schedule, error) {
	t.Helper()
	timing := schedule.Timing{Kind: schedule.TimingDaily,
		StartsOn: time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"), LocalTime: "10:00", TimeZone: "UTC"}
	value := schedule.Schedule{
		ID: mustID(t, schedule.NewID), OwnerID: ownerID, Version: 1, Status: status, ExecutionEpoch: 1,
		HostID: hostID, ModelID: "model-test", Objective: "iOS schedule grant test", Timing: timing,
		Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		Authorization: task.Authorization{Kind: string(identity.KindClient), TokenID: grant.ID,
			ClientInstallationID: grant.ClientInstallationID,
			Scopes:               []string{string(identity.ScopeScheduleRead), string(identity.ScopeScheduleManage)}},
		MisfirePolicy: schedule.MisfireSkip, NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted},
	}
	if status == schedule.StatusEnabled {
		occurrence, err := schedule.FirstOccurrence(timing, time.Now().UTC())
		if err != nil {
			return schedule.Schedule{}, err
		}
		value.NextOccurrenceKey, value.NextRunAt, value.NextCheckAt = occurrence.Key, occurrence.ScheduledAt, &occurrence.CheckAt
	}
	record := testIdempotency(ownerID, "schedule.create", "schedule-grant-test-"+value.ID)
	record.ResourceID = value.ID
	created, _, err := store.CreateSchedule(ctx, value, record)
	return created, err
}

func scheduleAuthorizationValidForTest(t *testing.T, ctx context.Context, store *Store, ownerID string, authorization task.Authorization) bool {
	t.Helper()
	var valid bool
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		valid, err = validateScheduleAuthorizationInTx(ctx, tx, ownerID, authorization)
		return err
	}); err != nil {
		t.Fatalf("validate schedule grant authorization: %v", err)
	}
	return valid
}

func scheduleTokenRevokedForTest(t *testing.T, ctx context.Context, store *Store, tokenID string) bool {
	t.Helper()
	var revoked pgtype.Timestamptz
	if err := store.pool.QueryRow(ctx, `SELECT revoked_at FROM tokens WHERE id=$1`, tokenID).Scan(&revoked); err != nil {
		t.Fatalf("read schedule grant revocation: %v", err)
	}
	return revoked.Valid
}

func assertRegularClientTokenActive(t *testing.T, ctx context.Context, store *Store, tokenID string) {
	t.Helper()
	var active bool
	if err := store.pool.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() FROM tokens WHERE id=$1`, tokenID).Scan(&active); err != nil || !active {
		t.Fatalf("regular client token %s active=%t err=%v", tokenID, active, err)
	}
}

func mustLoadSchedule(t *testing.T, ctx context.Context, store *Store, ownerID, id string) schedule.Schedule {
	t.Helper()
	value, err := store.GetSchedule(ctx, ownerID, id)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
