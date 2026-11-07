package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func TestProjectMessageProjectsOnlyCommittedAuthorizedAttachments(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	const clientID = "project-projection-message-001"
	scope := f.receive(t, clientID, []string{"project-projection-slot-001"})[0]
	content := []byte("Synthetic project attachment")
	metadata := syntheticProjectAttachmentMetadata(content)
	created, err := f.service.Initialize(ctx, f.member, scope, metadata)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.store.ListProjectConversationMessages(ctx, f.owner.UserID, f.projectID, f.conversationID, app.PageRequest{Limit: 20})
	if err != nil || len(before.Items) != 1 || len(before.Items[0].AttachmentIDs) != 0 {
		t.Fatalf("uncommitted attachment was exposed: %+v err=%v", before, err)
	}
	digest := sha256.Sum256(content)
	if _, err := f.service.PutChunk(ctx, f.member, scope, 0, hex.EncodeToString(digest[:]), bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Commit(ctx, f.member, scope); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{f.owner.UserID, f.member.UserID} {
		page, err := f.store.ListProjectConversationMessages(ctx, actor, f.projectID, f.conversationID, app.PageRequest{Limit: 20})
		if err != nil || len(page.Items) != 1 || len(page.Items[0].AttachmentIDs) != 1 || page.Items[0].AttachmentIDs[0] != created.AttachmentID {
			t.Fatalf("authorized project attachment projection for %s: %+v err=%v", actor, page, err)
		}
	}
	reconciled, found, err := f.store.FindConversationMessageByClientID(ctx, f.member.UserID, f.projectID, f.conversationID, clientID, f.instanceID)
	if err != nil || !found || len(reconciled.AttachmentIDs) != 1 || reconciled.AttachmentIDs[0] != created.AttachmentID {
		t.Fatalf("project outbox reconciliation lost attachment: %+v found=%t err=%v", reconciled, found, err)
	}
	if _, err := f.store.ListProjectConversationMessages(ctx, f.outsider.UserID, f.projectID, f.conversationID, app.PageRequest{Limit: 20}); err == nil || domain.CodeOf(err) == domain.CodeInternal {
		t.Fatalf("project outsider could read message attachments: %v", err)
	}
	current, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, f.store, f.owner.UserID, f.outsider.UserID, current, project.RoleViewer,
		current.Version, "projection-viewer-invite-001", "projection-viewer-accept-001"); err != nil {
		t.Fatal(err)
	}
	page, err := f.store.ListProjectConversationMessages(ctx, f.outsider.UserID, f.projectID, f.conversationID, app.PageRequest{Limit: 20})
	if err != nil || len(page.Items) != 1 || len(page.Items[0].AttachmentIDs) != 1 || page.Items[0].AttachmentIDs[0] != created.AttachmentID {
		t.Fatalf("viewer could not read project message attachments: %+v err=%v", page, err)
	}
}
