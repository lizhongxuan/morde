package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

type taskMediaCleanupStoreStub struct {
	TaskMediaStagedObjectStore
	objects []TaskMediaStagedObject
	events  *[]string
	deleted *[]string
	failAt  string
}

func (s taskMediaCleanupStoreStub) ListTaskMediaObjectsForCleanup(context.Context, time.Time, int32) ([]TaskMediaStagedObject, error) {
	return s.objects, nil
}

func (s taskMediaCleanupStoreStub) SetTaskMediaStagedObjectState(_ context.Context, taskID, operationID string, expected, next TaskMediaStagedObjectState) (TaskMediaStagedObject, error) {
	*s.events = append(*s.events, "expire")
	if s.failAt == "expire" {
		return TaskMediaStagedObject{}, errors.New("transition failed")
	}
	value := s.objects[0]
	if value.TaskID != taskID || value.OperationID != operationID || value.State != expected || next != TaskMediaPreviewExpired {
		return TaskMediaStagedObject{}, errors.New("unexpected expiration transition")
	}
	value.State = next
	return value, nil
}

func (s taskMediaCleanupStoreStub) ExpireTaskMediaPreviewApproval(context.Context, string, string) error {
	*s.events = append(*s.events, "approval-expired")
	if s.failAt == "approval-expired" {
		return errors.New("approval expiration failed")
	}
	return nil
}

func (s taskMediaCleanupStoreStub) CanCleanupTaskMediaStagedObject(context.Context, string, string) (bool, error) {
	*s.events = append(*s.events, "can-cleanup")
	return s.failAt != "not-ready", nil
}

func (s taskMediaCleanupStoreStub) MarkTaskMediaStagedObjectCleanupComplete(context.Context, string, string) error {
	*s.events = append(*s.events, "mark")
	if s.failAt == "mark" {
		return errors.New("mark failed")
	}
	return nil
}

func (s taskMediaCleanupStoreStub) DeleteCleanedTaskMediaStagedObject(_ context.Context, taskID, operationID string) error {
	*s.events = append(*s.events, "row")
	if s.deleted != nil {
		*s.deleted = append(*s.deleted, taskID+"/"+operationID)
	}
	if s.failAt == "row" {
		return errors.New("row delete failed")
	}
	return nil
}

type taskMediaCleanupContentStub struct {
	TaskMediaContentStore
	events      *[]string
	fail        bool
	failOwnerID string
}

func (s taskMediaCleanupContentStub) Delete(_ context.Context, ownerID, _, _ string) error {
	*s.events = append(*s.events, "content")
	if s.fail || ownerID == s.failOwnerID {
		return errors.New("content delete failed")
	}
	return nil
}

func TestTaskMediaCleanupExpiresAndDeletesStagedContent(t *testing.T) {
	now := time.Now().UTC()
	events := []string{}
	service, err := NewTaskMediaCleanupService(taskMediaCleanupStoreStub{
		objects: []TaskMediaStagedObject{{TaskID: "q1234567", OwnerID: "u1234567", OperationID: "o1234567",
			ExpiresAt: now.Add(-time.Minute), State: TaskMediaPreviewPending, CleanupState: TaskMediaCleanupPending}},
		events: &events,
	}, taskMediaCleanupContentStub{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), now, 10); err != nil {
		t.Fatal(err)
	}
	want := []string{"expire", "approval-expired", "content", "mark", "row"}
	if len(events) != len(want) {
		t.Fatalf("cleanup event order = %v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("cleanup event order = %v, want %v", events, want)
		}
	}
}

func TestTaskMediaCleanupRetriesOnlyMetadataAfterContentCleanup(t *testing.T) {
	now := time.Now().UTC()
	events := []string{}
	service, err := NewTaskMediaCleanupService(taskMediaCleanupStoreStub{
		objects: []TaskMediaStagedObject{{TaskID: "q1234567", OwnerID: "u1234567", OperationID: "o1234567",
			State: TaskMediaPreviewDiscard, CleanupState: TaskMediaCleanupComplete}},
		events: &events,
	}, taskMediaCleanupContentStub{events: &events, fail: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), now, 10); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "can-cleanup" || events[1] != "row" {
		t.Fatalf("completed cleanup retried non-metadata work: %v", events)
	}
}

func TestTaskMediaCleanupContinuesAfterOneObjectFails(t *testing.T) {
	now := time.Now().UTC()
	events := []string{}
	deleted := []string{}
	service, err := NewTaskMediaCleanupService(taskMediaCleanupStoreStub{
		objects: []TaskMediaStagedObject{
			{TaskID: "q1234567", OwnerID: "u1234567", OperationID: "o1234567", State: TaskMediaPreviewSaved, CleanupState: TaskMediaCleanupPending},
			{TaskID: "q2234567", OwnerID: "u2234567", OperationID: "o2234567", State: TaskMediaPreviewSaved, CleanupState: TaskMediaCleanupPending},
		},
		events: &events, deleted: &deleted,
	}, taskMediaCleanupContentStub{events: &events, failOwnerID: "u1234567"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), now, 10); err == nil {
		t.Fatal("first object failure was not returned")
	}
	wantEvents := []string{"content", "content", "mark", "row"}
	if len(events) != len(wantEvents) {
		t.Fatalf("cleanup events = %v, want %v", events, wantEvents)
	}
	for index := range wantEvents {
		if events[index] != wantEvents[index] {
			t.Fatalf("cleanup events = %v, want %v", events, wantEvents)
		}
	}
	if len(deleted) != 1 || deleted[0] != "q2234567/o2234567" {
		t.Fatalf("successful later cleanup rows = %v", deleted)
	}
}

func TestTaskMediaCleanupLeavesCompletedRowWhenMetadataDeleteFails(t *testing.T) {
	now := time.Now().UTC()
	events := []string{}
	service, err := NewTaskMediaCleanupService(taskMediaCleanupStoreStub{
		objects: []TaskMediaStagedObject{{TaskID: "q1234567", OwnerID: "u1234567", OperationID: "o1234567",
			State: TaskMediaPreviewExpired, CleanupState: TaskMediaCleanupComplete}},
		events: &events, failAt: "row",
	}, taskMediaCleanupContentStub{events: &events, fail: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), now, 10); err == nil {
		t.Fatal("metadata delete failure was not returned")
	}
	want := []string{"approval-expired", "row"}
	if len(events) != len(want) {
		t.Fatalf("failed complete-row retry events = %v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("failed complete-row retry events = %v, want %v", events, want)
		}
	}
}

func TestTaskMediaCleanupStopsAtFirstUnconfirmedStep(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name       string
		failAt     string
		contentErr bool
		want       []string
	}{
		{name: "content removal", contentErr: true, want: []string{"expire", "approval-expired", "content"}},
		{name: "cleanup marker", failAt: "mark", want: []string{"expire", "approval-expired", "content", "mark"}},
		{name: "metadata removal", failAt: "row", want: []string{"expire", "approval-expired", "content", "mark", "row"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			service, err := NewTaskMediaCleanupService(taskMediaCleanupStoreStub{
				objects: []TaskMediaStagedObject{{TaskID: "q1234567", OwnerID: "u1234567", OperationID: "o1234567",
					ExpiresAt: now.Add(-time.Minute), State: TaskMediaPreviewPending, CleanupState: TaskMediaCleanupPending}},
				events: &events, failAt: test.failAt,
			}, taskMediaCleanupContentStub{events: &events, fail: test.contentErr})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.RunOnce(context.Background(), now, 10); err == nil {
				t.Fatal("cleanup failure was not returned")
			}
			if len(events) != len(test.want) {
				t.Fatalf("cleanup event order = %v, want %v", events, test.want)
			}
			for index := range test.want {
				if events[index] != test.want[index] {
					t.Fatalf("cleanup event order = %v, want %v", events, test.want)
				}
			}
		})
	}
}

func TestTaskMediaCleanupWaitsForRejectedSaveResultToPersist(t *testing.T) {
	now := time.Now().UTC()
	events := []string{}
	service, err := NewTaskMediaCleanupService(taskMediaCleanupStoreStub{
		objects: []TaskMediaStagedObject{{TaskID: "q1234567", OwnerID: "u1234567", OperationID: "o1234567",
			State: TaskMediaPreviewDiscard, CleanupState: TaskMediaCleanupPending}},
		events: &events, failAt: "not-ready",
	}, taskMediaCleanupContentStub{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), now, 10); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0] != "can-cleanup" {
		t.Fatalf("cleanup ran before Task save result was durable: %v", events)
	}
}
