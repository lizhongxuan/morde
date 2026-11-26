package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/workspace"
)

type conversationReceiptStoreStub struct {
	*taskStoreStub
	receipts []ConversationMessageReceipt
}

func (s *conversationReceiptStoreStub) AcceptConversationMessage(_ context.Context, receipt ConversationMessageReceipt) (conversation.Message, bool, error) {
	s.receipts = append(s.receipts, receipt)
	return receipt.Message, false, nil
}

type conversationReceiptWorkspaceStoreStub struct {
	*conversationReceiptStoreStub
	value workspace.Workspace
}

func (s *conversationReceiptWorkspaceStoreStub) GetWorkspace(_ context.Context, id string) (workspace.Workspace, error) {
	if id != s.value.ID {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.value, nil
}

func (s *conversationReceiptWorkspaceStoreStub) GetWorkspaceManifestPageForOwner(_ context.Context, ownerID, workspaceID string,
	revision int64, _ string, _ string, _ int32) (workspace.ManifestPage, error) {
	if ownerID != s.value.OwnerID || workspaceID != s.value.ID || revision != s.value.Revision {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "workspace manifest not found")
	}
	return workspace.ManifestPage{WorkspaceID: workspaceID, Revision: revision,
		Files: []workspace.File{}, EmptyDirectories: []string{}, Directories: []workspace.Directory{}}, nil
}

func TestConversationReceiptCapturesWorkspaceInputSnapshotAndKeepsRetryHashStable(t *testing.T) {
	principal := taskTestPrincipal()
	base := &conversationReceiptStoreStub{taskStoreStub: &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: principal.UserID},
	}}}
	store := &conversationReceiptWorkspaceStoreStub{conversationReceiptStoreStub: base,
		value: workspace.Workspace{ID: "w1234567", OwnerID: principal.UserID, State: workspace.StateReady, Revision: 3}}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	command := AcceptConversationMessageCommand{ServiceInstanceID: "11111111-1111-4111-8111-111111111111",
		ClientMessageID: "message-workspace-01", IdempotencyKey: "message-workspace-01", Content: "分析这个工作区。", WorkspaceID: store.value.ID}
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); err != nil {
		t.Fatal(err)
	}
	first := base.receipts[0]
	if first.Input.WorkspaceInputRevision == nil || *first.Input.WorkspaceInputRevision != 3 || len(first.Input.WorkspaceContextDigest) != 32 {
		t.Fatalf("accepted receipt did not capture the selected Workspace snapshot: revision=%v digest_bytes=%d",
			first.Input.WorkspaceInputRevision, len(first.Input.WorkspaceContextDigest))
	}
	wantDigest, err := workspace.ManifestTreeWithMetadataSHA256(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Input.WorkspaceContextDigest, mustDecodeWorkspaceDigest(t, wantDigest)) {
		t.Fatalf("captured Workspace digest = %x, want manifest digest %s", first.Input.WorkspaceContextDigest, wantDigest)
	}

	store.value.Revision = 4
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); err != nil {
		t.Fatal(err)
	}
	second := base.receipts[1]
	if second.Input.WorkspaceInputRevision == nil || *second.Input.WorkspaceInputRevision != 4 {
		t.Fatalf("new receipt attempt did not capture its current Workspace revision: %v", second.Input.WorkspaceInputRevision)
	}
	if !bytes.Equal(first.RequestHash, second.RequestHash) {
		t.Fatal("server-captured Workspace revision changed the client message idempotency hash")
	}
}

func TestConversationReceiptHashIgnoresProjectWorkspaceAuthorizationSnapshot(t *testing.T) {
	firstVersion, nextVersion := int64(4), int64(5)
	firstRevision, nextRevision := int64(12), int64(13)
	first, err := ConversationReceiptHash("c1234567", "在项目工作区里处理文件。", ConversationRoutingInput{
		ProjectID: "pr123456", WorkspaceID: "w1234567", WorkspaceOwnerID: "u1234567",
		WorkspaceProjectResourceVersion: &firstVersion, WorkspaceInputRevision: &firstRevision,
		WorkspaceContextDigest: bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ConversationReceiptHash("c1234567", "在项目工作区里处理文件。", ConversationRoutingInput{
		ProjectID: "pr123456", WorkspaceID: "w1234567", WorkspaceOwnerID: "u7654321",
		WorkspaceProjectResourceVersion: &nextVersion, WorkspaceInputRevision: &nextRevision,
		WorkspaceContextDigest: bytes.Repeat([]byte{2}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("server-captured project Workspace authorization changed the client request hash")
	}
}

func TestConversationReceiptDoesNotAuthorizeForeignWorkspaceSnapshot(t *testing.T) {
	principal := taskTestPrincipal()
	base := &conversationReceiptStoreStub{taskStoreStub: &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: principal.UserID},
	}}}
	store := &conversationReceiptWorkspaceStoreStub{conversationReceiptStoreStub: base,
		value: workspace.Workspace{ID: "w1234567", OwnerID: "u7654321", State: workspace.StateReady, Revision: 3}}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	command := AcceptConversationMessageCommand{ServiceInstanceID: "11111111-1111-4111-8111-111111111111",
		ClientMessageID: "message-workspace-foreign", IdempotencyKey: "message-workspace-foreign", Content: "分析这个工作区。", WorkspaceID: store.value.ID}
	if _, _, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command); err != nil {
		t.Fatal(err)
	}
	if base.receipts[0].Input.WorkspaceID != store.value.ID || base.receipts[0].Input.WorkspaceInputRevision != nil ||
		len(base.receipts[0].Input.WorkspaceContextDigest) != 0 {
		t.Fatalf("foreign Workspace was treated as an authorized snapshot: %+v", base.receipts[0].Input)
	}
}

func mustDecodeWorkspaceDigest(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
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

func TestConversationReceiptAcceptsMessagesAroundEightKiBBytesAndRunes(t *testing.T) {
	principal := taskTestPrincipal()
	store := &conversationReceiptStoreStub{taskStoreStub: &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: principal.UserID},
	}}}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	contents := make([]string, 0, 6)
	for _, size := range []int{8191, 8192, 8193} {
		contents = append(contents, strings.Repeat("x", size))
	}
	for _, runes := range []int{8191, 8192, 8193} {
		contents = append(contents, strings.Repeat("中", runes)+"<&🙂")
	}
	for index, content := range contents {
		messageID := fmt.Sprintf("message-8k-%02d", index)
		command := AcceptConversationMessageCommand{
			ServiceInstanceID: "11111111-1111-4111-8111-111111111111",
			ClientMessageID:   messageID, IdempotencyKey: messageID, Content: content,
		}
		message, replayed, err := service.AcceptConversationMessage(context.Background(), principal, "c1234567", command)
		if err != nil || replayed || message.Content != content {
			t.Fatalf("8 KiB boundary message %d (bytes=%d runes=%d) = accepted=%t replayed=%t err=%v",
				index, len(content), len([]rune(content)), err == nil, replayed, err)
		}
	}
	if len(store.receipts) != len(contents) {
		t.Fatalf("stored %d of %d accepted boundary receipts", len(store.receipts), len(contents))
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
