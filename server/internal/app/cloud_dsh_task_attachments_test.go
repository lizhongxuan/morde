package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

type cloudDSHTaskAttachmentStore struct {
	TaskStore
	details  TaskDetails
	messages []conversation.Message
}

func (s *cloudDSHTaskAttachmentStore) GetTask(ctx context.Context, id, ownerID string) (TaskDetails, error) {
	if err := ctx.Err(); err != nil {
		return TaskDetails{}, err
	}
	if id != s.details.Task.ID || ownerID != s.details.Task.OwnerID {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	return s.details, nil
}

func (s *cloudDSHTaskAttachmentStore) ListMessages(ctx context.Context, ownerID, conversationID string, _ PageRequest) (MessagePage, error) {
	if err := ctx.Err(); err != nil {
		return MessagePage{}, err
	}
	if ownerID != s.details.Task.OwnerID || conversationID != s.details.Task.ConversationID {
		return MessagePage{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	return MessagePage{Items: s.messages}, nil
}

func (s *cloudDSHTaskAttachmentStore) CommitTaskProgress(_ context.Context, progress TaskProgress) (task.Event, error) {
	if progress.TaskID != s.details.Task.ID || progress.LeaseOwner != s.details.Task.Lease.Owner ||
		progress.LeaseVersion != s.details.Task.Lease.Version {
		return task.Event{}, domain.NewError(domain.CodeInvalidState, "stale task progress")
	}
	s.details.Task.Usage = progress.Usage
	return task.Event{}, nil
}

type cloudDSHTaskAttachmentFileStore struct {
	personalAttachmentStoreStub
	metadata map[string]personal.AttachmentMetadata
	content  map[string][]byte
	opened   []string
}

func (s *cloudDSHTaskAttachmentFileStore) OpenPersonalAttachment(_ context.Context, _ identity.Principal, id string) (io.ReadCloser, personal.AttachmentMetadata, error) {
	s.opened = append(s.opened, id)
	metadata, ok := s.metadata[id]
	if !ok {
		return nil, personal.AttachmentMetadata{}, domain.NewError(domain.CodeNotFound, "personal attachment not found")
	}
	return io.NopCloser(bytes.NewReader(s.content[id])), metadata, nil
}

type cloudDSHTaskAttachmentPersonalStore struct {
	personalMutationStoreStub
	objects map[string]personal.Object
}

func (s *cloudDSHTaskAttachmentPersonalStore) GetPersonalObject(_ context.Context, _ identity.Principal, objectType, objectID string) (personal.Object, error) {
	if objectType != personal.ObjectTypeLibraryItem {
		return personal.Object{}, domain.NewError(domain.CodeNotFound, "personal object not found")
	}
	object, ok := s.objects[objectID]
	if !ok {
		return personal.Object{}, domain.NewError(domain.CodeNotFound, "personal object not found")
	}
	return object, nil
}

func TestCloudDSHAttachmentReaderScopesFilesToCurrentTask(t *testing.T) {
	imageID := "pa_0123456789abcdef0123456789abcdef"
	textID := "pa_1123456789abcdef0123456789abcdef"
	unlinkedID := "pa_2123456789abcdef0123456789abcdef"
	pngID := "pa_3123456789abcdef0123456789abcdef"
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 1)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 1))); err != nil {
		t.Fatal(err)
	}
	imageBytes := jpegBytes.Bytes()
	pngImageBytes := pngBytes.Bytes()
	textBytes := []byte("Read this PDF text")
	fileStore := &cloudDSHTaskAttachmentFileStore{
		metadata: map[string]personal.AttachmentMetadata{
			imageID: {AttachmentID: imageID, ObjectType: personal.ObjectTypeLibraryItem, ObjectID: "li_image",
				MediaType: "image/jpeg", SizeBytes: int64(len(imageBytes)), SHA256: strings.Repeat("a", 64)},
			pngID: {AttachmentID: pngID, ObjectType: personal.ObjectTypeLibraryItem, ObjectID: "li_png",
				MediaType: "image/png", SizeBytes: int64(len(pngImageBytes)), SHA256: strings.Repeat("c", 64)},
			textID: {AttachmentID: textID, ObjectType: personal.ObjectTypeLibraryItem, ObjectID: "li_text",
				MediaType: "text/plain", SizeBytes: int64(len(textBytes)), SHA256: strings.Repeat("b", 64)},
		},
		content: map[string][]byte{imageID: imageBytes, pngID: pngImageBytes, textID: textBytes, unlinkedID: []byte("private")},
	}
	fileService, err := NewPersonalAttachmentService(fileStore, &personalAttachmentQuotaStub{})
	if err != nil {
		t.Fatal(err)
	}
	objects := make(map[string]personal.Object)
	for _, value := range []struct {
		id, name, mediaType string
		bytes               int
	}{
		{imageID, "photo.jpg", "image/jpeg", len(imageBytes)},
		{pngID, "diagram.png", "image/png", len(pngImageBytes)},
		{textID, "PDF-解析-01-文本.txt", "text/plain", len(textBytes)},
	} {
		body, marshalErr := json.Marshal(map[string]any{"name": value.name, "kind": "media", "mime": value.mediaType,
			"size_bytes": value.bytes, "attachment_id": value.id})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		objectID := fileStore.metadata[value.id].ObjectID
		objects[objectID] = personal.Object{ObjectType: personal.ObjectTypeLibraryItem, ObjectID: objectID, Revision: 1, Body: body}
	}
	personalStore := &cloudDSHTaskAttachmentPersonalStore{personalMutationStoreStub: personalMutationStoreStub{}, objects: objects}
	personalService, err := NewPersonalMutationService(personalStore)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC()
	current := task.Task{
		ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		ExecutionLocation: "server", Status: task.StatusRunning, AgentSessionID: "session-1",
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
			ExpiresAt: &expires, Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}},
		ConversationContext: &task.ConversationContext{OwnerID: "u1234567", InitiatorID: "u1234567",
			ConversationID: "c1234567", Scope: "personal"},
		Budget: task.Budget{ToolCalls: 10}, Lease: task.Lease{Owner: "worker-1", Version: 1, ExpiresAt: &expires},
	}
	store := &cloudDSHTaskAttachmentStore{
		details: TaskDetails{Task: current},
		messages: []conversation.Message{
			{ID: "m1111111", OwnerID: current.OwnerID, ConversationID: current.ConversationID, TaskID: current.ID,
				Role: conversation.RoleUser, AttachmentIDs: []string{imageID, pngID, textID}},
			{ID: "m2222222", OwnerID: current.OwnerID, ConversationID: current.ConversationID, TaskID: "q8765432",
				Role: conversation.RoleUser, AttachmentIDs: []string{unlinkedID}},
			{ID: "m3333333", OwnerID: current.OwnerID, ConversationID: current.ConversationID, TaskID: current.ID,
				Role: conversation.RoleAssistant, AttachmentIDs: []string{unlinkedID}},
		},
	}
	broker := &CloudDSHToolBroker{store: store, personalData: personalService, personalFiles: fileService}
	result, err := broker.Execute(context.Background(), current, ModelToolCall{
		CallID: "read-image", Name: cloudDSHTaskAttachmentReadToolName, Arguments: `{"attachment_id":"` + imageID + `"}`,
	})
	if err != nil || result.Status != AgentToolSucceeded || len(result.Images) != 1 ||
		result.Images[0].MediaType != "image/jpeg" || !bytes.Equal(result.Images[0].Data, imageBytes) {
		t.Fatalf("image attachment read = %#v, %v", result, err)
	}
	if len(fileStore.opened) != 4 || fileStore.opened[3] != imageID {
		t.Fatalf("opened attachments = %v; want metadata reads of only current Task files followed by the requested image", fileStore.opened)
	}
	pngResult, err := broker.Execute(context.Background(), current, ModelToolCall{
		CallID: "read-png", Name: cloudDSHTaskAttachmentReadToolName, Arguments: `{"attachment_id":"` + pngID + `"}`,
	})
	if err != nil || pngResult.Status != AgentToolSucceeded || len(pngResult.Images) != 1 ||
		pngResult.Images[0].MediaType != "image/png" || !bytes.Equal(pngResult.Images[0].Data, pngImageBytes) ||
		!validCloudDSHToolImages(cloudDSHTaskAttachmentReadToolName, pngResult) {
		t.Fatalf("PNG attachment read = %#v, %v", pngResult, err)
	}
	textResult, err := broker.Execute(context.Background(), current, ModelToolCall{
		CallID: "read-text", Name: cloudDSHTaskAttachmentReadToolName, Arguments: `{"attachment_id":"` + textID + `"}`,
	})
	if err != nil || textResult.Status != AgentToolSucceeded || len(textResult.Images) != 0 || !strings.Contains(textResult.Output, string(textBytes)) {
		t.Fatalf("text attachment read = %#v, %v", textResult, err)
	}
	officeID := "pa_4123456789abcdef0123456789abcdef"
	officeBytes := cloudDSHOfficeZipFixture(t, map[string]string{
		"word/document.xml": `<w:document><w:body><w:p><w:r><w:t>Quarterly report</w:t></w:r></w:p></w:body></w:document>`,
	})
	fileStore.metadata[officeID] = personal.AttachmentMetadata{AttachmentID: officeID, ObjectType: personal.ObjectTypeLibraryItem,
		ObjectID: "li_office", MediaType: cloudDSHOfficeDOCXMediaType, SizeBytes: int64(len(officeBytes)), SHA256: strings.Repeat("d", 64)}
	fileStore.content[officeID] = officeBytes
	officeBody, err := json.Marshal(map[string]any{"name": "report.docx", "kind": "artifact",
		"mime": cloudDSHOfficeDOCXMediaType, "size_bytes": len(officeBytes), "attachment_id": officeID})
	if err != nil {
		t.Fatal(err)
	}
	personalStore.objects["li_office"] = personal.Object{ObjectType: personal.ObjectTypeLibraryItem,
		ObjectID: "li_office", Revision: 1, Body: officeBody}
	store.messages[0].AttachmentIDs = append(store.messages[0].AttachmentIDs, officeID)
	officeResult, err := broker.Execute(context.Background(), current, ModelToolCall{
		CallID: "read-office", Name: cloudDSHTaskAttachmentReadToolName, Arguments: `{"attachment_id":"` + officeID + `"}`,
	})
	if err != nil || officeResult.Status != AgentToolSucceeded || !strings.Contains(officeResult.Output, "Quarterly report") ||
		strings.Contains(officeResult.Output, "not a zip") {
		t.Fatalf("Office attachment read = %#v, %v", officeResult, err)
	}
	before := len(fileStore.opened)
	_, err = broker.Execute(context.Background(), current, ModelToolCall{
		CallID: "read-unlinked", Name: cloudDSHTaskAttachmentReadToolName, Arguments: `{"attachment_id":"` + unlinkedID + `"}`,
	})
	if domain.CodeOf(err) != domain.CodeForbidden || len(fileStore.opened) != before+4 {
		t.Fatalf("unlinked attachment read error=%v opened=%v; want forbidden without opening the requested unlinked id", err, fileStore.opened)
	}
	if !validCloudDSHToolImages(cloudDSHTaskAttachmentReadToolName, result) ||
		!validCloudDSHToolImages(cloudDSHTaskAttachmentReadToolName, pngResult) ||
		!validCloudDSHTaskAttachmentMediaType("text/plain") || validCloudDSHTaskAttachmentMediaType("application/octet-stream") {
		t.Fatal("attachment tool image or media validation rejected a supported attachment")
	}

	ids := make([]string, cloudDSHTaskAttachmentLimit+1)
	for index := range ids {
		id := fmt.Sprintf("pa_%032x", index+100)
		ids[index] = id
		fileStore.metadata[id] = personal.AttachmentMetadata{AttachmentID: id, ObjectType: personal.ObjectTypeLibraryItem,
			ObjectID: fmt.Sprintf("li_%d", index), MediaType: "text/plain", SizeBytes: 0,
			SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
		fileStore.content[id] = nil
		body, marshalErr := json.Marshal(map[string]any{"name": fmt.Sprintf("file-%d.txt", index), "kind": "media",
			"mime": "text/plain", "size_bytes": 0, "attachment_id": id})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		personalStore.objects[fileStore.metadata[id].ObjectID] = personal.Object{ObjectType: personal.ObjectTypeLibraryItem,
			ObjectID: fileStore.metadata[id].ObjectID, Revision: 1, Body: body}
	}
	store.messages = []conversation.Message{{ID: "m4444444", OwnerID: current.OwnerID, ConversationID: current.ConversationID,
		TaskID: current.ID, Role: conversation.RoleUser, AttachmentIDs: ids}}
	listed, err := broker.currentTaskAttachments(context.Background(), current)
	if err != nil || len(listed) != cloudDSHTaskAttachmentLimit || listed[0].ID != ids[0] || listed[len(listed)-1].ID != ids[cloudDSHTaskAttachmentLimit-1] {
		t.Fatalf("task attachment list = %d files, err=%v; want newest %d", len(listed), err, cloudDSHTaskAttachmentLimit)
	}
}

func TestCloudDSHTaskAttachmentImageValidationRejectsMalformedOrOversizedImages(t *testing.T) {
	var jpegBytes, pngBytes, oversizedPNG bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 1)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 1))); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&oversizedPNG, image.NewRGBA(image.Rect(0, 0, 1281, 1))); err != nil {
		t.Fatal(err)
	}
	if !validCloudDSHTaskAttachmentImage("image/jpeg", jpegBytes.Bytes()) ||
		!validCloudDSHTaskAttachmentImage("image/png", pngBytes.Bytes()) {
		t.Fatal("valid bounded JPEG or PNG was rejected")
	}
	for _, test := range []struct {
		name      string
		mediaType string
		content   []byte
	}{
		{name: "declared type mismatch", mediaType: "image/png", content: jpegBytes.Bytes()},
		{name: "malformed image", mediaType: "image/png", content: []byte("not a PNG")},
		{name: "oversized dimensions", mediaType: "image/png", content: oversizedPNG.Bytes()},
		{name: "unsupported format", mediaType: "image/webp", content: pngBytes.Bytes()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validCloudDSHTaskAttachmentImage(test.mediaType, test.content) {
				t.Fatal("invalid or unsupported image was accepted")
			}
		})
	}
}
