package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
)

func TestOutboxPersonalWritesFenceServiceInstance(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	instance, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "11111111-1111-4111-8111-111111111111"
	if wrong == instance {
		wrong = "22222222-2222-4222-8222-222222222222"
	}
	badCtx, goodCtx := app.WithExpectedServiceInstance(ctx, wrong), app.WithExpectedServiceInstance(ctx, instance)
	mutation := personalMutationForTest(t, owner.ID, "outbox-scope-profile", 0, `{"name":"outbox","avatar":"dragon"}`, false)
	if _, err := store.ApplyPersonalMutation(badCtx, principal, mutation); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("wrong instance personal mutation: %v", err)
	}
	if _, err := store.ApplyPersonalMutation(goodCtx, principal, mutation); err != nil {
		t.Fatalf("same mutation at correct instance: %v", err)
	}
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-scope_1")
	content := []byte("hello")
	upload := newPersonalAttachmentUploadForTest(t, owner.ID, "library-scope_1", 1, content)
	if _, err := store.CreatePersonalAttachmentUpload(badCtx, principal, upload, "outbox-attachment-create"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("wrong instance upload initialization: %v", err)
	}
	created, err := store.CreatePersonalAttachmentUpload(goodCtx, principal, upload, "outbox-attachment-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePersonalAttachmentUpload(goodCtx, principal, created.UploadID, "reservation-scope-1"); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	chunk := personal.AttachmentChunk{UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
	if _, err := store.PutPersonalAttachmentChunk(badCtx, principal, chunk, bytes.NewReader(content)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("wrong instance upload chunk: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_chunks WHERE upload_id=$1`, created.UploadID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("wrong instance wrote a chunk: count=%d err=%v", count, err)
	}
	if _, err := store.PutPersonalAttachmentChunk(goodCtx, principal, chunk, bytes.NewReader(content)); err != nil {
		t.Fatalf("correct instance upload chunk: %v", err)
	}
	if _, err := store.CommitPersonalAttachment(badCtx, principal, created.UploadID, "outbox-attachment-commit"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("wrong instance upload commit: %v", err)
	}
}
