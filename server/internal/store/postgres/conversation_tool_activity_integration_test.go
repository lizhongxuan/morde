package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func toolActivityMessage(t *testing.T, page app.MessagePage, id string) conversation.Message {
	t.Helper()
	for _, message := range page.Items {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("message %s is missing from page", id)
	return conversation.Message{}
}

func TestPostgresConversationToolActivityStaysWithOriginalMessage(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "tool-activity-original-001", "请搜索公开网站的资料", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteTool, ToolName: "morde_web_search",
		ToolArguments: `{"query":"synthetic query"}`,
		ModelOutput:   `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"synthetic query"}}`}
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var internalTaskID, internalCallID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id,call_id FROM conversation_route_internal_tool_calls WHERE message_id=$1`, claim.Message.ID).
		Scan(&internalTaskID, &internalCallID); err != nil {
		t.Fatal(err)
	}
	ordinary := f.receive(t, "tool-activity-ordinary-001", "ordinary message", app.ConversationRoutingInput{})
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET task_id=$2 WHERE id=$1`, claim.Message.ID, internalTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET duplicate_of_message_id=$2 WHERE message_id=$1`,
		ordinary.Message.ID, claim.Message.ID); err != nil {
		t.Fatal(err)
	}
	authorization := projectConversationTestAuthorization{}
	service, err := app.NewConversationService(f.store, authorization)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession,
		TokenID: testSessionTokenID(f.ownerID)}
	assertStatus := func(want task.StepStatus) {
		t.Helper()
		page, err := service.ListMessages(ctx, principal, f.conversation.ID, app.PageRequest{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		message := toolActivityMessage(t, page, claim.Message.ID)
		if message.TaskID != "" || message.RelatedTaskID != "" || message.ToolActivity == nil ||
			message.ToolActivity.Tool != "web_search" || message.ToolActivity.Status != want {
			t.Fatalf("original message activity = %+v, want %s without an internal Task ID", message.ToolActivity, want)
		}
		duplicate := toolActivityMessage(t, page, ordinary.Message.ID)
		if duplicate.ToolActivity != nil || duplicate.RelatedTaskID != "" || duplicate.DuplicateOfMessageID != claim.Message.ID {
			t.Fatal("duplicate message inherited an internal tool activity or Task ID")
		}
	}
	assertStatus(task.StepQueued)
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='running',lease_owner='synthetic-worker',
lease_version=lease_version+1,lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, internalTaskID); err != nil {
		t.Fatal(err)
	}
	assertStatus(task.StepRunning)
	for _, terminal := range []struct {
		status task.Status
		want   task.StepStatus
	}{{task.StatusFailed, task.StepFailed}, {task.StatusCancelled, task.StepCancelled}} {
		if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status=$2,lease_owner='',lease_expires_at=NULL,
finished_at=clock_timestamp() WHERE id=$1`, internalTaskID, string(terminal.status)); err != nil {
			t.Fatal(err)
		}
		assertStatus(terminal.want)
	}
	unknownOperationID := mustID(t, domain.NewOperationID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
 operation_key,tool_name,request_hash,status,call_id)
VALUES ($1,$2,$3,NULL,'searxng-v2.10.0',1,0,1,1,$4,'morde_web_search',$5,'unknown',$6)`,
		unknownOperationID, internalTaskID, f.ownerID, app.AgentOperationKey(internalCallID), hashBytes("uncertain web call"), internalCallID); err != nil {
		t.Fatal(err)
	}
	assertStatus(task.StepUnknown)
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='running',lease_owner='synthetic-worker',
lease_version=lease_version+1,lease_expires_at=clock_timestamp()+interval '1 minute',finished_at=NULL WHERE id=$1`, internalTaskID); err != nil {
		t.Fatal(err)
	}
	assertStatus(task.StepUnknown)
	if _, err := f.store.pool.Exec(ctx, `DELETE FROM task_mcp_connector_operations WHERE id=$1`, unknownOperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='completed',lease_owner='',lease_expires_at=NULL,
finished_at=clock_timestamp() WHERE id=$1`, internalTaskID); err != nil {
		t.Fatal(err)
	}
	assertStatus(task.StepUnknown)
	stepID := mustID(t, domain.NewStepID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_steps(id,task_id,sequence,kind,status,title)
VALUES ($1,$2,1,'tool','succeeded','Synthetic web search')`, stepID, internalTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET current_step=$2 WHERE id=$1`, internalTaskID, stepID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []task.StepStatus{task.StepSucceeded, task.StepFailed, task.StepUnknown, task.StepCancelled} {
		if _, err := f.store.pool.Exec(ctx, `UPDATE task_steps SET status=$2 WHERE id=$1`, stepID, string(status)); err != nil {
			t.Fatal(err)
		}
		assertStatus(status)
	}
	receipt, err := service.FindMessageByClientID(ctx, principal, f.conversation.ID, "tool-activity-original-001", f.instanceID)
	if err != nil || len(receipt.Items) != 1 || receipt.Items[0].ToolActivity == nil || receipt.Items[0].ToolActivity.Status != task.StepCancelled {
		t.Fatalf("receipt lookup did not project the original activity: err=%v", err)
	}
	if receipt.Items[0].TaskID != "" || receipt.Items[0].RelatedTaskID != "" {
		t.Fatal("original receipt exposed an internal Task ID")
	}
	duplicateReceipt, err := service.FindMessageByClientID(ctx, principal, f.conversation.ID, "tool-activity-ordinary-001", f.instanceID)
	if err != nil || len(duplicateReceipt.Items) != 1 || duplicateReceipt.Items[0].RelatedTaskID != "" || duplicateReceipt.Items[0].ToolActivity != nil {
		t.Fatalf("duplicate receipt exposed internal metadata: err=%v", err)
	}
	other := createPreRetirementTestUser(t, f.store, 2, 2000, 4096)
	if _, err := f.store.ListMessages(ctx, other.ID, f.conversation.ID, app.PageRequest{Limit: 20}); err == nil {
		t.Fatal("another user could read the conversation")
	}
	if _, found, err := f.store.FindConversationMessageByClientID(ctx, other.ID, "", f.conversation.ID,
		"tool-activity-original-001", f.instanceID); err != nil || found {
		t.Fatalf("another user could read the internal tool receipt: found=%t err=%v", found, err)
	}
	if _, err := service.FindMessageByClientID(ctx, principal, "c7654321", "tool-activity-original-001", f.instanceID); err == nil {
		t.Fatal("receipt lookup crossed conversation scope")
	}
	list, err := f.store.ListTasks(ctx, f.ownerID, app.TaskListRequest{Page: app.PageRequest{Limit: 20}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range list.Items {
		if value.ID == internalTaskID {
			t.Fatal("internal tool Task appeared in the user task list")
		}
	}
	tasks, err := app.NewTaskService(f.store, authorization, authorization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Get(ctx, principal, internalTaskID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("internal Task detail was exposed: %v", err)
	}
	if _, err := tasks.GetStepDetail(ctx, principal, internalTaskID, stepID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("internal Task step detail was exposed: %v", err)
	}
	if _, err := tasks.ListEvents(ctx, principal, internalTaskID, 0, 20); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("internal Task events were exposed: %v", err)
	}
}

func TestPostgresProjectConversationToolActivityIsAuthorScoped(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	clientID := "project-tool-activity-001"
	digest := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.owner.UserID, ActorID: f.member.UserID,
			ConversationID: f.conversationID, Role: conversation.RoleUser, Content: "请浏览公开网站的文章"},
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: digest[:],
		Input: app.ConversationRoutingInput{ProjectID: f.projectID, ModelID: "p1234567",
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "project-tool-activity", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim project route: found=%t err=%v", found, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteTool, ToolName: "morde_web_read",
		ToolArguments: `{"url":"https://example.org/"}`,
		ModelOutput:   `{"action":"tool","tool_name":"morde_web_read","tool_arguments":{"url":"https://example.org/"}}`}
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var internalTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_route_internal_tool_calls WHERE message_id=$1`, message.ID).
		Scan(&internalTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET task_id=$2 WHERE id=$1`, message.ID, internalTaskID); err != nil {
		t.Fatal(err)
	}
	duplicateClientID := "project-tool-activity-duplicate-001"
	duplicateDigest := sha256.Sum256([]byte(duplicateClientID))
	duplicate, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.owner.UserID, ActorID: f.member.UserID,
			ConversationID: f.conversationID, Role: conversation.RoleUser, Content: "duplicate"},
		ServiceInstanceID: f.instanceID, ClientID: duplicateClientID, RequestHash: duplicateDigest[:],
		Input: app.ConversationRoutingInput{ProjectID: f.projectID, ModelID: "p1234567",
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET duplicate_of_message_id=$2 WHERE message_id=$1`,
		duplicate.ID, message.ID); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []identity.Principal{f.member, f.owner} {
		page, err := f.store.ListProjectConversationMessages(ctx, actor.UserID, f.projectID, f.conversationID, app.PageRequest{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		projected := toolActivityMessage(t, page, message.ID)
		if projected.TaskID != "" || projected.RelatedTaskID != "" {
			t.Fatal("project message exposed the internal Task ID")
		}
		duplicateProjection := toolActivityMessage(t, page, duplicate.ID)
		if duplicateProjection.RelatedTaskID != "" || duplicateProjection.TaskID != "" || duplicateProjection.ToolActivity != nil {
			t.Fatal("project duplicate exposed internal Task metadata")
		}
		activity := projected.ToolActivity
		if actor.UserID == f.member.UserID {
			if activity == nil || activity.Tool != "web_read" || activity.Status != task.StepQueued {
				t.Fatalf("message author activity = %+v", activity)
			}
		} else if activity != nil {
			t.Fatal("another project member saw the author's internal tool activity")
		}
	}
	if _, err := f.store.ListProjectConversationMessages(ctx, f.outsider.UserID, f.projectID, f.conversationID, app.PageRequest{Limit: 20}); err == nil {
		t.Fatal("project outsider could read the conversation")
	}
	projects, err := app.NewProjectService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	authorization := projectConversationTestAuthorization{}
	tasks, err := app.NewTaskService(f.store, authorization, authorization, projects)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewProjectConversationService(f.store, projects, tasks)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.FindMessageByClientID(ctx, f.member, f.projectID, f.conversationID, clientID, f.instanceID)
	if err != nil || len(receipt.Items) != 1 || receipt.Items[0].ToolActivity == nil || receipt.Items[0].ToolActivity.Tool != "web_read" ||
		receipt.Items[0].TaskID != "" || receipt.Items[0].RelatedTaskID != "" {
		t.Fatalf("project author's receipt lookup lost activity: err=%v", err)
	}
	duplicateReceipt, err := service.FindMessageByClientID(ctx, f.member, f.projectID, f.conversationID, duplicateClientID, f.instanceID)
	if err != nil || len(duplicateReceipt.Items) != 1 || duplicateReceipt.Items[0].TaskID != "" ||
		duplicateReceipt.Items[0].RelatedTaskID != "" || duplicateReceipt.Items[0].ToolActivity != nil {
		t.Fatalf("project duplicate receipt exposed internal metadata: err=%v", err)
	}
	otherReceipt, err := service.FindMessageByClientID(ctx, f.owner, f.projectID, f.conversationID, clientID, f.instanceID)
	if err != nil || len(otherReceipt.Items) != 0 {
		t.Fatalf("another project member read the author's receipt: err=%v", err)
	}
	if _, err := tasks.Get(ctx, f.member, internalTaskID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("internal project Task detail was exposed: %v", err)
	}
	if _, err := tasks.GetStepDetail(ctx, f.member, internalTaskID, mustID(t, domain.NewStepID)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("internal project Task step detail was exposed: %v", err)
	}
	if _, err := tasks.ListEvents(ctx, f.member, internalTaskID, 0, 20); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("internal project Task events were exposed: %v", err)
	}
}
