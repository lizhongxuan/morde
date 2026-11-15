package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type taskPriorityStoreStub struct {
	*taskStoreStub
	record         IdempotencyRecord
	taskID         string
	priority       task.QueuePriority
	expectedSeq    int64
	requestID      string
	priorityResult task.Task
	priorityErr    error
	priorityCalls  int
}

func (s *taskPriorityStoreStub) SetTaskPriority(_ context.Context, record IdempotencyRecord, taskID string,
	priority task.QueuePriority, expectedSeq int64, requestID string) (task.Task, error) {
	s.record, s.taskID, s.priority, s.expectedSeq, s.requestID = record, taskID, priority, expectedSeq, requestID
	s.priorityCalls++
	return s.priorityResult, s.priorityErr
}

func TestTaskServiceSetPriorityIsAuthorizedAndIdempotent(t *testing.T) {
	principal := taskTestPrincipal()
	value := task.Task{ID: "q1234567", OwnerID: principal.UserID, InitiatorID: principal.UserID,
		ConversationID: "c1234567", HostID: "h1234567", ExecutionLocation: "server", ModelID: "model",
		Status: task.StatusQueued, QueuePriority: task.QueuePriorityNormal, EventSequence: 7,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	store := &taskPriorityStoreStub{taskStoreStub: &taskStoreStub{details: TaskDetails{Task: value}}, priorityResult: value}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.SetPriority(context.Background(), principal, value.ID, task.QueuePriorityPinned, value.EventSequence, "priority-key-001")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != value.ID || store.priorityCalls != 1 || store.taskID != value.ID || store.priority != task.QueuePriorityPinned ||
		store.expectedSeq != value.EventSequence || store.record.OwnerID != principal.UserID || store.record.Operation != "task.priority.set" ||
		store.record.Key != "priority-key-001" || len(store.record.RequestHash) != 32 || store.record.RelatedID != value.ID ||
		store.requestID == "" || store.record.ResourceID != store.requestID {
		t.Fatalf("result=%#v store=%#v", result, store)
	}
}

func TestTaskServiceSetPriorityRejectsInvalidAndUnauthorizedRequests(t *testing.T) {
	principal := taskTestPrincipal()
	value := task.Task{ID: "q1234567", OwnerID: principal.UserID, InitiatorID: principal.UserID,
		ConversationID: "c1234567", HostID: "h1234567", ExecutionLocation: "server", ModelID: "model",
		Status: task.StatusQueued, QueuePriority: task.QueuePriorityNormal, EventSequence: 7,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	for _, test := range []struct {
		name       string
		priority   task.QueuePriority
		sequence   int64
		authorizer *taskAuthorizerStub
		wantCode   domain.ErrorCode
	}{
		{name: "unknown priority", priority: "urgent", sequence: 7, authorizer: &taskAuthorizerStub{}, wantCode: domain.CodeInvalidArgument},
		{name: "negative sequence", priority: task.QueuePriorityNormal, sequence: -1, authorizer: &taskAuthorizerStub{}, wantCode: domain.CodeInvalidArgument},
		{name: "unauthorized", priority: task.QueuePriorityPinned, sequence: 7, authorizer: &taskAuthorizerStub{err: domain.NewError(domain.CodeForbidden, "denied")}, wantCode: domain.CodeNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &taskPriorityStoreStub{taskStoreStub: &taskStoreStub{details: TaskDetails{Task: value}}, priorityResult: value}
			service, err := NewTaskService(store, test.authorizer, &modelCatalogStub{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.SetPriority(context.Background(), principal, value.ID, test.priority, test.sequence, "priority-key-002")
			if domain.CodeOf(err) != test.wantCode || store.priorityCalls != 0 {
				t.Fatalf("SetPriority() err=%v calls=%d, want code=%s and no writes", err, store.priorityCalls, test.wantCode)
			}
		})
	}
}
