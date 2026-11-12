package app

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

type conversationReceiptStoreStub struct {
	*taskStoreStub
	receipts []ConversationMessageReceipt
}

func (s *conversationReceiptStoreStub) AcceptConversationMessage(_ context.Context, receipt ConversationMessageReceipt) (conversation.Message, bool, error) {
	s.receipts = append(s.receipts, receipt)
	return receipt.Message, false, nil
}

func TestConversationReceiptDefersModelAndHostChecks(t *testing.T) {
	principal := taskTestPrincipal()
	store := &conversationReceiptStoreStub{taskStoreStub: &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: principal.UserID},
	}}}
	authorizer := &taskAuthorizerStub{hostErrors: map[string]error{"h1234567": errors.New("host unavailable")}}
	models := &modelCatalogStub{err: errors.New("model unavailable")}
	service, err := NewTaskService(store, authorizer, models)
	if err != nil {
		t.Fatal(err)
	}
	command := AcceptConversationMessageCommand{
		ServiceInstanceID: "11111111-1111-4111-8111-111111111111",
		ClientMessageID:   "message-0001", IdempotencyKey: "message-0001", Content: "帮我整理资料",
		HostID: "h1234567", WorkspaceID: "w1234567", ModelID: "unavailable-model",
		ChangeTaskTarget:    true,
		ClientAttachmentIDs: []string{"attachment-0001"},
	}
	message, replayed, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command)
	if err != nil || replayed || message.Content != command.Content || len(store.receipts) != 1 {
		t.Fatalf("accept message = %+v, replayed=%t, err=%v", message, replayed, err)
	}
	if authorizer.lastHost != "" || models.modelID != "" {
		t.Fatalf("receipt checked a tool or model: host=%q model=%q", authorizer.lastHost, models.modelID)
	}
	if store.receipts[0].Input.HostID != command.HostID || store.receipts[0].Input.WorkspaceID != command.WorkspaceID ||
		!store.receipts[0].Input.ChangeTaskTarget ||
		store.receipts[0].Message.DeliveryStatus != conversation.DeliveryPending {
		t.Fatalf("receipt did not preserve input and pending delivery: %+v", store.receipts[0])
	}
	_, _, err = service.AcceptConversationMessage(context.Background(), principal, "c1234567", command)
	if err != nil || !bytes.Equal(store.receipts[0].RequestHash, store.receipts[1].RequestHash) {
		t.Fatalf("same message produced a different fingerprint: %v", err)
	}
	command.Content = "改一下资料范围"
	_, _, err = service.AcceptConversationMessage(context.Background(), principal, "c1234567", command)
	if err != nil || bytes.Equal(store.receipts[0].RequestHash, store.receipts[2].RequestHash) {
		t.Fatalf("changed content reused the request fingerprint: %v", err)
	}
}

func TestConversationReceiptValidatesIdentityAndAllowsAttachmentOnly(t *testing.T) {
	principal := taskTestPrincipal()
	store := &conversationReceiptStoreStub{taskStoreStub: &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: principal.UserID},
	}}}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	command := AcceptConversationMessageCommand{ServiceInstanceID: "11111111-1111-4111-8111-111111111111", ClientMessageID: "message-0001", IdempotencyKey: "message-0001", ClientAttachmentIDs: []string{"attachment-0001"}}
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); err != nil {
		t.Fatalf("attachment-only message rejected: %v", err)
	}
	command.IdempotencyKey = "different-key"
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("mismatched idempotency key = %v", err)
	}
	command.IdempotencyKey = command.ClientMessageID
	command.ClientAttachmentIDs = []string{"attachment-0001", "attachment-0001"}
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("duplicate attachment identity = %v", err)
	}
	other := principal
	other.UserID = "u7654321"
	command.ClientAttachmentIDs = []string{"attachment-0001"}
	if _, _, err := service.AcceptConversationMessage(context.Background(), other, "c1234567", command); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another account accessed personal conversation = %v", err)
	}
}

func TestConversationReceiptValidatesAndFingerprintsTimeZone(t *testing.T) {
	principal := taskTestPrincipal()
	store := &conversationReceiptStoreStub{taskStoreStub: &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: principal.UserID},
	}}}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	command := AcceptConversationMessageCommand{ServiceInstanceID: "11111111-1111-4111-8111-111111111111",
		ClientMessageID: "message-timezone", IdempotencyKey: "message-timezone", Content: "现在几点？", TimeZone: "Asia/Shanghai"}
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); err != nil {
		t.Fatal(err)
	}
	if got := store.receipts[0].Input.TimeZone; got != "Asia/Shanghai" {
		t.Fatalf("time zone was not persisted: %q", got)
	}
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); err != nil ||
		!bytes.Equal(store.receipts[0].RequestHash, store.receipts[1].RequestHash) {
		t.Fatalf("identical time-zone retry changed its fingerprint: %v", err)
	}
	command.TimeZone = "America/Los_Angeles"
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); err != nil ||
		bytes.Equal(store.receipts[0].RequestHash, store.receipts[2].RequestHash) {
		t.Fatalf("changed time zone reused its fingerprint: %v", err)
	}
	for _, invalid := range []string{"Local", "../etc/passwd", " Asia/Shanghai", "unknown/time-zone"} {
		command.TimeZone = invalid
		if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("invalid time zone %q was accepted: %v", invalid, err)
		}
	}
}
