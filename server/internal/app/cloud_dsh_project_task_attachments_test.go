package app

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type projectTaskAttachmentFixture struct {
	*cloudDSHTaskAttachmentStore
	metadata ProjectConversationAttachmentMetadata
	content  []byte
	opened   bool
	revoked  bool
}

func (s *projectTaskAttachmentFixture) ListProjectTaskAttachments(_ context.Context, current task.Task) ([]ProjectConversationAttachmentMetadata, error) {
	if s.revoked || current.ID != s.details.Task.ID || current.InitiatorID != s.details.Task.InitiatorID {
		return nil, domain.NewError(domain.CodeForbidden, "project membership was revoked")
	}
	return []ProjectConversationAttachmentMetadata{s.metadata}, nil
}

func (s *projectTaskAttachmentFixture) OpenProjectTaskAttachment(_ context.Context, current task.Task, id string) (io.ReadCloser, ProjectConversationAttachmentMetadata, error) {
	if s.revoked || current.ID != s.details.Task.ID || id != s.metadata.AttachmentID {
		return nil, ProjectConversationAttachmentMetadata{}, domain.NewError(domain.CodeForbidden, "project attachment is not authorized")
	}
	s.opened = true
	return &projectTaskAttachmentFixtureReader{Reader: bytes.NewReader(s.content), fixture: s}, s.metadata, nil
}

func (s *projectTaskAttachmentFixture) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	if s.opened {
		return task.Event{}, domain.NewError(domain.CodeConflict, "attachment transaction is still open")
	}
	return s.cloudDSHTaskAttachmentStore.CommitTaskProgress(ctx, progress)
}

type projectTaskAttachmentFixtureReader struct {
	*bytes.Reader
	fixture *projectTaskAttachmentFixture
}

func (r *projectTaskAttachmentFixtureReader) Close() error {
	r.fixture.opened = false
	return nil
}

func TestCloudDSHProjectAttachmentReaderUsesOnlyAuthorizedTaskFiles(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC()
	current := task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u7654321", ConversationID: "c1234567", ProjectID: "j1234567",
		ExecutionLocation: "server", Status: task.StatusRunning, AgentSessionID: "session-1",
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1, ExpiresAt: &expires},
		ConversationContext: &task.ConversationContext{OwnerID: "u1234567", InitiatorID: "u7654321",
			ConversationID: "c1234567", ProjectID: "j1234567", Scope: "project"},
		Budget: task.Budget{ToolCalls: 10}, Lease: task.Lease{Owner: "worker-1", Version: 1, ExpiresAt: &expires}}
	content := []byte("Synthetic project note")
	id := "pca_0123456789abcdef0123456789abcdef"
	store := &projectTaskAttachmentFixture{cloudDSHTaskAttachmentStore: &cloudDSHTaskAttachmentStore{details: TaskDetails{Task: current}},
		metadata: ProjectConversationAttachmentMetadata{AttachmentID: id, Name: "note.txt", MediaType: "text/plain",
			SizeBytes: int64(len(content)), SHA256: strings.Repeat("a", 64)}, content: content}
	broker := &CloudDSHToolBroker{store: store}
	attachments, err := broker.currentTaskAttachments(context.Background(), current)
	if err != nil || len(attachments) != 1 || attachments[0].ID != id {
		t.Fatalf("project task attachments = %+v err=%v", attachments, err)
	}
	result, err := broker.executeTaskAttachmentRead(context.Background(), current, ModelToolCall{
		CallID: "project-read", Name: cloudDSHTaskAttachmentReadToolName, Arguments: `{"attachment_id":"` + id + `"}`}, false, nil)
	if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, string(content)) || store.opened {
		t.Fatalf("project attachment read = %+v opened=%t err=%v", result, store.opened, err)
	}
	_, err = broker.executeTaskAttachmentRead(context.Background(), current, ModelToolCall{
		CallID: "unlinked-read", Name: cloudDSHTaskAttachmentReadToolName,
		Arguments: `{"attachment_id":"pca_1123456789abcdef0123456789abcdef"}`}, false, nil)
	if domain.CodeOf(err) != domain.CodeForbidden || store.opened {
		t.Fatalf("unlinked project attachment was accepted: %v", err)
	}
	_, err = broker.executeTaskAttachmentRead(context.Background(), current, ModelToolCall{
		CallID: "personal-read", Name: cloudDSHTaskAttachmentReadToolName,
		Arguments: `{"attachment_id":"pa_0123456789abcdef0123456789abcdef"}`}, false, nil)
	if domain.CodeOf(err) != domain.CodeForbidden || store.opened {
		t.Fatalf("personal attachment was accepted in project scope: %v", err)
	}
	store.revoked = true
	if _, err := broker.currentTaskAttachments(context.Background(), current); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("revoked project membership was ignored: %v", err)
	}
}
