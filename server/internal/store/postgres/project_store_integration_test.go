package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/project"
)

func TestProjectStoreInvitationMembershipAndOwnerTransfer(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	invitee := createTestUser(t, store, 4, 20_000, 40_000)
	other := createTestUser(t, store, 4, 20_000, 40_000)

	created := createProjectStoreFixture(t, ctx, store, owner.ID, "Project store", "Test project APIs", "project-create-01")
	replayedCreate := created
	replayedCreate.ID = "j0000000"
	replayed, wasReplay, err := store.CreateProject(ctx, owner.ID, replayedCreate, "project-create-01")
	if err != nil || !wasReplay || replayed.ID != created.ID {
		t.Fatalf("CreateProject replay = %#v, replay=%t, err=%v", replayed, wasReplay, err)
	}
	conflictingCreate := replayedCreate
	conflictingCreate.Name = "Different project"
	if _, _, err := store.CreateProject(ctx, owner.ID, conflictingCreate, "project-create-01"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("CreateProject with changed payload = %v, want idempotency conflict", err)
	}

	invitations, wasReplay, err := store.CreateProjectInvitation(ctx, owner.ID, project.Invitation{
		ID: "r1234567", ProjectID: created.ID, InviterID: owner.ID, InviteeID: invitee.ID,
		Role: project.RoleMember, State: project.InvitationPending, Version: 1,
	}, 1, "project-invite-01")
	if err != nil || wasReplay || invitations.ID != "r1234567" {
		t.Fatalf("CreateProjectInvitation = %#v, replay=%t, err=%v", invitations, wasReplay, err)
	}
	if _, err := store.GetProject(ctx, invitee.ID, created.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("pending invitee read project = %v, want not found", err)
	}
	if _, _, err := store.AcceptProjectInvitation(ctx, invitations.ID, other.ID, "wrong-account-accept-01"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("wrong-account invitation acceptance = %v, want not found", err)
	}
	invitationReplay, wasReplay, err := store.CreateProjectInvitation(ctx, owner.ID, project.Invitation{
		ID: "r7654321", ProjectID: created.ID, InviterID: owner.ID, InviteeID: invitee.ID,
		Role: project.RoleMember, State: project.InvitationPending, Version: 1,
	}, 1, "project-invite-01")
	if err != nil || !wasReplay || invitationReplay.ID != invitations.ID {
		t.Fatalf("CreateProjectInvitation replay = %#v, replay=%t, err=%v", invitationReplay, wasReplay, err)
	}
	inviteeInbox, err := store.ListProjectInvitations(ctx, invitee.ID, app.PageRequest{Limit: 20})
	if err != nil || len(inviteeInbox.Items) != 1 || inviteeInbox.Items[0].ID != invitations.ID {
		t.Fatalf("invitee invitation inbox = %#v, err=%v", inviteeInbox, err)
	}
	ownerInbox, err := store.ListProjectInvitations(ctx, owner.ID, app.PageRequest{Limit: 20})
	if err != nil || len(ownerInbox.Items) != 0 {
		t.Fatalf("inviter invitation inbox = %#v, err=%v", ownerInbox, err)
	}

	member, wasReplay, err := store.AcceptProjectInvitation(ctx, invitations.ID, invitee.ID, "project-accept-01")
	if err != nil || wasReplay || member.Role != project.RoleMember || member.Version != 1 {
		t.Fatalf("AcceptProjectInvitation = %#v, replay=%t, err=%v", member, wasReplay, err)
	}
	memberReplay, wasReplay, err := store.AcceptProjectInvitation(ctx, invitations.ID, invitee.ID, "project-accept-01")
	if err != nil || !wasReplay || memberReplay != member {
		t.Fatalf("AcceptProjectInvitation replay = %#v, replay=%t, err=%v", memberReplay, wasReplay, err)
	}
	if current, err := store.GetProject(ctx, owner.ID, created.ID); err != nil || current.Version != 3 {
		t.Fatalf("project after invitation acceptance = %#v, err=%v; want version 3", current, err)
	}

	viewer, err := store.UpdateProjectMemberRole(ctx, owner.ID, created.ID, invitee.ID, project.RoleViewer, 3, "project-role-01")
	if err != nil || viewer.Role != project.RoleViewer || viewer.Version != 2 {
		t.Fatalf("UpdateProjectMemberRole to viewer = %#v, err=%v", viewer, err)
	}
	if _, err := store.UpdateProjectMemberRole(ctx, owner.ID, created.ID, invitee.ID, project.RoleMember, 3, "project-role-stale"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale role update = %v, want conflict", err)
	}
	member, err = store.UpdateProjectMemberRole(ctx, owner.ID, created.ID, invitee.ID, project.RoleMember, 4, "project-role-02")
	if err != nil || member.Role != project.RoleMember || member.Version != 3 {
		t.Fatalf("UpdateProjectMemberRole to member = %#v, err=%v", member, err)
	}

	transferred, err := store.TransferProjectOwner(ctx, owner.ID, created.ID, invitee.ID, 5, "project-transfer-01")
	if err != nil || transferred.OwnerID != invitee.ID || transferred.Version != 6 {
		t.Fatalf("TransferProjectOwner = %#v, err=%v", transferred, err)
	}
	replayedTransfer, err := store.TransferProjectOwner(ctx, owner.ID, created.ID, invitee.ID, 5, "project-transfer-01")
	if err != nil || replayedTransfer.OwnerID != transferred.OwnerID || replayedTransfer.Version != transferred.Version {
		t.Fatalf("TransferProjectOwner replay after demotion = %#v, err=%v", replayedTransfer, err)
	}
	if _, err := store.TransferProjectOwner(ctx, owner.ID, created.ID, other.ID, 5, "project-transfer-01"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("TransferProjectOwner changed-payload key reuse = %v, want idempotency conflict", err)
	}
	if _, err := store.TransferProjectOwner(ctx, owner.ID, created.ID, other.ID, 6, "project-transfer-new"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("former owner new transfer = %v, want forbidden", err)
	}
	oldOwner, err := store.GetProjectMember(ctx, created.ID, owner.ID)
	if err != nil || oldOwner.Role != project.RoleMember || oldOwner.Version != 2 {
		t.Fatalf("former project owner membership = %#v, err=%v", oldOwner, err)
	}
	newOwner, err := store.GetProjectMember(ctx, created.ID, invitee.ID)
	if err != nil || newOwner.Role != project.RoleOwner || newOwner.Version != 4 {
		t.Fatalf("new project owner membership = %#v, err=%v", newOwner, err)
	}

	if err := store.RemoveProjectMember(ctx, invitee.ID, created.ID, owner.ID, 6, "project-remove-01"); err != nil {
		t.Fatalf("RemoveProjectMember = %v", err)
	}
	if err := store.RemoveProjectMember(ctx, invitee.ID, created.ID, owner.ID, 6, "project-remove-01"); err != nil {
		t.Fatalf("RemoveProjectMember replay = %v", err)
	}
	if _, err := store.TransferProjectOwner(ctx, owner.ID, created.ID, invitee.ID, 5, "project-transfer-01"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("former owner replay after membership removal = %v, want not found", err)
	}
	if _, err := store.GetProjectMember(ctx, created.ID, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("removed project member lookup = %v, want not found", err)
	}

	pending, wasReplay, err := store.CreateProjectInvitation(ctx, invitee.ID, project.Invitation{
		ID: "r8765432", ProjectID: created.ID, InviterID: invitee.ID, InviteeID: other.ID,
		Role: project.RoleViewer, State: project.InvitationPending, Version: 1,
	}, 7, "project-invite-02")
	if err != nil || wasReplay {
		t.Fatalf("second invitation = %#v, replay=%t, err=%v", pending, wasReplay, err)
	}
	if err := store.RevokeProjectInvitation(ctx, invitee.ID, created.ID, pending.ID, 8, "project-revoke-01"); err != nil {
		t.Fatalf("RevokeProjectInvitation = %v", err)
	}
	if err := store.RevokeProjectInvitation(ctx, invitee.ID, created.ID, pending.ID, 8, "project-revoke-01"); err != nil {
		t.Fatalf("RevokeProjectInvitation replay = %v", err)
	}
	otherInbox, err := store.ListProjectInvitations(ctx, other.ID, app.PageRequest{Limit: 20})
	if err != nil || len(otherInbox.Items) != 0 {
		t.Fatalf("revoked invitation inbox = %#v, err=%v", otherInbox, err)
	}
	if _, _, err := store.AcceptProjectInvitation(ctx, pending.ID, other.ID, "project-accept-02"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("accept revoked invitation = %v, want conflict", err)
	}
}

func TestProjectStoreConcurrentOwnerTransfersKeepSingleOwner(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	first := createTestUser(t, store, 4, 20_000, 40_000)
	second := createTestUser(t, store, 4, 20_000, 40_000)
	value := createProjectStoreFixture(t, ctx, store, owner.ID, "Concurrent transfer", "Single owner", "transfer-project-01")
	for index, userID := range []string{first.ID, second.ID} {
		current, err := store.GetProject(ctx, owner.ID, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, userID, current, project.RoleMember,
			current.Version, fmt.Sprintf("transfer-invite-%02d", index), fmt.Sprintf("transfer-accept-%02d", index)); err != nil {
			t.Fatal(err)
		}
	}
	current, err := store.GetProject(ctx, owner.ID, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	type transferResult struct {
		ownerID string
		err     error
	}
	start := make(chan struct{})
	results := make(chan transferResult, 2)
	for index, targetID := range []string{first.ID, second.ID} {
		go func(index int, targetID string) {
			<-start
			_, err := store.TransferProjectOwner(ctx, owner.ID, value.ID, targetID, current.Version,
				fmt.Sprintf("transfer-owner-%02d", index))
			results <- transferResult{ownerID: targetID, err: err}
		}(index, targetID)
	}
	close(start)
	var winners []string
	for index := 0; index < 2; index++ {
		result := <-results
		if result.err == nil {
			winners = append(winners, result.ownerID)
		} else if code := domain.CodeOf(result.err); code != domain.CodeForbidden && code != domain.CodeConflict {
			t.Fatalf("losing owner transfer error = %v, want permission or version conflict", result.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("concurrent owner transfer winners = %v, want exactly one", winners)
	}
	var ownerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_members WHERE project_id=$1 AND role='owner'`, value.ID).Scan(&ownerCount); err != nil {
		t.Fatal(err)
	}
	if ownerCount != 1 {
		t.Fatalf("project owner membership count = %d, want exactly one", ownerCount)
	}
	final, err := store.GetProject(ctx, winners[0], value.ID)
	if err != nil || final.OwnerID != winners[0] {
		t.Fatalf("project after concurrent transfers = %#v, err=%v; winner=%s", final, err, winners[0])
	}
}

func TestProjectStoreResourceACLAndContentRevisions(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 30_000, 60_000)
	reader := createTestUser(t, store, 8, 30_000, 60_000)
	hostOwner := createTestUser(t, store, 8, 30_000, 60_000)
	workspaceOnlyOwner := createTestUser(t, store, 8, 30_000, 60_000)
	value := createProjectStoreFixture(t, ctx, store, owner.ID, "Resources and memory", "ACL and revisions", "project-create-02")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, reader.ID, value, project.RoleMember, 1, "project-invite-03", "project-accept-03"); err != nil {
		t.Fatal(err)
	}

	sharedHost := createRunningControlHost(t, ctx, store, hostOwner.ID, host.ControlAI, 0)
	if len(sharedHost.Workspaces) == 0 {
		t.Fatal("test Host was created without its default Workspace binding")
	}
	workspaceID := sharedHost.Workspaces[0].Workspace.ID
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET state='active' WHERE host_id=$1 AND workspace_id=$2`, sharedHost.ID, workspaceID); err != nil {
		t.Fatalf("activate test Workspace binding: %v", err)
	}
	if _, err := store.UpsertHostMember(ctx, host.Member{
		HostID: sharedHost.ID, OwnerID: hostOwner.ID, UserID: owner.ID,
		Permissions: []host.Permission{host.PermissionDesktopView, host.PermissionWorkspaceFileRead},
	}); err != nil {
		t.Fatal(err)
	}
	workspaceOnlyHost := createRunningControlHost(t, ctx, store, workspaceOnlyOwner.ID, host.ControlAI, 0)
	if _, err := store.UpsertHostMember(ctx, host.Member{
		HostID: workspaceOnlyHost.ID, OwnerID: workspaceOnlyOwner.ID, UserID: owner.ID,
		Permissions: []host.Permission{host.PermissionWorkspaceFileRead},
	}); err != nil {
		t.Fatal(err)
	}

	hostResource, replay, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: value.ID, Kind: project.ResourceHost, ResourceID: sharedHost.ID, Version: 1,
	}, 3, "project-resource-host-01")
	if err != nil || replay || hostResource.ResourceID != sharedHost.ID || hostResource.Version != 1 {
		t.Fatalf("AddProjectResource Host = %#v, replay=%t, err=%v", hostResource, replay, err)
	}
	workspaceResource, replay, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: value.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceID, Version: 1,
	}, 4, "project-resource-workspace-01")
	if err != nil || replay || workspaceResource.ResourceID != workspaceID {
		t.Fatalf("AddProjectResource Workspace = %#v, replay=%t, err=%v", workspaceResource, replay, err)
	}
	if _, _, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: value.ID, Kind: project.ResourceHost, ResourceID: workspaceOnlyHost.ID, Version: 1,
	}, 5, "project-resource-host-denied"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Workspace-only permission added Host resource = %v, want not found", err)
	}

	ownerResources, err := store.ListProjectResources(ctx, owner.ID, value.ID, app.PageRequest{Limit: 20})
	if err != nil || len(ownerResources.Items) != 2 {
		t.Fatalf("owner project resources = %#v, err=%v", ownerResources, err)
	}
	readerResources, err := store.ListProjectResources(ctx, reader.ID, value.ID, app.PageRequest{Limit: 20})
	if err != nil || len(readerResources.Items) != 0 {
		t.Fatalf("reader project resources without independent ACL = %#v, err=%v", readerResources, err)
	}
	if err := store.RevokeHostMember(ctx, sharedHost.ID, hostOwner.ID, owner.ID); err != nil {
		t.Fatalf("revoke independent resource ACL: %v", err)
	}
	ownerResources, err = store.ListProjectResources(ctx, owner.ID, value.ID, app.PageRequest{Limit: 20})
	if err != nil || len(ownerResources.Items) != 0 {
		t.Fatalf("resources after ACL revoke = %#v, err=%v", ownerResources, err)
	}

	content := project.Content{
		ProjectID: value.ID, Kind: project.ContentMemory, ID: "m_0123abcd", Revision: 1,
		Body: json.RawMessage(`{"content":"Shared project context"}`), UpdatedBy: owner.ID,
	}
	createdContent, replay, err := store.ApplyProjectContentMutation(ctx, owner.ID, content, 0, false, "project-content-01")
	if err != nil || replay || createdContent.Revision != 1 {
		t.Fatalf("create project memory = %#v, replay=%t, err=%v", createdContent, replay, err)
	}
	replayedContent, replay, err := store.ApplyProjectContentMutation(ctx, owner.ID, content, 0, false, "project-content-01")
	if err != nil || !replay || replayedContent.Revision != 1 || !bytes.Contains(replayedContent.Body, []byte("Shared project context")) {
		t.Fatalf("project memory replay = %#v, replay=%t, err=%v", replayedContent, replay, err)
	}
	readContent, err := store.GetProjectContent(ctx, reader.ID, value.ID, project.ContentMemory, content.ID)
	if err != nil || !bytes.Contains(readContent.Body, []byte("Shared project context")) {
		t.Fatalf("project reader content = %#v, err=%v", readContent, err)
	}
	stale := content
	stale.Body = json.RawMessage(`{"content":"stale write"}`)
	stale.Revision = 1
	if _, _, err := store.ApplyProjectContentMutation(ctx, owner.ID, stale, 0, false, "project-content-stale"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale project content revision = %v, want conflict", err)
	}

	deleted := content
	deleted.Revision = 2
	deleted.Body = nil
	deleted.UpdatedBy = owner.ID
	result, replay, err := store.ApplyProjectContentMutation(ctx, owner.ID, deleted, 1, true, "project-content-delete-01")
	if err != nil || replay || result.Revision != 2 || len(result.Body) != 0 {
		t.Fatalf("delete project memory = %#v, replay=%t, err=%v", result, replay, err)
	}
	result, replay, err = store.ApplyProjectContentMutation(ctx, owner.ID, deleted, 1, true, "project-content-delete-01")
	if err != nil || !replay || result.Revision != 2 || len(result.Body) != 0 {
		t.Fatalf("delete project memory replay = %#v, replay=%t, err=%v", result, replay, err)
	}
	if _, err := store.GetProjectContent(ctx, reader.ID, value.ID, content.Kind, content.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("read deleted project content = %v, want not found", err)
	}
}

func TestProjectMuseContentSearchAndRevisionPinnedCommentAuthorization(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	member := createTestUser(t, store, 4, 20_000, 40_000)
	hiddenOwner := createTestUser(t, store, 4, 20_000, 40_000)
	visibleProject := createProjectStoreFixture(t, ctx, store, owner.ID, "Visible Muse project", "Project-scoped Muse content", "muse-visible-project-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, visibleProject, project.RoleMember, 1,
		"muse-visible-invite-01", "muse-visible-accept-01"); err != nil {
		t.Fatal(err)
	}
	hiddenProject := createProjectStoreFixture(t, ctx, store, hiddenOwner.ID, "Hidden Muse project", "Must not leak", "muse-hidden-project-01")

	for _, content := range []project.Content{
		{ProjectID: visibleProject.ID, Kind: project.ContentFeedPost, ID: "feed-1", Revision: 1, Body: []byte(`{"title":"needle release","type":"note","created_at":"2026-02-01T00:00:00Z","body":"Visible feed body"}`), UpdatedBy: owner.ID},
		{ProjectID: visibleProject.ID, Kind: project.ContentIdea, ID: "idea-1", Revision: 1, Body: []byte(`{"title":"needle idea","content":"Visible idea body","tags":["planning"]}`), UpdatedBy: owner.ID},
		{ProjectID: visibleProject.ID, Kind: project.ContentLibrary, ID: "library-1", Revision: 1, Body: []byte(`{"name":"needle checklist.pdf","kind":"artifact","mime":"application/pdf","size_bytes":88}`), UpdatedBy: owner.ID},
		{ProjectID: visibleProject.ID, Kind: project.ContentBrief, ID: "brief-1", Revision: 1, Body: []byte(`{"title":"needle brief","content":"Visible project brief","tags":["weekly"]}`), UpdatedBy: owner.ID},
		{ProjectID: hiddenProject.ID, Kind: project.ContentBrief, ID: "private-brief", Revision: 1, Body: []byte(`{"title":"HIDDEN_PRIVATE_SEARCH_CANARY_9381","content":"needle hidden excerpt"}`), UpdatedBy: hiddenOwner.ID},
	} {
		if result, replay, err := store.ApplyProjectContentMutation(ctx, content.UpdatedBy, content, 0, false,
			"muse-content-"+content.ID+"-01"); err != nil || replay {
			t.Fatalf("create project Muse content %s/%s result=%+v replay=%t err=%v", content.Kind, content.ID, result, replay, err)
		}
		if result, replay, err := store.ApplyProjectContentMutation(ctx, content.UpdatedBy, content, 0, false,
			"muse-content-"+content.ID+"-01"); err != nil || !replay || result.Kind != content.Kind || result.Revision != 1 {
			t.Fatalf("replay project Muse content %s/%s result=%+v replay=%t err=%v", content.Kind, content.ID, result, replay, err)
		}
	}
	results, err := store.SearchProjectContent(ctx, member.ID, "needle", project.MaxContentSearchResults)
	if err != nil || len(results) != 4 {
		t.Fatalf("search readable project content = %#v, err=%v; want four results", results, err)
	}
	for _, result := range results {
		if result.ProjectID != visibleProject.ID || result.Revision != 1 {
			t.Fatalf("search returned out-of-scope or non-current content: %+v", result)
		}
	}
	privateResults, err := store.SearchProjectContent(ctx, member.ID, "HIDDEN_PRIVATE_SEARCH_CANARY_9381", project.MaxContentSearchResults)
	if err != nil || len(privateResults) != 0 {
		t.Fatalf("search leaked inaccessible project excerpt: %#v, err=%v", privateResults, err)
	}

	comment := project.ContentComment{
		ProjectID: visibleProject.ID, Kind: project.ContentBrief, ContentID: "brief-1", ContentRevision: 1,
		RowID: "acceptance-row", ID: "c1234567", Revision: 1, Body: "Confirm the acceptance evidence.", UpdatedBy: member.ID,
	}
	created, replay, err := store.ApplyProjectContentCommentMutation(ctx, member.ID, comment, 0, false, "muse-comment-create-01")
	if err != nil || replay || created.Revision != 1 || created.ContentRevision != 1 {
		t.Fatalf("create pinned comment = %+v replay=%t err=%v", created, replay, err)
	}
	if _, _, err := store.ApplyProjectContentCommentMutation(ctx, member.ID, comment, 0, false, "muse-comment-stale-01"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale comment base version error = %v, want conflict", err)
	}
	updated := created
	updated.Revision = 2
	updated.Body = "Updated acceptance annotation."
	updated.UpdatedBy = owner.ID
	updated, replay, err = store.ApplyProjectContentCommentMutation(ctx, owner.ID, updated, 1, false, "muse-comment-update-01")
	if err != nil || replay || updated.Revision != 2 || updated.ContentRevision != 1 || updated.RowID != "acceptance-row" {
		t.Fatalf("update pinned comment = %+v replay=%t err=%v", updated, replay, err)
	}
	newBriefRevision := project.Content{
		ProjectID: visibleProject.ID, Kind: project.ContentBrief, ID: "brief-1", Revision: 2,
		Body: []byte(`{"title":"needle brief v2","content":"New project brief revision","tags":["weekly"]}`), UpdatedBy: owner.ID,
	}
	if _, replay, err := store.ApplyProjectContentMutation(ctx, owner.ID, newBriefRevision, 1, false, "muse-brief-revision-02"); err != nil || replay {
		t.Fatalf("create next brief revision replay=%t err=%v", replay, err)
	}
	comments, err := store.ListProjectContentComments(ctx, owner.ID, visibleProject.ID, project.ContentBrief, "brief-1", 1, "acceptance-row", project.MaxContentCommentsPerRow)
	if err != nil || len(comments) != 1 || comments[0].Revision != 2 || comments[0].ContentRevision != 1 || comments[0].Body != "Updated acceptance annotation." {
		t.Fatalf("list pinned comments = %#v err=%v", comments, err)
	}
	if comments, err := store.ListProjectContentComments(ctx, owner.ID, visibleProject.ID, project.ContentBrief, "brief-1", 2, "acceptance-row", project.MaxContentCommentsPerRow); err != nil || len(comments) != 0 {
		t.Fatalf("comment lookup at a different content revision = %#v, %v; want empty", comments, err)
	}

	current, err := store.GetProject(ctx, owner.ID, visibleProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, visibleProject.ID, member.ID, current.Version, "muse-member-revoke-01"); err != nil {
		t.Fatal(err)
	}
	if results, err := store.SearchProjectContent(ctx, member.ID, "needle", project.MaxContentSearchResults); err != nil || len(results) != 0 {
		t.Fatalf("search after membership revocation = %#v, err=%v", results, err)
	}
	if _, err := store.ListProjectContentComments(ctx, member.ID, visibleProject.ID, project.ContentBrief, "brief-1", 1, "acceptance-row", project.MaxContentCommentsPerRow); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("comment read after membership revocation = %v, want not found", err)
	}
	comment.Revision = 2
	comment.Body = "Revoked member must not update."
	comment.UpdatedBy = member.ID
	if _, _, err := store.ApplyProjectContentCommentMutation(ctx, member.ID, comment, 1, false, "muse-comment-after-revoke"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("comment write after membership revocation = %v, want not found", err)
	}
}

func TestProjectResourcesKeepIndependentlySharedHostsSeparate(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	projectOwner := createTestUser(t, store, 4, 20_000, 40_000)
	firstHostOwner := createTestUser(t, store, 4, 20_000, 40_000)
	secondHostOwner := createTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, projectOwner.ID, "Two computers", "Independent Host access", "project-two-hosts-01")
	firstHost := createRunningControlHost(t, ctx, store, firstHostOwner.ID, host.ControlAI, 0)
	secondHost := createRunningControlHost(t, ctx, store, secondHostOwner.ID, host.ControlAI, 0)

	for _, grant := range []host.Member{
		{HostID: firstHost.ID, OwnerID: firstHostOwner.ID, UserID: projectOwner.ID, Permissions: []host.Permission{host.PermissionDesktopView}},
		{HostID: secondHost.ID, OwnerID: secondHostOwner.ID, UserID: projectOwner.ID, Permissions: []host.Permission{host.PermissionTerminalControl}},
	} {
		if _, err := store.UpsertHostMember(ctx, grant); err != nil {
			t.Fatalf("grant project owner access to Host %s: %v", grant.HostID, err)
		}
	}

	for i, hostID := range []string{firstHost.ID, secondHost.ID} {
		resource, replay, err := store.AddProjectResource(ctx, projectOwner.ID, project.Resource{
			ProjectID: projectValue.ID, Kind: project.ResourceHost, ResourceID: hostID, Version: 1,
		}, int64(i+1), fmt.Sprintf("project-two-host-resource-%02d", i+1))
		if err != nil || replay || resource.ResourceID != hostID {
			t.Fatalf("add Host %s as project resource = %#v, replay=%t, err=%v", hostID, resource, replay, err)
		}
	}

	resources, err := store.ListProjectResources(ctx, projectOwner.ID, projectValue.ID, app.PageRequest{Limit: 20})
	if err != nil || len(resources.Items) != 2 || resources.Items[0].Kind != project.ResourceHost || resources.Items[1].Kind != project.ResourceHost {
		t.Fatalf("project Host resources = %#v, err=%v; want two distinct Hosts", resources, err)
	}
	got := map[string]bool{resources.Items[0].ResourceID: true, resources.Items[1].ResourceID: true}
	if !got[firstHost.ID] || !got[secondHost.ID] || firstHost.ID == secondHost.ID {
		t.Fatalf("project Host resource IDs = %#v, want %s and %s", got, firstHost.ID, secondHost.ID)
	}

	if err := store.RevokeHostMember(ctx, firstHost.ID, firstHostOwner.ID, projectOwner.ID); err != nil {
		t.Fatalf("revoke access to first Host: %v", err)
	}
	resources, err = store.ListProjectResources(ctx, projectOwner.ID, projectValue.ID, app.PageRequest{Limit: 20})
	if err != nil || len(resources.Items) != 1 || resources.Items[0].ResourceID != secondHost.ID {
		t.Fatalf("project Host resources after independent revoke = %#v, err=%v; want only %s", resources, err, secondHost.ID)
	}
}

func createProjectStoreFixture(t *testing.T, ctx context.Context, store *Store, ownerID, name, description, key string) project.Project {
	t.Helper()
	id, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	value, err := project.New(id, ownerID, name, description)
	if err != nil {
		t.Fatal(err)
	}
	created, replay, err := store.CreateProject(ctx, ownerID, value, key)
	if err != nil || replay {
		t.Fatalf("create Project fixture = %#v, replay=%t, err=%v", created, replay, err)
	}
	return created
}

func inviteAndAcceptProjectStoreMember(t *testing.T, ctx context.Context, store *Store, ownerID, inviteeID string, value project.Project, role project.Role, expectedVersion int64, inviteKey, acceptKey string) (project.Member, error) {
	t.Helper()
	id, err := domain.NewPendingRequestID()
	if err != nil {
		return project.Member{}, err
	}
	if _, _, err := store.CreateProjectInvitation(ctx, ownerID, project.Invitation{
		ID: id, ProjectID: value.ID, InviterID: ownerID, InviteeID: inviteeID,
		Role: role, State: project.InvitationPending, Version: 1,
	}, expectedVersion, inviteKey); err != nil {
		return project.Member{}, err
	}
	member, _, err := store.AcceptProjectInvitation(ctx, id, inviteeID, acceptKey)
	return member, err
}

func TestProjectWorkItemMutationPostgresReplayAndFences(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	member := createTestUser(t, store, 4, 20_000, 40_000)
	outsider := createTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Work item replay", "Mutation fences", "work-project-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1, "work-invite-01", "work-accept-01"); err != nil {
		t.Fatal(err)
	}

	itemID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	item := project.WorkItem{
		ID: itemID, ProjectID: projectValue.ID, Title: "Prepare release notes", Description: "",
		AcceptanceCriteria: []string{"notes saved"}, AssigneeID: owner.ID, State: project.WorkItemOpen, Version: 1,
	}
	created, replay, err := store.CreateProjectWorkItem(ctx, owner.ID, item, "work-item-create-01")
	if err != nil || replay || created.ID != item.ID || created.Version != 1 {
		t.Fatalf("CreateProjectWorkItem() = %#v replay=%t err=%v", created, replay, err)
	}
	created, replay, err = store.CreateProjectWorkItem(ctx, owner.ID, item, "work-item-create-01")
	if err != nil || !replay || created.ID != item.ID {
		t.Fatalf("CreateProjectWorkItem(replay) = %#v replay=%t err=%v", created, replay, err)
	}
	changedCreate := item
	changedCreate.ID, err = project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	changedCreate.Title = "Different request"
	if _, _, err := store.CreateProjectWorkItem(ctx, owner.ID, changedCreate, "work-item-create-01"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("CreateProjectWorkItem(changed replay) error = %v, want idempotency conflict", err)
	}

	digest := sha256.Sum256([]byte("block this work item"))
	mutation := app.ProjectWorkItemMutation{
		Operation: "update_work_item", WorkItemID: item.ID, IdempotencyKey: "work-item-update-01", RequestDigest: digest[:],
	}
	blocked := item
	blocked.State = project.WorkItemBlocked
	blocked.BlockedReason = "waiting for approval"
	blocked.Version = 2
	updated, err := store.UpdateProjectWorkItem(ctx, owner.ID, blocked, 1, mutation)
	if err != nil || updated.Version != 2 || updated.State != project.WorkItemBlocked {
		t.Fatalf("UpdateProjectWorkItem() = %#v, %v", updated, err)
	}
	updated, err = store.UpdateProjectWorkItem(ctx, owner.ID, blocked, 1, mutation)
	if err != nil || updated.Version != 2 || updated.State != project.WorkItemBlocked {
		t.Fatalf("UpdateProjectWorkItem(replay) = %#v, %v", updated, err)
	}
	read, replay, err := store.GetProjectWorkItemMutation(ctx, owner.ID, projectValue.ID, mutation)
	if err != nil || !replay || read.ID != item.ID || read.Version != 2 || read.State != project.WorkItemBlocked {
		t.Fatalf("GetProjectWorkItemMutation(replay) = %#v replay=%t err=%v", read, replay, err)
	}
	changedDigest := sha256.Sum256([]byte("different request body"))
	changedMutation := mutation
	changedMutation.RequestDigest = changedDigest[:]
	if _, _, err := store.GetProjectWorkItemMutation(ctx, owner.ID, projectValue.ID, changedMutation); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("GetProjectWorkItemMutation(changed digest) error = %v, want idempotency conflict", err)
	}
	_, replay, err = store.GetProjectWorkItemMutation(ctx, member.ID, projectValue.ID, mutation)
	if err != nil || replay {
		t.Fatalf("GetProjectWorkItemMutation(other actor) replay=%t err=%v, want independent miss", replay, err)
	}

	otherProject := createProjectStoreFixture(t, ctx, store, owner.ID, "Second work project", "Project id fence", "work-project-02")
	if _, _, err := store.GetProjectWorkItemMutation(ctx, owner.ID, otherProject.ID, mutation); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("GetProjectWorkItemMutation(other project) error = %v, want idempotency conflict", err)
	}
	staleDigest := sha256.Sum256([]byte("stale version request"))
	staleMutation := app.ProjectWorkItemMutation{
		Operation: mutation.Operation, WorkItemID: item.ID, IdempotencyKey: "work-item-update-02", RequestDigest: staleDigest[:],
	}
	if _, err := store.UpdateProjectWorkItem(ctx, owner.ID, blocked, 1, staleMutation); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("UpdateProjectWorkItem(stale version) error = %v, want conflict", err)
	}
	if _, _, err := store.GetProjectWorkItemMutation(ctx, outsider.ID, projectValue.ID, mutation); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("GetProjectWorkItemMutation(nonmember) error = %v, want not found", err)
	}

	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, 3, "work-remove-member-01"); err != nil {
		t.Fatalf("remove potential assignee: %v", err)
	}
	removedAssignee := blocked
	removedAssignee.AssigneeID = member.ID
	removedAssignee.Version = 3
	removedDigest := sha256.Sum256([]byte("assign removed member"))
	removedMutation := app.ProjectWorkItemMutation{
		Operation: mutation.Operation, WorkItemID: item.ID, IdempotencyKey: "work-item-update-03", RequestDigest: removedDigest[:],
	}
	if _, err := store.UpdateProjectWorkItem(ctx, owner.ID, removedAssignee, 2, removedMutation); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("UpdateProjectWorkItem(removed assignee) error = %v, want conflict", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status='disabled' WHERE id=$1`, owner.ID); err != nil {
		t.Fatalf("disable actor for transaction fence test: %v", err)
	}
	if _, _, err := store.GetProjectWorkItemMutation(ctx, owner.ID, projectValue.ID, mutation); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("GetProjectWorkItemMutation(disabled actor) error = %v, want not found", err)
	}
	if _, err := store.UpdateProjectWorkItem(ctx, owner.ID, blocked, 1, mutation); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("UpdateProjectWorkItem(disabled actor) error = %v, want not found", err)
	}
}
