package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskValidationProjectionRequiresCurrentAuthorizationIdentity(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load running Task: current=%+v err=%v", current.Task, err)
	}
	for _, model := range []struct{ id, name, endpoint, model string }{
		{"p1234567", "review execution", "https://api.openai.com/v1", "gpt-6-luna"},
		{"p2234567", "review reviewer", "https://api.deepseek.com", "deepseek-flash"},
	} {
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text)
VALUES ($1,NULL,$2,$3,$4,decode(repeat('00',29),'hex'),clock_timestamp(),true)`, model.id, model.name, model.endpoint, model.model); err != nil {
			t.Fatal(err)
		}
	}
	executionModel := seedTaskValidationExecutionModelResponse(t, f.store, lease.Fence)
	var sourceMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages
WHERE task_id=$1 AND role='user' AND delivery_status='applied' ORDER BY sequence LIMIT 1`, current.Task.ID).Scan(&sourceMessageID); err != nil {
		t.Fatal(err)
	}
	result := "The requested change is complete and verified."
	resultStepID := mustID(t, domain.NewStepID)
	event, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: resultStepID, Result: result, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: resultStepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
		ValidationClaims: []app.TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRef{
			{Kind: app.TaskValidationEvidenceSource, ID: sourceMessageID},
		}}},
	})
	if err != nil {
		t.Fatalf("complete Task with validation evidence: %v", err)
	}
	seedTaskResultBatchForLateValidation(t, f.store, current.Task.ID, sourceMessageID, resultStepID, event.Sequence, result, false)
	claims, err := f.store.ClaimTaskValidationReviews(ctx, "auth-projection-reviewer", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim validation review: claims=%+v err=%v", claims, err)
	}
	current, err = f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil || current.ValidationEvidence == nil {
		t.Fatalf("load validation evidence: evidence=%+v err=%v", current.ValidationEvidence, err)
	}
	completion := taskValidationReviewCompletionFixture(t, f.store, claims[0], current.ValidationEvidence)
	completion.ExecutionModel = executionModel
	completion.ReviewerModel.SessionID = completion.SessionID
	completion.ReviewerModel.ThroughSequence = 0
	completion.ReviewerModel, completion.ResponseText = settleTaskValidationReviewModelResponse(t, f.store, claims[0], completion.ReviewerModel, completion.Report)
	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || !updated {
		t.Fatalf("complete validation review: updated=%t err=%v", updated, err)
	}
	// This test isolates projection invalidation. The fixture's evaluator leaves
	// H07 pending because production has no trusted completeness fact, so seed a
	// stored PASS after the real review/model-response bindings are committed.
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET validation_state='PASS' WHERE task_id=$1`, current.Task.ID); err != nil {
		t.Fatalf("seed reviewed PASS for projection test: %v", err)
	}

	source := app.ConversationTaskResultSource{TaskID: current.Task.ID, TaskRevision: *current.Task.CurrentRevision,
		EventSequence: event.Sequence, Content: result}
	assertCurrentVerdict := func(want app.TaskValidationState) {
		t.Helper()
		details, err := f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
		if err != nil || details.Validation.State != want {
			t.Fatalf("Task detail validation=%q err=%v; want %q", details.Validation.State, err, want)
		}
		page, err := f.store.ListTasks(ctx, current.Task.OwnerID, app.TaskListRequest{Page: app.PageRequest{Limit: 20}})
		if err != nil || page.K2Projections[current.Task.ID].Validation.State != want {
			t.Fatalf("Task list validation=%q err=%v; want %q", page.K2Projections[current.Task.ID].Validation.State, err, want)
		}
		facts, err := f.store.TaskEventChainFacts(ctx, current.Task.OwnerID, current.Task.ID)
		if err != nil || facts.ValidationState != want && !(want == app.TaskValidationPending && facts.ValidationState == "") {
			t.Fatalf("Task event chain validation=%q err=%v; want %q", facts.ValidationState, err, want)
		}
		var receiptState app.TaskValidationState
		if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			receiptState, err = conversationTaskResultValidationStateInTx(ctx, tx, current.Task.OwnerID, source)
			return err
		}); err != nil || receiptState != want {
			t.Fatalf("first receipt validation=%q err=%v; want %q", receiptState, err, want)
		}
	}
	assertCurrentVerdict(app.TaskValidationPass)

	var responseCallID string
	if err := f.store.pool.QueryRow(ctx, `SELECT execution_model_call_id FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).Scan(&responseCallID); err != nil {
		t.Fatal(err)
	}
	resultUpdate, err := f.store.pool.Exec(ctx, `UPDATE task_model_call_responses
SET authorization_version=authorization_version+1 WHERE task_id=$1 AND call_id=$2`, current.Task.ID, responseCallID)
	if err != nil || resultUpdate.RowsAffected() != 1 {
		t.Fatalf("make persisted response authorization identity stale: err=%v", err)
	}
	assertCurrentVerdict(app.TaskValidationPending)
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_model_call_responses response
SET authorization_version=task.auth_version FROM tasks task
WHERE task.id=response.task_id AND response.task_id=$1 AND response.call_id=$2`, current.Task.ID, responseCallID); err != nil {
		t.Fatalf("restore response authorization identity: %v", err)
	}
	var authKind, installationID string
	if err := f.store.pool.QueryRow(ctx, `SELECT auth_kind,COALESCE(auth_client_installation_id,'') FROM tasks WHERE id=$1`, current.Task.ID).
		Scan(&authKind, &installationID); err != nil {
		t.Fatal(err)
	}
	if authKind != "client" || installationID == "" {
		t.Fatalf("fixture does not use a client installation credential: auth_kind=%q installation=%q", authKind, installationID)
	}
	revokeResult, err := f.store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND revoked_at IS NULL`, installationID, current.Task.InitiatorID)
	if err != nil || revokeResult.RowsAffected() != 1 {
		t.Fatalf("revoke Task client installation after PASS: err=%v", err)
	}
	assertCurrentVerdict(app.TaskValidationPending)
}
