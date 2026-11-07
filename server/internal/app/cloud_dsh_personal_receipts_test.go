package app

import (
	"bytes"
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
)

type cloudDSHPersonalReceiptTaskStore struct {
	*cloudDSHBrokerTaskStore
	requests []CloudDSHPersonalReceiptRequest
	err      error
}

func (s *cloudDSHPersonalReceiptTaskStore) ConfirmCloudDSHPersonalMutationReceipt(_ context.Context, request CloudDSHPersonalReceiptRequest) error {
	if !ValidCloudDSHPersonalMutationCommit(request.Commit) {
		return domain.NewError(domain.CodeInvalidState, "invalid receipt request")
	}
	s.requests = append(s.requests, request)
	return s.err
}

func TestCloudDSHPersonalReceiptBrokerChecksArgumentsWithoutWriting(t *testing.T) {
	broker, baseStore, claimed, _, _ := cloudDSHGoalScheduleTaskFixture(t)
	store := &cloudDSHPersonalReceiptTaskStore{cloudDSHBrokerTaskStore: baseStore}
	broker.store = store
	personalStore := &personalMutationStoreStub{}
	personalService, err := NewPersonalMutationService(personalStore)
	if err != nil {
		t.Fatal(err)
	}
	broker.personalData = personalService
	fence := AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentSessionID: claimed.AgentSessionID, AuthorizationVersion: claimed.Authorization.Version}
	call := ModelToolCall{CallID: "receipt-call", Name: "morde_personal_object_update",
		Arguments: `{"object_type":"memory","object_id":"global","base_revision":1,"body":"{\"content\":\"updated\"}"}`}
	version, output, err := broker.ConfirmPersonalContextWrite(context.Background(), claimed, fence, 3, bytes.Repeat([]byte{1}, 32), call)
	if err != nil || version.ObjectType != personal.ObjectTypeMemory || version.ID != "global" || version.Revision != 2 ||
		output != `{"object_type":"memory","object_id":"global","revision":2}` {
		t.Fatalf("receipt confirmation: version=%+v output=%q err=%v", version, output, err)
	}
	if personalStore.calls != 0 || len(store.requests) != 1 || store.requests[0].Commit.CallID != call.CallID ||
		string(store.requests[0].Commit.Mutation.Body) != `{"content":"updated"}` {
		t.Fatalf("receipt confirmation wrote data or changed arguments: writes=%d requests=%+v", personalStore.calls, store.requests)
	}
	wrong := call
	wrong.Name = "morde_goal_update"
	if _, _, err := broker.ConfirmPersonalContextWrite(context.Background(), claimed, fence, 3, bytes.Repeat([]byte{1}, 32), wrong); err == nil {
		t.Fatal("unsupported goal receipt was accepted")
	}
	store.err = domain.NewError(domain.CodeInvalidState, "receipt missing")
	if _, _, err := broker.ConfirmPersonalContextWrite(context.Background(), claimed, fence, 3, bytes.Repeat([]byte{1}, 32), call); err == nil {
		t.Fatal("missing committed receipt was trusted")
	}
}
