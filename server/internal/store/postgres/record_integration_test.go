package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

func recordTestPrincipal(t *testing.T, store *Store, role identity.Role) identity.Principal {
	t.Helper()
	user := createTestUser(t, store, 1, 2000, 4096)
	if role != identity.RoleUser {
		if _, err := store.pool.Exec(context.Background(), `UPDATE users SET role=$2 WHERE id=$1`, user.ID, string(role)); err != nil {
			t.Fatalf("set record test role: %v", err)
		}
	}
	tokenID := testSessionTokenID(user.ID)
	secretHash := sha256.Sum256([]byte("record-test-session:" + tokenID))
	if _, err := store.CreateToken(context.Background(), identity.Token{
		ID: tokenID, OwnerID: user.ID, Kind: identity.KindSession, SecretHash: secretHash[:],
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create record test session: %v", err)
	}
	return identity.Principal{UserID: user.ID, Role: role, Kind: identity.KindSession, TokenID: tokenID}
}

func TestRecordConsentVersionsAndQueueReservation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)

	consents, err := store.ListRecordConsents(ctx, owner)
	if err != nil || len(consents) != 2 || consents[0].Enabled || consents[1].Enabled || consents[0].Version != 0 || consents[1].Version != 0 {
		t.Fatalf("default record consents: %#v, %v", consents, err)
	}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn,
		Payload: []byte("synthetic record body"), Attachments: []record.AttachmentInput{{
			FileName: "sample.txt", MediaType: "text/plain", Content: []byte("synthetic attachment"),
		}}}
	if item, err := store.EnqueueRecordCapture(ctx, owner, input); err != nil || item.ID != "" {
		t.Fatalf("capture while consent is off: %#v, %v", item, err)
	}

	first, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil || !first.Enabled || first.Version != 1 {
		t.Fatalf("first full-record consent: %#v, %v", first, err)
	}
	unchanged, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil || unchanged.Version != first.Version {
		t.Fatalf("unchanged consent advanced version: %#v, %v", unchanged, err)
	}
	item, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil || item.State != record.QueueQueued || item.ConsentVersion != first.Version {
		t.Fatalf("enqueue optional capture: %#v, %v", item, err)
	}
	quota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.LimitBytes != record.DefaultQuotaBytes || quota.RetentionDays != record.DefaultRetentionDays ||
		quota.ReservedBytes != int64(len(input.Payload)+len(input.Attachments[0].Content)) {
		t.Fatalf("default record quota reservation: %#v, %v", quota, err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "records-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != item.ID || claimed[0].LeaseVersion != 1 {
		t.Fatalf("claim record queue item: %#v, %v", claimed, err)
	}

	withdrawn, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false)
	if err != nil || withdrawn.Enabled || withdrawn.Version != 2 {
		t.Fatalf("withdraw full-record consent: %#v, %v", withdrawn, err)
	}
	quota, err = store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.ReservedBytes != 0 {
		t.Fatalf("withdrawal did not release reservation: %#v, %v", quota, err)
	}
	var queueState string
	var queuedPayload []byte
	var queuedAttachments int
	if err := pool.QueryRow(ctx, `SELECT state,payload FROM record_capture_queue WHERE id=$1`, item.ID).Scan(&queueState, &queuedPayload); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_capture_queue_attachments WHERE queue_item_id=$1`, item.ID).Scan(&queuedAttachments); err != nil {
		t.Fatal(err)
	}
	if queueState != string(record.QueueRejected) || len(queuedPayload) != 0 || queuedAttachments != 0 {
		t.Fatalf("withdrawn queue retained content: state=%q payload=%v attachments=%d", queueState, queuedPayload, queuedAttachments)
	}
	if _, err := store.CommitRecordCapture(ctx, item.ID, "records-test", claimed[0].LeaseVersion); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("withdrawn queue commit error = %v", err)
	}
	third, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil || third.Version != 3 {
		t.Fatalf("re-enabled consent was not monotonic: %#v, %v", third, err)
	}

	superAdmin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	if _, err := store.SetRecordQuota(ctx, superAdmin, owner.UserID, 1, 2, 20); err != nil {
		t.Fatalf("set record quota: %v", err)
	}
	quota, err = store.GetRecordQuota(ctx, superAdmin, owner.UserID)
	if err != nil || quota.RetentionDays != 2 || quota.LimitBytes != 20 {
		t.Fatalf("updated record quota: %#v, %v", quota, err)
	}
	if _, err := store.SetRecordQuota(ctx, superAdmin, owner.UserID, 1, 3, 30); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale record quota version: %v", err)
	}
	if _, err := store.EnqueueRecordCapture(ctx, owner, input); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota optional capture error = %v", err)
	}
	var consentReads, consentUpdates, quotaReads, quotaUpdates int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action='record.consent.read'),
count(*) FILTER (WHERE action='record.consent.update'),
	count(*) FILTER (WHERE action='record.quota.read')
FROM audit_logs WHERE actor_id=$1`, owner.UserID).Scan(&consentReads, &consentUpdates, &quotaReads); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_id=$1 AND action='record.quota.update'`, superAdmin.UserID).Scan(&quotaUpdates); err != nil {
		t.Fatal(err)
	}
	if consentReads < 1 || consentUpdates != 4 || quotaReads < 2 || quotaUpdates != 1 {
		t.Fatalf("record consent/quota audit rows: reads=%d updates=%d quota_reads=%d quota_updates=%d", consentReads, consentUpdates, quotaReads, quotaUpdates)
	}
}

func TestRecordAdminGrantsExportAttachmentAndIdempotentDelete(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	admin := recordTestPrincipal(t, store, identity.RoleAdmin)
	superAdmin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn,
		Payload: []byte("private synthetic body"), Attachments: []record.AttachmentInput{{
			FileName: "sample.txt", MediaType: "text/plain", Content: []byte("private synthetic attachment"),
		}}}
	queueItem, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-access-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim record for access test: %#v, %v", claimed, err)
	}
	metadata, err := store.CommitRecordCapture(ctx, queueItem.ID, "record-access-test", claimed[0].LeaseVersion)
	if err != nil || metadata.State != "active" || metadata.PayloadBytes != int64(len(input.Payload)) || metadata.AttachmentCount != 1 {
		t.Fatalf("commit record capture: %#v, %v", metadata, err)
	}
	if _, err := store.ListRecordMetadata(ctx, admin, owner.UserID, record.Query{Limit: 10}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("metadata was readable without view grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, admin, owner.UserID, admin.UserID, record.CapabilityView, true); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-superadmin grant change was allowed: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityView, true); err != nil {
		t.Fatal(err)
	}
	settings, err := store.GetAdminRecordSettings(ctx, superAdmin, owner.UserID)
	if err != nil || len(settings.Consents) != 2 || !settings.Consents[0].Enabled ||
		settings.Quota.LimitBytes != record.DefaultQuotaBytes || len(settings.Grants) != 1 || !settings.Grants[0].Granted {
		t.Fatalf("superadmin record settings: %#v, %v", settings, err)
	}
	if _, err := store.GetAdminRecordSettings(ctx, admin, owner.UserID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-superadmin read of record settings was allowed: %v", err)
	}
	adminAccess, err := store.GetRecordAdminAccess(ctx, admin, owner.UserID)
	if err != nil || len(adminAccess.Consents) != 2 || len(adminAccess.Grants) != 4 || !adminAccess.Grants[0].Granted && !adminAccess.Grants[1].Granted {
		t.Fatalf("administrator's own record access: %#v, %v", adminAccess, err)
	}
	for _, grant := range adminAccess.Grants {
		if grant.AdminID != admin.UserID {
			t.Fatalf("record access exposed another administrator's grant: %#v", grant)
		}
	}
	page, err := store.ListRecordMetadata(ctx, admin, owner.UserID, record.Query{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != metadata.ID {
		t.Fatalf("metadata with view grant: %#v, %v", page, err)
	}
	content, err := store.ReadRecord(ctx, admin, owner.UserID, metadata.ID)
	if err != nil || string(content.Payload) != string(input.Payload) {
		t.Fatalf("detail with view grant: %#v, %v", content, err)
	}
	if _, err := store.ExportRecord(ctx, admin, owner.UserID, metadata.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("export was allowed without export grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityExport, true); err != nil {
		t.Fatal(err)
	}
	if exported, err := store.ExportRecord(ctx, admin, owner.UserID, metadata.ID); err != nil || string(exported.Payload) != string(input.Payload) {
		t.Fatalf("explicit export grant: %#v, %v", exported, err)
	}
	if _, err := store.ListRecordAttachments(ctx, admin, owner.UserID, metadata.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("attachment metadata was allowed without attachment grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityAttachment, true); err != nil {
		t.Fatal(err)
	}
	attachments, err := store.ListRecordAttachments(ctx, admin, owner.UserID, metadata.ID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("list record attachments: %#v, %v", attachments, err)
	}
	attachment, err := store.ReadRecordAttachment(ctx, admin, owner.UserID, metadata.ID, attachments[0].ID)
	if err != nil || string(attachment.Content) != string(input.Attachments[0].Content) {
		t.Fatalf("read record attachment: %#v, %v", attachment, err)
	}
	if _, err := store.RequestRecordDelete(ctx, admin, owner.UserID, metadata.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("record deletion was allowed without delete grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityDelete, true); err != nil {
		t.Fatal(err)
	}
	firstDelete, err := store.RequestRecordDelete(ctx, admin, owner.UserID, metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayedDelete, err := store.RequestRecordDelete(ctx, admin, owner.UserID, metadata.ID)
	if err != nil || firstDelete.ID != replayedDelete.ID {
		t.Fatalf("delete request was not idempotent: %#v %#v %v", firstDelete, replayedDelete, err)
	}
	processed, err := store.ProcessRecordDeletes(ctx, 10)
	if err != nil || processed != 1 {
		t.Fatalf("process record deletion: processed=%d err=%v", processed, err)
	}
	var deleteState string
	var completedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT state,completed_at FROM record_delete_tombstones WHERE id=$1`, firstDelete.ID).Scan(&deleteState, &completedAt); err != nil {
		t.Fatal(err)
	}
	if deleteState != string(record.DeleteCompleted) || completedAt == nil {
		t.Fatalf("delete tombstone was not retained as complete: %s %v", deleteState, completedAt)
	}
	var turnBodies, attachmentsLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_turn_bodies WHERE record_id=$1`, metadata.ID).Scan(&turnBodies); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_attachments WHERE record_id=$1`, metadata.ID).Scan(&attachmentsLeft); err != nil {
		t.Fatal(err)
	}
	if turnBodies != 0 || attachmentsLeft != 0 {
		t.Fatalf("record deletion left content: bodies=%d attachments=%d", turnBodies, attachmentsLeft)
	}
	var ordinaryAttachments, queuedAttachments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_uploads`).Scan(&ordinaryAttachments); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_capture_queue_attachments WHERE queue_item_id=$1`, queueItem.ID).Scan(&queuedAttachments); err != nil {
		t.Fatal(err)
	}
	if ordinaryAttachments != 0 || queuedAttachments != 0 {
		t.Fatalf("record cleanup crossed attachment stores: personal=%d queue=%d", ordinaryAttachments, queuedAttachments)
	}
	quota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.UsedBytes != 0 || quota.ReservedBytes != 0 {
		t.Fatalf("record deletion did not release quota: %#v, %v", quota, err)
	}

	secondQueue, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimRecordCaptures(ctx, "record-access-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != secondQueue.ID {
		t.Fatalf("claim second record: %#v, %v", claimed, err)
	}
	second, err := store.CommitRecordCapture(ctx, secondQueue.ID, "record-access-test", claimed[0].LeaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestRecordDelete(ctx, admin, owner.UserID, second.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("administrator deleted after consent withdrawal: %v", err)
	}
	ownerDelete, err := store.RequestRecordDelete(ctx, owner, owner.UserID, second.ID)
	if err != nil {
		t.Fatalf("owner could not delete after withdrawing consent: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE record_quotas SET used_bytes=0 WHERE owner_id=$1`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if processed, err := store.ProcessRecordDeletes(ctx, 10); domain.CodeOf(err) != domain.CodeInternal || processed != 0 {
		t.Fatalf("failed cleanup result: processed=%d err=%v", processed, err)
	}
	var failureState string
	if err := pool.QueryRow(ctx, `SELECT state FROM record_delete_tombstones WHERE id=$1`, ownerDelete.ID).Scan(&failureState); err != nil || failureState != string(record.DeleteFailed) {
		t.Fatalf("failed cleanup lost its tombstone: state=%q err=%v", failureState, err)
	}
	var retainedBody int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_turn_bodies WHERE record_id=$1`, second.ID).Scan(&retainedBody); err != nil || retainedBody != 1 {
		t.Fatalf("failed cleanup partially removed content: rows=%d err=%v", retainedBody, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE record_quotas SET used_bytes=$2 WHERE owner_id=$1`, owner.UserID,
		int64(len(input.Payload)+len(input.Attachments[0].Content))); err != nil {
		t.Fatal(err)
	}
	if processed, err := store.ProcessRecordDeletes(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("retry owner deletion: processed=%d err=%v", processed, err)
	}
	wantActions := map[string]bool{
		"record.grant.update": false, "record.settings.read": false, "record.access.read": false,
		"record.list": false, "record.read": false, "record.export": false,
		"record.attachments.read": false, "record.attachment.read": false,
		"record.delete": false, "record.delete.complete": false,
	}
	rows, err := pool.Query(ctx, `SELECT action,detail::text FROM audit_logs WHERE action LIKE 'record.%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if _, exists := wantActions[action]; exists {
			wantActions[action] = true
		}
		if strings.Contains(detail, string(input.Payload)) || strings.Contains(detail, string(input.Attachments[0].Content)) {
			rows.Close()
			t.Fatalf("audit detail exposed record contents: %q", detail)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	for action, found := range wantActions {
		if !found {
			t.Errorf("missing audited record operation %q", action)
		}
	}
}

func TestRecordCaptureCommitRechecksConsentVersion(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeTrainingContribution, true); err != nil {
		t.Fatal(err)
	}
	item, err := store.EnqueueRecordCapture(ctx, owner, record.CaptureInput{
		Purpose: record.PurposeTrainingContribution, Kind: record.ItemSample, Payload: []byte("synthetic sample"),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-version-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim training sample: %#v, %v", claimed, err)
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeTrainingContribution, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRecordCapture(ctx, item.ID, "record-version-test", claimed[0].LeaseVersion); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale consent capture commit error = %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_entries WHERE owner_id=$1`, owner.UserID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale capture was committed: records=%d", count)
	}
}

func TestRecordCaptureWithdrawalAndReconsentRaceIsLinearizable(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	consent, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 8; iteration++ {
		item, err := store.EnqueueRecordCapture(ctx, owner, record.CaptureInput{
			Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("race synthetic record"),
		})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimRecordCaptures(ctx, "record-race-test", time.Now().UTC(), time.Minute, 10)
		if err != nil || len(claimed) != 1 || claimed[0].ID != item.ID {
			t.Fatalf("claim race capture: %#v, %v", claimed, err)
		}
		start := make(chan struct{})
		commitResult := make(chan error, 1)
		withdrawResult := make(chan error, 1)
		go func() {
			<-start
			_, err := store.CommitRecordCapture(ctx, item.ID, "record-race-test", claimed[0].LeaseVersion)
			commitResult <- err
		}()
		go func() {
			<-start
			if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false); err != nil {
				withdrawResult <- err
				return
			}
			_, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
			withdrawResult <- err
		}()
		close(start)
		commitErr := <-commitResult
		if err := <-withdrawResult; err != nil {
			t.Fatalf("withdraw and re-enable consent: %v", err)
		}
		var state, recordID string
		if err := pool.QueryRow(ctx, `SELECT state,COALESCE(record_id,'') FROM record_capture_queue WHERE id=$1`, item.ID).Scan(&state, &recordID); err != nil {
			t.Fatal(err)
		}
		switch state {
		case string(record.QueueStored):
			if commitErr != nil || recordID == "" {
				t.Fatalf("committed queue result: state=%s id=%q err=%v", state, recordID, commitErr)
			}
			var capturedVersion int64
			if err := pool.QueryRow(ctx, `SELECT consent_version FROM record_entries WHERE id=$1`, recordID).Scan(&capturedVersion); err != nil || capturedVersion != consent.Version {
				t.Fatalf("stored consent version=%d want=%d err=%v", capturedVersion, consent.Version, err)
			}
		case string(record.QueueRejected):
			if domain.CodeOf(commitErr) != domain.CodeInvalidState || recordID != "" {
				t.Fatalf("rejected queue result: id=%q err=%v", recordID, commitErr)
			}
		default:
			t.Fatalf("queue did not reach a terminal state after consent race: %s", state)
		}
		var reserved int64
		if err := pool.QueryRow(ctx, `SELECT reserved_bytes FROM record_quotas WHERE owner_id=$1`, owner.UserID).Scan(&reserved); err != nil || reserved != 0 {
			t.Fatalf("race left reserved quota: %d err=%v", reserved, err)
		}
		consent.Version += 2
	}
}
