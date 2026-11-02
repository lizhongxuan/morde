package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

func TestPersonalDataQuotaArithmeticAndReservationIDValidation(t *testing.T) {
	if got, ok := addPersonalQuotaBytes(10, 20); !ok || got != 30 {
		t.Fatalf("quota addition = %d, %t", got, ok)
	}
	for _, pair := range [][2]int64{{math.MaxInt64, 1}, {math.MinInt64, -1}} {
		if _, ok := addPersonalQuotaBytes(pair[0], pair[1]); ok {
			t.Errorf("overflowing addition %d + %d accepted", pair[0], pair[1])
		}
	}
	if !validPersonalDataQuotaReservationID("pq_0123456789abcdef0123456789abcdef") {
		t.Fatal("valid reservation ID rejected")
	}
	for _, id := range []string{"r1234567", "pq_0123456789ABCDEF0123456789abcdef", "pq_short"} {
		if validPersonalDataQuotaReservationID(id) {
			t.Errorf("invalid reservation ID %q accepted", id)
		}
	}
}

func TestPostgresPersonalDataQuotaDefaultsAdminCASAndOverQuotaAdjustment(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	admin := personalQuotaAdminPrincipal(t, store)
	service, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	quota, err := service.GetAdminPersonalDataQuota(ctx, admin, owner.ID)
	if err != nil || quota.OwnerID != owner.ID || quota.UsedBytes != 0 || quota.ReservedBytes != 0 ||
		quota.QuotaBytes != app.DefaultPersonalDataQuotaBytes || quota.MaxAttachmentBytes != app.DefaultPersonalDataMaxAttachmentBytes || quota.Version != 1 {
		t.Fatalf("default quota = %+v, err=%v", quota, err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, owner.ID, 24)
	}); err != nil {
		t.Fatal(err)
	}
	quota, err = service.GetAdminPersonalDataQuota(ctx, admin, owner.ID)
	if err != nil || quota.UsedBytes != 24 || quota.QuotaBytes != app.DefaultPersonalDataQuotaBytes || quota.Version != 1 {
		t.Fatalf("quota before limit reduction = %+v, err=%v", quota, err)
	}
	updated, err := service.SetAdminPersonalDataQuota(ctx, admin, owner.ID, quota.Version, 16, 8, "192.0.2.10")
	if err != nil || updated.UsedBytes != 24 || updated.QuotaBytes != 16 || updated.MaxAttachmentBytes != 8 || updated.Version != 2 {
		t.Fatalf("over-limit quota adjustment = %+v, err=%v", updated, err)
	}
	var auditActor, auditKind, auditAction, auditTarget, auditIP string
	var auditDetail []byte
	if err := store.pool.QueryRow(ctx, `SELECT actor_id,actor_kind,action,target,ip,detail FROM audit_logs
WHERE action=$1 AND target=$2 ORDER BY id DESC LIMIT 1`, string(audit.ActionPersonalDataQuotaUpdate), owner.ID).
		Scan(&auditActor, &auditKind, &auditAction, &auditTarget, &auditIP, &auditDetail); err != nil {
		t.Fatalf("read quota audit: %v", err)
	}
	var auditValues struct {
		QuotaBytesBefore int64 `json:"quota_bytes_before"`
		QuotaBytesAfter  int64 `json:"quota_bytes_after"`
	}
	if err := json.Unmarshal(auditDetail, &auditValues); err != nil {
		t.Fatalf("decode quota audit detail: %v", err)
	}
	if auditActor != admin.UserID || auditKind != string(audit.ActorSession) ||
		auditAction != string(audit.ActionPersonalDataQuotaUpdate) || auditTarget != owner.ID || auditIP != "192.0.2.10" ||
		auditValues.QuotaBytesBefore != app.DefaultPersonalDataQuotaBytes || auditValues.QuotaBytesAfter != 16 {
		t.Fatalf("quota audit actor=%s kind=%s action=%s target=%s ip=%s detail=%s err=%v", auditActor, auditKind, auditAction, auditTarget, auditIP, auditDetail, err)
	}
	if _, err := service.SetAdminPersonalDataQuota(ctx, admin, owner.ID, quota.Version, 10, 5, "192.0.2.10"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale quota version error = %v, want conflict", err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, owner.ID, 1)
	}); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("positive usage while over quota = %v, want quota_exceeded", err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, owner.ID, -8)
	}); err != nil {
		t.Fatalf("negative usage delta while over quota: %v", err)
	}
	quota, err = service.GetAdminPersonalDataQuota(ctx, admin, owner.ID)
	if err != nil || quota.UsedBytes != 16 {
		t.Fatalf("quota after negative usage delta = %+v, err=%v", quota, err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, owner.ID, -17)
	}); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("negative usage underflow = %v, want internal", err)
	}
}

func TestPostgresPersonalDataQuotaAuditFailureRollsBackLimitChange(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	admin := personalQuotaAdminPrincipal(t, store)
	service, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE audit_logs ADD CONSTRAINT test_fail_personal_quota_audit
CHECK (action <> 'personal_data.quota.update')`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetAdminPersonalDataQuota(ctx, admin, owner.ID, 1, 2048, 1024, "192.0.2.99"); err == nil {
		t.Fatal("quota update succeeded despite audit insert failure")
	}
	quota, err := service.GetAdminPersonalDataQuota(ctx, admin, owner.ID)
	if err != nil || quota.QuotaBytes != app.DefaultPersonalDataQuotaBytes || quota.MaxAttachmentBytes != 300<<20 || quota.Version != 1 {
		t.Fatalf("quota after failed audit = %+v, %v", quota, err)
	}
}

func TestPostgresPersonalAttachmentQuotaReserveIsOwnerUploadIdempotent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	other := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	quotaService, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	uploadID := createPendingPersonalQuotaUpload(t, store, owner.ID, 40, time.Now().Add(time.Hour))

	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := quotaService.ReservePersonalAttachment(ctx, principal, uploadID, 40)
			ids <- id
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	var canonicalID string
	for id := range ids {
		if canonicalID == "" {
			canonicalID = id
		} else if id != canonicalID {
			t.Fatalf("concurrent reserve returned different IDs %q and %q", canonicalID, id)
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reserve: %v", err)
		}
	}
	if !validPersonalDataQuotaReservationID(canonicalID) {
		t.Fatalf("reservation ID %q is invalid", canonicalID)
	}
	var used, reserved int64
	var reservationCount int64
	var uploadReservation string
	err = store.pool.QueryRow(ctx, `SELECT used_bytes,reserved_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&used, &reserved)
	if err != nil || used != 0 || reserved != 40 {
		t.Fatalf("quota totals used=%d reserved=%d err=%v", used, reserved, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_data_quota_reservations WHERE owner_id=$1 AND upload_id=$2`, owner.ID, uploadID).Scan(&reservationCount); err != nil || reservationCount != 1 {
		t.Fatalf("reservation count=%d err=%v", reservationCount, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT reservation_id FROM personal_attachment_uploads WHERE owner_id=$1 AND upload_id=$2`, owner.ID, uploadID).Scan(&uploadReservation); err != nil || uploadReservation != canonicalID {
		t.Fatalf("canonical upload reservation=%q err=%v", uploadReservation, err)
	}
	if _, err := quotaService.ReservePersonalAttachment(ctx, principal, uploadID, 39); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed reservation size error=%v", err)
	}
	if err := quotaService.ReleasePersonalAttachmentReservation(ctx, other.ID, canonicalID); err != nil {
		t.Fatalf("foreign owner release should be an owner-scoped no-op: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&reserved); err != nil || reserved != 40 {
		t.Fatalf("foreign release changed reserved bytes to %d: %v", reserved, err)
	}
	if err := quotaService.ReleasePersonalAttachmentReservation(ctx, owner.ID, canonicalID); err != nil {
		t.Fatal(err)
	}
	if err := quotaService.ReleasePersonalAttachmentReservation(ctx, owner.ID, canonicalID); err != nil {
		t.Fatalf("idempotent release retry: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("released quota bytes=%d err=%v", reserved, err)
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM personal_data_quota_reservations WHERE owner_id=$1 AND reservation_id=$2`, owner.ID, canonicalID).Scan(&state); err != nil || state != "released" {
		t.Fatalf("released reservation state=%q err=%v", state, err)
	}
}

func TestPostgresPersonalAttachmentQuotaAppliesCurrentLimitsAtReserveAndCommit(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	admin := personalQuotaAdminPrincipal(t, store)
	quotaService, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := quotaService.SetAdminPersonalDataQuota(ctx, admin, owner.ID, 1, 100, 20, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	tooLarge := createPendingPersonalQuotaUpload(t, store, owner.ID, 21, time.Now().Add(time.Hour))
	if _, err := quotaService.ReservePersonalAttachment(ctx, principal, tooLarge, 21); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("per-attachment reserve error=%v", err)
	}
	within := createPendingPersonalQuotaUpload(t, store, owner.ID, 20, time.Now().Add(time.Hour))
	reservationID, err := quotaService.ReservePersonalAttachment(ctx, principal, within, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE personal_attachment_uploads SET state='active' WHERE owner_id=$1 AND upload_id=$2`, owner.ID, within); err != nil {
		t.Fatal(err)
	}
	if _, err := quotaService.SetAdminPersonalDataQuota(ctx, admin, owner.ID, 2, 100, 19, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = store.WithTx(ctx, func(tx pgx.Tx) error {
		return commitPersonalAttachmentQuotaReservationInTx(ctx, tx, instanceID, owner.ID, reservationID, within, 20)
	})
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("commit above current attachment limit error=%v", err)
	}
	var used, reserved int64
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes,reserved_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&used, &reserved); err != nil || used != 0 || reserved != 20 {
		t.Fatalf("failed commit changed quota totals used=%d reserved=%d err=%v", used, reserved, err)
	}
	if _, err := quotaService.SetAdminPersonalDataQuota(ctx, admin, owner.ID, 3, 10, 20, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	err = store.WithTx(ctx, func(tx pgx.Tx) error {
		return commitPersonalAttachmentQuotaReservationInTx(ctx, tx, instanceID, owner.ID, reservationID, within, 20)
	})
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("commit above current account limit error=%v", err)
	}
	if err := quotaService.ReleasePersonalAttachmentReservation(ctx, owner.ID, reservationID); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresExpiredPersonalDataQuotaReservationReclaimsUnactivatedUpload(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	quotaService, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	uploadID := createPendingPersonalQuotaUpload(t, store, owner.ID, 90, time.Now().Add(time.Hour))
	reservationID, err := quotaService.ReservePersonalAttachment(ctx, principal, uploadID, 90)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.pool.Exec(ctx, `UPDATE personal_attachment_uploads SET expires_at=$3 WHERE owner_id=$1 AND upload_id=$2`, owner.ID, uploadID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE personal_data_quota_reservations SET expires_at=$3 WHERE owner_id=$1 AND upload_id=$2`, owner.ID, uploadID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	released, err := quotaService.CleanupExpiredPersonalDataQuotaReservations(ctx, now, 10)
	if err != nil || released != 1 {
		t.Fatalf("expired orphan cleanup count=%d err=%v", released, err)
	}
	var reserved int64
	if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("expired reservation left reserved bytes=%d err=%v", reserved, err)
	}
	var state, uploadState, uploadReservation string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM personal_data_quota_reservations WHERE owner_id=$1 AND reservation_id=$2`, owner.ID, reservationID).Scan(&state); err != nil || state != "released" {
		t.Fatalf("expired reservation state=%q err=%v", state, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state,reservation_id FROM personal_attachment_uploads WHERE owner_id=$1 AND upload_id=$2`, owner.ID, uploadID).Scan(&uploadState, &uploadReservation); err != nil || uploadState != "pending" || uploadReservation != reservationID {
		t.Fatalf("unactivated upload state=%q reservation=%q err=%v", uploadState, uploadReservation, err)
	}
	if released, err := quotaService.CleanupExpiredPersonalDataQuotaReservations(ctx, now, 10); err != nil || released != 0 {
		t.Fatalf("repeated expired cleanup count=%d err=%v", released, err)
	}
	if err := quotaService.ReleasePersonalAttachmentReservation(ctx, owner.ID, reservationID); err != nil {
		t.Fatalf("idempotent release after cleanup: %v", err)
	}
}

func TestPostgresPersonalDataQuotaUsageDeltaRollsBackOnQuotaFailure(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	admin := personalQuotaAdminPrincipal(t, store)
	service, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetAdminPersonalDataQuota(ctx, admin, owner.ID, 1, 3, 2, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO account_change_counters (service_instance_id,owner_id,last_sequence)
VALUES ($1::uuid,$2,0) ON CONFLICT (service_instance_id,owner_id) DO NOTHING`, instanceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	err = store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO personal_objects (service_instance_id,owner_id,object_type,object_id,body)
VALUES ($1::uuid,$2,'profile','default','{"name":"A"}'::jsonb)`, instanceID, owner.ID); err != nil {
			return err
		}
		return applyPersonalObjectUsageDeltaInTx(ctx, tx, instanceID, owner.ID, 4)
	})
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota object transaction error=%v", err)
	}
	var objects int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_objects WHERE owner_id=$1 AND object_type='profile'`, owner.ID).Scan(&objects); err != nil || objects != 0 {
		t.Fatalf("failed quota write left %d personal objects: %v", objects, err)
	}
	var canonicalBytes int64
	if err := store.pool.QueryRow(ctx, `SELECT octet_length('{"z":1,"a":"é"}'::jsonb::text)`).Scan(&canonicalBytes); err != nil || canonicalBytes <= 0 {
		t.Fatalf("canonical JSONB byte count=%d err=%v", canonicalBytes, err)
	}
}

func personalQuotaAdminPrincipal(t *testing.T, store *Store) identity.Principal {
	t.Helper()
	admin := createTestUser(t, store, 3, 4000, 8192)
	if _, err := store.pool.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	tokens := createTestCredentials(t, store, admin.ID)
	return identity.Principal{UserID: admin.ID, TokenID: tokens[0], Kind: identity.KindSession, Role: identity.RoleAdmin}
}

func createPendingPersonalQuotaUpload(t *testing.T, store *Store, ownerID string, sizeBytes int64, expiresAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	uploadID, err := personal.NewPersonalAttachmentUploadID()
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte(uploadID))
	idempotency := "quota-upload-" + uploadID[len("pu_"):12]
	_, err = store.pool.Exec(ctx, `INSERT INTO personal_attachment_uploads
(service_instance_id,owner_id,upload_id,object_type,object_id,base_revision,size_bytes,sha256,media_type,request_hash,idempotency_key,state,expires_at)
VALUES ($1::uuid,$2,$3,'library_item','library-entry-1',0,$4,$5,'application/octet-stream',$6,$7,'pending',$8)`,
		instanceID, ownerID, uploadID, sizeBytes, hex.EncodeToString(make([]byte, 32)), requestHash[:], idempotency, expiresAt)
	if err != nil {
		t.Fatalf("insert pending personal attachment upload: %v", err)
	}
	return uploadID
}
