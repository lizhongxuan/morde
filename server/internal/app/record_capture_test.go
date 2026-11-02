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
	captured    int
	rejected    int
	statusCalls int
	statusQuery record.CaptureStatusQuery
	item        record.QueueItem
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
func (s *recordCaptureTestStore) RejectRecordCapture(_ context.Context, principal identity.Principal, taskID string, purpose record.Purpose, kind record.ItemKind) (record.QueueItem, error) {
	s.rejected++
	return record.QueueItem{ID: "j1234567", OwnerID: principal.UserID, TaskID: taskID, Purpose: purpose, Kind: kind,
		State: record.QueueFailed, FailureCode: "content_rejected"}, nil
}
func (s *recordCaptureTestStore) ListRecordCaptureStatuses(_ context.Context, _ identity.Principal, query record.CaptureStatusQuery) (record.CaptureStatusPage, error) {
	s.statusCalls++
	s.statusQuery = query
	return record.CaptureStatusPage{Items: []record.CaptureStatus{{ID: "j1234567", State: record.QueueQueued}}}, nil
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
	input.TaskID = "q1234567"
	input.Payload = []byte(`{"api_key":"synthetic-secret-value"}`)
	if _, err := service.Capture(context.Background(), principal, input); domain.CodeOf(err) != domain.CodeInvalidArgument || store.captured != 1 || store.rejected != 1 {
		t.Fatalf("credential capture reached payload store or lost failed status: err=%v calls=%d rejected=%d", err, store.captured, store.rejected)
	}
	input.Payload = []byte("safe")
	input.Attachments = []record.AttachmentInput{{FileName: "notes.txt", MediaType: "text/plain", Content: []byte("token: synthetic-secret-value")}}
	if _, err := service.Capture(context.Background(), principal, input); domain.CodeOf(err) != domain.CodeInvalidArgument || store.captured != 1 || store.rejected != 2 {
		t.Fatalf("credential attachment reached payload store or lost failed status: err=%v calls=%d rejected=%d", err, store.captured, store.rejected)
	}
	if _, err := service.Commit(context.Background(), "bad", "worker", 1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid worker queue id: %v", err)
	}
	if _, err := service.Statuses(context.Background(), identity.Principal{Kind: identity.KindSystem}, record.CaptureStatusQuery{}); domain.CodeOf(err) != domain.CodeUnauthenticated || store.statusCalls != 0 {
		t.Fatalf("status query accepted a system principal: err=%v calls=%d", err, store.statusCalls)
	}
	if _, err := service.Statuses(context.Background(), principal, record.CaptureStatusQuery{BeforeID: "bad"}); domain.CodeOf(err) != domain.CodeInvalidArgument || store.statusCalls != 0 {
		t.Fatalf("invalid status cursor reached store: err=%v calls=%d", err, store.statusCalls)
	}
	page, err := service.Statuses(context.Background(), principal, record.CaptureStatusQuery{})
	if err != nil || store.statusCalls != 1 || store.statusQuery.Limit != 50 || len(page.Items) != 1 || page.Items[0].State != record.QueueQueued {
		t.Fatalf("default status query: page=%#v query=%#v calls=%d err=%v", page, store.statusQuery, store.statusCalls, err)
	}
}
