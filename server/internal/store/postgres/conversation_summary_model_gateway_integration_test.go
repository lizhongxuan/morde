package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func prepareConversationSummaryModelFence(t *testing.T, store *Store, claim app.ConversationRouteClaim) (app.ConversationSummaryModelFence, string) {
	t.Helper()
	ctx := context.Background()
	if _, err := app.ConversationSummaryScopeForClaim(claim); err != nil {
		t.Fatal(err)
	}
	head, err := store.ReadConversationSummaryHead(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	operationID := mustID(t, domain.NewOperationID)
	inputDigest := sha256.Sum256([]byte("authorized conversation summary input"))
	operation, _, err := store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: operationID, InputSHA256: inputDigest[:], BaselineVersion: head.Version,
		LeaseOwner: "summary-model-test-worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := operation.Identity
	checkpointDigest := sha256.Sum256([]byte("summary model checkpoint"))
	if _, err := store.SaveConversationSummaryCheckpoint(ctx, app.ConversationSummaryCheckpoint{
		Claim: claim, OperationID: operationID, LeaseOwner: operation.LeaseOwner, LeaseVersion: operation.LeaseVersion,
		SessionID: "summary-session-01", Through: claim.Message.Sequence + 100, SHA256: checkpointDigest[:],
	}); err != nil {
		t.Fatal(err)
	}
	fence := app.ConversationSummaryModelFence{Claim: claim, Identity: identity, OperationID: operationID,
		InputSHA256: inputDigest[:], LeaseOwner: operation.LeaseOwner, LeaseVersion: operation.LeaseVersion,
		SessionID: "summary-session-01", ThroughSequence: claim.Message.Sequence + 100, BaselineVersion: head.Version,
		ContextWindowTokens: 32_768}
	callID, err := app.DSHConversationSummaryModelCallID(claim.Message.ConversationID, operationID,
		fence.SessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	return fence, callID
}

func summaryModelRequest() app.ModelRequest {
	return app.ModelRequest{Instructions: "Create a concise factual summary.",
		Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "Summarize the authorized conversation history."}},
		MaxOutputTokens: 64}
}

func summaryModelGateway(t *testing.T, gateway *app.ModelGatewayService) *app.ModelGatewayService {
	t.Helper()
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetConversationSummarySealer(sealer); err != nil {
		t.Fatal(err)
	}
	return gateway
}

func TestConversationSummaryModelGatewaySettlesAndRecoversAccountAndProjectCalls(t *testing.T) {
	for _, useProject := range []bool{false, true} {
		name := "account"
		if useProject {
			name = "project"
		}
		t.Run(name, func(t *testing.T) {
			client := &routeModelClientTest{reply: "The user prefers morning meetings."}
			store, gateway, _, claim, routeContext := routeModelFixture(t, useProject, client)
			gateway = summaryModelGateway(t, gateway)
			fence, callID := prepareConversationSummaryModelFence(t, store, claim)
			request := summaryModelRequest()
			response, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, request)
			if err != nil || response.Text != client.reply || client.calls.Load() != 1 {
				t.Fatalf("summary model call = %+v calls=%d err=%v", response, client.calls.Load(), err)
			}
			var state string
			var personal bool
			if err := store.pool.QueryRow(context.Background(), `SELECT state,personal_model FROM model_call_ledger
WHERE owner_id=$1 AND source='compaction' AND call_id=$2`, claim.Message.ActorID, callID).Scan(&state, &personal); err != nil || state != "settled" || personal {
				t.Fatalf("summary account ledger = %s personal=%t err=%v", state, personal, err)
			}
			var ciphertext []byte
			if err := store.pool.QueryRow(context.Background(), `SELECT response_ciphertext FROM conversation_compaction_operations
WHERE service_instance_id=$1::uuid AND operation_id=$2`, claim.ServiceInstanceID, fence.OperationID).Scan(&ciphertext); err != nil ||
				len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(client.reply)) {
				t.Fatalf("summary result was not stored only as ciphertext: bytes=%d err=%v", len(ciphertext), err)
			}
			if useProject {
				var status string
				if err := store.pool.QueryRow(context.Background(), `SELECT status FROM project_budget_ledger
WHERE project_id=$1 AND source='compaction' AND call_id=$2`, claim.Input.ProjectID, callID).Scan(&status); err != nil || status != "settled" {
					t.Fatalf("summary project ledger = %s err=%v", status, err)
				}
			}
			replayed, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, request)
			if err != nil || replayed.Text != client.reply || client.calls.Load() != 1 {
				t.Fatalf("summary result recovery = %+v calls=%d err=%v", replayed, client.calls.Load(), err)
			}
			changed := request
			changed.Input = append([]app.ModelInput(nil), request.Input...)
			changed.Input[0].Text = "Different authorized input"
			if _, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict || client.calls.Load() != 1 {
				t.Fatalf("changed request replay error=%v calls=%d", err, client.calls.Load())
			}
		})
	}
}

func TestConversationSummaryModelGatewayPersonalModelNeedsNoPlatformBudget(t *testing.T) {
	client := &routeModelClientTest{reply: "Private summary."}
	store, _, principal, claim, _ := routeModelFixture(t, false, client)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	models, err := app.NewModelConnectionService(store, sealer, client, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	contextWindow := 32_768
	created, err := models.Create(context.Background(), principal, app.ModelConnectionCommand{
		Name: "summary personal model", BaseURL: "https://model.example/v1", ModelName: "personal-summary-model", APIKey: "synthetic-key",
		ContextWindowTokens: &contextWindow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(context.Background(), `UPDATE model_connections SET capability_text=true,checked_at=clock_timestamp()
WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(context.Background(), `DELETE FROM model_account_budgets WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, models, client)
	if err != nil {
		t.Fatal(err)
	}
	gateway = summaryModelGateway(t, gateway)
	fence, callID := prepareConversationSummaryModelFence(t, store, claim)
	if _, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, created.ID, summaryModelRequest()); err != nil {
		t.Fatal(err)
	}
	var state string
	var personal bool
	if err := store.pool.QueryRow(context.Background(), `SELECT state,personal_model FROM model_call_ledger
WHERE owner_id=$1 AND source='compaction' AND call_id=$2`, principal.UserID, callID).Scan(&state, &personal); err != nil || state != "settled" || !personal {
		t.Fatalf("personal summary ledger = %s personal=%t err=%v", state, personal, err)
	}
}

func TestConversationSummaryModelUnknownUsageRequiresAdminReconciliation(t *testing.T) {
	client := &routeModelClientTest{err: errors.New("synthetic timeout")}
	store, gateway, principal, claim, routeContext := routeModelFixture(t, true, client)
	gateway = summaryModelGateway(t, gateway)
	fence, callID := prepareConversationSummaryModelFence(t, store, claim)
	request := summaryModelRequest()
	if _, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, request); err == nil {
		t.Fatal("timed out summary model call unexpectedly succeeded")
	}
	var ledgerState, operationState string
	if err := store.pool.QueryRow(context.Background(), `SELECT l.state,o.state FROM model_call_ledger l
JOIN conversation_compaction_operations o ON o.actor_id=l.owner_id AND o.model_call_id=l.call_id
WHERE l.owner_id=$1 AND l.source='compaction' AND l.call_id=$2`, principal.UserID, callID).Scan(&ledgerState, &operationState); err != nil ||
		ledgerState != "unknown" || operationState != "unknown" {
		t.Fatalf("unknown summary call states = %s/%s err=%v", ledgerState, operationState, err)
	}
	var projectState string
	if err := store.pool.QueryRow(context.Background(), `SELECT status FROM project_budget_ledger
WHERE project_id=$1 AND source='compaction' AND call_id=$2`, claim.Input.ProjectID, callID).Scan(&projectState); err != nil || projectState != "unknown" {
		t.Fatalf("unknown summary project state = %s err=%v", projectState, err)
	}
	if _, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, request); domain.CodeOf(err) != domain.CodeConflict || client.calls.Load() != 1 {
		t.Fatalf("unknown summary call was redispatched: calls=%d err=%v", client.calls.Load(), err)
	}
	admin, err := app.NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := admin.ListUnknown(context.Background(), principal, principal.UserID, 10)
	if err != nil || len(unknown) != 1 || unknown[0].Source != app.ModelCallCompaction || unknown[0].CallID != callID {
		t.Fatalf("unknown summary usage = %+v err=%v", unknown, err)
	}
	command := app.ModelUsageReconcileCommand{OwnerID: principal.UserID, Source: app.ModelCallCompaction, CallID: callID,
		InputTokens: 8, OutputTokens: 3, Evidence: "synthetic provider usage record"}
	if replayed, err := admin.Reconcile(context.Background(), principal, command); err != nil || replayed {
		t.Fatalf("reconcile unknown summary usage replayed=%t err=%v", replayed, err)
	}
	if replayed, err := admin.Reconcile(context.Background(), principal, command); err != nil || !replayed {
		t.Fatalf("replay summary usage reconciliation replayed=%t err=%v", replayed, err)
	}
	if err := store.pool.QueryRow(context.Background(), `SELECT status FROM project_budget_ledger
WHERE project_id=$1 AND source='compaction' AND call_id=$2`, claim.Input.ProjectID, callID).Scan(&projectState); err != nil || projectState != "settled" {
		t.Fatalf("reconciled summary project state = %s err=%v", projectState, err)
	}
	if _, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, request); domain.CodeOf(err) != domain.CodeConflict || client.calls.Load() != 1 {
		t.Fatalf("reconciled summary call was redispatched: calls=%d err=%v", client.calls.Load(), err)
	}
}

func TestConversationSummaryStaleDispatchUsesSummaryReconciliation(t *testing.T) {
	client := &routeModelClientTest{started: make(chan struct{}), release: make(chan struct{}), reply: "Recovered summary."}
	defer releaseRouteTestClient(client)
	store, gateway, principal, claim, routeContext := routeModelFixture(t, false, client)
	gateway = summaryModelGateway(t, gateway)
	fence, callID := prepareConversationSummaryModelFence(t, store, claim)
	result := make(chan error, 1)
	go func() {
		_, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, summaryModelRequest())
		result <- err
	}()
	select {
	case <-client.started:
	case <-time.After(10 * time.Second):
		t.Fatal("summary provider call did not start")
	}
	if _, err := store.pool.Exec(context.Background(), `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '3 minutes'
WHERE owner_id=$1 AND source='compaction' AND call_id=$2`, principal.UserID, callID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(context.Background(), `UPDATE conversation_compaction_operations SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE actor_id=$1 AND model_call_id=$2`, principal.UserID, callID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(context.Background(), `UPDATE conversation_summary_heads h SET lease_expires_at=clock_timestamp()-interval '1 second'
FROM conversation_compaction_operations o WHERE o.actor_id=$1 AND o.model_call_id=$2
 AND h.service_instance_id=o.service_instance_id AND h.owner_id=o.owner_id AND h.actor_id=o.actor_id
 AND h.conversation_id=o.conversation_id AND h.scope_kind=o.scope_kind AND h.scope_id=o.scope_id`, principal.UserID, callID); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkStaleModelCallsUnknown(context.Background()); err != nil || marked != 0 {
		t.Fatalf("generic stale-call sweep handled compaction: marked=%d err=%v", marked, err)
	}
	unknown, err := store.ListUnknownModelCalls(context.Background(), principal, principal.UserID, 10)
	if err != nil || len(unknown) != 1 || unknown[0].Source != app.ModelCallCompaction || unknown[0].CallID != callID {
		t.Fatalf("stale summary dispatch candidate = %+v err=%v", unknown, err)
	}
	admin, err := app.NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Reconcile(context.Background(), principal, app.ModelUsageReconcileCommand{
		OwnerID: principal.UserID, Source: app.ModelCallCompaction, CallID: callID,
		InputTokens: 8, OutputTokens: 3, Evidence: "synthetic stale call record",
	}); err != nil {
		t.Fatalf("reconcile stale summary dispatch: %v", err)
	}
	releaseRouteTestClient(client)
	if err := <-result; err != nil {
		t.Fatalf("late synthetic provider completion: %v", err)
	}
	var ledgerState, operationState string
	if err := store.pool.QueryRow(context.Background(), `SELECT l.state,o.state FROM model_call_ledger l
JOIN conversation_compaction_operations o ON o.actor_id=l.owner_id AND o.model_call_id=l.call_id
WHERE l.owner_id=$1 AND l.source='compaction' AND l.call_id=$2`, principal.UserID, callID).Scan(&ledgerState, &operationState); err != nil ||
		ledgerState != "settled" || operationState != "failed" {
		t.Fatalf("reconciled stale summary states = %s/%s err=%v", ledgerState, operationState, err)
	}
}

func TestConversationSummaryModelDispatchRechecksAuthorization(t *testing.T) {
	client := &routeModelClientTest{reply: "Should not be generated."}
	store, gateway, _, claim, routeContext := routeModelFixture(t, false, client)
	gateway = summaryModelGateway(t, gateway)
	fence, _ := prepareConversationSummaryModelFence(t, store, claim)
	if _, err := store.pool.Exec(context.Background(), `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, claim.Authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, summaryModelRequest()); err == nil || client.calls.Load() != 0 {
		t.Fatalf("revoked route credential reached model provider: calls=%d err=%v", client.calls.Load(), err)
	}
	var rows int
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_call_ledger WHERE source='compaction'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("revoked summary call wrote account ledger rows=%d err=%v", rows, err)
	}
}

func TestConversationSummaryProjectBudgetRejectsBeforeDispatch(t *testing.T) {
	client := &routeModelClientTest{reply: "Should not be generated."}
	store, gateway, _, claim, routeContext := routeModelFixture(t, true, client)
	gateway = summaryModelGateway(t, gateway)
	if _, err := store.pool.Exec(context.Background(), `UPDATE project_budgets SET limit_micros=1 WHERE project_id=$1`, claim.Input.ProjectID); err != nil {
		t.Fatal(err)
	}
	fence, _ := prepareConversationSummaryModelFence(t, store, claim)
	if _, err := gateway.CompleteConversationSummaryModel(context.Background(), fence, routeContext.ModelID, summaryModelRequest()); domain.CodeOf(err) != domain.CodeQuotaExceeded || client.calls.Load() != 0 {
		t.Fatalf("insufficient project budget dispatch calls=%d err=%v", client.calls.Load(), err)
	}
	var accountRows, projectRows int
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_call_ledger WHERE source='compaction'`).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM project_budget_ledger WHERE source='compaction'`).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 0 || projectRows != 0 {
		t.Fatalf("failed project reservation left account/project holds %d/%d", accountRows, projectRows)
	}
}
