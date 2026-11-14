package app

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type conversationVariantResumeStoreStub struct {
	TaskStore
	conversation conversation.Conversation
	details      TaskDetails
	lookupErr    error
	operation    ConversationMessageVariantOperation
	variantState string
	stateCalls   int
	resumeErr    error
}

func (s *conversationVariantResumeStoreStub) GetConversation(_ context.Context, conversationID, ownerID string) (conversation.Conversation, error) {
	if s.conversation.ID != conversationID || s.conversation.OwnerID != ownerID {
		return conversation.Conversation{}, errors.New("conversation not found")
	}
	return s.conversation, nil
}

func (s *conversationVariantResumeStoreStub) GetTask(context.Context, string, string) (TaskDetails, error) {
	return s.details, s.lookupErr
}

func (s *conversationVariantResumeStoreStub) CreateConversationMessageVariant(_ context.Context, _ identity.Principal,
	command CreateConversationMessageVariantCommand) (ConversationMessageVariantOperation, error) {
	operation := s.operation
	operation.OperationID = command.OperationID
	operation.VariantGroupID = command.SourceMessageID
	operation.VariantVersion = command.ExpectedVersion + 1
	operation.Action = command.Action
	operation.State = "accepted"
	return operation, nil
}

func (s *conversationVariantResumeStoreStub) SetConversationMessageVariantState(_ context.Context, _ identity.Principal, _ string, state string) error {
	s.variantState = state
	s.stateCalls++
	return nil
}

func (s *conversationVariantResumeStoreStub) RequestTaskAction(_ context.Context, _ IdempotencyRecord, _ string, _ task.Action,
	_ task.Status, _ *TaskActionRevisionFence, _ string) (TaskActionResult, error) {
	return TaskActionResult{}, s.resumeErr
}

type conversationVariantResumeAuthorizerStub struct{}

func (conversationVariantResumeAuthorizerStub) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string,
	[]identity.Scope, identity.Scope) error {
	return nil
}

func (a conversationVariantResumeAuthorizerStub) AuthorizeTaskExecution(_ context.Context, principal identity.Principal,
	_ string, _ identity.Scope) (task.Authorization, error) {
	return authorizationSnapshot(principal), nil
}

func TestConversationVariantResumeFailureReturnsDurableUnknownOperation(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*conversationVariantResumeStoreStub)
	}{
		{
			name: "task lookup failed after variant acceptance",
			setup: func(store *conversationVariantResumeStoreStub) {
				store.lookupErr = errors.New("task lookup response lost")
			},
		},
		{
			name: "resume result is unknown",
			setup: func(store *conversationVariantResumeStoreStub) {
				current, desired := int64(4), int64(4)
				store.details = TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
					Status: task.StatusPaused, CurrentRevision: &current, DesiredRevision: &desired}}
				store.resumeErr = errors.New("resume transaction response lost")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &conversationVariantResumeStoreStub{conversation: conversation.Conversation{ID: "c1234567", OwnerID: "u1234567"},
				operation: ConversationMessageVariantOperation{TaskID: "q1234567"}}
			test.setup(store)
			authorizer := conversationVariantResumeAuthorizerStub{}
			service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer,
				actions: store}

			principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
			operation, err := service.CreateConversationMessageVariant(context.Background(), principal, "", "c1234567",
				"m1234567", "11111111-1111-4111-8111-111111111111", ConversationMessageVariantContinue, 1, "", "variant-resume-001")
			if err != nil {
				t.Fatalf("durably accepted resume returned a definite error: %v", err)
			}
			if operation.State != "unknown" || operation.OperationID == "" || operation.VariantVersion != 2 ||
				operation.TaskID != "q1234567" || store.stateCalls != 1 || store.variantState != "unknown" {
				t.Fatalf("resume response did not preserve the accepted unknown operation: operation=%+v stored_state=%q calls=%d",
					operation, store.variantState, store.stateCalls)
			}
		})
	}
}
