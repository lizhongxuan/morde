package app

import (
	"bytes"
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskStepRetryStoreStub struct {
	*taskStoreStub
	records  map[string]TaskStepRetryRecord
	requests []TaskStepRetryRequest
	keys     []IdempotencyRecord
}

func (s *taskStepRetryStoreStub) RequestTaskStepRetry(_ context.Context, record IdempotencyRecord,
	request TaskStepRetryRequest) (TaskStepRetryRecord, error) {
	s.requests = append(s.requests, request)
	s.keys = append(s.keys, record)
	key := record.OwnerID + ":" + record.Operation + ":" + record.Key
	if prior, ok := s.records[key]; ok {
		if !bytes.Equal(record.RequestHash, s.keys[0].RequestHash) {
			return TaskStepRetryRecord{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key conflict")
		}
		prior.Replayed = true
		return prior, nil
	}
	intent := task.StepRetryIntent{TaskID: request.TaskID, OwnerID: record.OwnerID, SourceStepID: request.StepID,
		AttemptStepID: request.AttemptStepID, Attempt: request.ExpectedAttempt + 1, RetryCallID: request.RetryCallID,
		ExpiresAt: time.Now().Add(time.Hour)}
	result := TaskStepRetryRecord{Intent: intent}
	if s.records == nil {
		s.records = make(map[string]TaskStepRetryRecord)
	}
	s.records[key] = result
	return result, nil
}

func TestTaskStepRetryUsesOneIdempotentAttemptIdentity(t *testing.T) {
	principal := taskTestPrincipal()
	authorization := authorizationSnapshot(principal)
	authorization.Version = 1
	store := &taskStepRetryStoreStub{taskStoreStub: &taskStoreStub{details: TaskDetails{Task: task.Task{
		ID: "q1234567", OwnerID: principal.UserID, InitiatorID: principal.UserID, HostID: "h1234567",
		Authorization: authorization,
	}}}}
	authorizer := &taskAuthorizerStub{}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer}

	first, err := service.RetryStep(context.Background(), principal, "q1234567", "s1234567", 4, 1, "retry-key-001")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RetryStep(context.Background(), principal, "q1234567", "s1234567", 4, 1, "retry-key-001")
	if err != nil {
		t.Fatal(err)
	}
	if first.AttemptStepID == "" || first.AttemptStepID != second.AttemptStepID || first.Attempt != 2 || second.Attempt != 2 ||
		first.Replayed || !second.Replayed || len(store.requests) != 2 || len(store.keys) != 2 ||
		store.requests[0].AttemptStepID == store.requests[1].AttemptStepID ||
		store.requests[0].RetryCallID != "morde_step_retry_"+store.requests[0].AttemptStepID ||
		store.requests[1].RetryCallID != "morde_step_retry_"+store.requests[1].AttemptStepID ||
		!bytes.Equal(store.keys[0].RequestHash, store.keys[1].RequestHash) {
		t.Fatalf("same-key retry generated multiple attempts: first=%+v second=%+v requests=%+v records=%+v", first, second, store.requests, store.keys)
	}

	principal.Kind = identity.KindSession
	third, err := service.RetryStep(context.Background(), principal, "q1234567", "s1234567", 4, 1, "retry-key-002")
	if err != nil || third.AttemptStepID == first.AttemptStepID || third.Replayed {
		t.Fatalf("different key did not create a distinct idempotent attempt: result=%+v err=%v", third, err)
	}
}
