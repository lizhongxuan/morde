package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func receiveCausalMessage(t *testing.T, store *Store, claim app.ConversationRouteClaim, clientID, previousID, content string, reference ...string) conversation.Message {
	t.Helper()
	input := app.ConversationRoutingInput{ProjectID: claim.Input.ProjectID, ModelID: claim.Input.ModelID, PreviousClientMessageID: previousID,
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}}
	if len(reference) != 0 {
		input.ReferenceTaskID = reference[0]
	}
	digest, err := app.ConversationReceiptHash(claim.Message.ConversationID, content, input)
	if err != nil {
		t.Fatal(err)
	}
	message, _, err := store.AcceptConversationMessage(context.Background(), app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: claim.Message.OwnerID,
			ActorID: claim.Message.ActorID, ConversationID: claim.Message.ConversationID, Role: conversation.RoleUser, Content: content},
		ServiceInstanceID: claim.ServiceInstanceID, ClientID: clientID, Input: input, RequestHash: digest, Authorization: claim.Authorization,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func claimCausalMessage(t *testing.T, store *Store, expected string) app.ConversationRouteClaim {
	t.Helper()
	claim, found, err := store.ClaimConversationRoute(context.Background(), "causal-route-worker", time.Minute)
	if err != nil || !found || claim.Message.ID != expected {
		t.Fatalf("claim causal message: id=%s found=%t err=%v", claim.Message.ID, found, err)
	}
	return claim
}

func classifyCausalMessage(t *testing.T, store *Store, gateway *app.ModelGatewayService, claim app.ConversationRouteClaim) (app.ConversationRouteSnapshot, app.ConversationRouteDecision) {
	t.Helper()
	ctx := context.Background()
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID, err = gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := gateway.ClassifyConversationRoute(ctx, claim, snapshot.Context)
	if err != nil {
		t.Fatalf("classify causal message: %v", err)
	}
	return snapshot, decision
}

func TestConversationRouteLatePredecessorResumesWithSeparateAccountedClassification(t *testing.T) {
	ctx := context.Background()
	client := &routeModelClientTest{reply: `{"action":"clarify","task_id":"","question":"Need the previous message."}`}
	store, gateway, _, fixture, _ := routeModelFixture(t, false, client)
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, fixture.Message.ActorID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailConversationRoute(ctx, fixture, "routing_action_unavailable"); err != nil {
		t.Fatal(err)
	}
	dependent := receiveCausalMessage(t, store, fixture, "causal-dependent-001", "causal-previous-001", "继续上面的任务")
	claim := claimCausalMessage(t, store, dependent.ID)
	snapshot, decision := classifyCausalMessage(t, store, gateway, claim)
	if !snapshot.PreviousMissing || !snapshot.Context.PreviousMissing || decision.Action != app.ConversationRouteClarify {
		t.Fatalf("missing predecessor snapshot=%+v decision=%+v", snapshot, decision)
	}
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var awaiting bool
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state,awaiting_predecessor FROM conversation_message_routes WHERE message_id=$1`, dependent.ID).
		Scan(&state, &awaiting); err != nil || state != "waiting_user" || !awaiting {
		t.Fatalf("missing predecessor did not wait: state=%q awaiting=%t err=%v", state, awaiting, err)
	}
	independent := receiveCausalMessage(t, store, fixture, "causal-descendant-001", "causal-dependent-001", "另外写一首春天的短诗")
	independentClaim := claimCausalMessage(t, store, independent.ID)
	client.reply = `{"action":"new","task_id":"","question":""}`
	independentSnapshot, independentDecision := classifyCausalMessage(t, store, gateway, independentClaim)
	if err := store.ApplyConversationRoute(ctx, independentClaim, independentSnapshot, independentDecision); err != nil {
		t.Fatal(err)
	}
	var independentTaskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, independent.ID).Scan(&independentTaskID); err != nil {
		t.Fatal(err)
	}
	previous := receiveCausalMessage(t, store, fixture, "causal-previous-001", "", "整理一份本地合成报告")
	previousClaim := claimCausalMessage(t, store, previous.ID)
	beforePrevious, err := store.LoadConversationRouteSnapshot(ctx, previousClaim)
	if err != nil {
		t.Fatal(err)
	}
	for _, history := range beforePrevious.Context.History {
		if history.MessageID == dependent.ID || history.MessageID == independent.ID {
			t.Fatal("a direct or transitive descendant leaked into its predecessor's history")
		}
	}
	for _, candidate := range beforePrevious.Context.Candidates {
		if candidate.TaskID == independentTaskID {
			t.Fatal("a causally later Task leaked into its predecessor's candidates")
		}
	}
	client.reply = `{"action":"new","task_id":"","question":""}`
	previousSnapshot, previousDecision := classifyCausalMessage(t, store, gateway, previousClaim)
	if err := store.ApplyConversationRoute(ctx, previousClaim, previousSnapshot, previousDecision); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, previous.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	future := receiveCausalMessage(t, store, fixture, "causal-unrelated-001", "", "另外一条后到的消息")
	resumed := claimCausalMessage(t, store, dependent.ID)
	client.reply = fmt.Sprintf(`{"action":"status","task_id":%q,"question":""}`, taskID)
	snapshot, decision = classifyCausalMessage(t, store, gateway, resumed)
	if snapshot.PreviousMissing || snapshot.PreviousMessageID != previous.ID || snapshot.PreviousSequence <= dependent.Sequence ||
		decision.Action != app.ConversationRouteStatus || decision.TaskID != taskID {
		t.Fatalf("late predecessor not used as the causal target: snapshot=%+v decision=%+v", snapshot, decision)
	}
	seenPrevious := false
	for _, history := range snapshot.Context.History {
		if history.MessageID == previous.ID {
			seenPrevious = true
		}
		if history.MessageID == future.ID {
			t.Fatal("unrelated future message leaked into the routing context")
		}
	}
	if !seenPrevious {
		t.Fatal("exact late predecessor was absent from the routing context")
	}
	if err := store.ApplyConversationRoute(ctx, resumed, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var calls, settled int
	if err := store.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE state='settled')
FROM conversation_route_model_calls WHERE message_id=$1`, dependent.ID).Scan(&calls, &settled); err != nil || calls != 2 || settled != 2 {
		t.Fatalf("causal classifications were not independently settled: calls=%d settled=%d err=%v", calls, settled, err)
	}
}

func TestConversationRouteMissingPredecessorAllowsOnlyVerifiedIndependentWork(t *testing.T) {
	ctx := context.Background()
	client := &routeModelClientTest{}
	store, gateway, _, fixture, _ := routeModelFixture(t, false, client)
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, fixture.Message.ActorID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailConversationRoute(ctx, fixture, "routing_action_unavailable"); err != nil {
		t.Fatal(err)
	}
	message := receiveCausalMessage(t, store, fixture, "causal-independent-001", "causal-never-arrived", "写一首关于春天的短诗")
	claim := claimCausalMessage(t, store, message.ID)
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || !snapshot.PreviousMissing {
		t.Fatalf("missing predecessor not detected: %+v %v", snapshot, err)
	}
	snapshot.Context.ModelID = claim.Input.ModelID
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err == nil {
		t.Fatal("unclassified work created a Task with an unknown predecessor")
	}
	snapshot, decision := classifyCausalMessage(t, store, gateway, claim)
	if decision.Action != app.ConversationRouteNew {
		t.Fatalf("independence classification = %+v", decision)
	}
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(task_id,'') FROM conversation_messages WHERE id=$1`, message.ID).Scan(&taskID); err != nil || taskID == "" {
		t.Fatalf("independent request did not create a task: %q %v", taskID, err)
	}
}

func TestConversationRouteRejectsCyclicPredecessors(t *testing.T) {
	ctx := context.Background()
	store, _, _, fixture, _ := routeModelFixture(t, false, &routeModelClientTest{})
	if err := store.FailConversationRoute(ctx, fixture, "routing_action_unavailable"); err != nil {
		t.Fatal(err)
	}
	first := receiveCausalMessage(t, store, fixture, "causal-cycle-first", "causal-cycle-second", "第一条")
	receiveCausalMessage(t, store, fixture, "causal-cycle-second", "causal-cycle-first", "第二条")
	claim := claimCausalMessage(t, store, first.ID)
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || !snapshot.PreviousCycle {
		t.Fatalf("cyclic predecessor was not detected: %+v %v", snapshot, err)
	}
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err == nil {
		t.Fatal("cyclic predecessor created a Task")
	}
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteClarify, Question: "消息顺序存在循环，请重新发送。"}); err != nil {
		t.Fatal(err)
	}
	var state string
	var awaiting bool
	if err := store.pool.QueryRow(ctx, `SELECT state,awaiting_predecessor FROM conversation_message_routes WHERE message_id=$1`, first.ID).
		Scan(&state, &awaiting); err != nil || state != "waiting_user" || awaiting {
		t.Fatalf("cycle state=%q awaiting=%t err=%v", state, awaiting, err)
	}
}

func TestConversationRouteCausalityMigrationPreservesAndProtectsLedger(t *testing.T) {
	ctx := context.Background()
	client := &routeModelClientTest{}
	store, gateway, _, fixture, value := routeModelFixture(t, false, client)
	if _, err := gateway.ClassifyConversationRoute(ctx, fixture, value); err != nil {
		t.Fatal(err)
	}
	provider, closeProvider, err := newMigrationProvider(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 159); err != nil {
		t.Fatalf("roll back ordinary route history: %v", err)
	}
	if _, err := provider.UpTo(ctx, 160); err != nil {
		t.Fatalf("restore causal routing migration: %v", err)
	}
	var phase, state string
	if err := store.pool.QueryRow(ctx, `SELECT phase,state FROM conversation_route_model_calls WHERE message_id=$1`, fixture.Message.ID).
		Scan(&phase, &state); err != nil || phase != "normal" || state != "settled" {
		t.Fatalf("historical route evidence changed: phase=%q state=%q err=%v", phase, state, err)
	}
	missing := receiveCausalMessage(t, store, fixture, "migration-missing-message", "migration-previous-message", "全新独立的问题")
	claim := claimCausalMessage(t, store, missing.ID)
	_, decision := classifyCausalMessage(t, store, gateway, claim)
	if decision.Action != app.ConversationRouteNew {
		t.Fatalf("independence decision=%+v", decision)
	}
	if _, err := provider.DownTo(ctx, 159); err == nil || !strings.Contains(err.Error(), "cannot remove causal routing") {
		t.Fatalf("rollback discarded independence ledger: %v", err)
	}
}

func TestConversationRouteUnknownIndependenceCallCannotDispatchNextPhase(t *testing.T) {
	ctx := context.Background()
	client := &routeModelClientTest{err: errors.New("synthetic transport failure")}
	store, gateway, _, fixture, _ := routeModelFixture(t, false, client)
	if err := store.FailConversationRoute(ctx, fixture, "routing_action_unavailable"); err != nil {
		t.Fatal(err)
	}
	message := receiveCausalMessage(t, store, fixture, "causal-unknown-message", "causal-unknown-previous", "请继续")
	claim := claimCausalMessage(t, store, message.ID)
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || !snapshot.PreviousMissing {
		t.Fatalf("missing predecessor snapshot=%+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = claim.Input.ModelID
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, snapshot.Context); err == nil {
		t.Fatal("synthetic unknown provider result was accepted")
	}
	receiveCausalMessage(t, store, fixture, "causal-unknown-previous", "", "上一条内容")
	snapshot, err = store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || snapshot.PreviousMissing {
		t.Fatalf("late predecessor snapshot=%+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = claim.Input.ModelID
	client.err = nil
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, snapshot.Context); domain.CodeOf(err) != domain.CodeInvalidState || client.calls.Load() != 1 {
		t.Fatalf("unknown independence call was dispatched again: calls=%d err=%v", client.calls.Load(), err)
	}
}

func TestConversationRoutePredecessorArrivalFencesModelDispatch(t *testing.T) {
	for _, scenario := range []string{"reserved", "begin_conflict", "dispatched", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			client := &routeModelClientTest{}
			store, _, _, fixture, _ := routeModelFixture(t, true, client)
			if err := store.FailConversationRoute(ctx, fixture, "routing_action_unavailable"); err != nil {
				t.Fatal(err)
			}
			message := receiveCausalMessage(t, store, fixture, "race-dependent-message", "race-previous-message", "请继续")
			claim := claimCausalMessage(t, store, message.ID)
			snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil || !snapshot.PreviousMissing {
				t.Fatalf("missing predecessor snapshot=%+v err=%v", snapshot, err)
			}
			snapshot.Context.ModelID = claim.Input.ModelID
			call := prepareRouteTestReservation(t, store, claim, snapshot.Context)
			if scenario == "dispatched" || scenario == "unknown" {
				if err := store.BeginConversationRouteModelCall(ctx, call); err != nil {
					t.Fatal(err)
				}
			}
			receiveCausalMessage(t, store, fixture, "race-previous-message", "", "上一条合成消息")
			if scenario == "begin_conflict" {
				if err := store.BeginConversationRouteModelCall(ctx, call); domain.CodeOf(err) != domain.CodeConflict {
					t.Fatalf("changed predecessor did not fence model dispatch: %v", err)
				}
			}
			if scenario == "unknown" {
				if err := store.SettleConversationRouteModelCall(ctx, call, nil, app.ModelUsage{}, true); err != nil {
					t.Fatal(err)
				}
			}
			requeued, err := store.RequeueConversationRouteAfterPredecessor(ctx, claim, snapshot)
			wantRequeued := scenario == "reserved" || scenario == "begin_conflict"
			if err != nil || requeued != wantRequeued || client.calls.Load() != 0 {
				t.Fatalf("predecessor race requeued=%t want=%t calls=%d err=%v", requeued, wantRequeued, client.calls.Load(), err)
			}
			var routeState string
			var calls, account, projectCalls int
			if err := store.pool.QueryRow(ctx, `SELECT r.state,
 (SELECT count(*) FROM conversation_route_model_calls c WHERE c.message_id=r.message_id),
 (SELECT count(*) FROM model_call_ledger l WHERE l.source='route' AND l.call_id=$2),
 (SELECT count(*) FROM project_budget_ledger p WHERE p.source='route' AND p.call_id=$2)
FROM conversation_message_routes r WHERE r.message_id=$1`, message.ID, call.CallID).
				Scan(&routeState, &calls, &account, &projectCalls); err != nil {
				t.Fatal(err)
			}
			if wantRequeued && (routeState != "pending" || calls != 0 || account != 0 || projectCalls != 0) {
				t.Fatalf("undispatched reservation was not released: state=%s route=%d account=%d project=%d", routeState, calls, account, projectCalls)
			}
			if !wantRequeued && (routeState != "processing" || calls != 1 || account != 1 || projectCalls != 1) {
				t.Fatalf("dispatched evidence was discarded: state=%s route=%d account=%d project=%d", routeState, calls, account, projectCalls)
			}
		})
	}
}

func TestConversationRouteLateRevisionsReachRealNodeInCausalOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &routeModelClientTest{}
	store, gateway, _, fixture, _ := routeModelFixture(t, false, client)
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, fixture.Message.ActorID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailConversationRoute(ctx, fixture, "routing_action_unavailable"); err != nil {
		t.Fatal(err)
	}
	seed := receiveCausalMessage(t, store, fixture, "causal-revision-seed", "", "整理收入报告")
	seedClaim := claimCausalMessage(t, store, seed.ID)
	seedSnapshot, seedDecision := classifyCausalMessage(t, store, gateway, seedClaim)
	if err := store.ApplyConversationRoute(ctx, seedClaim, seedSnapshot, seedDecision); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, seed.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	second := receiveCausalMessage(t, store, fixture, "causal-revision-second", "causal-revision-first", "再只看净利润", taskID)
	secondClaim := claimCausalMessage(t, store, second.ID)
	client.reply = `{"action":"clarify","task_id":"","question":"Wait for the previous update."}`
	secondSnapshot, secondDecision := classifyCausalMessage(t, store, gateway, secondClaim)
	if err := store.ApplyConversationRoute(ctx, secondClaim, secondSnapshot, secondDecision); err != nil {
		t.Fatal(err)
	}
	first := receiveCausalMessage(t, store, fixture, "causal-revision-first", "", "只看收入", taskID)
	firstClaim := claimCausalMessage(t, store, first.ID)
	client.reply = fmt.Sprintf(`{"action":"update","task_id":%q,"question":""}`, taskID)
	firstSnapshot, firstDecision := classifyCausalMessage(t, store, gateway, firstClaim)
	if firstDecision.Action != app.ConversationRouteUpdate {
		t.Fatalf("first revision decision=%+v", firstDecision)
	}
	if err := store.ApplyConversationRoute(ctx, firstClaim, firstSnapshot, firstDecision); err != nil {
		t.Fatal(err)
	}
	secondClaim = claimCausalMessage(t, store, second.ID)
	secondSnapshot, secondDecision = classifyCausalMessage(t, store, gateway, secondClaim)
	if secondSnapshot.PreviousRoutingState != "waiting_adjustment" || secondDecision.Action != app.ConversationRouteUpdate || secondDecision.ExpectedRevision != 1 {
		t.Fatalf("second causal revision snapshot=%+v decision=%+v", secondSnapshot, secondDecision)
	}
	if err := store.ApplyConversationRoute(ctx, secondClaim, secondSnapshot, secondDecision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := store.ClaimNextTaskStop(ctx, "causal-stop-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim causal stop: found=%t err=%v", found, err)
	}
	if _, err := store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("apply causal revisions: applied=%t err=%v", applied, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "causal-task-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim causal Task: found=%t err=%v", found, err)
	}
	model := &revisionBlockingModel{started: make(chan struct{})}
	executor := revisionTestExecutor(t, store, model)
	done := make(chan error, 1)
	go func() { done <- executor.ExecuteTask(ctx, claimed) }()
	select {
	case <-model.started:
	case err := <-done:
		t.Fatalf("causal Agent exited before model: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("causal Agent did not reach the local model")
	}
	model.mu.Lock()
	raw := append([]byte(nil), model.calls[0].RawRequest...)
	model.mu.Unlock()
	var request struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	var ordered []string
	for _, message := range request.Messages {
		if message.ID == first.ID || message.ID == second.ID {
			ordered = append(ordered, message.ID)
		}
	}
	if len(ordered) != 2 || ordered[0] != first.ID || ordered[1] != second.ID {
		t.Fatalf("causal revisions reached the model in the wrong order: %v", ordered)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("causal Agent did not stop")
	}
}

func TestConversationRouteLateRevisionPreservesInitialTaskContext(t *testing.T) {
	ctx := context.Background()
	client := &routeModelClientTest{reply: `{"action":"clarify","task_id":"","question":"Wait for the previous message."}`}
	store, gateway, _, fixture, _ := routeModelFixture(t, false, client)
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, fixture.Message.ActorID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailConversationRoute(ctx, fixture, "routing_action_unavailable"); err != nil {
		t.Fatal(err)
	}
	revision := receiveCausalMessage(t, store, fixture, "late-root-revision", "late-root-original", "只看收入")
	revisionClaim := claimCausalMessage(t, store, revision.ID)
	snapshot, decision := classifyCausalMessage(t, store, gateway, revisionClaim)
	if err := store.ApplyConversationRoute(ctx, revisionClaim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	original := receiveCausalMessage(t, store, fixture, "late-root-original", "", "整理报告")
	originalClaim := claimCausalMessage(t, store, original.ID)
	client.reply = `{"action":"new","task_id":"","question":""}`
	snapshot, decision = classifyCausalMessage(t, store, gateway, originalClaim)
	if err := store.ApplyConversationRoute(ctx, originalClaim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, original.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	revisionClaim = claimCausalMessage(t, store, revision.ID)
	client.reply = fmt.Sprintf(`{"action":"update","task_id":%q,"question":""}`, taskID)
	snapshot, decision = classifyCausalMessage(t, store, gateway, revisionClaim)
	if decision.Action != app.ConversationRouteUpdate {
		t.Fatalf("late revision decision=%+v", decision)
	}
	if err := store.ApplyConversationRoute(ctx, revisionClaim, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := store.ClaimNextTaskStop(ctx, "late-root-stop", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim stopped revision: found=%t err=%v", found, err)
	}
	if _, err := store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("apply late revision: applied=%t err=%v", applied, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "late-root-task", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.ConversationContext == nil {
		t.Fatalf("late revision invalidated original context: found=%t task=%s err=%v", found, claimed.ID, err)
	}
	for _, message := range claimed.ConversationContext.Messages {
		if message.ID == revision.ID {
			t.Fatal("later revision leaked into the original Task context")
		}
	}
}
