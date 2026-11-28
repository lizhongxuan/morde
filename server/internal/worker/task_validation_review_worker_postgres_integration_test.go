package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskValidationReviewPostgresModelClient struct {
	reviewText string
	models     []string
}

func (c *taskValidationReviewPostgresModelClient) Complete(_ context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if len(request.Tools) != 0 {
		return app.ModelResponse{}, errors.New("synthetic review gateway received tools")
	}
	c.models = append(c.models, request.Endpoint.ModelID)
	response := app.ModelResponse{StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 24, OutputTokens: 12}}
	switch request.Endpoint.ModelID {
	case "gpt-6-luna":
		response.Text = "Verified synthetic task result."
		response.ProviderReportedModel = "gpt-6-luna"
	case "deepseek-flash":
		response.Text = c.reviewText
		response.ProviderReportedModel = "deepseek-flash"
	default:
		return app.ModelResponse{}, errors.New("unexpected synthetic model endpoint")
	}
	return response, nil
}

func (*taskValidationReviewPostgresModelClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true}, nil
}

func TestTaskValidationReviewWorkerPersistsGatewayResultWithPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify local owner session: principal=%#v err=%v", principal, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	modelClient := &taskValidationReviewPostgresModelClient{reviewText: taskValidationReviewInsufficientReport(t)}
	connections, err := app.NewModelConnectionService(store, sealer, modelClient, []string{"api.openai.com", "api.deepseek.com"})
	if err != nil {
		t.Fatal(err)
	}
	createConnection := func(name, endpoint, model string) string {
		t.Helper()
		created, err := connections.Create(ctx, principal, app.ModelConnectionCommand{
			Name: name, BaseURL: endpoint, ModelName: model, APIKey: "local-synthetic-review-key",
		})
		if err != nil {
			t.Fatalf("create synthetic %s model connection: %v", name, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,
capability_tools=false,capability_vision=false WHERE id=$1`, created.ID); err != nil {
			t.Fatalf("mark synthetic %s model connection checked: %v", name, err)
		}
		return created.ID
	}
	executionModelID := createConnection("review E2E execution", "https://api.openai.com/v1", "gpt-6-luna")
	createConnection("review E2E verifier", "https://api.deepseek.com", "deepseek-flash")
	gateway, err := app.NewModelGatewayService(store, connections, modelClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{ModelID: executionModelID,
		Objective: "Verify the synthetic task result.", Budget: task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: task.MaxModelTokens},
		IdempotencyKey: "review-worker-e2e-submit"})
	if err != nil {
		t.Fatalf("submit review E2E Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "review-e2e-task-worker", 30*time.Second)
	if err != nil || !found || claimed.ID != submission.Task.ID {
		t.Fatalf("claim review E2E Task: claimed=%t task=%q err=%v", found, claimed.ID, err)
	}
	stopKey := bytes.Repeat([]byte{0x42}, 32)
	stopAuthenticator, err := app.NewAgentStopAuthenticator("review-e2e-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	nonce[0] = 1
	agent, _, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "review-e2e-agent", AgentSessionID: "review-e2e-session", SupervisorID: "review-e2e-supervisor",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.2-rc.1", ProfileVersion: "v1",
	}, SupervisorNonce: nonce[:], LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("start review E2E Agent instance: %v", err)
	}
	current, err := store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load running review E2E Task: task=%+v err=%v", current.Task, err)
	}
	fence := app.ModelTaskFence{TaskID: current.Task.ID, OwnerID: current.Task.OwnerID, InitiatorID: current.Task.InitiatorID,
		LeaseOwner: current.Task.Lease.Owner, LeaseVersion: current.Task.Lease.Version,
		AgentInstanceID: agent.Fence.AgentInstanceID, AgentGeneration: agent.Fence.AgentGeneration,
		AgentSessionID: agent.Fence.AgentSessionID, AgentRootSessionID: agent.Fence.AgentSessionID,
		AuthorizationVersion: agent.Fence.AuthorizationVersion, EngineVersion: agent.Fence.EngineVersion,
		ProfileVersion: agent.Fence.ProfileVersion}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPayload := []byte(`{"header":{"id":"review-e2e-session"},"from_seq":0,"events":[{"seq":0,"type":"message"}]}`)
	checkpointDigest := sha256.Sum256(checkpointPayload)
	checkpointFence := app.AgentCheckpointFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
		AuthorizationVersion: fence.AuthorizationVersion, LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: checkpointFence, From: 0, Through: 0,
		Digest: checkpointDigest[:], Payload: checkpointPayload}); err != nil {
		t.Fatalf("commit root DSH checkpoint before model call: %v", err)
	}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	modelRequest, marshalErr := json.Marshal(map[string]any{"provider": "morde", "model": executionModelID, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "verify"}}}}, "tools": []any{}, "max_tokens": 128})
	if marshalErr != nil {
		t.Fatalf("marshal synthetic task model request: %v", marshalErr)
	}
	executionResponse, err := gateway.Complete(ctx, app.ModelCall{Source: app.ModelCallTask, Task: &fence, ModelID: executionModelID,
		CallID: callID, RawRequest: modelRequest})
	if err != nil || executionResponse.Text != "Verified synthetic task result." {
		t.Fatalf("complete and persist execution model call: response=%+v err=%v", executionResponse, err)
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	resultPayload, err := json.Marshal(map[string]string{"status": "completed"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.Task.ID,
		LeaseOwner: current.Task.Lease.Owner, LeaseVersion: current.Task.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: executionResponse.Text, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: resultPayload}})
	if err != nil {
		t.Fatalf("persist completed review E2E Task: %v", err)
	}
	var boundExecutionCall string
	if err := pool.QueryRow(ctx, `SELECT execution_model_call_id FROM task_validation_reviews WHERE task_id=$1 AND task_revision=$2`,
		current.Task.ID, *current.Task.CurrentRevision).Scan(&boundExecutionCall); err != nil || boundExecutionCall != callID {
		t.Fatalf("review outbox execution model call=%q want=%q err=%v", boundExecutionCall, callID, err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable")
	}
	runtimeScript, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin-account.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	runtimeSupervisor := &f01NodeAccountSupervisor{node: node, script: runtimeScript}
	runtimeManager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: runtimeControl, Supervisor: runtimeSupervisor, Guardian: &f01NodeTaskGuardian{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "review-e2e-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeSession, err := runtimeManager.Acquire(ctx, owner.ID, current.Task.ConversationID)
	if err != nil {
		t.Fatalf("start real Node Account Runtime for the review: %v; stderr=%s", err, runtimeSupervisor.stderrSnapshot())
	}
	defer func() {
		_ = runtimeSession.process.Input.Close()
		_ = runtimeSession.process.Output.Close()
		if runtimeSupervisor.cmd != nil && runtimeSupervisor.cmd.Process != nil {
			_ = runtimeSupervisor.cmd.Process.Kill()
			_ = runtimeSupervisor.cmd.Wait()
		}
	}()
	worker, err := NewTaskValidationReviewWorker(TaskValidationReviewWorkerOptions{
		Queue: store, Snapshots: tasks, AccountRuntime: runtimeManager,
		Models: gateway, WorkerID: "review-e2e-worker", LeaseDuration: 30 * time.Second,
		PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err = worker.RunOnce(ctx)
	if err != nil || !found {
		t.Fatalf("run full review Worker against PostgreSQL and real DSH Account Runtime: found=%t models=%v err=%v",
			found, modelClient.models, err)
	}
	var reviewState, executionFamily, reviewerFamily string
	var persistedReport []byte
	if err := pool.QueryRow(ctx, `SELECT state,execution_model_family,reviewer_model_family,review_report
FROM task_validation_reviews WHERE task_id=$1 AND task_revision=$2`, current.Task.ID, *current.Task.CurrentRevision).
		Scan(&reviewState, &executionFamily, &reviewerFamily, &persistedReport); err != nil || reviewState != "reviewed" ||
		executionFamily != "openai-gpt6" || reviewerFamily != "deepseek-v4" || !json.Valid(persistedReport) {
		t.Fatalf("persisted independent review: state=%q families=%q/%q report=%s err=%v", reviewState,
			executionFamily, reviewerFamily, persistedReport, err)
	}
	var settledReviewCalls int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='review' AND state='settled'`, owner.ID).
		Scan(&settledReviewCalls); err != nil || settledReviewCalls != 1 {
		t.Fatalf("settled review model calls=%d err=%v", settledReviewCalls, err)
	}
	if len(modelClient.models) != 2 || modelClient.models[0] != "gpt-6-luna" || modelClient.models[1] != "deepseek-flash" {
		t.Fatalf("model gateway calls did not use independent configured families: %v", modelClient.models)
	}
	completed, err := tasks.Get(ctx, principal, current.Task.ID)
	if err != nil || completed.Task.Status != task.StatusCompleted || completed.Validation.State != app.TaskValidationPending {
		t.Fatalf("review completion changed Task lifecycle or trusted verdict: status=%q validation=%q err=%v",
			completed.Task.Status, completed.Validation.State, err)
	}
}

func taskValidationReviewInsufficientReport(t *testing.T) string {
	t.Helper()
	report := app.TaskValidationReviewReport{Recommendation: app.TaskValidationFail,
		Summary:  "The synthetic task result lacks reviewable evidence details.",
		Findings: make([]app.TaskValidationReviewFinding, 0, 7)}
	for _, id := range []app.TaskHardCheckID{app.TaskHardCheckConclusionFirst, app.TaskHardCheckEvidence,
		app.TaskHardCheckIndependent, app.TaskHardCheckClarification, app.TaskHardCheckApproval,
		app.TaskHardCheckCorrection, app.TaskHardCheckReplay} {
		report.Findings = append(report.Findings, app.TaskValidationReviewFinding{ID: id,
			Assessment: app.TaskValidationReviewInsufficient, Rationale: "No eligible evidence was supplied.",
			EvidenceRefs: []app.TaskValidationEvidenceRefRecord{}})
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) {
		t.Fatal("synthetic review report is invalid JSON")
	}
	return string(encoded)
}
