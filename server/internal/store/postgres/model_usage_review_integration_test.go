package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresModelSettlementRejectsReviewTextDigestOutsideReview(t *testing.T) {
	store := &Store{}
	err := store.SettleModelCall(context.Background(), app.ModelCallSettlement{Source: app.ModelCallCompaction, OwnerID: "u1234567",
		CallID: "compaction-call-01", Summary: &app.ConversationSummaryModelFence{},
		Usage: app.ModelUsage{Known: true, InputTokens: 1}, ResponseCiphertext: []byte{1}, ResponseDigest: make([]byte, 32),
		ResponseTextDigest: make([]byte, 32)})
	if domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("compaction accepted a review-only response digest: %v", err)
	}
}

func TestPostgresReviewModelUsageIsIndependentFromTaskExecutionBudget(t *testing.T) {
	f, fence := completedReviewModelFence(t)
	ctx := context.Background()
	call := reviewModelReservation(t, f.store, fence, "")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=37,budget_model_tokens=1 WHERE id=$1`, fence.Claim.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReserveModelCall(ctx, call); err != nil {
		t.Fatalf("reserve review call beyond the completed Task execution budget: %v", err)
	}
	if err := f.store.AbandonModelCallReservation(ctx, call); err != nil {
		t.Fatalf("abandon undispatched review reservation: %v", err)
	}
	var reservations int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='review' AND call_id=$2`,
		call.OwnerID, call.CallID).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("abandoned review reservation count=%d err=%v", reservations, err)
	}
	if err := f.store.ReserveModelCall(ctx, call); err != nil {
		t.Fatalf("re-reserve abandoned review call: %v", err)
	}
	if err := f.store.BeginModelCallDispatch(ctx, call); err != nil {
		t.Fatalf("begin review model dispatch: %v", err)
	}
	if err := f.store.SettleModelCall(ctx, app.ModelCallSettlement{Source: app.ModelCallReview, OwnerID: call.OwnerID,
		CallID: call.CallID, Unknown: true}); err != nil {
		t.Fatalf("retain unknown review usage without a delivery fence: %v", err)
	}
	var state, taskID string
	var revision, sequence, generation int64
	if err := f.store.pool.QueryRow(ctx, `SELECT state,task_id,review_task_revision,review_result_event_sequence,review_claim_generation
FROM model_call_ledger WHERE owner_id=$1 AND source='review' AND call_id=$2`, call.OwnerID, call.CallID).
		Scan(&state, &taskID, &revision, &sequence, &generation); err != nil || state != "unknown" || taskID != fence.Claim.TaskID ||
		revision != fence.Claim.TaskRevision || sequence != fence.Claim.ResultEventSequence || generation != fence.Claim.ClaimGeneration {
		t.Fatalf("review account ledger = %q task=%q revision=%d sequence=%d generation=%d err=%v", state, taskID, revision, sequence, generation, err)
	}
	admin := reviewUsageAdminPrincipal(t, f.store, f.principal.UserID)
	if _, err := f.store.ReconcileUnknownModelCall(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: call.OwnerID,
		Source: app.ModelCallReview, CallID: call.CallID, InputTokens: 8, OutputTokens: 4,
		Evidence: "provider invoice ref", IP: "127.0.0.1"}); err != nil {
		t.Fatalf("reconcile review usage: %v", err)
	}
	var usageTokens int64
	if err := f.store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, fence.Claim.TaskID).Scan(&usageTokens); err != nil || usageTokens != 37 {
		t.Fatalf("review call changed Task execution usage to %d: %v", usageTokens, err)
	}
	var accountTokens int64
	if err := f.store.pool.QueryRow(ctx, `SELECT input_tokens+output_tokens FROM model_call_ledger WHERE owner_id=$1 AND source='review' AND call_id=$2 AND state='settled'`,
		call.OwnerID, call.CallID).Scan(&accountTokens); err != nil || accountTokens != 12 {
		t.Fatalf("settled review token usage=%d err=%v", accountTokens, err)
	}
}

func TestPostgresReviewModelDispatchRejectsExpiredClaimButAllowsSafeAbandon(t *testing.T) {
	f, fence := completedReviewModelFence(t)
	ctx := context.Background()
	call := reviewModelReservation(t, f.store, fence, "")
	if err := f.store.ReserveModelCall(ctx, call); err != nil {
		t.Fatalf("reserve review call: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND task_revision=$2`, fence.Claim.TaskID, fence.Claim.TaskRevision); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BeginModelCallDispatch(ctx, call); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch with expired review lease = %v, want conflict", err)
	}
	if err := f.store.AbandonModelCallReservation(ctx, call); err != nil {
		t.Fatalf("release undispatched reservation after lease expiry: %v", err)
	}
	var reservations int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='review' AND call_id=$2`,
		call.OwnerID, call.CallID).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("expired review reservation count=%d err=%v", reservations, err)
	}
}

func TestPostgresProjectReviewModelUsageAttributesAndReconcilesToReviewedTask(t *testing.T) {
	f, fence := completedReviewModelFence(t)
	ctx := context.Background()
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	projectRecord, err := project.New(projectID, f.principal.UserID, "Review budget", "review usage attribution")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.CreateProject(ctx, f.principal.UserID, projectRecord, "review-project-create-01"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := f.store.SetProjectBudget(ctx, f.principal.UserID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 1000, Version: 1}, 0, "review-budget-create-01"); err != nil {
		t.Fatalf("create project budget: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET project_id=$2,usage_model_tokens=37,budget_model_tokens=1 WHERE id=$1`, fence.Claim.TaskID, projectID); err != nil {
		t.Fatal(err)
	}
	call := reviewModelReservation(t, f.store, fence, projectID)
	usage := project.Usage{InputTokens: 20, OutputTokens: 20}
	rate := project.RateSnapshot{Provider: "test-provider", ModelID: call.ModelID, Model: call.ProviderModel,
		Currency: "USD", Version: 1, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	reservedMicros, err := rate.EstimateTokenCost(usage)
	if err != nil {
		t.Fatal(err)
	}
	projectDigest := sha256.Sum256([]byte("review project request"))
	reservation, _, err := f.store.ReserveProjectModelCall(ctx, app.ProjectModelReservation{ProjectID: projectID,
		ActorID: call.OwnerID, TaskID: fence.Claim.TaskID, Source: app.ModelCallReview, CallID: call.CallID,
		ModelID: call.ModelID, Usage: usage, ReservedTokens: usage.InputTokens + usage.OutputTokens,
		Rate: rate, ReservedMicros: reservedMicros, RequestDigest: projectDigest[:], AccountCall: call})
	if err != nil {
		t.Fatalf("reserve project review call: %v", err)
	}
	if err := f.store.BeginModelCallDispatch(ctx, reservation.AccountCall); err != nil {
		t.Fatalf("begin project review dispatch: %v", err)
	}
	if err := f.store.SettleProjectModelCall(ctx, app.SettleProjectModelCallCommand{ProjectID: projectID,
		Source: app.ModelCallReview, CallID: call.CallID, Unknown: true}); err != nil {
		t.Fatalf("retain unknown project review usage: %v", err)
	}
	admin := reviewUsageAdminPrincipal(t, f.store, f.principal.UserID)
	if _, err := f.store.ReconcileUnknownModelCall(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: call.OwnerID,
		Source: app.ModelCallReview, CallID: call.CallID, InputTokens: 10, OutputTokens: 5,
		Evidence: "provider invoice ref", IP: "127.0.0.1"}); err != nil {
		t.Fatalf("reconcile project review usage: %v", err)
	}
	var projectState, actorID, taskID string
	var actualInput, actualOutput, actualMicros int64
	if err := f.store.pool.QueryRow(ctx, `SELECT status,actor_id,task_id,actual_input_tokens,actual_output_tokens,actual_micros
FROM project_budget_ledger WHERE project_id=$1 AND source='review' AND call_id=$2`, projectID, call.CallID).
		Scan(&projectState, &actorID, &taskID, &actualInput, &actualOutput, &actualMicros); err != nil || projectState != "settled" ||
		actorID != call.OwnerID || taskID != fence.Claim.TaskID || actualInput != 10 || actualOutput != 5 || actualMicros != 15 {
		t.Fatalf("project review ledger = state=%q actor=%q task=%q usage=%d/%d micros=%d err=%v", projectState, actorID, taskID,
			actualInput, actualOutput, actualMicros, err)
	}
	var usageTokens int64
	if err := f.store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, fence.Claim.TaskID).Scan(&usageTokens); err != nil || usageTokens != 37 {
		t.Fatalf("project review call changed Task execution usage to %d: %v", usageTokens, err)
	}
}

func completedReviewModelFence(t *testing.T) (agentLifecycleFixture, app.TaskValidationReviewModelFence) {
	t.Helper()
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load Task for review: task=%+v err=%v", current.Task, err)
	}
	stepID := mustID(t, domain.NewStepID)
	event, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: "Reviewed task result.", Usage: current.Task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	})
	if err != nil {
		t.Fatalf("complete Task for review: %v", err)
	}
	claims, err := f.store.ClaimTaskValidationReviews(ctx, "review-model-worker", 30*time.Second, 1)
	if err != nil || len(claims) != 1 || claims[0].ResultEventSequence != event.Sequence {
		t.Fatalf("claim completed Task review: claims=%+v err=%v", claims, err)
	}
	claim := claims[0]
	return f, app.TaskValidationReviewModelFence{Claim: claim, InitiatorID: claim.InitiatorID,
		SessionID: "review-session-1", ThroughSequence: 1}
}

func reviewModelReservation(t *testing.T, store *Store, fence app.TaskValidationReviewModelFence, projectID string) app.ModelCallReservation {
	t.Helper()
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,1000,3)
ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, fence.InitiatorID); err != nil {
		t.Fatal(err)
	}
	var connectionVersion time.Time
	if err := store.pool.QueryRow(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,
capability_text,capability_tools,capability_vision,checked_at) VALUES ('p7654321',NULL,'review model','https://model.example/v1','test-model',
decode(repeat('01',29),'hex'),true,true,false,now()) RETURNING updated_at`).Scan(&connectionVersion); err != nil {
		t.Fatal(err)
	}
	callID, err := app.DSHTaskValidationReviewModelCallID(fence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("review model request"))
	return app.ModelCallReservation{Source: app.ModelCallReview, Review: &fence, ProjectID: projectID,
		OwnerID: fence.InitiatorID, ModelID: "p7654321", ProviderModel: "test-model", ConnectionVersion: connectionVersion,
		CallID: callID, RequestDigest: digest[:], ReservedTokens: 40}
}

func reviewUsageAdminPrincipal(t *testing.T, store *Store, ownerID string) identity.Principal {
	t.Helper()
	if _, err := store.pool.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1`, ownerID); err != nil {
		t.Fatal(err)
	}
	tokenID := createTestCredentials(t, store, ownerID)[0]
	return identity.Principal{UserID: ownerID, TokenID: tokenID, Kind: identity.KindSession, Role: identity.RoleAdmin}
}
