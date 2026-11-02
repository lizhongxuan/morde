package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

type recordCaptureTestStore struct {
	captured int
	item     record.QueueItem
}

func (s *recordCaptureTestStore) EnqueueRecordCapture(_ context.Context, principal identity.Principal, input record.CaptureInput) (record.QueueItem, error) {
	s.captured++
	s.item = record.QueueItem{OwnerID: principal.UserID, Purpose: input.Purpose, Kind: input.Kind}
	return s.item, nil
}
func (*recordCaptureTestStore) ClaimRecordCaptures(context.Context, string, time.Time, time.Duration, int32) ([]record.QueueItem, error) {
	return []record.QueueItem{}, nil
}
func (*recordCaptureTestStore) CommitRecordCapture(context.Context, string, string, int64) (record.RecordMetadata, error) {
	return record.RecordMetadata{}, nil
}
func (*recordCaptureTestStore) FailRecordCapture(context.Context, string, string, int64, string) error {
	return nil
}

func TestRecordCaptureServiceRejectsBadIdentityAndPayload(t *testing.T) {
	store := &recordCaptureTestStore{}
	service, err := NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleUser}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("synthetic")}
	if _, err := service.Capture(context.Background(), identity.Principal{Kind: identity.KindSystem}, input); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("system capture principal: %v", err)
	}
	input.Payload = nil
	if _, err := service.Capture(context.Background(), principal, input); domain.CodeOf(err) != domain.CodeRequestTooLarge || store.captured != 0 {
		t.Fatalf("invalid capture reached store: err=%v calls=%d", err, store.captured)
	}
	input.Payload = []byte("synthetic")
	if _, err := service.Capture(context.Background(), principal, input); err != nil || store.captured != 1 {
		t.Fatalf("valid optional capture: err=%v calls=%d", err, store.captured)
	}
	input.Payload = []byte(`{"api_key":"synthetic-secret-value"}`)
	if _, err := service.Capture(context.Background(), principal, input); domain.CodeOf(err) != domain.CodeInvalidArgument || store.captured != 1 {
		t.Fatalf("credential capture reached store: err=%v calls=%d", err, store.captured)
	}
	input.Payload = []byte("safe")
	input.Attachments = []record.AttachmentInput{{FileName: "notes.txt", MediaType: "text/plain", Content: []byte("token: synthetic-secret-value")}}
	if _, err := service.Capture(context.Background(), principal, input); domain.CodeOf(err) != domain.CodeInvalidArgument || store.captured != 1 {
		t.Fatalf("credential attachment reached store: err=%v calls=%d", err, store.captured)
	}
	if _, err := service.Commit(context.Background(), "bad", "worker", 1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid worker queue id: %v", err)
	}
}
