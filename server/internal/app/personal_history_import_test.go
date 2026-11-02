package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type personalHistoryImportStoreStub struct {
	calls     int
	principal identity.Principal
	input     conversation.HistoryImport
	result    conversation.HistoryImportResult
	err       error
}

func (s *personalHistoryImportStoreStub) ImportPersonalHistory(_ context.Context, principal identity.Principal, input conversation.HistoryImport) (conversation.HistoryImportResult, error) {
	s.calls++
	s.principal, s.input = principal, input
	return s.result, s.err
}

func validPersonalHistoryPrincipal() identity.Principal {
	return identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567",
	}
}

func validPersonalHistoryInput() conversation.HistoryImport {
	return conversation.HistoryImport{
		SourceID: "export", DeviceAccountID: "device-account", SourceConversationID: "chat-1",
		Messages: []conversation.HistoryMessage{{SourceMessageID: "message-1", Role: conversation.RoleUser, Content: "hello"}},
	}
}

func TestPersonalHistoryImportUsesPrincipalOwnerAndDedicatedStore(t *testing.T) {
	input := validPersonalHistoryInput()
	store := &personalHistoryImportStoreStub{result: conversation.HistoryImportResult{
		Conversation: conversation.Conversation{ID: "c1234567", OwnerID: "u1234567", Title: "Imported"},
		Messages: []conversation.ImportedHistoryMessage{{SourceMessageID: "message-1", Message: conversation.Message{
			ID: "m1234567", ConversationID: "c1234567", OwnerID: "u1234567", Role: conversation.RoleUser,
			Content: "hello", SourceKind: conversation.HistoryImportSourceKind, Sequence: 1,
		}}},
	}}
	service, err := NewPersonalHistoryImportService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := validPersonalHistoryPrincipal()
	result, err := service.Import(context.Background(), principal, input)
	if err != nil || result.Conversation.ID != "c1234567" || store.calls != 1 || store.principal.UserID != principal.UserID ||
		store.input.SourceConversationID != input.SourceConversationID {
		t.Fatalf("Import() = %+v, %v; store calls=%d principal=%+v input=%+v", result, err, store.calls, store.principal, store.input)
	}
}

func TestPersonalHistoryImportRejectsNonPortalCredentialBeforeStore(t *testing.T) {
	store := &personalHistoryImportStoreStub{}
	service, err := NewPersonalHistoryImportService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := validPersonalHistoryPrincipal()
	principal.Kind = identity.KindAPIKey
	if _, err := service.Import(context.Background(), principal, validPersonalHistoryInput()); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API key error = %v, want forbidden", err)
	}
	principal = validPersonalHistoryPrincipal()
	principal.TokenID = ""
	if _, err := service.Import(context.Background(), principal, validPersonalHistoryInput()); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("missing token ID error = %v, want forbidden", err)
	}
	if store.calls != 0 {
		t.Fatalf("invalid principal reached store %d times", store.calls)
	}
}

func TestPersonalHistoryImportRejectsInvalidInputBeforeStore(t *testing.T) {
	store := &personalHistoryImportStoreStub{}
	service, err := NewPersonalHistoryImportService(store)
	if err != nil {
		t.Fatal(err)
	}
	input := validPersonalHistoryInput()
	input.Messages[0].Role = conversation.Role("tool")
	if _, err := service.Import(context.Background(), validPersonalHistoryPrincipal(), input); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid role error = %v, want invalid_argument", err)
	}
	if store.calls != 0 {
		t.Fatalf("invalid input reached store %d times", store.calls)
	}
}

func TestPersonalHistoryImportRejectsStoreReceiptContainingTask(t *testing.T) {
	store := &personalHistoryImportStoreStub{result: conversation.HistoryImportResult{
		Conversation: conversation.Conversation{ID: "c1234567", OwnerID: "u1234567"},
		Messages: []conversation.ImportedHistoryMessage{{SourceMessageID: "message-1", Message: conversation.Message{
			ID: "m1234567", ConversationID: "c1234567", OwnerID: "u1234567", Role: conversation.RoleUser,
			Content: "hello", TaskID: "q1234567", SourceKind: conversation.HistoryImportSourceKind, Sequence: 1,
		}}},
	}}
	service, err := NewPersonalHistoryImportService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Import(context.Background(), validPersonalHistoryPrincipal(), validPersonalHistoryInput()); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("Task-linked receipt error = %v, want internal", err)
	}
}
