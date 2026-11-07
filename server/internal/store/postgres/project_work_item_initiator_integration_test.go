package postgres

import (
	"context"
	"encoding/hex"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
)

func TestProjectWorkItemStoreEnforcesTaskInitiatorAndCheckpoint(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Work item initiator", "Task attribution boundary", "work-init-project-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"work-init-invite-01", "work-init-accept-01"); err != nil {
		t.Fatalf("accept project member invitation: %v", err)
	}
	projectValue, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatalf("read project after inviting member: %v", err)
	}

	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "work-init-workspace")
	if _, _, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: projectValue.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, projectValue.Version, "work-init-resource-01"); err != nil {
		t.Fatalf("register project Workspace: %v", err)
	}
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	hostValue := createTestHost(t, ctx, store, owner.ID, capacity, []string{workspaceValue.ID})
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='running',control_state='ai' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatalf("start Task Host fixture: %v", err)
	}
	binding := hostValue.Workspaces[0]
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0); err != nil {
		t.Fatalf("activate Task Workspace binding: %v", err)
	}

	conversationID := mustID(t, domain.NewConversationID)
	if _, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: conversationID, OwnerID: owner.ID, ProjectID: projectValue.ID, Title: "Task initiator authorization",
	}, testIdempotency(owner.ID, "project_conversation.create", "work-init-conversation-01")); err != nil {
		t.Fatalf("create project conversation: %v", err)
	}

	inputRevision := int64(0)
	manifestDigest, err := workspace.ManifestSHA256([]workspace.File{})
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "work-init-task-01", "save an authorized project result")
	submission.Task.ProjectID = projectValue.ID
	submission.Task.InitiatorID = owner.ID
	submission.Task.ConversationID = conversationID
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &inputRevision
	submission.Task.WorkspaceCheckpointRevision = &inputRevision
	submission.Task.ContextDigest = contextDigest
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization.TokenID = testSessionTokenID(owner.ID)
	submission.Message.ConversationID = conversationID
	submission.Message.ActorID = owner.ID
	submission.Idempotency = testIdempotency(owner.ID, "task.submit", "work-init-task-01")
	createdTask, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("create owner-initiated project Task: %v", err)
	}
	if createdTask.Task.ProjectID != projectValue.ID || createdTask.Task.InitiatorID != owner.ID {
		t.Fatalf("Task attribution = project %q initiator %q; want project %q initiator %q",
			createdTask.Task.ProjectID, createdTask.Task.InitiatorID, projectValue.ID, owner.ID)
	}
	if err := seedProjectWorkItemPublishReceipt(ctx, store, owner.ID, workspaceValue.ID, createdTask.Task.ID); err != nil {
		t.Fatalf("seed saved Task delivery receipt: %v", err)
	}

	itemID := mustID(t, project.NewID)
	item := project.WorkItem{
		ID: itemID, ProjectID: projectValue.ID, Title: "Link and deliver saved Task", Description: "",
		AcceptanceCriteria: []string{"delivery matches the Task checkpoint"}, AssigneeID: owner.ID,
		State: project.WorkItemOpen, Version: 1,
	}
	item, replay, err := store.CreateProjectWorkItem(ctx, owner.ID, item, "work-init-item-create-01")
	if err != nil || replay {
		t.Fatalf("create project work item = %#v replay=%t err=%v", item, replay, err)
	}

	memberLink := item
	memberLink.TaskIDs = []string{createdTask.Task.ID}
	memberLink.Version = 2
	memberMutation := app.ProjectWorkItemMutation{
		Operation: "update_work_item", WorkItemID: item.ID, IdempotencyKey: "work-init-member-link-01",
		RequestDigest: hashBytes("member links another user's Task"),
	}
	if _, err := store.UpdateProjectWorkItem(ctx, member.ID, memberLink, 1, memberMutation); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("active member linking owner's Task = %v, want forbidden", err)
	}
	unchanged, err := store.GetProjectWorkItem(ctx, owner.ID, projectValue.ID, item.ID)
	if err != nil || unchanged.Version != 1 || len(unchanged.TaskIDs) != 0 {
		t.Fatalf("work item after rejected link = %#v err=%v; want version 1 with no Task links", unchanged, err)
	}

	ownerLink := item
	ownerLink.TaskIDs = []string{createdTask.Task.ID}
	ownerLink.Version = 2
	ownerMutation := app.ProjectWorkItemMutation{
		Operation: "update_work_item", WorkItemID: item.ID, IdempotencyKey: "work-init-owner-link-01",
		RequestDigest: hashBytes("owner links own Task"),
	}
	linked, err := store.UpdateProjectWorkItem(ctx, owner.ID, ownerLink, 1, ownerMutation)
	if err != nil || linked.Version != 2 || len(linked.TaskIDs) != 1 || linked.TaskIDs[0] != createdTask.Task.ID {
		t.Fatalf("owner linking own Task = %#v err=%v", linked, err)
	}

	delivered := linked
	delivered.State = project.WorkItemDelivered
	delivered.Delivery = &project.WorkItemDelivery{
		TaskID: createdTask.Task.ID, WorkspaceID: workspaceValue.ID, Revision: 1,
	}
	delivered.Version = 3
	deliveryMutation := app.ProjectWorkItemMutation{
		Operation: "update_work_item", WorkItemID: item.ID, IdempotencyKey: "work-init-owner-delivery-01",
		RequestDigest: hashBytes("owner records saved Task delivery"),
	}
	persisted, err := store.UpdateProjectWorkItem(ctx, owner.ID, delivered, 2, deliveryMutation)
	if err != nil {
		t.Fatalf("persist authorized Task delivery: %v", err)
	}
	var checkpointRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT workspace_checkpoint_revision FROM tasks WHERE id=$1`, createdTask.Task.ID).Scan(&checkpointRevision); err != nil {
		t.Fatalf("read Task checkpoint revision: %v", err)
	}
	stored, err := store.GetProjectWorkItem(ctx, owner.ID, projectValue.ID, item.ID)
	if err != nil {
		t.Fatalf("read persisted project work item delivery: %v", err)
	}
	if persisted.Delivery == nil || stored.Delivery == nil || stored.Version != 3 ||
		stored.Delivery.TaskID != createdTask.Task.ID || stored.Delivery.WorkspaceID != workspaceValue.ID ||
		stored.Delivery.Revision != checkpointRevision {
		t.Fatalf("persisted delivery = %#v; Task checkpoint revision = %d", stored.Delivery, checkpointRevision)
	}
}
