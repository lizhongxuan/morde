package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
)

func TestProjectWorkItemAuthorizersRequireProjectTaskAndSavedDelivery(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	outsider := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Work item authorizers", "Task and evidence fences", "work-auth-project-01")
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "work-auth-workspace")
	if _, _, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: projectValue.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, projectValue.Version, "work-auth-resource-01"); err != nil {
		t.Fatalf("register Project Workspace: %v", err)
	}
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	hostValue := createTestHost(t, ctx, store, owner.ID, capacity, []string{workspaceValue.ID})
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='running',control_state='ai' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	binding := hostValue.Workspaces[0]
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0); err != nil {
		t.Fatalf("activate Task Workspace binding: %v", err)
	}
	conversationID := mustID(t, domain.NewConversationID)
	if _, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: conversationID, OwnerID: owner.ID, ProjectID: projectValue.ID, Title: "Work item authorization",
	}, testIdempotency(owner.ID, "project_conversation.create", "work-auth-conversation")); err != nil {
		t.Fatalf("create project conversation: %v", err)
	}

	revision := int64(0)
	manifestDigest, err := workspace.ManifestSHA256([]workspace.File{})
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "work-auth-project-task", "save a project result")
	submission.Task.ProjectID = projectValue.ID
	submission.Task.InitiatorID = owner.ID
	submission.Task.ConversationID = conversationID
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.WorkspaceCheckpointRevision = &revision
	submission.Task.ContextDigest = contextDigest
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization.TokenID = testSessionTokenID(owner.ID)
	submission.Message.ConversationID = conversationID
	submission.Message.ActorID = owner.ID
	submission.Idempotency = testIdempotency(owner.ID, "conversation.append_message", "work-auth-project-task")
	created, err := store.AppendMessage(ctx, submission)
	if err != nil {
		t.Fatalf("create project Task: %v", err)
	}

	if err := seedProjectWorkItemPublishReceipt(ctx, store, owner.ID, workspaceValue.ID, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	ownerPrincipal := projectWorkItemTestPrincipal(owner.ID)
	if err := store.AuthorizeProjectTaskLink(ctx, ownerPrincipal, projectValue.ID, created.Task.ID); err != nil {
		t.Fatalf("authorize project Task link: %v", err)
	}
	if err := store.AuthorizeSavedProjectDelivery(ctx, ownerPrincipal, projectValue.ID, project.WorkItemDelivery{
		TaskID: created.Task.ID, WorkspaceID: workspaceValue.ID, Revision: 1,
	}); err != nil {
		t.Fatalf("authorize saved Project delivery: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_workspace_publish_receipts
(task_id,owner_id,workspace_id,operation_key,request_hash,base_revision,state,manifest_sha256,published_revision)
SELECT task_id,owner_id,workspace_id,'project-work-item-delivery-duplicate',request_hash,base_revision,'succeeded',manifest_sha256,published_revision
FROM task_workspace_publish_receipts WHERE task_id=$1 AND operation_key='project-work-item-delivery'`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AuthorizeSavedProjectDelivery(ctx, ownerPrincipal, projectValue.ID, project.WorkItemDelivery{
		TaskID: created.Task.ID, WorkspaceID: workspaceValue.ID, Revision: 1,
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("ambiguous delivery publish receipts = %v, want conflict", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM task_workspace_publish_receipts WHERE task_id=$1 AND operation_key='project-work-item-delivery-duplicate'`, created.Task.ID); err != nil {
		t.Fatal(err)
	}

	otherProject := createProjectStoreFixture(t, ctx, store, owner.ID, "Other Project", "Wrong Task scope", "work-auth-other-project")
	if err := store.AuthorizeProjectTaskLink(ctx, ownerPrincipal, otherProject.ID, created.Task.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("cross-project Task link = %v, want forbidden", err)
	}
	if err := store.AuthorizeProjectTaskLink(ctx, projectWorkItemTestPrincipal(outsider.ID), projectValue.ID, created.Task.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("nonmember Task link = %v, want not found", err)
	}
	if err := store.AuthorizeSavedProjectDelivery(ctx, ownerPrincipal, projectValue.ID, project.WorkItemDelivery{
		TaskID: created.Task.ID, WorkspaceID: "w1234567", Revision: 1,
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("wrong delivery Workspace = %v, want conflict", err)
	}
	if err := store.AuthorizeSavedProjectDelivery(ctx, ownerPrincipal, projectValue.ID, project.WorkItemDelivery{
		TaskID: created.Task.ID, WorkspaceID: workspaceValue.ID, Revision: 2,
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("wrong delivery revision = %v, want conflict", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM task_workspace_publish_receipts WHERE task_id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AuthorizeSavedProjectDelivery(ctx, ownerPrincipal, projectValue.ID, project.WorkItemDelivery{
		TaskID: created.Task.ID, WorkspaceID: workspaceValue.ID, Revision: 1,
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("delivery without publish receipt = %v, want conflict", err)
	}
}

func projectWorkItemTestPrincipal(userID string) identity.Principal {
	return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(userID)}
}

func seedProjectWorkItemPublishReceipt(ctx context.Context, store *Store, ownerID, workspaceID, taskID string) error {
	manifestDigest := sha256.Sum256([]byte("saved project Task result"))
	requestDigest := sha256.Sum256([]byte("project Task publish receipt"))
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id,revision,file_count,total_bytes)
VALUES ($1,1,0,0)`, workspaceID); err != nil {
		return err
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=1,updated_at=clock_timestamp() WHERE id=$1`, workspaceID); err != nil {
		return err
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET workspace_checkpoint_revision=1 WHERE id=$1 AND workspace_id=$2`, taskID, workspaceID); err != nil {
		return err
	}
	_, err := store.pool.Exec(ctx, `INSERT INTO task_workspace_publish_receipts
(task_id,owner_id,workspace_id,operation_key,request_hash,base_revision,state,manifest_sha256,published_revision)
VALUES ($1,$2,$3,'project-work-item-delivery',$4,0,'succeeded',$5,1)`, taskID, ownerID, workspaceID, requestDigest[:], hex.EncodeToString(manifestDigest[:]))
	return err
}
