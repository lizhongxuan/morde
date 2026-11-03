package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func TestProjectContentACLViewerReadAndMembershipRevocation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	viewer := createTestUser(t, store, 4, 20_000, 40_000)
	member := createTestUser(t, store, 4, 20_000, 40_000)

	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Content ACL", "Project memory ACL", "content-acl-create-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, viewer.ID, projectValue, project.RoleViewer,
		projectValue.Version, "content-acl-viewer-invite-01", "content-acl-viewer-accept-01"); err != nil {
		t.Fatalf("invite and accept viewer: %v", err)
	}
	current, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, current, project.RoleMember,
		current.Version, "content-acl-member-invite-01", "content-acl-member-accept-01"); err != nil {
		t.Fatalf("invite and accept member: %v", err)
	}

	content := project.Content{
		ProjectID: projectValue.ID,
		Kind:      project.ContentMemory,
		ID:        "m_0123abcd",
		Revision:  1,
		Body:      json.RawMessage(`{"content":"Shared project context"}`),
		UpdatedBy: owner.ID,
	}
	if _, replay, err := store.ApplyProjectContentMutation(ctx, owner.ID, content, 0, false, "content-acl-create-memory-01"); err != nil || replay {
		t.Fatalf("create project memory: replay=%t err=%v", replay, err)
	}

	viewerContent, err := store.GetProjectContent(ctx, viewer.ID, projectValue.ID, content.Kind, content.ID)
	if err != nil || !bytes.Contains(viewerContent.Body, []byte("Shared project context")) {
		t.Fatalf("viewer read shared project memory = %#v, err=%v", viewerContent, err)
	}
	viewerMutation := content
	viewerMutation.Revision = 2
	viewerMutation.Body = json.RawMessage(`{"content":"Unauthorized viewer edit"}`)
	viewerMutation.UpdatedBy = viewer.ID
	if _, _, err := store.ApplyProjectContentMutation(ctx, viewer.ID, viewerMutation, 1, false, "content-acl-viewer-edit-01"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("viewer project memory mutation = %v, want forbidden", err)
	}

	memberContent, err := store.GetProjectContent(ctx, member.ID, projectValue.ID, content.Kind, content.ID)
	if err != nil || !bytes.Contains(memberContent.Body, []byte("Shared project context")) {
		t.Fatalf("active member read shared project memory = %#v, err=%v", memberContent, err)
	}
	current, err = store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, current.Version, "content-acl-remove-member-01"); err != nil {
		t.Fatalf("remove active project member: %v", err)
	}

	if _, err := store.GetProjectContent(ctx, member.ID, projectValue.ID, content.Kind, content.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("removed member project memory read = %v, want not found", err)
	}
	removedMemberMutation := content
	removedMemberMutation.Revision = 2
	removedMemberMutation.Body = json.RawMessage(`{"content":"Unauthorized removed-member edit"}`)
	removedMemberMutation.UpdatedBy = member.ID
	if _, _, err := store.ApplyProjectContentMutation(ctx, member.ID, removedMemberMutation, 1, false, "content-acl-removed-member-edit-01"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("removed member project memory mutation = %v, want not found", err)
	}
}
