package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

func TestPostgresPersonalAttachmentUploadIdempotencyCancelAndCleanup(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	if !requirePersonalAttachmentSchema(t, ctx, pool) {
		return
	}
	owner := createTestUser(t, store, 2, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-item_1")

	content := bytes.Repeat([]byte("x"), int(personal.MaxAttachmentChunkBytes+1))
	first := newPersonalAttachmentUploadForTest(t, owner.ID, "library-item_1", 1, content)
	created, err := store.CreatePersonalAttachmentUpload(ctx, principal, first, "attachment-create-001")
	if err != nil || created.State != personal.AttachmentUploadPending || created.ChunkCount != 2 || len(created.ReceivedSequences) != 0 {
		t.Fatalf("create upload = %+v, %v", created, err)
	}
	replayed, err := store.CreatePersonalAttachmentUpload(ctx, principal, newPersonalAttachmentUploadForTest(t, owner.ID, "library-item_1", 1, content), "attachment-create-001")
	if err != nil || !replayed.Replay || replayed.UploadID != created.UploadID || len(replayed.ReceivedSequences) != 0 {
		t.Fatalf("create replay = %+v, %v", replayed, err)
	}
	conflicting := newPersonalAttachmentUploadForTest(t, owner.ID, "library-item_1", 1, []byte("other"))
	if _, err := store.CreatePersonalAttachmentUpload(ctx, principal, conflicting, "attachment-create-001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed upload replay error = %v", err)
	}

	if _, err := store.ActivatePersonalAttachmentUpload(ctx, principal, created.UploadID, "reservation-001"); err != nil {
		t.Fatal(err)
	}
	firstChunk := content[:personal.MaxAttachmentChunkBytes]
	digest := sha256.Sum256(firstChunk)
	chunk := personal.AttachmentChunk{UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(firstChunk)), SHA256: hex.EncodeToString(digest[:])}
	put, err := store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(firstChunk))
	if err != nil || put.Replay {
		t.Fatalf("first chunk = %+v, %v", put, err)
	}
	put, err = store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(firstChunk))
	if err != nil || !put.Replay {
		t.Fatalf("chunk replay = %+v, %v", put, err)
	}
	changed := bytes.Repeat([]byte("y"), len(firstChunk))
	changedDigest := sha256.Sum256(changed)
	chunk.SHA256 = hex.EncodeToString(changedDigest[:])
	if _, err := store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(changed)); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed chunk replay error = %v", err)
	}

	resumed, err := store.CreatePersonalAttachmentUpload(ctx, principal,
		newPersonalAttachmentUploadForTest(t, owner.ID, "library-item_1", 1, content), "attachment-create-001")
	if err != nil || !resumed.Replay || len(resumed.ReceivedSequences) != 1 || resumed.ReceivedSequences[0] != 0 {
		t.Fatalf("partial upload resume = %+v, %v", resumed, err)
	}
	activated, err := store.ActivatePersonalAttachmentUpload(ctx, principal, created.UploadID, "reservation-001")
	if err != nil || len(activated.ReceivedSequences) != 1 || activated.ReceivedSequences[0] != 0 {
		t.Fatalf("active upload resume = %+v, %v", activated, err)
	}

	reservation, err := store.CancelPersonalAttachmentUpload(ctx, principal, created.UploadID)
	if err != nil || reservation.ReservationID != "reservation-001" {
		t.Fatalf("cancel reservation = %+v, %v", reservation, err)
	}
	if err := store.AcknowledgePersonalAttachmentReservationReleased(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	reservation, err = store.CancelPersonalAttachmentUpload(ctx, principal, created.UploadID)
	if err != nil || reservation.ReservationID != "" {
		t.Fatalf("cancel replay after release = %+v, %v", reservation, err)
	}
	var chunkCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_chunks WHERE owner_id=$1 AND upload_id=$2`, owner.ID, created.UploadID).Scan(&chunkCount); err != nil || chunkCount != 0 {
		t.Fatalf("cancelled chunk count = %d, %v", chunkCount, err)
	}

	second := newPersonalAttachmentUploadForTest(t, owner.ID, "library-item_1", 1, content)
	secondCreated, err := store.CreatePersonalAttachmentUpload(ctx, principal, second, "attachment-create-002")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePersonalAttachmentUpload(ctx, principal, secondCreated.UploadID, "reservation-002"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE personal_attachment_uploads SET expires_at=clock_timestamp()-interval '1 second'
WHERE owner_id=$1 AND upload_id=$2`, owner.ID, secondCreated.UploadID); err != nil {
		t.Fatal(err)
	}
	reservations, err := store.CleanupExpiredPersonalAttachmentUploads(ctx, time.Now().UTC(), 10)
	if err != nil || len(reservations) != 1 || reservations[0].ReservationID != "reservation-002" {
		t.Fatalf("expired cleanup = %+v, %v", reservations, err)
	}
	reservations, err = store.CleanupExpiredPersonalAttachmentUploads(ctx, time.Now().UTC(), 10)
	if err != nil || len(reservations) != 1 || reservations[0].ReservationID != "reservation-002" {
		t.Fatalf("cleanup retry before release ACK = %+v, %v", reservations, err)
	}
	if err := store.AcknowledgePersonalAttachmentReservationReleased(ctx, reservations[0]); err != nil {
		t.Fatal(err)
	}
	reservations, err = store.CleanupExpiredPersonalAttachmentUploads(ctx, time.Now().UTC(), 10)
	if err != nil || len(reservations) != 0 {
		t.Fatalf("cleanup after release ACK = %+v, %v", reservations, err)
	}
}

func TestPostgresPersonalAttachmentReadRequiresCurrentOwnerReference(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	if !requirePersonalAttachmentSchema(t, ctx, pool) {
		return
	}
	owner := createTestUser(t, store, 2, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-item_1")
	content := []byte("private bytes")
	upload := newPersonalAttachmentUploadForTest(t, owner.ID, "library-item_1", 1, content)
	created, err := store.CreatePersonalAttachmentUpload(ctx, principal, upload, "attachment-read-001")
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := store.ReservePersonalAttachment(ctx, principal, created.UploadID, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePersonalAttachmentUpload(ctx, principal, created.UploadID, reservationID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	chunk := personal.AttachmentChunk{UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
	if _, err := store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	committed, err := store.CommitPersonalAttachment(ctx, principal, created.UploadID, "attachment-read-commit")
	if err != nil {
		t.Fatal(err)
	}
	reader, metadata, err := store.OpenPersonalAttachment(ctx, principal, committed.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, content) || metadata.AttachmentID != committed.AttachmentID || metadata.SizeBytes != int64(len(content)) {
		t.Fatalf("read attachment = %q metadata=%+v err=%v", got, metadata, err)
	}

	other := createTestUser(t, store, 1, 1000, 2048)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, other.ID)
	if reader, _, err := store.OpenPersonalAttachment(ctx, otherPrincipal, committed.AttachmentID); domain.CodeOf(err) != domain.CodeNotFound {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("cross-owner attachment read error = %v", err)
	}
}

func TestPostgresPersonalAttachmentCleanupRetainsThenDeletesUnreferencedContent(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	if !requirePersonalAttachmentSchema(t, ctx, pool) {
		return
	}
	owner := createTestUser(t, store, 2, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-gc-source")
	content := []byte("synthetic attachment bytes for cleanup")
	upload := newPersonalAttachmentUploadForTest(t, owner.ID, "library-gc-source", 1, content)
	created, err := store.CreatePersonalAttachmentUpload(ctx, principal, upload, "attachment-gc-create-001")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.ReservePersonalAttachment(ctx, principal, created.UploadID, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePersonalAttachmentUpload(ctx, principal, created.UploadID, reservation); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	chunk := personal.AttachmentChunk{UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
	if _, err := store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	committed, err := store.CommitPersonalAttachment(ctx, principal, created.UploadID, "attachment-gc-commit-001")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.CleanupExpiredPersonalAttachmentUploads(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	var orphanedAt *time.Time
	var chunks int
	if err := pool.QueryRow(ctx, `SELECT orphaned_at,(SELECT count(*) FROM personal_attachment_chunks c
WHERE c.service_instance_id=u.service_instance_id AND c.owner_id=u.owner_id AND c.upload_id=u.upload_id)
FROM personal_attachment_uploads u WHERE owner_id=$1 AND upload_id=$2`, owner.ID, created.UploadID).Scan(&orphanedAt, &chunks); err != nil || orphanedAt != nil || chunks != 1 {
		t.Fatalf("referenced attachment cleanup orphaned_at=%v chunks=%d err=%v", orphanedAt, chunks, err)
	}

	deleted := personal.Mutation{OwnerID: owner.ID, MutationID: "library-delete-gc-001", ObjectType: personal.ObjectTypeLibraryItem,
		ObjectID: "library-gc-source", BaseRevision: committed.ResultRevision, Delete: true}
	deleted.RequestHash, err = personal.HashMutationContent(deleted)
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := store.OpenPersonalAttachment(ctx, principal, committed.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	deleteCtx, cancelDelete := context.WithTimeout(ctx, 250*time.Millisecond)
	_, activeReadDeleteErr := store.ApplyPersonalMutation(deleteCtx, principal, deleted)
	cancelDelete()
	if activeReadDeleteErr == nil {
		_ = reader.Close()
		t.Fatal("deleting an attachment reference succeeded while its download was active")
	}
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatalf("close incomplete attachment read: %v", closeErr)
	}
	if _, err := store.ApplyPersonalMutation(ctx, principal, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CleanupExpiredPersonalAttachmentUploads(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT orphaned_at,(SELECT count(*) FROM personal_attachment_chunks c
WHERE c.service_instance_id=u.service_instance_id AND c.owner_id=u.owner_id AND c.upload_id=u.upload_id)
FROM personal_attachment_uploads u WHERE owner_id=$1 AND upload_id=$2`, owner.ID, created.UploadID).Scan(&orphanedAt, &chunks); err != nil || orphanedAt == nil || chunks != 1 {
		t.Fatalf("newly orphaned attachment cleanup orphaned_at=%v chunks=%d err=%v", orphanedAt, chunks, err)
	}
	if _, err := store.CleanupExpiredPersonalAttachmentUploads(ctx, now.Add(29*24*time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_chunks WHERE owner_id=$1 AND upload_id=$2`, owner.ID, created.UploadID).Scan(&chunks); err != nil || chunks != 1 {
		t.Fatalf("attachment content was removed before retention elapsed: chunks=%d err=%v", chunks, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE personal_attachment_uploads SET orphaned_at=$3
WHERE owner_id=$1 AND upload_id=$2`, owner.ID, created.UploadID, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CleanupExpiredPersonalAttachmentUploads(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	var contentDeletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT content_deleted_at,(SELECT count(*) FROM personal_attachment_chunks c
WHERE c.service_instance_id=u.service_instance_id AND c.owner_id=u.owner_id AND c.upload_id=u.upload_id)
FROM personal_attachment_uploads u WHERE owner_id=$1 AND upload_id=$2`, owner.ID, created.UploadID).Scan(&contentDeletedAt, &chunks); err != nil || contentDeletedAt == nil || chunks != 0 {
		t.Fatalf("expired orphan content cleanup deleted_at=%v chunks=%d err=%v", contentDeletedAt, chunks, err)
	}
	if reader, _, err := store.OpenPersonalAttachment(ctx, principal, committed.AttachmentID); domain.CodeOf(err) != domain.CodeNotFound {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("physically deleted attachment remained readable: %v", err)
	}

	replacement := personal.Mutation{OwnerID: owner.ID, MutationID: "library-reuse-gc-001", ObjectType: personal.ObjectTypeLibraryItem,
		ObjectID: "library-gc-replacement", Body: []byte(`{"name":"replacement","kind":"artifact","attachment_id":"` + committed.AttachmentID + `","mime":"application/octet-stream","size_bytes":` + fmt.Sprint(len(content)) + `}`)}
	replacement.RequestHash, err = personal.HashMutationContent(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPersonalMutation(ctx, principal, replacement); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("re-attaching garbage-collected content returned %v, want not found", err)
	}
}

func TestPostgresPersonalAttachmentCommitSettlesQuotaAndReferenceAtomically(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	if !requirePersonalAttachmentSchema(t, ctx, pool) {
		return
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_personal_attachment_commit() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.state='committed' THEN RAISE EXCEPTION 'forced attachment receipt failure'; END IF;
  RETURN NEW;
END $$`); err != nil {
		t.Fatal(err)
	}
	owner := createTestUser(t, store, 2, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-item_1")
	content := []byte("atomic attachment")
	upload := newPersonalAttachmentUploadForTest(t, owner.ID, "library-item_1", 1, content)
	created, err := store.CreatePersonalAttachmentUpload(ctx, principal, upload, "attachment-atomic-create")
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := store.ReservePersonalAttachment(ctx, principal, created.UploadID, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePersonalAttachmentUpload(ctx, principal, created.UploadID, reservationID); err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	chunk := personal.AttachmentChunk{UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
	if _, err := store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER reject_personal_attachment_commit
BEFORE UPDATE ON personal_attachment_uploads FOR EACH ROW EXECUTE FUNCTION reject_personal_attachment_commit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitPersonalAttachment(ctx, principal, created.UploadID, "attachment-atomic-commit"); err == nil {
		t.Fatal("attachment commit succeeded despite forced receipt failure")
	}
	var uploadState, quotaState string
	var hasAttachment bool
	if err := pool.QueryRow(ctx, `SELECT u.state,q.state,p.body ? 'attachment_id'
FROM personal_attachment_uploads u
JOIN personal_data_quota_reservations q ON q.service_instance_id=u.service_instance_id
 AND q.owner_id=u.owner_id AND q.reservation_id=u.reservation_id
JOIN personal_objects p ON p.service_instance_id=u.service_instance_id AND p.owner_id=u.owner_id
 AND p.object_type=u.object_type AND p.object_id=u.object_id
WHERE u.service_instance_id=$1::uuid AND u.owner_id=$2 AND u.upload_id=$3`,
		instanceID, owner.ID, created.UploadID).Scan(&uploadState, &quotaState, &hasAttachment); err != nil ||
		uploadState != "active" || quotaState != "reserved" || hasAttachment {
		t.Fatalf("failed commit left upload=%q quota=%q attachment=%t err=%v", uploadState, quotaState, hasAttachment, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_personal_attachment_commit ON personal_attachment_uploads`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION reject_personal_attachment_commit()`); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.CommitPersonalAttachment(ctx, principal, created.UploadID, "attachment-atomic-commit")
	if err != nil || receipt.ResultRevision != 2 || receipt.AccountChangeSequence != 2 || !personal.ValidPersonalAttachmentID(receipt.AttachmentID) {
		t.Fatalf("attachment commit = %+v, %v", receipt, err)
	}
	replayed, err := store.CommitPersonalAttachment(ctx, principal, created.UploadID, "attachment-atomic-commit")
	if err != nil || !replayed.Replay || replayed.AttachmentID != receipt.AttachmentID {
		t.Fatalf("attachment commit replay = %+v, %v", replayed, err)
	}
	if _, err := store.CommitPersonalAttachment(ctx, principal, created.UploadID, "attachment-other-key"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed attachment commit key error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT q.state FROM personal_data_quota_reservations q
WHERE q.service_instance_id=$1::uuid AND q.owner_id=$2 AND q.upload_id=$3`, instanceID, owner.ID, created.UploadID).Scan(&quotaState); err != nil || quotaState != "committed" {
		t.Fatalf("settled quota state=%q err=%v", quotaState, err)
	}
}

func TestPostgresPersonalAttachmentCommitAfterQuotaReductionReleasesReservation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	if !requirePersonalAttachmentSchema(t, ctx, pool) {
		return
	}
	owner := createTestUser(t, store, 2, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-item_1")
	content := []byte("quota reduced during upload")
	digest := sha256.Sum256(content)
	service, err := app.NewPersonalAttachmentService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreatePersonalAttachmentUpload(ctx, principal, app.PersonalAttachmentUploadCommand{
		IdempotencyKey: "attachment-reduced-create", ObjectID: "library-item_1", BaseRevision: 1,
		SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	chunkDigest := hex.EncodeToString(digest[:])
	if _, err := service.PutPersonalAttachmentChunk(ctx, principal, app.PersonalAttachmentChunkCommand{
		UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(content)), SHA256: chunkDigest, Body: bytes.NewReader(content),
	}); err != nil {
		t.Fatal(err)
	}
	var usedBefore int64
	if err := pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&usedBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE personal_data_quotas SET quota_bytes=used_bytes WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CommitPersonalAttachment(ctx, principal, created.UploadID, "attachment-reduced-commit"); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("commit after quota reduction = %v, want quota_exceeded", err)
	}
	var uploadState, reservationState string
	var reservationReleased bool
	var usedAfter, reservedAfter int64
	if err := pool.QueryRow(ctx, `SELECT u.state,u.reservation_released,r.state,q.used_bytes,q.reserved_bytes
FROM personal_attachment_uploads u
JOIN personal_data_quota_reservations r ON r.service_instance_id=u.service_instance_id
  AND r.owner_id=u.owner_id AND r.upload_id=u.upload_id
JOIN personal_data_quotas q ON q.service_instance_id=u.service_instance_id AND q.owner_id=u.owner_id
WHERE u.owner_id=$1 AND u.upload_id=$2`, owner.ID, created.UploadID).Scan(
		&uploadState, &reservationReleased, &reservationState, &usedAfter, &reservedAfter); err != nil {
		t.Fatal(err)
	}
	if uploadState != string(personal.AttachmentUploadCancelled) || !reservationReleased || reservationState != "released" ||
		usedAfter != usedBefore || reservedAfter != 0 {
		t.Fatalf("quota rejection left upload=%s ack=%t reservation=%s used=%d reserved=%d; want cancelled/released/%d/0",
			uploadState, reservationReleased, reservationState, usedAfter, reservedAfter, usedBefore)
	}
	var attachmentID string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(body->>'attachment_id','') FROM personal_objects
WHERE owner_id=$1 AND object_type=$2 AND object_id='library-item_1'`, owner.ID, personal.ObjectTypeLibraryItem).Scan(&attachmentID); err != nil || attachmentID != "" {
		t.Fatalf("quota-rejected commit changed library reference to %q: %v", attachmentID, err)
	}
}

func requirePersonalAttachmentSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.personal_attachment_uploads') IS NOT NULL
 AND to_regclass('public.personal_attachment_chunks') IS NOT NULL
 AND to_regclass('public.conversation_message_attachments') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Skip("personal attachment migration is not installed")
		return false
	}
	return true
}

func createPersonalLibraryItemForAttachmentTest(t *testing.T, store *Store, principal identity.Principal, objectID string) {
	t.Helper()
	mutation := personal.Mutation{
		OwnerID: principal.UserID, MutationID: "library-create-attachment-001", ObjectType: personal.ObjectTypeLibraryItem,
		ObjectID: objectID, BaseRevision: 0, Body: []byte(`{"name":"attachment test","kind":"artifact"}`),
	}
	var err error
	mutation.RequestHash, err = personal.HashMutationContent(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPersonalMutation(context.Background(), principal, mutation); err != nil {
		t.Fatal(err)
	}
}

func newPersonalAttachmentUploadForTest(t *testing.T, ownerID, objectID string, revision int64, content []byte) personal.AttachmentUpload {
	t.Helper()
	uploadID, err := personal.NewPersonalAttachmentUploadID()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	upload := personal.AttachmentUpload{
		UploadID: uploadID, OwnerID: ownerID, ObjectType: personal.ObjectTypeLibraryItem, ObjectID: objectID,
		BaseRevision: revision, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), MediaType: "application/octet-stream",
	}
	upload.RequestHash, err = personal.HashAttachmentUploadContent(upload)
	if err != nil {
		t.Fatal(err)
	}
	return upload
}
