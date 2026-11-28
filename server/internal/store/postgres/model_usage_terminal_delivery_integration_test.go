package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type terminalDeliveryModelResolverIntegration struct{ version time.Time }

func (r terminalDeliveryModelResolverIntegration) ResolveGatewayEndpoint(context.Context, string, string, bool, bool) (app.ModelGatewayEndpoint, error) {
	return app.ModelGatewayEndpoint{Endpoint: app.ModelEndpoint{BaseURL: "https://model.example/v1", APIKey: "synthetic-key", ModelID: "test-model"},
		Version: r.version}, nil
}

func (r terminalDeliveryModelResolverIntegration) ResolveProbeEndpoint(ctx context.Context, _ identity.Principal, _ string) (app.ModelGatewayEndpoint, error) {
	return r.ResolveGatewayEndpoint(ctx, "", "", false, false)
}

func (terminalDeliveryModelResolverIntegration) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time,
	value app.ModelCapabilities) (app.ModelCapabilities, error) {
	return value, nil
}

type terminalDeliveryModelClientIntegration struct {
	response app.ModelResponse
	err      error
	calls    int
	request  app.ModelRequest
}

func (c *terminalDeliveryModelClientIntegration) Complete(_ context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	c.calls++
	c.request = request
	return c.response, c.err
}

func (*terminalDeliveryModelClientIntegration) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected model check")
}

type terminalDeliveryModelIntegrationFixture struct {
	store          *Store
	principal      identity.Principal
	claim          TaskTerminalDeliveryClaim
	fence          app.TaskTerminalDeliveryModelFence
	projectID      string
	modelID        string
	modelVersion   time.Time
	projectBudgets *app.ProjectBudgetService
	sealer         *app.Sealer
	rawRequest     json.RawMessage
}

func newTerminalDeliveryModelIntegrationFixture(t *testing.T) terminalDeliveryModelIntegrationFixture {
	t.Helper()
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	projectRecord := createProjectStoreFixture(t, ctx, f.store, f.principal.UserID,
		"Terminal model budget", "Terminal delivery model usage", "terminal-model-project-create")
	if _, err := f.store.SetProjectBudget(ctx, f.principal.UserID, projectRecord.ID,
		project.Budget{Currency: "USD", LimitMicros: 1_000_000, Version: 1}, 0, "terminal-model-budget-create"); err != nil {
		t.Fatalf("create terminal model project budget: %v", err)
	}
	conversationID := mustID(t, domain.NewConversationID)
	if _, _, err := f.store.CreateProjectConversation(ctx, f.principal.UserID,
		conversation.Conversation{ID: conversationID, OwnerID: f.principal.UserID, ProjectID: projectRecord.ID},
		testIdempotency(f.principal.UserID, "project_conversation.create", "terminal-model-conversation")); err != nil {
		t.Fatalf("create terminal model project conversation: %v", err)
	}
	baseline, err := f.store.GetTask(ctx, f.request.Fence.TaskID, f.request.Fence.OwnerID)
	if err != nil {
		t.Fatalf("load Task authorization baseline: %v", err)
	}
	modelID := "p7654321"
	submission := newTestTaskSubmission(t, f.principal.UserID, "", "terminal-model-task", "Please summarize why this task failed.")
	submission.Task.ProjectID = projectRecord.ID
	submission.Task.InitiatorID = f.principal.UserID
	submission.Task.ConversationID = conversationID
	submission.Task.ModelID = modelID
	submission.Task.Authorization = baseline.Task.Authorization
	submission.Message.ConversationID = conversationID
	submission.Message.ActorID = f.principal.UserID
	submission.Idempotency = testIdempotency(f.principal.UserID, "task.submit", "terminal-model-task")
	created, err := f.store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("create project-scoped terminal model Task: %v", err)
	}
	claimedTask, found, err := f.store.ClaimNextTask(ctx, "terminal-model-task-worker", time.Minute)
	if err != nil || !found || claimedTask.ID != created.Task.ID {
		t.Fatalf("claim project-scoped terminal model Task: found=%t task=%q err=%v", found, claimedTask.ID, err)
	}
	stepID := mustID(t, domain.NewStepID)
	_, err = f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimedTask.ID, LeaseOwner: claimedTask.Lease.Owner, LeaseVersion: claimedTask.Lease.Version,
		Status: task.StatusFailed, CurrentStep: stepID, Result: "The provider request failed before producing a usable answer.", Usage: claimedTask.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: claimedTask.ID, Sequence: 1, Kind: "model", Status: task.StepFailed,
			Title: "Generate answer", Summary: "The model request failed."}},
		Event: task.Event{TaskID: claimedTask.ID, Type: "task.result", Payload: []byte(`{"status":"failed","reason":"provider_unavailable"}`)},
	})
	if err != nil {
		t.Fatalf("commit failed project-scoped Task: %v", err)
	}
	claims, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-model-worker", 30*time.Second, 1)
	if err != nil || len(claims) != 1 || claims[0].TaskID != claimedTask.ID {
		t.Fatalf("claim terminal model delivery: claims=%+v err=%v", claims, err)
	}
	claim := claims[0]
	fence := app.TaskTerminalDeliveryModelFence{
		DeliveryID: claim.ID, TaskID: claim.TaskID, OwnerID: claim.OwnerID, ConversationID: claim.ConversationID,
		InitiatorID: claim.InitiatorID, TaskRevision: claim.TaskRevision, AuthorizationVersion: claim.AuthorizationVersion,
		SourceKind: claim.SourceKind, SourceEventSequence: claim.SourceEventSequence, SourceEventType: claim.SourceEventType,
		SourceSHA256: append([]byte(nil), claim.SourceSHA256...), ResultSHA256: append([]byte(nil), claim.ResultSHA256...),
		ValidationClaimGeneration: cloneNullableInt64(claim.ValidationClaimGeneration), VerdictSHA256: append([]byte(nil), claim.VerdictSHA256...),
		ClaimGeneration: claim.ClaimGeneration, LeaseOwner: claim.LeaseOwner, LeaseExpiresAt: claim.LeaseExpiresAt,
		ModelCallID: claim.ModelCallID,
	}
	if !app.ValidTaskTerminalDeliveryModelDispatchFence(fence) {
		t.Fatalf("model dispatch fence from outbox claim is invalid: %+v", fence)
	}
	if disposition, err := f.store.TaskTerminalDeliveryModelCallDisposition(context.Background(), fence); err != nil ||
		disposition != app.TaskTerminalDeliveryModelNotDispatched {
		t.Fatalf("fresh terminal delivery claim disposition = %q, err=%v", disposition, err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,5000,4) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`,
		fence.InitiatorID); err != nil {
		t.Fatalf("configure account model budget: %v", err)
	}
	var modelVersion time.Time
	if err := f.store.pool.QueryRow(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,
capability_text,capability_tools,capability_vision,checked_at)
VALUES ($1,NULL,'terminal delivery test','https://model.example/v1','test-model',decode(repeat('01',29),'hex'),
true,false,false,clock_timestamp()) RETURNING updated_at`, modelID).Scan(&modelVersion); err != nil {
		t.Fatalf("create shared terminal model connection: %v", err)
	}
	rate, err := app.NewConfiguredProjectRates([]project.RateSnapshot{{Provider: "test-provider", ModelID: modelID,
		Model: "test-model", Currency: "USD", Version: 1, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	projectAccess, err := app.NewProjectService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	projectBudgets, err := app.NewProjectBudgetService(f.store, projectAccess, rate)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	rawRequest, err := app.BuildTaskTerminalDeliveryDSHModelRequest(modelID, terminalDeliveryFactsForModelTest(modelID, claim,
		"Please summarize why this task failed."))
	if err != nil {
		t.Fatalf("build terminal delivery model request: %v", err)
	}
	return terminalDeliveryModelIntegrationFixture{store: f.store, principal: f.principal, claim: claim, fence: fence,
		projectID: projectRecord.ID, modelID: modelID, modelVersion: modelVersion, projectBudgets: projectBudgets,
		sealer: sealer, rawRequest: rawRequest}
}

func terminalDeliveryFactsForModelTest(modelID string, claim TaskTerminalDeliveryClaim,
	objective string) app.TaskTerminalDeliverySourceFacts {
	facts := app.TaskTerminalDeliverySourceFacts{
		Identity: app.TaskTerminalDeliveryIdentity{DeliveryID: claim.ID, TaskID: claim.TaskID, OwnerID: claim.OwnerID,
			ConversationID: claim.ConversationID, InitiatorID: claim.InitiatorID, TaskRevision: claim.TaskRevision,
			AuthorizationVersion: claim.AuthorizationVersion, SourceKind: claim.SourceKind,
			SourceEventSequence: claim.SourceEventSequence, SourceEventType: claim.SourceEventType, SourceSHA256: claim.SourceSHA256},
		ModelID: modelID, TaskStatus: "failed", ObjectiveSummary: objective,
		ObjectiveSHA256: terminalSHA256Bytes([]byte(objective)), FailureCode: "provider_unavailable",
		StepCounts: app.TaskTerminalDeliveryStepCounts{},
	}
	facts.FactsSHA256, _ = app.TaskTerminalDeliveryFactsDigest(facts)
	return facts
}

func TestPostgresTerminalDeliveryModelProjectSettlementAndReplyRecovery(t *testing.T) {
	f := newTerminalDeliveryModelIntegrationFixture(t)
	ctx := context.Background()
	client := &terminalDeliveryModelClientIntegration{response: app.ModelResponse{Text: "The provider request failed before returning a usable response.",
		StopReason: "stop", ProviderReportedModel: "test-model", Usage: app.ModelUsage{Known: true, InputTokens: 24, OutputTokens: 12}}}
	gateway, err := app.NewModelGatewayService(f.store, terminalDeliveryModelResolverIntegration{version: f.modelVersion}, client,
		f.projectBudgets)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(f.sealer); err != nil {
		t.Fatal(err)
	}
	modelID, err := gateway.ResolveTaskTerminalDeliveryModel(ctx, f.fence)
	if err != nil || modelID != f.modelID {
		t.Fatalf("resolve model from current terminal delivery claim: model=%q err=%v", modelID, err)
	}
	response, disposition, err := gateway.CompleteTaskTerminalDeliveryDSHModel(ctx, f.fence, modelID, f.rawRequest)
	if err != nil || disposition != app.TaskTerminalDeliveryModelReplyReady || response.Text != client.response.Text ||
		client.calls != 1 || len(client.request.Tools) != 0 {
		t.Fatalf("complete terminal delivery model call: response=%+v disposition=%q calls=%d tools=%d err=%v",
			response, disposition, client.calls, len(client.request.Tools), err)
	}
	if disposition, err := f.store.TaskTerminalDeliveryModelCallDisposition(ctx, f.fence); err != nil ||
		disposition != app.TaskTerminalDeliveryModelReplyReady {
		t.Fatalf("settled terminal model call disposition = %q, err=%v", disposition, err)
	}
	var accountState, projectState, taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT state,task_id FROM model_call_ledger
WHERE owner_id=$1 AND source='terminal_delivery' AND call_id=$2`, f.fence.InitiatorID, f.fence.ModelCallID).Scan(&accountState, &taskID); err != nil ||
		accountState != "settled" || taskID != f.fence.TaskID {
		t.Fatalf("terminal account usage ledger = state %q task %q err %v", accountState, taskID, err)
	}
	var projectActor, projectTask string
	if err := f.store.pool.QueryRow(ctx, `SELECT status,actor_id,task_id FROM project_budget_ledger
WHERE project_id=$1 AND source='terminal_delivery' AND call_id=$2`, f.projectID, f.fence.ModelCallID).
		Scan(&projectState, &projectActor, &projectTask); err != nil || projectState != "settled" || projectActor != f.fence.InitiatorID || projectTask != f.fence.TaskID {
		t.Fatalf("terminal project usage ledger = state %q actor %q task %q err %v", projectState, projectActor, projectTask, err)
	}
	readyRows, err := f.store.ListTaskTerminalDeliveryReplyReady(ctx, 1)
	if err != nil || len(readyRows) != 1 || readyRows[0].DeliveryID != f.claim.ID {
		t.Fatalf("terminal reply-ready row: rows=%+v err=%v", readyRows, err)
	}
	readyFence := f.fence
	readyFence.LeaseOwner, readyFence.LeaseExpiresAt = "", time.Time{}
	if !app.ValidTaskTerminalDeliveryModelFence(readyFence) || app.ValidTaskTerminalDeliveryModelDispatchFence(readyFence) {
		t.Fatalf("reply-ready recovery fence has a lease: %+v", readyFence)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_connections SET model_name='changed-current-config',updated_at=updated_at+interval '1 second' WHERE id=$1`, f.modelID); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := gateway.RecoverTaskTerminalDeliveryDSHModelResponse(ctx, readyFence, f.rawRequest)
	if err != nil || !found || recovered.Text != response.Text {
		t.Fatalf("recover reply-ready response after model config change: found=%t response=%+v err=%v", found, recovered, err)
	}
	changedRequest, err := app.BuildTaskTerminalDeliveryDSHModelRequest(f.modelID,
		terminalDeliveryFactsForModelTest(f.modelID, f.claim, "A changed objective."))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := gateway.RecoverTaskTerminalDeliveryDSHModelResponse(ctx, readyFence, changedRequest); found || domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("recover accepted a different DSH request: found=%t err=%v", found, err)
	}
}

func TestPostgresTerminalDeliveryUnknownDoesNotRedispatchAndReconcilesToSafeFallback(t *testing.T) {
	f := newTerminalDeliveryModelIntegrationFixture(t)
	ctx := context.Background()
	client := &terminalDeliveryModelClientIntegration{err: &app.ModelClientError{Kind: app.ModelErrorTimeout}}
	resolver := terminalDeliveryModelResolverIntegration{version: f.modelVersion}
	gateway, err := app.NewModelGatewayService(f.store, resolver, client, f.projectBudgets)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(f.sealer); err != nil {
		t.Fatal(err)
	}
	modelID, err := gateway.ResolveTaskTerminalDeliveryModel(ctx, f.fence)
	if err != nil {
		t.Fatal(err)
	}
	_, disposition, err := gateway.CompleteTaskTerminalDeliveryDSHModel(ctx, f.fence, modelID, f.rawRequest)
	if err == nil || disposition != app.TaskTerminalDeliveryModelUnknown || client.calls != 1 {
		t.Fatalf("ambiguous provider call was not classified unknown: disposition=%q calls=%d err=%v", disposition, client.calls, err)
	}
	if changed, err := f.store.MarkTaskTerminalDeliveryUnknown(ctx, f.claim); err != nil || !changed {
		t.Fatalf("mark unknown terminal delivery claim: changed=%t err=%v", changed, err)
	}
	reclaimedRows, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-model-reclaim", 30*time.Second, 1)
	if err != nil || len(reclaimedRows) != 1 || reclaimedRows[0].ModelCallID != f.claim.ModelCallID ||
		reclaimedRows[0].ClaimGeneration != f.claim.ClaimGeneration+1 {
		t.Fatalf("reclaim unknown terminal delivery: rows=%+v err=%v", reclaimedRows, err)
	}
	claimedAgain := reclaimedRows[0]
	secondFence := terminalDeliveryModelFenceFromClaim(claimedAgain)
	_, disposition, err = gateway.CompleteTaskTerminalDeliveryDSHModel(ctx, secondFence, modelID, f.rawRequest)
	if err == nil || disposition != app.TaskTerminalDeliveryModelUnknown || client.calls != 1 {
		t.Fatalf("unknown stable call was dispatched again: disposition=%q calls=%d err=%v", disposition, client.calls, err)
	}
	if changed, err := f.store.MarkTaskTerminalDeliveryUnknown(ctx, claimedAgain); err != nil || !changed {
		t.Fatalf("release current unknown delivery lease: changed=%t err=%v", changed, err)
	}
	admin := reviewUsageAdminPrincipal(t, f.store, f.principal.UserID)
	if _, err := f.store.ReconcileUnknownModelCall(ctx, admin, app.ModelUsageReconcileCommand{
		OwnerID: f.fence.InitiatorID, Source: app.ModelCallTerminalDelivery, CallID: f.fence.ModelCallID,
		InputTokens: 20, OutputTokens: 10, Evidence: "provider usage record", IP: "127.0.0.1",
	}); err != nil {
		t.Fatalf("reconcile terminal delivery provider usage: %v", err)
	}
	settledFence := terminalDeliveryModelFenceFromClaim(claimedAgain)
	if disposition, err := f.store.TaskTerminalDeliveryModelCallDisposition(ctx, settledFence); err != nil || disposition != app.TaskTerminalDeliveryModelFallbackSafe {
		t.Fatalf("reconciled usage without response did not allow safe fact fallback: disposition=%q err=%v", disposition, err)
	}
	var outboxState string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_terminal_delivery_outbox WHERE id=$1`, f.claim.ID).Scan(&outboxState); err != nil ||
		outboxState != TaskTerminalDeliveryStateUnknown {
		t.Fatalf("unknown reconciliation changed terminal outbox state to %q: %v", outboxState, err)
	}
}

func terminalDeliveryModelFenceFromClaim(claim TaskTerminalDeliveryClaim) app.TaskTerminalDeliveryModelFence {
	return app.TaskTerminalDeliveryModelFence{
		DeliveryID: claim.ID, TaskID: claim.TaskID, OwnerID: claim.OwnerID, ConversationID: claim.ConversationID,
		InitiatorID: claim.InitiatorID, TaskRevision: claim.TaskRevision, AuthorizationVersion: claim.AuthorizationVersion,
		SourceKind: claim.SourceKind, SourceEventSequence: claim.SourceEventSequence, SourceEventType: claim.SourceEventType,
		SourceSHA256: append([]byte(nil), claim.SourceSHA256...), ResultSHA256: append([]byte(nil), claim.ResultSHA256...),
		ValidationClaimGeneration: cloneNullableInt64(claim.ValidationClaimGeneration), VerdictSHA256: append([]byte(nil), claim.VerdictSHA256...),
		ClaimGeneration: claim.ClaimGeneration, LeaseOwner: claim.LeaseOwner, LeaseExpiresAt: claim.LeaseExpiresAt,
		ModelCallID: claim.ModelCallID,
	}
}

func terminalSHA256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
