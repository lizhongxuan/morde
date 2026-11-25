package postgres

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type routeUpdateClarificationFixture struct {
	routeActionsTestFixture
	taskID        string
	claim         app.ConversationRouteClaim
	clarification task.ClarificationRequest
	workspace     *workspace.Workspace
	modelClient   *routeModelClientTest
}

func newRouteUpdateClarificationFixture(t *testing.T, withWorkspace bool) routeUpdateClarificationFixture {
	t.Helper()
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	scopes := []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit), string(identity.ScopeTaskControl)}
	f.authorization.Scopes = scopes
	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	initialIntent := task.FileDeliveryNone
	if withWorkspace {
		initialIntent = task.FileDeliveryRequired
	}
	client := &routeModelClientTest{reply: `{"action":"new","task_id":"","file_delivery_intent":"` + string(initialIntent) + `"}`}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.Seal([]byte("route clarification integration key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_connections SET base_url='https://model.example/v1',model_name='test-model',
api_key_cipher=$2,checked_at=clock_timestamp(),capability_text=true,capability_tools=false,context_window_tokens=32768,
context_window_override_tokens=32768,context_window_source='legacy_manual',context_window_resolved_at=NULL
WHERE id='p1234567' AND owner_id=$1`, f.ownerID, ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,50000,2)`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	modelConnections, err := app.NewModelConnectionService(f.store, sealer, client, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(f.store, modelConnections, client)
	if err != nil {
		t.Fatal(err)
	}
	input := app.ConversationRoutingInput{}
	var selected *workspace.Workspace
	if withWorkspace {
		value := createTestWorkspace(t, ctx, f.store, f.ownerID, "route clarification snapshot")
		selected = &value
		page, err := f.store.GetWorkspaceManifestPageForOwner(ctx, f.ownerID, value.ID, value.Revision, "", "", 1000)
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
		revision := value.Revision
		input.WorkspaceID, input.WorkspaceInputRevision, input.WorkspaceContextDigest = value.ID, &revision, digest
		initialIntent = task.FileDeliveryRequired
	}
	initialClaim := f.receive(t, "route-update-clarification-seed", "整理这项工作。", input)
	initialSnapshot, initialDecision := classifyFileDeliveryRoute(t, f.store, gateway, initialClaim)
	if initialDecision.InvalidOutput || initialDecision.Action != app.ConversationRouteNew || initialDecision.FileDeliveryIntent != initialIntent {
		t.Fatalf("initial route decision = %+v", initialDecision)
	}
	if err := f.store.ApplyConversationRoute(ctx, initialClaim, initialSnapshot, initialDecision); err != nil {
		t.Fatalf("create Task for route clarification: %v", err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initialClaim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimedTask, found, err := f.store.ClaimNextTask(ctx, "route-update-clarification-seed-worker", time.Minute)
	if err != nil || !found || claimedTask.ID != taskID {
		t.Fatalf("claim initial Task before testing its update: task=%s found=%t err=%v", claimedTask.ID, found, err)
	}
	message := receiveCausalMessage(t, f.store, initialClaim, "route-update-clarification-source", initialClaim.Message.ClientMessageID,
		"继续调整这项工作。", taskID)
	claim := claimCausalMessage(t, f.store, message.ID)
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Context.Candidates) != 1 || snapshot.Context.Candidates[0].TaskID != taskID {
		t.Fatalf("route candidates = %#v, want target Task %s", snapshot.Context.Candidates, taskID)
	}
	client.secondReply = `{"action":"update","task_id":"` + taskID + `","file_delivery_intent":"unclear"}`
	snapshot, decision := classifyFileDeliveryRoute(t, f.store, gateway, claim)
	if decision.InvalidOutput || decision.Action != app.ConversationRouteUpdate || decision.TaskID != taskID ||
		decision.FileDeliveryIntent != task.FileDeliveryUnclear {
		t.Fatalf("unclear update route decision = %+v", decision)
	}
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatalf("apply unclear Task update: %v", err)
	}
	details, err := f.store.GetTask(ctx, taskID, f.ownerID)
	if err != nil || len(details.ClarificationRequests) != 1 {
		t.Fatalf("load pending route clarification: details=%#v err=%v", details, err)
	}
	return routeUpdateClarificationFixture{routeActionsTestFixture: f, taskID: taskID, claim: claim,
		clarification: details.ClarificationRequests[0], workspace: selected, modelClient: client}
}

func completeRouteUpdateClarificationStop(t *testing.T, f routeUpdateClarificationFixture) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND status='pending'`, f.taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := f.store.ClaimNextTaskStop(ctx, "route-update-clarification-stop", time.Minute)
	if err != nil || !found || stop.Task.ID != f.taskID {
		t.Fatalf("claim route clarification Task stop: found=%t task=%s err=%v", found, stop.Task.ID, err)
	}
	if _, err := f.store.CompleteTaskStop(ctx, f.taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatalf("complete route clarification Task stop: %v", err)
	}
}

func TestConversationRouteUnclearAnswerGatesExactRevisionAndReplays(t *testing.T) {
	for _, test := range []struct {
		name           string
		withWorkspace  bool
		intent         task.FileDeliveryIntent
		staleWorkspace bool
	}{
		{name: "none without Workspace", intent: task.FileDeliveryNone},
		{name: "required with current Workspace", withWorkspace: true, intent: task.FileDeliveryRequired},
		{name: "required rejects stale Workspace", withWorkspace: true, intent: task.FileDeliveryRequired, staleWorkspace: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRouteUpdateClarificationFixture(t, test.withWorkspace)
			ctx := context.Background()
			completeRouteUpdateClarificationStop(t, f)
			if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || applied {
				t.Fatalf("unanswered route clarification allowed revision: applied=%t err=%v", applied, err)
			}
			assertConversationRouteActionTaskState(t, f.routeActionsTestFixture, f.taskID, task.StatusPaused, 0, 1)
			var routeState string
			if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_message_routes WHERE message_id=$1`, f.claim.Message.ID).Scan(&routeState); err != nil || routeState != "waiting_user" {
				t.Fatalf("route state before answer = %q err=%v, want waiting_user", routeState, err)
			}

			staleRevision := testIdempotency(f.ownerID, "task.clarification.answer", "route-update-clarification-stale-revision")
			staleRevision.ResourceID, staleRevision.RelatedID = f.taskID, f.clarification.ID
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=2 WHERE id=$1`, f.taskID); err != nil {
				t.Fatal(err)
			}
			if err := f.store.AnswerClarificationRequest(ctx, staleRevision, f.taskID, f.clarification.ID, f.clarification.Version,
				"不需要文件", task.FileDeliveryNone, ""); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("stale Task revision answer error = %v, want conflict", err)
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=1 WHERE id=$1`, f.taskID); err != nil {
				t.Fatal(err)
			}
			staleVersion := testIdempotency(f.ownerID, "task.clarification.answer", "route-update-clarification-stale-version")
			staleVersion.ResourceID, staleVersion.RelatedID = f.taskID, f.clarification.ID
			if err := f.store.AnswerClarificationRequest(ctx, staleVersion, f.taskID, f.clarification.ID, f.clarification.Version+1,
				"不需要文件", task.FileDeliveryNone, ""); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("stale clarification version answer error = %v, want conflict", err)
			}

			if test.staleWorkspace {
				if _, err := f.store.pool.Exec(ctx, `UPDATE workspaces SET revision=revision+1 WHERE id=$1`, f.workspace.ID); err != nil {
					t.Fatal(err)
				}
				record := testIdempotency(f.ownerID, "task.clarification.answer", "route-update-clarification-stale-workspace")
				record.ResourceID, record.RelatedID = f.taskID, f.clarification.ID
				if err := f.store.AnswerClarificationRequest(ctx, record, f.taskID, f.clarification.ID, f.clarification.Version,
					"需要文件", task.FileDeliveryRequired, ""); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
					t.Fatalf("required answer with stale Workspace error = %v, want Workspace conflict", err)
				}
				if _, err := f.store.pool.Exec(ctx, `UPDATE workspaces SET revision=revision-1 WHERE id=$1`, f.workspace.ID); err != nil {
					t.Fatal(err)
				}
			}

			answer := "不需要文件"
			if test.intent == task.FileDeliveryRequired {
				answer = "需要交付文件"
			}
			record := testIdempotency(f.ownerID, "task.clarification.answer", "route-update-clarification-answer-"+string(test.intent))
			record.ResourceID, record.RelatedID = f.taskID, f.clarification.ID
			if err := f.store.AnswerClarificationRequest(ctx, record, f.taskID, f.clarification.ID, f.clarification.Version,
				answer, test.intent, ""); err != nil {
				t.Fatalf("answer route clarification: %v", err)
			}
			var eventSequenceAfterAnswer int64
			if err := f.store.pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, f.taskID).Scan(&eventSequenceAfterAnswer); err != nil {
				t.Fatal(err)
			}
			if err := f.store.AnswerClarificationRequest(ctx, record, f.taskID, f.clarification.ID, f.clarification.Version,
				answer, test.intent, ""); err != nil {
				t.Fatalf("replay same route clarification answer: %v", err)
			}
			var storedIntent, clarificationStatus, routeClarificationStatus, routeStateAfterAnswer string
			var eventSequenceAfterReplay int64
			if err := f.store.pool.QueryRow(ctx, `SELECT task.file_delivery_intent,request.status,route_clarification.status,route.state,task.event_seq
FROM tasks task
JOIN task_clarification_requests request ON request.task_id=task.id AND request.id=$2
JOIN conversation_route_file_delivery_clarifications route_clarification ON route_clarification.task_id=task.id AND route_clarification.clarification_id=request.id
JOIN conversation_message_routes route ON route.message_id=route_clarification.message_id
WHERE task.id=$1`, f.taskID, f.clarification.ID).
				Scan(&storedIntent, &clarificationStatus, &routeClarificationStatus, &routeStateAfterAnswer, &eventSequenceAfterReplay); err != nil {
				t.Fatal(err)
			}
			expectedIntentBeforeApply := task.FileDeliveryNone
			if test.withWorkspace {
				expectedIntentBeforeApply = task.FileDeliveryRequired
			}
			if storedIntent != string(expectedIntentBeforeApply) || clarificationStatus != string(task.ClarificationAnswered) ||
				routeClarificationStatus != "answered" || routeStateAfterAnswer != "waiting_adjustment" ||
				eventSequenceAfterReplay != eventSequenceAfterAnswer {
				t.Fatalf("answer/replay state = intent:%s clarification:%s routeClarification:%s route:%s event_seq:%d->%d",
					storedIntent, clarificationStatus, routeClarificationStatus, routeStateAfterAnswer, eventSequenceAfterAnswer, eventSequenceAfterReplay)
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE task_revision_intents SET available_after=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND state='stopping'`, f.taskID); err != nil {
				t.Fatal(err)
			}
			if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
				t.Fatalf("apply exactly the answered route revision: applied=%t err=%v", applied, err)
			}
			var currentRevision, desiredRevision int64
			if err := f.store.pool.QueryRow(ctx, `SELECT current_revision,desired_revision,file_delivery_intent FROM tasks WHERE id=$1`, f.taskID).
				Scan(&currentRevision, &desiredRevision, &storedIntent); err != nil {
				t.Fatal(err)
			}
			if currentRevision != 1 || desiredRevision != 1 || storedIntent != string(test.intent) {
				t.Fatalf("applied revision = current:%d desired:%d intent:%s, want current=desired=1 intent=%s",
					currentRevision, desiredRevision, storedIntent, test.intent)
			}
		})
	}
}

func TestConversationRouteUnclearStopCancelsPendingAnswer(t *testing.T) {
	f := newRouteUpdateClarificationFixture(t, false)
	ctx := context.Background()
	completeRouteUpdateClarificationStop(t, f)
	record := testIdempotency(f.ownerID, "task.action.cancel", "route-update-clarification-cancel")
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		t.Fatal(err)
	}
	record.ResourceID, record.RelatedID = requestID, f.taskID
	fence := app.TaskActionRevisionFence{CurrentRevision: 0, DesiredRevision: 1}
	if _, err := f.store.RequestTaskAction(ctx, record, f.taskID, task.ActionCancel, task.StatusPaused, &fence, requestID); err != nil {
		t.Fatalf("cancel Task with pending route clarification: %v", err)
	}
	answer := testIdempotency(f.ownerID, "task.clarification.answer", "route-update-clarification-late-answer")
	answer.ResourceID, answer.RelatedID = f.taskID, f.clarification.ID
	if err := f.store.AnswerClarificationRequest(ctx, answer, f.taskID, f.clarification.ID, f.clarification.Version,
		"不需要文件", task.FileDeliveryNone, ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("late answer after Task cancellation error = %v, want conflict", err)
	}
	var taskStatus, clarificationStatus, routeClarificationStatus, routeState string
	if err := f.store.pool.QueryRow(ctx, `SELECT task.status,request.status,route_clarification.status,route.state
FROM tasks task
JOIN task_clarification_requests request ON request.task_id=task.id AND request.id=$2
JOIN conversation_route_file_delivery_clarifications route_clarification ON route_clarification.task_id=task.id AND route_clarification.clarification_id=request.id
JOIN conversation_message_routes route ON route.message_id=route_clarification.message_id
WHERE task.id=$1`, f.taskID, f.clarification.ID).
		Scan(&taskStatus, &clarificationStatus, &routeClarificationStatus, &routeState); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(task.StatusCancelling) || clarificationStatus != string(task.ClarificationCancelled) ||
		routeClarificationStatus != "cancelled" || routeState != "failed" {
		t.Fatalf("cancelled route clarification = task:%s request:%s binding:%s route:%s",
			taskStatus, clarificationStatus, routeClarificationStatus, routeState)
	}
}

func TestConversationRouteUnclearRevisionDefersAfterCredentialRevocation(t *testing.T) {
	f := newRouteUpdateClarificationFixture(t, false)
	ctx := context.Background()
	completeRouteUpdateClarificationStop(t, f)
	answer := testIdempotency(f.ownerID, "task.clarification.answer", "route-update-clarification-revoke-answer")
	answer.ResourceID, answer.RelatedID = f.taskID, f.clarification.ID
	if err := f.store.AnswerClarificationRequest(ctx, answer, f.taskID, f.clarification.ID, f.clarification.Version,
		"只需聊天答复", task.FileDeliveryNone, ""); err != nil {
		t.Fatalf("answer route clarification before revoking credential: %v", err)
	}
	makeRevisionDue := func() {
		t.Helper()
		if _, err := f.store.pool.Exec(ctx, `UPDATE task_revision_intents SET available_after=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND state='stopping'`, f.taskID); err != nil {
			t.Fatal(err)
		}
	}
	makeRevisionDue()
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || applied {
		t.Fatalf("revoked route receipt credential applied Task revision: applied=%t err=%v", applied, err)
	}
	assertConversationRouteActionTaskState(t, f.routeActionsTestFixture, f.taskID, task.StatusPaused, 0, 1)
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, f.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	makeRevisionDue()
	if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("valid restored route receipt credential did not apply Task revision: applied=%t err=%v", applied, err)
	}
	assertConversationRouteActionTaskState(t, f.routeActionsTestFixture, f.taskID, task.StatusQueued, 1, 1)
}
