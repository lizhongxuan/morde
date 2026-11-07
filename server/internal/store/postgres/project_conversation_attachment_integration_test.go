package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type projectAttachmentFixture struct {
	store          *Store
	service        *app.ProjectConversationAttachmentService
	owner          identity.Principal
	member         identity.Principal
	outsider       identity.Principal
	projectID      string
	conversationID string
	instanceID     string
}

func newProjectAttachmentFixture(t *testing.T) projectAttachmentFixture {
	t.Helper()
	ctx := context.Background()
	store, _ := newTemporaryStore(t)
	owner := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	member := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	outsider := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Synthetic attachments", "Test", "attachment-project-001")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"attachment-invite-001", "attachment-accept-001"); err != nil {
		t.Fatal(err)
	}
	projects, err := app.NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	authorization := projectConversationTestAuthorization{}
	tasks, err := app.NewTaskService(store, authorization, authorization, projects)
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewProjectConversationService(store, projects, tasks)
	if err != nil {
		t.Fatal(err)
	}
	principal := func(userID string) identity.Principal {
		return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(userID)}
	}
	ownerPrincipal := principal(owner.ID)
	shared, _, err := conversations.Create(ctx, ownerPrincipal, projectValue.ID,
		app.CreateProjectConversationCommand{Title: "Synthetic conversation", IdempotencyKey: "attachment-conversation-001"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewProjectConversationAttachmentService(store)
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return projectAttachmentFixture{store: store, service: service, owner: ownerPrincipal, member: principal(member.ID), outsider: principal(outsider.ID),
		projectID: projectValue.ID, conversationID: shared.ID, instanceID: instanceID}
}

func (f projectAttachmentFixture) receive(t *testing.T, clientID string, slots []string) []app.ProjectConversationAttachmentScope {
	t.Helper()
	ctx := context.Background()
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.owner.UserID, ActorID: f.member.UserID,
			ConversationID: f.conversationID, Role: conversation.RoleUser, Content: "Synthetic attachment message"},
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: digest[:],
		Input:         app.ConversationRoutingInput{ProjectID: f.projectID, ClientAttachmentIDs: slots},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	scopes := make([]app.ProjectConversationAttachmentScope, 0, len(slots))
	for _, slot := range slots {
		scopes = append(scopes, app.ProjectConversationAttachmentScope{ProjectID: f.projectID, ConversationID: f.conversationID,
			MessageID: message.ID, ClientAttachmentID: slot, ServiceInstanceID: f.instanceID})
	}
	return scopes
}

func syntheticProjectAttachmentMetadata(content []byte) app.ProjectConversationAttachmentMetadata {
	digest := sha256.Sum256(content)
	return app.ProjectConversationAttachmentMetadata{Name: "synthetic.txt", MediaType: "text/plain", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
}

func TestProjectConversationAttachmentUploadIsolationAndRecovery(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	scopes := f.receive(t, "project-attachment-message-001", []string{"project-attachment-001", "project-attachment-002"})
	content := append(bytes.Repeat([]byte("x"), int(app.ProjectConversationAttachmentChunk)), []byte("synthetic tail")...)
	metadata := syntheticProjectAttachmentMetadata(content)
	const workers = 4
	results := make(chan app.ProjectConversationAttachmentStatus, workers)
	errors := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := f.service.Initialize(ctx, f.member, scopes[0], metadata)
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent initialization: %v", err)
		}
	}
	var attachmentID string
	for result := range results {
		if attachmentID == "" {
			attachmentID = result.AttachmentID
		}
		if result.AttachmentID != attachmentID || result.State != "uploading" || result.TotalChunks != 2 {
			t.Fatalf("concurrent initialization returned inconsistent receipt: %+v", result)
		}
	}
	reference := app.ProjectConversationAttachmentReference{ProjectID: f.projectID, ConversationID: f.conversationID,
		MessageID: scopes[0].MessageID, AttachmentID: attachmentID, ServiceInstanceID: f.instanceID}
	if _, err := f.service.GetByID(ctx, f.owner, reference); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("uncommitted metadata was visible to another member: %v", err)
	}
	changed := metadata
	changed.Name = "different.txt"
	if _, err := f.service.Initialize(ctx, f.member, scopes[0], changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed metadata accepted: %v", err)
	}
	if _, err := f.service.Get(ctx, f.owner, scopes[0]); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other project member accessed uploader status: %v", err)
	}
	wrongSlot := scopes[0]
	wrongSlot.ClientAttachmentID = "not-declared-slot"
	if _, err := f.service.Initialize(ctx, f.member, wrongSlot, metadata); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("undeclared slot accepted: %v", err)
	}
	wrongInstance := scopes[0]
	wrongInstance.ServiceInstanceID = "22222222-2222-4222-8222-222222222222"
	if _, err := f.service.Get(ctx, f.member, wrongInstance); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("wrong service instance accepted: %v", err)
	}
	if _, _, err := f.service.Open(ctx, f.member, scopes[0]); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("uncommitted file was readable: %v", err)
	}
	if _, err := f.service.Commit(ctx, f.member, scopes[0]); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("incomplete file committed: %v", err)
	}
	put := func(scope app.ProjectConversationAttachmentScope, sequence int32, chunk []byte) (bool, error) {
		digest := sha256.Sum256(chunk)
		return f.service.PutChunk(ctx, f.member, scope, sequence, hex.EncodeToString(digest[:]), bytes.NewReader(chunk), int64(len(chunk)))
	}
	if replay, err := put(scopes[0], 1, content[app.ProjectConversationAttachmentChunk:]); err != nil || replay {
		t.Fatalf("store final chunk: replay=%t err=%v", replay, err)
	}
	if replay, err := put(scopes[0], 1, content[app.ProjectConversationAttachmentChunk:]); err != nil || !replay {
		t.Fatalf("replay final chunk: replay=%t err=%v", replay, err)
	}
	if _, err := put(scopes[0], 1, bytes.Repeat([]byte("y"), len(content)-int(app.ProjectConversationAttachmentChunk))); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("different chunk accepted: %v", err)
	}
	status, err := f.service.Get(ctx, f.member, scopes[0])
	if err != nil || len(status.ReceivedSequences) != 1 || status.ReceivedSequences[0] != 1 {
		t.Fatalf("resume state: %+v err=%v", status, err)
	}
	if replay, err := put(scopes[0], 0, content[:app.ProjectConversationAttachmentChunk]); err != nil || replay {
		t.Fatalf("store first chunk: replay=%t err=%v", replay, err)
	}
	committed, err := f.service.Commit(ctx, f.member, scopes[0])
	if err != nil || committed.State != "committed" || committed.AttachmentID != attachmentID || len(committed.ReceivedSequences) != 2 {
		t.Fatalf("commit attachment: %+v err=%v", committed, err)
	}
	if replay, err := f.service.Commit(ctx, f.member, scopes[0]); err != nil || !replay.Replay || replay.AttachmentID != attachmentID {
		t.Fatalf("commit replay: %+v err=%v", replay, err)
	}
	reader, info, err := f.service.Open(ctx, f.owner, scopes[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || info.AttachmentID != attachmentID || !bytes.Equal(got, content) {
		t.Fatalf("project member read failed: size=%d err=%v", len(got), err)
	}
	if _, _, err := f.service.Open(ctx, f.outsider, scopes[0]); err == nil {
		t.Fatal("nonmember read a project attachment")
	}
	metadataByID, err := f.service.GetByID(ctx, f.owner, reference)
	if err != nil || metadataByID.AttachmentID != attachmentID || metadataByID.Name != metadata.Name {
		t.Fatalf("member could not resolve attachment by ID: %+v err=%v", metadataByID, err)
	}
	readerByID, _, err := f.service.OpenByID(ctx, f.owner, reference)
	if err != nil {
		t.Fatal(err)
	}
	gotByID, err := io.ReadAll(readerByID)
	_ = readerByID.Close()
	if err != nil || !bytes.Equal(gotByID, content) {
		t.Fatalf("member read by ID returned different bytes: size=%d err=%v", len(gotByID), err)
	}
	if _, err := f.service.GetByID(ctx, f.outsider, reference); err == nil {
		t.Fatal("nonmember resolved project attachment metadata")
	}
	wrongMessage := reference
	wrongMessage.MessageID = mustID(t, domain.NewMessageID)
	if _, err := f.service.GetByID(ctx, f.owner, wrongMessage); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("attachment was resolved under a different message: %v", err)
	}
	wrongConversation := reference
	wrongConversation.ConversationID = mustID(t, domain.NewConversationID)
	if _, err := f.service.GetByID(ctx, f.owner, wrongConversation); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("attachment was resolved under a different conversation: %v", err)
	}
	wrongProject := reference
	wrongProject.ProjectID = "j1234567"
	if _, err := f.service.GetByID(ctx, f.owner, wrongProject); err == nil {
		t.Fatal("attachment was resolved under a different project")
	}
	wrongReferenceInstance := reference
	wrongReferenceInstance.ServiceInstanceID = "22222222-2222-4222-8222-222222222222"
	if _, err := f.service.GetByID(ctx, f.owner, wrongReferenceInstance); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("attachment was resolved under a different service instance: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE project_members SET role='viewer' WHERE project_id=$1 AND user_id=$2`, f.projectID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	viewerReader, _, err := f.service.Open(ctx, f.member, scopes[0])
	if err != nil {
		t.Fatalf("project viewer cannot read a shared committed attachment: %v", err)
	}
	_ = viewerReader.Close()
	viewerReader, _, err = f.service.OpenByID(ctx, f.member, reference)
	if err != nil {
		t.Fatalf("project viewer cannot read committed attachment by ID: %v", err)
	}
	var firstByte [1]byte
	if _, err := viewerReader.Read(firstByte[:]); err != nil {
		t.Fatal(err)
	}
	if err := viewerReader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Get(ctx, f.member, scopes[0]); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("project viewer could modify the uploader's slot: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE project_members SET role='member' WHERE project_id=$1 AND user_id=$2`, f.projectID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	secondContent := []byte("restart")
	secondMetadata := syntheticProjectAttachmentMetadata(secondContent)
	second, err := f.service.Initialize(ctx, f.member, scopes[1], secondMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put(scopes[1], 0, secondContent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE project_conversation_attachments SET expires_at=clock_timestamp()-interval '1 second'
WHERE message_id=$1 AND client_attachment_id=$2`, scopes[1].MessageID, scopes[1].ClientAttachmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := put(scopes[1], 0, secondContent); domain.CodeOf(err) != domain.CodeTransferExpired {
		t.Fatalf("expired chunk was reported as accepted: %v", err)
	}
	resumed, err := f.service.Initialize(ctx, f.member, scopes[1], secondMetadata)
	if err != nil || resumed.AttachmentID != second.AttachmentID || resumed.State != "uploading" || len(resumed.ReceivedSequences) != 0 {
		t.Fatalf("expired upload restart: %+v err=%v", resumed, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.member.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Get(ctx, f.member, scopes[0]); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked token accessed project attachment: %v", err)
	}
	if _, err := f.service.GetByID(ctx, f.member, reference); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked token resolved project attachment by ID: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, f.member.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, f.projectID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Get(ctx, f.member, scopes[0]); err == nil {
		t.Fatal("removed member accessed an upload")
	}
}

func TestProjectConversationAttachmentQuotaIncludesReservedUploads(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	var scopes []app.ProjectConversationAttachmentScope
	for page, count := range []int{16, 2} {
		slots := make([]string, count)
		for i := range slots {
			slots[i] = fmt.Sprintf("quota-slot-%02d-%02d", page, i)
		}
		scopes = append(scopes, f.receive(t, fmt.Sprintf("quota-message-%02d", page), slots)...)
	}
	metadata := app.ProjectConversationAttachmentMetadata{Name: "synthetic.bin", MediaType: "application/octet-stream",
		SizeBytes: app.ProjectConversationAttachmentMaxBytes, SHA256: string(bytes.Repeat([]byte("a"), 64))}
	for index, scope := range scopes {
		_, err := f.service.Initialize(ctx, f.member, scope, metadata)
		if index < 17 && err != nil || index == 17 && domain.CodeOf(err) != domain.CodeQuotaExceeded {
			t.Fatalf("quota initialization %d: %v", index, err)
		}
	}
}
