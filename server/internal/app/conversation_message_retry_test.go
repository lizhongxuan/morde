package app

import (
	"context"
	"testing"

	"morde/server/internal/domain/identity"
)

type conversationMessageRetryStoreStub struct {
	*taskStoreStub
	eligibility    ConversationMessageRetryEligibility
	err            error
	calls          int
	projectID      string
	conversationID string
	messageID      string
	instanceID     string
}

func (s *conversationMessageRetryStoreStub) RetryConversationRoute(context.Context, identity.Principal,
	string, string, string, string, string) error {
	return nil
}

func (s *conversationMessageRetryStoreStub) GetConversationMessageRetryEligibility(_ context.Context, _ identity.Principal,
	projectID, conversationID, messageID, instanceID string) (ConversationMessageRetryEligibility, error) {
	s.calls++
	s.projectID, s.conversationID, s.messageID, s.instanceID = projectID, conversationID, messageID, instanceID
	return s.eligibility, s.err
}

func TestTaskServiceGetConversationMessageRetryEligibilityValidatesAndDelegates(t *testing.T) {
	store := &conversationMessageRetryStoreStub{taskStoreStub: &taskStoreStub{}, eligibility: ConversationMessageRetryEligibility{Eligible: true,
		Reason: ConversationMessageRetryReasonAvailable, Warning: ConversationMessageRetryWarningPersonalOutcomeUnknown}}
	service := &TaskService{store: store}
	principal := taskTestPrincipal()
	instanceID := "11111111-1111-4111-8111-111111111111"
	got, err := service.GetConversationMessageRetryEligibility(context.Background(), principal, "j1234567", "c1234567", "m1234567", instanceID)
	if err != nil || got != store.eligibility || store.calls != 1 || store.projectID != "j1234567" || store.conversationID != "c1234567" ||
		store.messageID != "m1234567" || store.instanceID != instanceID {
		t.Fatalf("eligibility = %+v, err=%v, store=%+v", got, err, store)
	}
}

func TestTaskServiceGetConversationMessageRetryEligibilityRejectsMissingSubmitScope(t *testing.T) {
	store := &conversationMessageRetryStoreStub{taskStoreStub: &taskStoreStub{}}
	service := &TaskService{store: store}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeTaskRead}}
	_, err := service.GetConversationMessageRetryEligibility(context.Background(), principal, "", "c1234567", "m1234567",
		"11111111-1111-4111-8111-111111111111")
	if err == nil || store.calls != 0 {
		t.Fatalf("missing task.submit scope error=%v store_calls=%d", err, store.calls)
	}
}
