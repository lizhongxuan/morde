package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestConversationRouteNewTaskUsesStructuredFileDeliveryIntent(t *testing.T) {
	for _, test := range []struct {
		name              string
		content           string
		intent            task.FileDeliveryIntent
		wantFieldError    string
		wantClarification bool
	}{
		{name: "model says none despite file wording", content: "请生成 PDF 文件并提供下载。", intent: task.FileDeliveryNone},
		{name: "model requires file without workspace", content: "请分析这个问题并给出结论。", intent: task.FileDeliveryRequired, wantFieldError: "workspace_id"},
		{name: "model cannot determine file intent", content: "请处理这个请求。", intent: task.FileDeliveryUnclear, wantClarification: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &routeModelClientTest{reply: `{"action":"new","task_id":"","file_delivery_intent":"` + string(test.intent) + `"}`}
			store, gateway, _, claim, _ := routeModelFixtureWithMemberAndContent(t, false, false, client, test.content)
			ctx := context.Background()
			if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, claim.Message.OwnerID); err != nil {
				t.Fatal(err)
			}
			snapshot, decision := classifyFileDeliveryRoute(t, store, gateway, claim)
			if decision.InvalidOutput || decision.Action != app.ConversationRouteNew || decision.FileDeliveryIntent != test.intent {
				t.Fatalf("model route decision = %+v", decision)
			}
			err := store.ApplyConversationRoute(ctx, claim, snapshot, decision)
			if test.wantFieldError != "" {
				var domainErr *domain.Error
				if !errors.As(err, &domainErr) || domainErr.Code != domain.CodeInvalidArgument || domainErr.Details["field"] != test.wantFieldError {
					t.Fatalf("ApplyConversationRoute() error = %v, want invalid %s", err, test.wantFieldError)
				}
				var taskCount int
				if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE id=$1 AND task_id IS NOT NULL`, claim.Message.ID).Scan(&taskCount); err != nil {
					t.Fatal(err)
				}
				if taskCount != 0 {
					t.Fatalf("route created %d tasks despite missing Workspace", taskCount)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.wantClarification {
				var taskID string
				if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
					t.Fatal(err)
				}
				details, err := store.GetTask(ctx, taskID, claim.Message.OwnerID)
				if err != nil {
					t.Fatal(err)
				}
				if details.Task.FileDeliveryIntent != task.FileDeliveryUnclear || details.Task.Status != task.StatusWaitingUser ||
					details.Task.WaitingReason != "clarification_required" || len(details.Steps) != 2 ||
					details.Task.CurrentStep != details.Steps[1].ID || len(details.ClarificationRequests) != 1 {
					t.Fatalf("routed unclear task did not enter clarification: task=%#v steps=%#v clarifications=%#v",
						details.Task, details.Steps, details.ClarificationRequests)
				}
				request := details.ClarificationRequests[0]
				if request.Version != 1 || request.Status != task.ClarificationPending || request.CallID != app.FileDeliveryIntentClarificationCallID ||
					request.ModelStepID != details.Steps[0].ID || request.Question != details.Steps[1].Summary {
					t.Fatalf("routed clarification is not version-bound: request=%#v steps=%#v", request, details.Steps)
				}
				return
			}
			var got string
			if err := store.pool.QueryRow(ctx, `SELECT t.file_delivery_intent FROM tasks t JOIN conversation_messages m ON m.task_id=t.id WHERE m.id=$1`, claim.Message.ID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != string(test.intent) {
				t.Fatalf("created task file_delivery_intent = %q, want %q", got, test.intent)
			}
		})
	}
}

func TestConversationRouteProjectWorkspaceFileDeliveryFailsClosed(t *testing.T) {
	client := &routeModelClientTest{reply: `{"action":"new","task_id":"","file_delivery_intent":"required"}`}
	store, gateway, _, claim, _ := routeModelFixtureWithMemberAndContent(t, true, false, client,
		"请在这个项目里整理资料并交付文件。")
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, claim.Message.OwnerID); err != nil {
		t.Fatal(err)
	}
	snapshot, decision := classifyFileDeliveryRoute(t, store, gateway, claim)
	if decision.InvalidOutput || decision.Action != app.ConversationRouteNew || decision.FileDeliveryIntent != task.FileDeliveryRequired {
		t.Fatalf("project route decision = %+v", decision)
	}
	err := store.ApplyConversationRoute(ctx, claim, snapshot, decision)
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) || domainErr.Code != domain.CodeInvalidArgument ||
		domainErr.Details["field"] != "workspace_id" || !strings.Contains(domainErr.Message, "project Workspace") {
		t.Fatalf("project Workspace-less file delivery = %v, want explicit fail-closed workspace error", err)
	}
	var taskCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE id=$1 AND task_id IS NOT NULL`, claim.Message.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 0 {
		t.Fatalf("project route created %d Tasks without a supported project Workspace snapshot", taskCount)
	}
}

type projectRouteWorkspaceFixture struct {
	attachment projectAttachmentFixture
	workspace  workspace.Workspace
	resource   project.Resource
	digest     []byte
}

func newProjectRouteWorkspaceFixture(t *testing.T) projectRouteWorkspaceFixture {
	t.Helper()
	ctx := context.Background()
	attachment := newProjectAttachmentFixture(t)
	selected := createTestWorkspace(t, ctx, attachment.store, attachment.owner.UserID, "project route input")
	hostValue := createTestHost(t, ctx, attachment.store, attachment.owner.UserID,
		app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{selected.ID})
	if _, err := attachment.store.pool.Exec(ctx, `UPDATE hosts SET state='running' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.store.pool.Exec(ctx, `UPDATE workspace_bindings SET state='active' WHERE host_id=$1 AND workspace_id=$2`,
		hostValue.ID, selected.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.store.UpsertHostMember(ctx, host.Member{HostID: hostValue.ID, OwnerID: attachment.owner.UserID,
		UserID: attachment.member.UserID, Permissions: []host.Permission{host.PermissionWorkspaceFileRead}}); err != nil {
		t.Fatal(err)
	}
	projectValue, err := attachment.store.GetProject(ctx, attachment.owner.UserID, attachment.projectID)
	if err != nil {
		t.Fatal(err)
	}
	resource, _, err := attachment.store.AddProjectResource(ctx, attachment.owner.UserID, project.Resource{
		ProjectID: attachment.projectID, Kind: project.ResourceWorkspace, ResourceID: selected.ID, Version: 1,
	}, projectValue.Version, "project-route-workspace-resource")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'project route test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`,
		attachment.member.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`,
		attachment.member.UserID); err != nil {
		t.Fatal(err)
	}
	page, err := attachment.store.GetWorkspaceManifestPageForOwner(ctx, attachment.owner.UserID, selected.ID, selected.Revision, "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	digestText, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString(digestText)
	if err != nil {
		t.Fatal(err)
	}
	return projectRouteWorkspaceFixture{attachment: attachment, workspace: selected, resource: resource, digest: digest}
}

func (f projectRouteWorkspaceFixture) receive(t *testing.T, clientID string, contextDigest []byte) app.ConversationRouteClaim {
	t.Helper()
	ctx := context.Background()
	token, err := f.attachment.store.GetTokenByID(ctx, f.attachment.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	input := app.ConversationRoutingInput{ProjectID: f.attachment.projectID, ModelID: "p1234567", WorkspaceID: f.workspace.ID,
		WorkspaceInputRevision: &f.workspace.Revision, WorkspaceContextDigest: append([]byte(nil), contextDigest...),
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}}
	requestHash := sha256.Sum256([]byte(clientID))
	message, _, err := f.attachment.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.attachment.owner.UserID,
			ActorID: f.attachment.member.UserID, ConversationID: f.attachment.conversationID,
			Role: conversation.RoleUser, Content: "请在项目中交付文件。"},
		ServiceInstanceID: f.attachment.instanceID, ClientID: clientID, RequestHash: requestHash[:], Input: input,
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.attachment.store.ClaimConversationRoute(ctx, "project-route-workspace-test", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim project Workspace message: found=%t err=%v", found, err)
	}
	return claim
}

func TestConversationRouteProjectRequiredBindsAuthorizedWorkspaceInputSnapshot(t *testing.T) {
	for _, staleDigest := range []bool{false, true} {
		name := "authorized project Workspace"
		if staleDigest {
			name = "stale manifest digest"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newProjectRouteWorkspaceFixture(t)
			digest := fixture.digest
			if staleDigest {
				digest = bytes.Repeat([]byte("x"), sha256.Size)
			}
			clientID := "project-route-workspace-authorized"
			if staleDigest {
				clientID = "project-route-workspace-stale-digest"
			}
			claim := fixture.receive(t, clientID, digest)
			snapshot, err := fixture.attachment.store.LoadConversationRouteSnapshot(context.Background(), claim)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = claim.Input.ModelID
			err = fixture.attachment.store.ApplyConversationRoute(context.Background(), claim, snapshot, app.ConversationRouteDecision{
				Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryRequired,
				ModelOutput: `{"action":"new","task_id":"","file_delivery_intent":"required"}`,
			})
			if staleDigest {
				var domainErr *domain.Error
				if !errors.As(err, &domainErr) || domainErr.Code != domain.CodeWorkspaceConflict {
					t.Fatalf("ApplyConversationRoute() error = %v, want stale project manifest conflict", err)
				}
				var taskCount int
				if err := fixture.attachment.store.pool.QueryRow(context.Background(),
					`SELECT count(*) FROM conversation_messages WHERE id=$1 AND task_id IS NOT NULL`, claim.Message.ID).Scan(&taskCount); err != nil {
					t.Fatal(err)
				}
				if taskCount != 0 {
					t.Fatalf("stale project Workspace snapshot created %d Tasks", taskCount)
				}
				return
			}
			if err != nil {
				t.Fatalf("apply project Workspace file-delivery route: %v", err)
			}
			var taskID, workspaceID, workspaceOwnerID string
			var revision, checkpointRevision, resourceVersion int64
			var contextDigest []byte
			if err := fixture.attachment.store.pool.QueryRow(context.Background(), `SELECT t.id,t.workspace_id,t.workspace_owner_id,
t.workspace_project_resource_version,t.input_revision,t.workspace_checkpoint_revision,t.context_digest
FROM tasks t JOIN conversation_messages m ON m.task_id=t.id WHERE m.id=$1`, claim.Message.ID).Scan(&taskID, &workspaceID,
				&workspaceOwnerID, &resourceVersion, &revision, &checkpointRevision, &contextDigest); err != nil {
				t.Fatal(err)
			}
			if taskID == "" || workspaceID != fixture.workspace.ID || workspaceOwnerID != fixture.attachment.owner.UserID ||
				resourceVersion != fixture.resource.Version || revision != fixture.workspace.Revision || checkpointRevision != fixture.workspace.Revision ||
				!bytes.Equal(contextDigest, fixture.digest) {
				t.Fatalf("project route Task snapshot = workspace=%q owner=%q resource=%d input=%d checkpoint=%d digest=%x",
					workspaceID, workspaceOwnerID, resourceVersion, revision, checkpointRevision, contextDigest)
			}
			var grantFenceCount int
			if err := fixture.attachment.store.pool.QueryRow(context.Background(),
				`SELECT count(*) FROM task_workspace_grant_fences WHERE task_id=$1`, taskID).Scan(&grantFenceCount); err != nil {
				t.Fatal(err)
			}
			if grantFenceCount != 1 {
				t.Fatalf("project route Task grant fences = %d, want 1", grantFenceCount)
			}
		})
	}
}

func TestConversationRouteProjectUnclearRetainsAuthorizedWorkspaceInputSnapshot(t *testing.T) {
	fixture := newProjectRouteWorkspaceFixture(t)
	claim := fixture.receive(t, "project-route-workspace-unclear", fixture.digest)
	snapshot, err := fixture.attachment.store.LoadConversationRouteSnapshot(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = claim.Input.ModelID
	if err := fixture.attachment.store.ApplyConversationRoute(context.Background(), claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryUnclear,
		ModelOutput: `{"action":"new","task_id":"","file_delivery_intent":"unclear"}`,
	}); err != nil {
		t.Fatalf("apply project Workspace clarification route: %v", err)
	}
	var taskID, workspaceID, workspaceOwnerID, status, waitingReason string
	var revision, checkpointRevision, resourceVersion int64
	var contextDigest []byte
	if err := fixture.attachment.store.pool.QueryRow(context.Background(), `SELECT t.id,t.workspace_id,t.workspace_owner_id,
t.workspace_project_resource_version,t.input_revision,t.workspace_checkpoint_revision,t.context_digest,t.status,t.waiting_reason
FROM tasks t JOIN conversation_messages m ON m.task_id=t.id WHERE m.id=$1`, claim.Message.ID).Scan(&taskID, &workspaceID,
		&workspaceOwnerID, &resourceVersion, &revision, &checkpointRevision, &contextDigest, &status, &waitingReason); err != nil {
		t.Fatal(err)
	}
	if taskID == "" || workspaceID != fixture.workspace.ID || workspaceOwnerID != fixture.attachment.owner.UserID ||
		resourceVersion != fixture.resource.Version || revision != fixture.workspace.Revision || checkpointRevision != fixture.workspace.Revision ||
		!bytes.Equal(contextDigest, fixture.digest) || status != string(task.StatusWaitingUser) || waitingReason != "clarification_required" {
		t.Fatalf("project unclear route did not retain authorized Workspace snapshot: workspace=%q owner=%q resource=%d input=%d checkpoint=%d status=%q reason=%q digest=%x",
			workspaceID, workspaceOwnerID, resourceVersion, revision, checkpointRevision, status, waitingReason, contextDigest)
	}
	var grantFenceCount int
	if err := fixture.attachment.store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM task_workspace_grant_fences WHERE task_id=$1`, taskID).Scan(&grantFenceCount); err != nil {
		t.Fatal(err)
	}
	if grantFenceCount != 1 {
		t.Fatalf("project unclear route Task grant fences = %d, want 1", grantFenceCount)
	}
}

func TestConversationRouteNewRequiredBindsCurrentWorkspaceInputSnapshot(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	selected := createTestWorkspace(t, ctx, f.store, f.ownerID, "conversation route required")
	page, err := f.store.GetWorkspaceManifestPageForOwner(ctx, f.ownerID, selected.ID, selected.Revision, "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	digestText, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString(digestText)
	if err != nil {
		t.Fatal(err)
	}
	revision := selected.Revision
	claim := f.receive(t, "route-required-workspace-01", "创建一份报告并保存在所选工作区。", app.ConversationRoutingInput{
		WorkspaceID: selected.ID, WorkspaceInputRevision: &revision, WorkspaceContextDigest: digest,
	})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = claim.Input.ModelID
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryRequired,
		ModelOutput: `{"action":"new","task_id":"","file_delivery_intent":"required"}`,
	}); err != nil {
		t.Fatalf("apply required route with selected Workspace snapshot: %v", err)
	}
	var taskID, workspaceID string
	var inputRevision, checkpointRevision int64
	var contextDigest []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT t.id,t.workspace_id,t.input_revision,t.workspace_checkpoint_revision,t.context_digest
FROM tasks t JOIN conversation_messages m ON m.task_id=t.id WHERE m.id=$1`, claim.Message.ID).
		Scan(&taskID, &workspaceID, &inputRevision, &checkpointRevision, &contextDigest); err != nil {
		t.Fatal(err)
	}
	if taskID == "" || workspaceID != selected.ID || inputRevision != selected.Revision ||
		checkpointRevision != selected.Revision || !bytes.Equal(contextDigest, digest) {
		t.Fatalf("routed Task did not preserve the selected Workspace input snapshot: workspace=%q input=%d checkpoint=%d digest=%x",
			workspaceID, inputRevision, checkpointRevision, contextDigest)
	}
}

func TestConversationRouteNewUnclearBindsWorkspaceForRequiredClarification(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	selected := createTestWorkspace(t, ctx, f.store, f.ownerID, "conversation route unclear")
	page, err := f.store.GetWorkspaceManifestPageForOwner(ctx, f.ownerID, selected.ID, selected.Revision, "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	digestText, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString(digestText)
	if err != nil {
		t.Fatal(err)
	}
	revision := selected.Revision
	claim := f.receive(t, "route-unclear-workspace-01", "请处理这个请求。", app.ConversationRoutingInput{
		WorkspaceID: selected.ID, WorkspaceInputRevision: &revision, WorkspaceContextDigest: digest,
	})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = claim.Input.ModelID
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryUnclear,
		ModelOutput: `{"action":"new","task_id":"","file_delivery_intent":"unclear"}`,
	}); err != nil {
		t.Fatalf("apply unclear route with selected Workspace snapshot: %v", err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, taskID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Task.FileDeliveryIntent != task.FileDeliveryUnclear || details.Task.Status != task.StatusWaitingUser ||
		details.Task.WorkspaceID != selected.ID || details.Task.WorkspaceOwnerID != f.ownerID ||
		details.Task.InputRevision == nil || *details.Task.InputRevision != selected.Revision ||
		details.Task.WorkspaceCheckpointRevision == nil || *details.Task.WorkspaceCheckpointRevision != selected.Revision ||
		!bytes.Equal(details.Task.ContextDigest, digest) || len(details.ClarificationRequests) != 1 ||
		!strings.Contains(details.ClarificationRequests[0].Question, "已绑定") {
		t.Fatalf("unclear route did not retain the selected Workspace snapshot: task=%#v clarifications=%#v",
			details.Task, details.ClarificationRequests)
	}
	request := details.ClarificationRequests[0]
	answer := "需要交付文件"
	objective := details.Task.Objective + "\n\nUser clarification about file delivery: " + answer
	record := testIdempotency(f.ownerID, "task.clarification.answer", "route-unclear-required-answer")
	record.ResourceID, record.RelatedID = taskID, request.ID
	if err := f.store.AnswerClarificationRequest(ctx, record, taskID, request.ID, request.Version, answer,
		task.FileDeliveryRequired, objective); err != nil {
		t.Fatalf("answer routed file intent clarification as required: %v", err)
	}
	details, err = f.store.GetTask(ctx, taskID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Task.Status != task.StatusQueued || details.Task.FileDeliveryIntent != task.FileDeliveryRequired ||
		details.Task.WorkspaceID != selected.ID || details.Task.InputRevision == nil || *details.Task.InputRevision != selected.Revision ||
		details.Task.WorkspaceCheckpointRevision == nil || *details.Task.WorkspaceCheckpointRevision != selected.Revision ||
		!bytes.Equal(details.Task.ContextDigest, digest) || details.ClarificationRequests[0].Status != task.ClarificationAnswered {
		t.Fatalf("required clarification answer did not queue against its bound Workspace snapshot: task=%#v clarifications=%#v",
			details.Task, details.ClarificationRequests)
	}
}

func TestConversationRouteNewRequiredRejectsStaleOrInvalidWorkspaceSnapshot(t *testing.T) {
	for _, test := range []struct {
		name          string
		foreignOwner  bool
		mutateReceipt func(**int64, *[]byte)
		mutateStore   func(context.Context, *Store, workspace.Workspace) error
		wantCode      domain.ErrorCode
	}{
		{name: "receipt without snapshot", wantCode: domain.CodeInvalidArgument,
			mutateReceipt: func(revision **int64, digest *[]byte) { *revision = nil; *digest = nil }},
		{name: "workspace revision changed after receipt", wantCode: domain.CodeWorkspaceConflict,
			mutateStore: func(ctx context.Context, store *Store, value workspace.Workspace) error {
				_, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=revision+1 WHERE id=$1`, value.ID)
				return err
			}},
		{name: "input revision is negative", wantCode: domain.CodeInvalidArgument,
			mutateReceipt: func(revision **int64, _ *[]byte) {
				invalid := int64(-1)
				*revision = &invalid
			}},
		{name: "manifest digest is stale", wantCode: domain.CodeWorkspaceConflict,
			mutateReceipt: func(_ **int64, digest *[]byte) {
				*digest = make([]byte, 32)
				(*digest)[0] = 1
			}},
		{name: "manifest digest has invalid length", wantCode: domain.CodeInvalidArgument,
			mutateReceipt: func(_ **int64, digest *[]byte) {
				*digest = make([]byte, 31)
			}},
		{name: "foreign Workspace owner is not authorized", wantCode: domain.CodeNotFound, foreignOwner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, f.ownerID); err != nil {
				t.Fatal(err)
			}
			workspaceOwnerID := f.ownerID
			if test.foreignOwner {
				workspaceOwnerID = createPreRetirementTestUser(t, f.store, 2, 2000, 4096).ID
			}
			selected := createTestWorkspace(t, ctx, f.store, workspaceOwnerID, "stale conversation route workspace")
			page, err := f.store.GetWorkspaceManifestPageForOwner(ctx, workspaceOwnerID, selected.ID, selected.Revision, "", "", 1000)
			if err != nil {
				t.Fatal(err)
			}
			digestText, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := hex.DecodeString(digestText)
			if err != nil {
				t.Fatal(err)
			}
			revision := selected.Revision
			input := app.ConversationRoutingInput{WorkspaceID: selected.ID,
				WorkspaceInputRevision: &revision, WorkspaceContextDigest: digest}
			if test.mutateReceipt != nil {
				test.mutateReceipt(&input.WorkspaceInputRevision, &input.WorkspaceContextDigest)
			}
			claim := f.receive(t, "route-required-stale-"+strings.ReplaceAll(test.name, " ", "-"),
				"创建一份报告并保存在所选工作区。", input)
			if test.mutateStore != nil {
				if err := test.mutateStore(ctx, f.store, selected); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = claim.Input.ModelID
			err = f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
				Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryRequired,
				ModelOutput: `{"action":"new","task_id":"","file_delivery_intent":"required"}`,
			})
			if domain.CodeOf(err) != test.wantCode {
				t.Fatalf("ApplyConversationRoute() error = %v, want code %s", err, test.wantCode)
			}
			var taskCount int
			if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE id=$1 AND task_id IS NOT NULL`, claim.Message.ID).Scan(&taskCount); err != nil {
				t.Fatal(err)
			}
			if taskCount != 0 {
				t.Fatalf("rejected stale or invalid Workspace snapshot created %d Tasks", taskCount)
			}
		})
	}
}

func TestConversationRouteRevisionReadsStructuredFileDeliveryDecision(t *testing.T) {
	for _, test := range []struct {
		name             string
		content          string
		revisionIntent   string
		want             task.FileDeliveryIntent
		wantFieldError   string
		mismatchRevision bool
	}{
		{name: "empty means inherit despite file wording", content: "请生成 PDF 文件并提供下载。", want: task.FileDeliveryNone},
		{name: "explicit none overrides file wording", content: "请生成 PDF 文件并提供下载。", revisionIntent: "none", want: task.FileDeliveryNone},
		{name: "explicit required without Workspace snapshot is rejected", content: "不要生成文件，只用文字说明。", revisionIntent: "required", wantFieldError: "workspace_id"},
		{name: "wrong stored revision is deferred", content: "请生成 PDF 文件并提供下载。", revisionIntent: "none", want: task.FileDeliveryNone, mismatchRevision: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			newReply := `{"action":"new","task_id":"","file_delivery_intent":"none"}`
			client := &routeModelClientTest{reply: newReply}
			store, gateway, _, initialClaim, _ := routeModelFixtureWithMemberAndContent(t, false, false, client, "请分析这段内容。")
			ctx := context.Background()
			if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, initialClaim.Message.OwnerID); err != nil {
				t.Fatal(err)
			}
			initialSnapshot, initialDecision := classifyFileDeliveryRoute(t, store, gateway, initialClaim)
			if err := store.ApplyConversationRoute(ctx, initialClaim, initialSnapshot, initialDecision); err != nil {
				t.Fatal(err)
			}
			var taskID string
			if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initialClaim.Message.ID).Scan(&taskID); err != nil {
				t.Fatal(err)
			}

			revisionMessage := receiveCausalMessage(t, store, initialClaim, "fd-revision-001", initialClaim.Message.ClientMessageID, test.content)
			revisionClaim := claimCausalMessage(t, store, revisionMessage.ID)
			client.secondReply = `{"action":"update","task_id":"` + taskID + `"}`
			if test.revisionIntent != "" {
				client.secondReply = `{"action":"update","task_id":"` + taskID + `","file_delivery_intent":"` + test.revisionIntent + `"}`
			}
			revisionSnapshot, revisionDecision := classifyFileDeliveryRoute(t, store, gateway, revisionClaim)
			if revisionDecision.InvalidOutput || revisionDecision.Action != app.ConversationRouteUpdate {
				t.Fatalf("revision route decision = %+v", revisionDecision)
			}
			err := store.ApplyConversationRoute(ctx, revisionClaim, revisionSnapshot, revisionDecision)
			if test.wantFieldError != "" {
				var domainErr *domain.Error
				if !errors.As(err, &domainErr) || domainErr.Code != domain.CodeInvalidArgument || domainErr.Details["field"] != test.wantFieldError {
					t.Fatalf("ApplyConversationRoute() error = %v, want invalid %s", err, test.wantFieldError)
				}
				var status, intent string
				var desiredRevision int64
				if err := store.pool.QueryRow(ctx, `SELECT status,file_delivery_intent,desired_revision FROM tasks WHERE id=$1`, taskID).
					Scan(&status, &intent, &desiredRevision); err != nil {
					t.Fatal(err)
				}
				if status != string(task.StatusQueued) || intent != string(task.FileDeliveryNone) || desiredRevision != 0 {
					t.Fatalf("rejected Workspace-less revision changed task: status=%s intent=%s desired_revision=%d",
						status, intent, desiredRevision)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.mismatchRevision {
				result, err := store.pool.Exec(ctx, `UPDATE conversation_route_model_calls SET decision=jsonb_set(decision,'{ExpectedRevision}','1'::jsonb,true)
WHERE message_id=$1 AND phase='normal' AND attempt=(SELECT route_attempt FROM conversation_message_routes WHERE message_id=$1)`, revisionMessage.ID)
				if err != nil || result.RowsAffected() != 1 {
					t.Fatalf("tamper synthetic stored revision fence: rows=%d err=%v", result.RowsAffected(), err)
				}
			}

			if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
				t.Fatal(err)
			}
			stop, found, err := store.ClaimNextTaskStop(ctx, "file-delivery-revision-stop", time.Minute)
			if err != nil || !found || stop.Task.ID != taskID {
				t.Fatalf("claim revision stop: found=%t err=%v", found, err)
			}
			if _, err := store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
				t.Fatal(err)
			}
			applied, err := store.ApplyNextConversationTaskRevision(ctx)
			if test.mismatchRevision {
				if err != nil || applied {
					t.Fatalf("mismatched durable revision was applied: applied=%t err=%v", applied, err)
				}
				var status, intent string
				if err := store.pool.QueryRow(ctx, `SELECT status,file_delivery_intent FROM tasks WHERE id=$1`, taskID).Scan(&status, &intent); err != nil ||
					status != string(task.StatusPaused) || intent != string(task.FileDeliveryNone) {
					t.Fatalf("mismatched durable revision changed task: status=%s intent=%s err=%v", status, intent, err)
				}
				return
			}
			if err != nil || !applied {
				t.Fatalf("apply structured revision: applied=%t err=%v", applied, err)
			}
			var got string
			if err := store.pool.QueryRow(ctx, `SELECT file_delivery_intent FROM tasks WHERE id=$1`, taskID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != string(test.want) {
				t.Fatalf("revised file_delivery_intent = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConversationRouteUpdateUnclearRequiresVersionedAnswerBeforeRevision(t *testing.T) {
	client := &routeModelClientTest{reply: `{"action":"new","task_id":"","file_delivery_intent":"none"}`}
	store, gateway, _, initialClaim, _ := routeModelFixtureWithMemberAndContent(t, false, false, client, "请整理这份工作。")
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, initialClaim.Message.OwnerID); err != nil {
		t.Fatal(err)
	}
	initialSnapshot, initialDecision := classifyFileDeliveryRoute(t, store, gateway, initialClaim)
	if err := store.ApplyConversationRoute(ctx, initialClaim, initialSnapshot, initialDecision); err != nil {
		t.Fatalf("create initial Task: %v", err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initialClaim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}

	revisionMessage := receiveCausalMessage(t, store, initialClaim, "route-update-unclear-revision", initialClaim.Message.ClientMessageID,
		"请继续调整这项工作。")
	revisionClaim := claimCausalMessage(t, store, revisionMessage.ID)
	client.secondReply = `{"action":"update","task_id":"` + taskID + `","file_delivery_intent":"unclear"}`
	revisionSnapshot, revisionDecision := classifyFileDeliveryRoute(t, store, gateway, revisionClaim)
	if revisionDecision.InvalidOutput || revisionDecision.Action != app.ConversationRouteUpdate ||
		revisionDecision.TaskID != taskID || revisionDecision.FileDeliveryIntent != task.FileDeliveryUnclear {
		t.Fatalf("revision route decision = %+v", revisionDecision)
	}
	if err := store.ApplyConversationRoute(ctx, revisionClaim, revisionSnapshot, revisionDecision); err != nil {
		t.Fatalf("apply unclear Task update: %v", err)
	}

	details, err := store.GetTask(ctx, taskID, initialClaim.Message.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Task.Status != task.StatusPausing || details.Task.DesiredRevision == nil || *details.Task.DesiredRevision != 1 ||
		len(details.ClarificationRequests) != 1 {
		t.Fatalf("unclear update did not wait behind a versioned clarification: task=%#v clarifications=%#v",
			details.Task, details.ClarificationRequests)
	}
	request := details.ClarificationRequests[0]
	if request.Version != 1 || request.Status != task.ClarificationPending ||
		request.CallID != task.RouteFileDeliveryIntentClarificationCallID {
		t.Fatalf("route clarification identity = %#v", request)
	}

	rejected := testIdempotency(initialClaim.Message.OwnerID, "task.clarification.answer", "route-update-required-without-workspace")
	rejected.ResourceID, rejected.RelatedID = taskID, request.ID
	err = store.AnswerClarificationRequest(ctx, rejected, taskID, request.ID, request.Version,
		"需要文件", task.FileDeliveryRequired, "")
	if domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("required answer without Workspace error = %v, want invalid argument", err)
	}
	details, err = store.GetTask(ctx, taskID, initialClaim.Message.OwnerID)
	if err != nil || details.ClarificationRequests[0].Status != task.ClarificationPending || details.Task.DesiredRevision == nil ||
		*details.Task.DesiredRevision != 1 {
		t.Fatalf("rejected required answer changed clarification or revision: task=%#v clarifications=%#v err=%v",
			details.Task, details.ClarificationRequests, err)
	}

	record := testIdempotency(initialClaim.Message.OwnerID, "task.clarification.answer", "route-update-unclear-answer-none")
	record.ResourceID, record.RelatedID = taskID, request.ID
	if err := store.AnswerClarificationRequest(ctx, record, taskID, request.ID, request.Version, "不需要文件", task.FileDeliveryNone, ""); err != nil {
		t.Fatalf("answer route file delivery clarification: %v", err)
	}
	details, err = store.GetTask(ctx, taskID, initialClaim.Message.OwnerID)
	if err != nil || details.ClarificationRequests[0].Status != task.ClarificationAnswered || details.Task.Status != task.StatusPausing {
		t.Fatalf("answer should resolve the route question without starting revision early: task=%#v clarifications=%#v err=%v",
			details.Task, details.ClarificationRequests, err)
	}
	answeredSequence := details.Task.EventSequence
	if err := store.AnswerClarificationRequest(ctx, record, taskID, request.ID, request.Version, "不需要文件", task.FileDeliveryNone, ""); err != nil {
		t.Fatalf("replay the same route clarification answer: %v", err)
	}
	details, err = store.GetTask(ctx, taskID, initialClaim.Message.OwnerID)
	if err != nil || details.Task.EventSequence != answeredSequence || details.ClarificationRequests[0].Status != task.ClarificationAnswered {
		t.Fatalf("idempotent answer replay changed Task state: task=%#v clarifications=%#v err=%v",
			details.Task, details.ClarificationRequests, err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := store.ClaimNextTaskStop(ctx, "route-update-unclear-stop", time.Minute)
	if err != nil || !found || stop.Task.ID != taskID {
		t.Fatalf("claim Task revision stop: found=%t err=%v", found, err)
	}
	if _, err := store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatalf("complete Task revision stop: %v", err)
	}
	applied, err := store.ApplyNextConversationTaskRevision(ctx)
	if err != nil || !applied {
		t.Fatalf("apply answered Task revision: applied=%t err=%v", applied, err)
	}
	details, err = store.GetTask(ctx, taskID, initialClaim.Message.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Task.Status != task.StatusQueued || details.Task.CurrentRevision == nil || *details.Task.CurrentRevision != 1 ||
		details.Task.DesiredRevision == nil || *details.Task.DesiredRevision != 1 ||
		details.Task.FileDeliveryIntent != task.FileDeliveryNone {
		t.Fatalf("applied Task revision does not match the structured clarification: task=%#v", details.Task)
	}
}

func classifyFileDeliveryRoute(t *testing.T, store *Store, gateway *app.ModelGatewayService,
	claim app.ConversationRouteClaim) (app.ConversationRouteSnapshot, app.ConversationRouteDecision) {
	t.Helper()
	ctx := context.Background()
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	modelID, err := gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = modelID
	decision, err := gateway.ClassifyConversationRoute(ctx, claim, snapshot.Context)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, decision
}
