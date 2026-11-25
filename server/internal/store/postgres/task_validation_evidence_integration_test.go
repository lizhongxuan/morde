package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
)

func TestPostgresTaskValidationEvidenceIsRevisionBoundAndTransactional(t *testing.T) {
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
	var sourceMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' AND delivery_status='applied' ORDER BY sequence LIMIT 1`, current.Task.ID).Scan(&sourceMessageID); err != nil {
		t.Fatal(err)
	}
	other := newTestTaskSubmission(t, current.Task.OwnerID, "", "other-evidence-task", "independent task")
	otherResult, err := f.store.SubmitTask(ctx, other)
	if err != nil {
		t.Fatalf("create other Task source: %v", err)
	}
	var otherSourceMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user'`, otherResult.Task.ID).Scan(&otherSourceMessageID); err != nil {
		t.Fatal(err)
	}

	result := "Updated the file and verified the change."
	claim := app.TaskResultClaim{ID: "private-key-123", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRef{
		{Kind: app.TaskValidationEvidenceSource, ID: sourceMessageID},
	}}
	stepID := mustID(t, domain.NewStepID)
	base := app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: result, Usage: current.Task.Usage,
		Steps:            []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event:            task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
		ValidationClaims: []app.TaskResultClaim{claim},
	}
	forged := base
	forged.Event.Payload = []byte(`{"validation_evidence":{"task_revision":999}}`)
	if _, err := f.store.CommitTaskProgress(ctx, forged); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("caller supplied validation evidence error = %v, want invalid argument", err)
	}
	foreign := base
	foreign.ValidationClaims = []app.TaskResultClaim{{ID: claim.ID, StartByte: claim.StartByte, EndByte: claim.EndByte, EvidenceRefs: []app.TaskValidationEvidenceRef{
		{Kind: app.TaskValidationEvidenceSource, ID: otherSourceMessageID},
	}}}
	if _, err := f.store.CommitTaskProgress(ctx, foreign); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-Task evidence reference error = %v, want invalid argument", err)
	}
	var persisted int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND id=$2`, current.Task.ID, stepID).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("failed evidence transaction persisted its Task Step: count=%d err=%v", persisted, err)
	}

	base.ValidationClaims[0].EvidenceRefs = append(base.ValidationClaims[0].EvidenceRefs, app.TaskValidationEvidenceRef{
		Kind: app.TaskValidationEvidenceAction, ID: stepID,
	})
	if _, err := f.store.CommitTaskProgress(ctx, base); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unversioned Task Step evidence error = %v, want invalid argument", err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND id=$2`, current.Task.ID, stepID).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("unversioned evidence transaction persisted its Task Step: count=%d err=%v", persisted, err)
	}

	callDigest := sha256.Sum256([]byte("test_tool:{}"))
	actionStepID := mustID(t, domain.NewStepID)
	base.CurrentStep = actionStepID
	base.Steps = []task.Step{{ID: actionStepID, TaskID: current.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded}}
	base.ValidationClaims[0].EvidenceRefs = []app.TaskValidationEvidenceRef{{
		Kind: app.TaskValidationEvidenceSource, ID: "current",
	}}
	base.ValidationClaims[0].EvidenceRefs = append(base.ValidationClaims[0].EvidenceRefs, app.TaskValidationEvidenceRef{
		Kind: app.TaskValidationEvidenceAction, CallID: "call-validation-evidence",
	})
	base.ToolCall = &app.TaskToolCallProgress{
		AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
		AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
		TaskRevision: *current.Task.CurrentRevision, CallKey: "call-validation-evidence", ToolName: "test_tool",
		RequestDigest: callDigest[:], Phase: "execute", DetailState: "not_recorded",
	}
	unresolvedCall := base
	unresolvedCall.ValidationClaims = []app.TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRef{
		{Kind: app.TaskValidationEvidenceSource, ID: "current"},
		{Kind: app.TaskValidationEvidenceAction, CallID: "call-from-another-session"},
	}}}
	if _, err := f.store.CommitTaskProgress(ctx, unresolvedCall); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unresolved action call evidence error = %v, want invalid argument", err)
	}
	event, err := f.store.CommitTaskProgress(ctx, base)
	if err != nil {
		t.Fatalf("commit result with Task validation evidence: %v", err)
	}
	if _, err := f.store.CommitTaskProgress(ctx, base); err == nil {
		t.Fatal("completed Task accepted a duplicate result commit")
	}

	details, err := f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil || details.ValidationEvidence == nil || details.ValidationEvidence.TaskRevision != *current.Task.CurrentRevision ||
		details.Validation.State != app.TaskValidationPending ||
		details.ValidationEvidence.EventSequence != event.Sequence || len(details.ValidationEvidence.Claims) != 1 ||
		len(details.ValidationEvidence.Claims[0].EvidenceRefs) != 2 || details.ValidationEvidence.Claims[0].ID != "c1" ||
		details.ValidationEvidence.Claims[0].EvidenceRefs[0].ID != sourceMessageID ||
		details.ValidationEvidence.Claims[0].EvidenceRefs[1].ID != actionStepID ||
		details.ValidationEvidence.Claims[0].EvidenceRefs[1].CallID != "call-validation-evidence" {
		t.Fatalf("loaded Task validation evidence = %+v err=%v", details.ValidationEvidence, err)
	}
	var eventCount int
	var eventRevision int64
	var storedDigest []byte
	var payload []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) OVER(),task_revision,result_content_sha256,payload
FROM task_events WHERE task_id=$1 AND event_type='task.result' AND payload ? 'validation_evidence' ORDER BY sequence DESC LIMIT 1`, current.Task.ID).
		Scan(&eventCount, &eventRevision, &storedDigest, &payload); err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256([]byte(result))
	var storedPayload map[string]json.RawMessage
	if json.Unmarshal(payload, &storedPayload) != nil || eventCount != 1 || eventRevision != *current.Task.CurrentRevision ||
		len(storedDigest) != 32 || string(storedDigest) != string(wantDigest[:]) || len(storedPayload["validation_evidence"]) == 0 {
		t.Fatalf("stored evidence event count=%d revision=%d digest=%x payload=%s", eventCount, eventRevision, storedDigest, payload)
	}
	var reviewCount int
	var reviewSequence int64
	var reviewRevision int64
	var reviewDigest []byte
	var reviewState string
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) OVER(),result_event_sequence,task_revision,result_sha256,state
FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).
		Scan(&reviewCount, &reviewSequence, &reviewRevision, &reviewDigest, &reviewState); err != nil {
		t.Fatal(err)
	}
	if reviewCount != 1 || reviewSequence != event.Sequence || reviewRevision != *current.Task.CurrentRevision ||
		!bytes.Equal(reviewDigest, wantDigest[:]) || reviewState != "pending" {
		t.Fatalf("Task review outbox count=%d sequence=%d revision=%d digest=%x state=%q", reviewCount, reviewSequence,
			reviewRevision, reviewDigest, reviewState)
	}
}

func TestPostgresTaskValidationReviewOutboxCoversResultsWithoutEvidence(t *testing.T) {
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
	result := "The request is complete."
	stepID := mustID(t, domain.NewStepID)
	event, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: result, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	})
	if err != nil {
		t.Fatalf("complete Task without evidence claims: %v", err)
	}
	wantDigest := sha256.Sum256([]byte(result))
	var count int
	var state string
	var sequence, revision int64
	var digest, eventDigest []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) OVER(),review.state,review.result_event_sequence,review.task_revision,review.result_sha256,event.result_content_sha256
FROM task_validation_reviews review JOIN task_events event
 ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence AND event.task_revision=review.task_revision
WHERE review.task_id=$1`, current.Task.ID).
		Scan(&count, &state, &sequence, &revision, &digest, &eventDigest); err != nil {
		t.Fatal(err)
	}
	if count != 1 || state != "pending" || sequence != event.Sequence || revision != *current.Task.CurrentRevision ||
		!bytes.Equal(digest, wantDigest[:]) || !bytes.Equal(eventDigest, wantDigest[:]) {
		t.Fatalf("review row count=%d state=%q sequence=%d revision=%d digest=%x event_digest=%x", count, state, sequence, revision, digest, eventDigest)
	}
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: result, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	}); err == nil {
		t.Fatal("completed Task result was accepted a second time")
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("review outbox rows after duplicate result = %d err=%v; want exactly one", count, err)
	}
}

func TestPostgresTaskValidationFactsTreatHubOperationAsExecution(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task target','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatalf("insert Task target: %v", err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "validation-hub-ledger", "inspect the authorized target")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	requestHash := sha256.Sum256([]byte("morde_hub_command:{}"))
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_hub_operations
(task_id,owner_id,operation_key,tool_name,target_installation_id,action,task_authorization_version,
 target_grant_version,lease_version,request_hash,status)
VALUES ($1,$2,'call:hub-command','morde_hub_command',$3,'command',1,1,1,$4,'prepared')`,
		created.Task.ID, owner.ID, targetID, requestHash[:]); err != nil {
		t.Fatalf("insert Hub operation: %v", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	facts, err := loadTaskValidationVerdictFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: created.Task.ID}, "", nil, app.TaskValidationReviewReport{}, nil)
	if err != nil {
		t.Fatalf("load Task validation facts: %v", err)
	}
	if facts.TaskExecution != app.TaskValidationFactPresent || facts.IrreversibleAction != app.TaskValidationFactAbsent ||
		facts.RepeatedError != app.TaskValidationFactAbsent {
		t.Fatalf("Hub operation facts = %+v; want execution present and no irreversible action or repeated error (E09: Hub actions are task-scope governed, not per-call approval actions)", facts)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_hub_operations SET status='dispatched' WHERE task_id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	if err := loadTaskValidationExecutionFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: created.Task.ID}, &facts); err != nil {
		t.Fatal(err)
	}
	if facts.IrreversibleAction != app.TaskValidationFactUnknown || facts.ApprovalHistoryVerified {
		t.Fatalf("dispatched Hub command facts = %+v; want approval unknown", facts)
	}
}

func TestPostgresTaskValidationFactsTreatPendingRequestReconciliationAsExecution(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatalf("start Task Agent: %v", err)
	}
	details, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || details.Task.CurrentRevision == nil {
		t.Fatalf("load running Task: current=%+v err=%v", details.Task, err)
	}
	before, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeFacts, err := loadTaskValidationVerdictFacts(ctx, before, app.TaskValidationReviewClaim{TaskID: lease.Fence.TaskID}, "", nil, app.TaskValidationReviewReport{}, nil)
	_ = before.Rollback(ctx)
	if err != nil {
		t.Fatalf("load validation facts before request reconciliation: %v", err)
	}
	if beforeFacts.TaskExecution != app.TaskValidationFactAbsent {
		t.Fatalf("initial Task execution facts = %+v; want no recorded execution", beforeFacts)
	}
	reconciliation := newTaskAgentRequestReconciliation(lease.Fence, lease.Fence.AgentSessionID,
		*details.Task.CurrentRevision, app.TaskAgentRequestPhaseTool, app.TaskAgentRequestKindToolCall,
		"call_timeout_validation", strings.Repeat("a", 64), 1)
	if accepted, err := f.store.RecordTaskAgentRequestReconciliation(ctx, reconciliation); err != nil || !accepted {
		t.Fatalf("record pending request reconciliation: accepted=%t err=%v", accepted, err)
	}
	tx, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	facts, err := loadTaskValidationVerdictFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: lease.Fence.TaskID}, "", nil, app.TaskValidationReviewReport{}, nil)
	if err != nil {
		t.Fatalf("load Task validation facts: %v", err)
	}
	if facts.TaskExecution != app.TaskValidationFactPresent || facts.IrreversibleAction != app.TaskValidationFactUnknown ||
		facts.RepeatedError != app.TaskValidationFactAbsent {
		t.Fatalf("pending request reconciliation facts = %+v; want execution present, approval unknown, and no repeated error", facts)
	}
}

func TestPostgresTaskValidationReviewClaimsAreLeasedAndFenced(t *testing.T) {
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
	result := "A reviewable completion."
	stepID := mustID(t, domain.NewStepID)
	event, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: result, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	})
	if err != nil {
		t.Fatalf("complete Task: %v", err)
	}

	first, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-one", 30*time.Second, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first review claim = %+v err=%v", first, err)
	}
	claim := first[0]
	if claim.OwnerID != current.Task.OwnerID || claim.TaskID != current.Task.ID || claim.TaskRevision != *current.Task.CurrentRevision ||
		claim.ResultEventSequence != event.Sequence || claim.ClaimGeneration != 1 || claim.LeaseOwner != "review-worker-one" {
		t.Fatalf("review claim identity = %+v", claim)
	}
	if next, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-two", 30*time.Second, 10); err != nil || len(next) != 0 {
		t.Fatalf("active review lease was claimed twice: claims=%+v err=%v", next, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE task_id=$1`, current.Task.ID); err != nil {
		t.Fatal(err)
	}
	second, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-two", 30*time.Second, 10)
	if err != nil || len(second) != 1 || second[0].ClaimGeneration != claim.ClaimGeneration+1 || second[0].LeaseOwner != "review-worker-two" {
		t.Fatalf("expired review lease was not fenced on reclaim: claims=%+v err=%v", second, err)
	}
	if released, err := f.store.RetryTaskValidationReview(ctx, claim, 0); err != nil || released {
		t.Fatalf("stale review claim changed queue state: released=%v err=%v", released, err)
	}
	if released, err := f.store.RetryTaskValidationReview(ctx, second[0], time.Second); err != nil || !released {
		t.Fatalf("current review claim was not retried: released=%v err=%v", released, err)
	}
	if next, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-three", 30*time.Second, 10); err != nil || len(next) != 0 {
		t.Fatalf("review retry ignored its delay: claims=%+v err=%v", next, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET available_at=clock_timestamp()-interval '1 second' WHERE task_id=$1`, current.Task.ID); err != nil {
		t.Fatal(err)
	}
	third, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-three", 30*time.Second, 10)
	if err != nil || len(third) != 1 || third[0].ClaimGeneration != second[0].ClaimGeneration+1 {
		t.Fatalf("delayed review was not reclaimed: claims=%+v err=%v", third, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=current_revision+1 WHERE id=$1`, current.Task.ID); err != nil {
		t.Fatal(err)
	}
	if stale, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-four", 30*time.Second, 10); err != nil || len(stale) != 0 {
		t.Fatalf("stale Task revision was claimed: claims=%+v err=%v", stale, err)
	}
	var reviewState, taskStatus, taskLeaseOwner string
	if err := f.store.pool.QueryRow(ctx, `SELECT review.state,task.status,task.lease_owner FROM task_validation_reviews review JOIN tasks task ON task.id=review.task_id WHERE review.task_id=$1`, current.Task.ID).
		Scan(&reviewState, &taskStatus, &taskLeaseOwner); err != nil {
		t.Fatal(err)
	}
	if reviewState != "superseded" || taskStatus != string(task.StatusCompleted) || taskLeaseOwner != "" {
		t.Fatalf("review supersession changed Task lifecycle or retained a lease: review=%q status=%q lease=%q", reviewState, taskStatus, taskLeaseOwner)
	}
	if released, err := f.store.RetryTaskValidationReview(ctx, third[0], 0); err != nil || released {
		t.Fatalf("superseded review claim was still active: released=%v err=%v", released, err)
	}
}

func TestPostgresTaskValidationReviewRetryLimitFailsClosed(t *testing.T) {
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
	stepID := mustID(t, domain.NewStepID)
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: "Retry limit test result.", Usage: current.Task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	}); err != nil {
		t.Fatalf("complete Task: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET attempts=7 WHERE task_id=$1`, current.Task.ID); err != nil {
		t.Fatal(err)
	}
	claims, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-retry-limit", 30*time.Second, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim final review attempt: claims=%+v err=%v", claims, err)
	}
	if retried, err := f.store.RetryTaskValidationReview(ctx, claims[0], 0); err != nil || !retried {
		t.Fatalf("finish retry-limited claim: retried=%v err=%v", retried, err)
	}
	var reviewState, taskStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT review.state,task.status FROM task_validation_reviews review JOIN tasks task ON task.id=review.task_id WHERE review.task_id=$1`, current.Task.ID).
		Scan(&reviewState, &taskStatus); err != nil {
		t.Fatal(err)
	}
	if reviewState != "failed" || taskStatus != string(task.StatusCompleted) {
		t.Fatalf("retry exhaustion state changed Task lifecycle: review=%q task=%q", reviewState, taskStatus)
	}
	if claims, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-after-limit", 30*time.Second, 10); err != nil || len(claims) != 0 {
		t.Fatalf("failed review was reclaimed: claims=%+v err=%v", claims, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET state='processing',lease_owner='abandoned-review-worker',
lease_expires_at=clock_timestamp()-interval '1 second' WHERE task_id=$1`, current.Task.ID); err != nil {
		t.Fatal(err)
	}
	if claims, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-after-crash", 30*time.Second, 10); err != nil || len(claims) != 0 {
		t.Fatalf("expired exhausted review lease was claimed: claims=%+v err=%v", claims, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).Scan(&reviewState); err != nil || reviewState != "failed" {
		t.Fatalf("expired exhausted review state = %q err=%v", reviewState, err)
	}
}

func TestPostgresTaskValidationReviewCompletionRequiresLiveFencesAndPersistsIdentity(t *testing.T) {
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
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' AND delivery_status='applied' ORDER BY sequence LIMIT 1`, current.Task.ID).Scan(&sourceMessageID); err != nil {
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
		t.Fatalf("complete Task with evidence: %v", err)
	}
	var boundExecutionCallID string
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(execution_model_call_id,'') FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).
		Scan(&boundExecutionCallID); err != nil || boundExecutionCallID != executionModel.CallID {
		t.Fatalf("review outbox execution model binding = %q, want %q, err=%v", boundExecutionCallID, executionModel.CallID, err)
	}
	claims, err := f.store.ClaimTaskValidationReviews(ctx, "review-worker-one", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim review: claims=%+v err=%v", claims, err)
	}
	current, err = f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil || current.ValidationEvidence == nil {
		t.Fatalf("load result evidence: evidence=%+v err=%v", current.ValidationEvidence, err)
	}
	completion := taskValidationReviewCompletionFixture(t, f.store, claims[0], current.ValidationEvidence)
	completion.ExecutionModel = executionModel
	completion.Report.Findings[0].Assessment = app.TaskValidationReviewContradicts
	completion.Report.Recommendation = app.TaskValidationFail
	completion.ReviewerModel.SessionID = completion.SessionID
	completion.ReviewerModel.ThroughSequence = 0
	completion.ReviewerModel, completion.ResponseText = settleTaskValidationReviewModelResponse(t, f.store, claims[0], completion.ReviewerModel, completion.Report)
	invalid := completion
	invalid.Report.Findings = append([]app.TaskValidationReviewFinding(nil), completion.Report.Findings...)
	invalid.Report.Findings[0].EvidenceRefs = []app.TaskValidationEvidenceRefRecord{{
		Kind: app.TaskValidationEvidenceSource, ID: "m9999999", TaskRevision: *current.Task.CurrentRevision, Sequence: 1,
	}}
	invalidText, err := json.Marshal(invalid.Report)
	if err != nil {
		t.Fatal(err)
	}
	invalid.ResponseText = string(invalidText)
	if updated, err := f.store.CompleteTaskValidationReview(ctx, invalid); err == nil || updated {
		t.Fatalf("unavailable evidence completion: updated=%v err=%v; want validation failure", updated, err)
	}
	invalid = completion
	invalid.ReviewerModel.ConnectionVersion = invalid.ReviewerModel.ConnectionVersion.Add(time.Second)
	if updated, err := f.store.CompleteTaskValidationReview(ctx, invalid); err == nil || updated {
		t.Fatalf("changed reviewer response version completion: updated=%v err=%v; want response fence failure", updated, err)
	}
	var reviewState string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).Scan(&reviewState); err != nil || reviewState != "processing" {
		t.Fatalf("invalid review changed outbox state: state=%q err=%v", reviewState, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE task_id=$1`, current.Task.ID); err != nil {
		t.Fatal(err)
	}
	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || updated {
		t.Fatalf("expired lease completion: updated=%v err=%v; want fenced", updated, err)
	}
	claims, err = f.store.ClaimTaskValidationReviews(ctx, "review-worker-two", 30*time.Second, 1)
	if err != nil || len(claims) != 1 || claims[0].ClaimGeneration != completion.Claim.ClaimGeneration+1 {
		t.Fatalf("reclaim expired review: claims=%+v err=%v", claims, err)
	}
	completion = taskValidationReviewCompletionFixture(t, f.store, claims[0], current.ValidationEvidence)
	completion.ExecutionModel = executionModel
	completion.Report.Findings[0].Assessment = app.TaskValidationReviewContradicts
	completion.Report.Recommendation = app.TaskValidationFail
	completion.ReviewerModel.SessionID = completion.SessionID
	completion.ReviewerModel.ThroughSequence = 0
	completion.ReviewerModel, completion.ResponseText = settleTaskValidationReviewModelResponse(t, f.store, claims[0], completion.ReviewerModel, completion.Report)
	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || !updated {
		t.Fatalf("complete review: updated=%v err=%v", updated, err)
	}
	var state, validationState, sessionID, executionFamily, reviewerFamily, reportSummary string
	var storedSequence int64
	var storedDigest []byte
	var executionConnectionVersion, reviewerConnectionVersion time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT state,validation_state,reviewer_session_id,execution_model_family,reviewer_model_family,
review_report->>'summary',result_event_sequence,result_sha256,execution_connection_version,reviewer_connection_version
FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).
		Scan(&state, &validationState, &sessionID, &executionFamily, &reviewerFamily, &reportSummary, &storedSequence, &storedDigest,
			&executionConnectionVersion, &reviewerConnectionVersion); err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256([]byte(result))
	if state != "reviewed" || validationState != string(app.TaskValidationFail) || sessionID != completion.SessionID || executionFamily != "openai-gpt6" || reviewerFamily != "deepseek-v4" ||
		reportSummary != completion.Report.Summary || storedSequence != event.Sequence || !bytes.Equal(storedDigest, wantDigest[:]) ||
		!executionConnectionVersion.Equal(completion.ExecutionModel.ConnectionVersion) ||
		!reviewerConnectionVersion.Equal(completion.ReviewerModel.ConnectionVersion) {
		t.Fatalf("persisted review identity/state mismatch: state=%q validation=%q session=%q families=%q/%q summary=%q seq=%d digest=%x versions=%v/%v",
			state, validationState, sessionID, executionFamily, reviewerFamily, reportSummary, storedSequence, storedDigest,
			executionConnectionVersion, reviewerConnectionVersion)
	}
	details, err := f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil || details.Validation.State != app.TaskValidationFail {
		t.Fatalf("Task details validation = %#v, err=%v; want FAIL from a contradictory H01 finding", details.Validation, err)
	}
	page, err := f.store.ListTasks(ctx, current.Task.OwnerID, app.TaskListRequest{Page: app.PageRequest{Limit: 20}})
	if err != nil || page.K2Projections[current.Task.ID].Validation.State != app.TaskValidationFail {
		t.Fatalf("Task list validation projection = %#v, err=%v; want FAIL", page.K2Projections[current.Task.ID], err)
	}
	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || updated {
		t.Fatalf("duplicate completion changed persisted review: updated=%v err=%v", updated, err)
	}
	var lateValidationDeliveries int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox
WHERE task_id=$1 AND source_kind='late_validation'`, current.Task.ID).Scan(&lateValidationDeliveries); err != nil || lateValidationDeliveries != 0 {
		t.Fatalf("unpublished first result created %d late-validation deliveries err=%v", lateValidationDeliveries, err)
	}
	var taskStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, current.Task.ID).Scan(&taskStatus); err != nil || taskStatus != string(task.StatusCompleted) {
		t.Fatalf("review changed Task lifecycle: status=%q err=%v", taskStatus, err)
	}
	newRevision := *current.Task.CurrentRevision + 1
	newResult := "A later Task revision is complete and has not been reviewed."
	newDigest := sha256.Sum256([]byte(newResult))
	var newSequence int64
	if err := f.store.pool.QueryRow(ctx, `UPDATE tasks SET current_revision=$2,desired_revision=$2,event_seq=event_seq+1,
result=$3,status='completed' WHERE id=$1 RETURNING event_seq`, current.Task.ID, newRevision, newResult).Scan(&newSequence); err != nil {
		t.Fatalf("advance Task revision for projection regression: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload,task_revision,result_content_sha256)
VALUES ($1,$2,'task.result','{"status":"completed"}'::jsonb,$3,$4)`, current.Task.ID, newSequence, newRevision, newDigest[:]); err != nil {
		t.Fatalf("insert later revision result event: %v", err)
	}
	details, err = f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil || details.Task.CurrentRevision == nil || *details.Task.CurrentRevision != newRevision || details.Validation.State != app.TaskValidationPending {
		t.Fatalf("later Task revision inherited old validation verdict: revision=%v validation=%#v err=%v", details.Task.CurrentRevision, details.Validation, err)
	}
	page, err = f.store.ListTasks(ctx, current.Task.OwnerID, app.TaskListRequest{Page: app.PageRequest{Limit: 20}})
	if err != nil || page.K2Projections[current.Task.ID].Validation.State != app.TaskValidationPending {
		t.Fatalf("later Task revision list validation = %#v, err=%v; want pending", page.K2Projections[current.Task.ID].Validation, err)
	}
	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || updated {
		t.Fatalf("stale old-revision review changed the new revision: updated=%v err=%v", updated, err)
	}
	var reviewCount int
	var storedRevision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*),max(task_revision) FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).
		Scan(&reviewCount, &storedRevision); err != nil || reviewCount != 1 || storedRevision != *current.Task.CurrentRevision || storedRevision == newRevision {
		t.Fatalf("review rows after stale completion = count:%d revision:%d err:%v; want one old-revision record", reviewCount, storedRevision, err)
	}
}

func seedTaskValidationExecutionModelResponse(t *testing.T, store *Store, lease app.AgentInstanceFence) app.TaskValidationReviewModel {
	t.Helper()
	ctx := context.Background()
	connectionID, endpoint, configuredModel := "p1234567", "https://api.openai.com/v1", "gpt-6-luna"
	var connectionVersion time.Time
	if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, connectionID).Scan(&connectionVersion); err != nil {
		t.Fatal(err)
	}
	checkpointDigest := sha256.Sum256([]byte("review execution checkpoint"))
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET model_id=$2,checkpoint_through_seq=0,checkpoint_digest=$3 WHERE id=$1`,
		lease.TaskID, connectionID, checkpointDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, lease.TaskID, lease.AgentSessionID, checkpointDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,
 engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,$3,$4,$5,$5,0,0,$6,decode('01','hex'),$7,$8,$9,$10)`, lease.TaskID, lease.OwnerID,
		lease.AgentInstanceID, lease.AgentGeneration, lease.AgentSessionID, checkpointDigest[:],
		lease.EngineVersion, lease.ProfileVersion, lease.AuthorizationVersion, lease.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	fence := app.ModelTaskFence{TaskID: lease.TaskID, OwnerID: lease.OwnerID, InitiatorID: lease.InitiatorID,
		LeaseOwner: lease.LeaseOwner, LeaseVersion: lease.LeaseVersion, AgentInstanceID: lease.AgentInstanceID,
		AgentGeneration: lease.AgentGeneration, AgentSessionID: lease.AgentSessionID, AgentRootSessionID: lease.AgentSessionID,
		AuthorizationVersion: lease.AuthorizationVersion, ThroughSequence: 0, EngineVersion: lease.EngineVersion,
		ProfileVersion: lease.ProfileVersion}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	requestDigest := sha256.Sum256([]byte("review execution model request"))
	reservation := app.ModelCallReservation{Source: app.ModelCallTask, Task: &fence, OwnerID: fence.InitiatorID,
		ModelID: connectionID, ProviderEndpoint: endpoint, ProviderModel: configuredModel, ConnectionVersion: connectionVersion,
		CallID: callID, RequestDigest: requestDigest[:], ReservedTokens: 128}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,1000,3)
ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,max_concurrent=EXCLUDED.max_concurrent`, fence.InitiatorID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatalf("reserve execution model response: %v", err)
	}
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("dispatch execution model response: %v", err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	response := app.ModelResponse{Text: "The main execution model produced the completed result.", StopReason: "stop",
		Usage: app.ModelUsage{InputTokens: 8, OutputTokens: 5, Known: true}, ProviderReportedModel: configuredModel}
	stored, err := app.SealTaskModelResponse(sealer, reservation, response)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{Source: app.ModelCallTask, OwnerID: reservation.OwnerID,
		CallID: reservation.CallID, Usage: response.Usage, ProviderReportedModel: response.ProviderReportedModel,
		TaskCall: &reservation, ResponseCiphertext: stored.Ciphertext, ResponseDigest: stored.Digest}); err != nil {
		t.Fatalf("settle execution model response: %v", err)
	}
	identity, ok := app.ResolveTaskValidationModelIdentity(endpoint, configuredModel, response.ProviderReportedModel)
	if !ok {
		t.Fatal("execution model is missing from the review catalog")
	}
	return app.TaskValidationReviewModel{CallID: callID, ConnectionID: connectionID, ConnectionVersion: connectionVersion,
		Endpoint: endpoint, ConfiguredModel: configuredModel, ReportedModel: response.ProviderReportedModel, Identity: identity,
		ResponseDigest: stored.Digest, SessionID: fence.AgentSessionID, ThroughSequence: fence.ThroughSequence}
}

func settleTaskValidationReviewModelResponse(t *testing.T, store *Store, claim app.TaskValidationReviewClaim,
	model app.TaskValidationReviewModel, report app.TaskValidationReviewReport) (app.TaskValidationReviewModel, string) {
	t.Helper()
	ctx := context.Background()
	text, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	requestFence := app.TaskValidationReviewModelFence{Claim: claim, InitiatorID: claim.InitiatorID,
		SessionID: model.SessionID, ThroughSequence: model.ThroughSequence}
	callID, err := app.DSHTaskValidationReviewModelCallID(requestFence)
	if err != nil {
		t.Fatal(err)
	}
	requestDigest := sha256.Sum256(append([]byte("review response request:"), text...))
	reservation := app.ModelCallReservation{Source: app.ModelCallReview, Review: &requestFence, OwnerID: claim.InitiatorID,
		ModelID: model.ConnectionID, ProviderEndpoint: model.Endpoint, ProviderModel: model.ConfiguredModel,
		ConnectionVersion: model.ConnectionVersion, CallID: callID, RequestDigest: requestDigest[:], ReservedTokens: 500}
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatalf("reserve reviewer response: %v", err)
	}
	if err := store.BeginModelCallDispatch(ctx, reservation); err != nil {
		t.Fatalf("dispatch reviewer response: %v", err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	response := app.ModelResponse{Text: string(text), StopReason: "stop",
		Usage: app.ModelUsage{InputTokens: 120, OutputTokens: 80, Known: true}, ProviderReportedModel: model.ReportedModel}
	stored, err := app.SealTaskValidationReviewModelResponse(sealer, reservation, response)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{Source: app.ModelCallReview, OwnerID: reservation.OwnerID,
		CallID: reservation.CallID, Usage: response.Usage, ProviderReportedModel: response.ProviderReportedModel,
		ReviewCall: &reservation, ResponseCiphertext: stored.Ciphertext, ResponseDigest: stored.Digest,
		ResponseTextDigest: stored.TextDigest}); err != nil {
		t.Fatalf("settle reviewer response: %v", err)
	}
	model.CallID, model.ResponseDigest = callID, stored.Digest
	return model, string(text)
}

func taskValidationReviewCompletionFixture(t *testing.T, store *Store, claim app.TaskValidationReviewClaim, evidence *app.TaskValidationEvidenceRecord) app.TaskValidationReviewCompletion {
	t.Helper()
	if evidence == nil || len(evidence.Claims) == 0 || len(evidence.Claims[0].EvidenceRefs) == 0 {
		t.Fatal("review completion fixture requires persisted evidence")
	}
	var executionConnectionVersion, reviewerConnectionVersion time.Time
	if err := store.pool.QueryRow(context.Background(), `SELECT updated_at FROM model_connections WHERE id='p1234567'`).Scan(&executionConnectionVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(context.Background(), `SELECT updated_at FROM model_connections WHERE id='p2234567'`).Scan(&reviewerConnectionVersion); err != nil {
		t.Fatal(err)
	}
	identity := func(endpoint, configured string) app.TaskValidationModelIdentity {
		value, ok := app.ResolveTaskValidationModelIdentity(endpoint, configured, configured)
		if !ok {
			t.Fatalf("model %q is missing from the review catalog", configured)
		}
		return value
	}
	ref := evidence.Claims[0].EvidenceRefs[0]
	findings := make([]app.TaskValidationReviewFinding, 0, 7)
	for _, id := range []app.TaskHardCheckID{app.TaskHardCheckConclusionFirst, app.TaskHardCheckEvidence, app.TaskHardCheckIndependent,
		app.TaskHardCheckClarification, app.TaskHardCheckApproval, app.TaskHardCheckCorrection, app.TaskHardCheckReplay} {
		findings = append(findings, app.TaskValidationReviewFinding{ID: id, Assessment: app.TaskValidationReviewSupports,
			Rationale: "The cited task evidence supports this check.", EvidenceRefs: []app.TaskValidationEvidenceRefRecord{ref}})
	}
	return app.TaskValidationReviewCompletion{
		Claim: claim, SessionID: "review_session_1234567890",
		ExecutionModel: app.TaskValidationReviewModel{ConnectionID: "p1234567", ConnectionVersion: executionConnectionVersion, Endpoint: "https://api.openai.com/v1",
			ConfiguredModel: "gpt-6-luna", ReportedModel: "gpt-6-luna", Identity: identity("https://api.openai.com/v1", "gpt-6-luna")},
		ReviewerModel: app.TaskValidationReviewModel{ConnectionID: "p2234567", ConnectionVersion: reviewerConnectionVersion, Endpoint: "https://api.deepseek.com",
			ConfiguredModel: "deepseek-flash", ReportedModel: "deepseek-flash", Identity: identity("https://api.deepseek.com", "deepseek-flash")},
		Report: app.TaskValidationReviewReport{Recommendation: app.TaskValidationPass, Summary: "The task result is supported by its evidence.", Findings: findings},
	}
}

func TestPostgresTaskValidationEvidenceResolvesFinalResultAndDelegatedStep(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load running Task: current=%+v err=%v", current, err)
	}
	var sourceMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' AND delivery_status='applied' ORDER BY sequence LIMIT 1`, current.Task.ID).Scan(&sourceMessageID); err != nil {
		t.Fatal(err)
	}

	callID := "call_delegated_validation"
	callDigest := sha256.Sum256([]byte(`dsh_delegate:{"title":"Record child step","objective":"Verify provenance mapping"}`))
	toolStepID := mustID(t, domain.NewStepID)
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusRunning, CurrentStep: toolStepID, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: toolStepID, TaskID: current.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded}},
		ToolCall: &app.TaskToolCallProgress{
			AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
			AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			TaskRevision: *current.Task.CurrentRevision, CallKey: callID, ToolName: "dsh_delegate",
			RequestDigest: callDigest[:], Phase: "execute", DetailState: "not_recorded",
		},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.step", Payload: []byte(`{"step_id":"` + toolStepID + `"}`)},
	}); err != nil {
		t.Fatalf("commit delegated tool step: %v", err)
	}

	children, err := app.NewAgentChildSessionService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := children.Create(ctx, app.AgentChildSessionCommand{
		Fence: lease.Fence, ParentSessionID: lease.Fence.AgentSessionID, SpawnKey: callID,
		RootCheckpointThroughSequence: rootAgentCheckpointBaselineForTest(t, f.store, current.Task.ID,
			current.Task.OwnerID, lease.Fence.AgentSessionID),
		Arguments: []byte(`{"title":"Record child step","objective":"Verify provenance mapping"}`),
		Title:     "Record child step", Summary: "Record provenance mapping",
	})
	if err != nil {
		t.Fatalf("create delegated child: %v", err)
	}
	childActionCallID := "dsh_" + strings.Repeat("a", 64)
	childActionRefID := taskValidationNamespacedDSHCallID(current.Task.ID, child.SessionID, childActionCallID)
	commitChildToolStep := func(callID, toolName string) string {
		t.Helper()
		stepID := mustID(t, domain.NewStepID)
		var sequence int64
		if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.Task.ID).Scan(&sequence); err != nil {
			t.Fatalf("allocate child tool step sequence: %v", err)
		}
		digest := sha256.Sum256([]byte(toolName + `:{}`))
		if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
			Status: task.StatusRunning, CurrentStep: stepID, Usage: current.Task.Usage,
			Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: sequence, Kind: "tool", Status: task.StepSucceeded}},
			ToolCall: &app.TaskToolCallProgress{
				AgentSessionID: child.SessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
				AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
				TaskRevision: *current.Task.CurrentRevision, CallKey: callID, ToolName: toolName,
				RequestDigest: digest[:], Phase: "execute", DetailState: "not_recorded",
			},
			Event: task.Event{TaskID: current.Task.ID, Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
		}); err != nil {
			t.Fatalf("commit child tool step: %v", err)
		}
		return stepID
	}
	childActionStepID := commitChildToolStep(childActionCallID, "lookup")
	childBaselineCallID := "dsh_" + strings.Repeat("b", 64)
	childBaselineRefID := taskValidationNamespacedDSHCallID(current.Task.ID, child.SessionID, childBaselineCallID)
	childBaselineStepID := commitChildToolStep(childBaselineCallID, "benchmark")
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(f.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	childPayload := []byte(fmt.Sprintf(`{"header":{"id":%q},"from_seq":0,"events":[{"seq":0,"text":"done"}]}`, child.SessionID))
	childDigest := sha256.Sum256(childPayload)
	childFence := app.AgentCheckpointFence{
		TaskID: lease.Fence.TaskID, OwnerID: lease.Fence.OwnerID, InitiatorID: lease.Fence.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: child.SessionID, AgentRootSessionID: lease.Fence.AgentSessionID,
		AuthorizationVersion: lease.Fence.AuthorizationVersion, LeaseOwner: lease.Fence.LeaseOwner,
		LeaseVersion: lease.Fence.LeaseVersion, EngineVersion: lease.Fence.EngineVersion,
		ProfileVersion: lease.Fence.ProfileVersion,
	}
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{
		Fence: childFence, From: 0, Through: 0, Digest: childDigest[:], Payload: childPayload,
	}); err != nil {
		t.Fatalf("save child checkpoint: %v", err)
	}
	if _, _, err := children.Settle(ctx, app.AgentChildSessionSettleCommand{
		Fence: lease.Fence, ActorSessionID: lease.Fence.AgentSessionID, SessionID: child.SessionID,
		Status: task.StepSucceeded, CheckpointThrough: 0, CheckpointDigest: childDigest[:], Summary: "Child step recorded.",
	}); err != nil {
		t.Fatalf("settle delegated child: %v", err)
	}

	result := "The delegated step and final result were recorded."
	resultStepID := mustID(t, domain.NewStepID)
	invalidComparison := []app.TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)),
		EvidenceRefs: []app.TaskValidationEvidenceRef{{Kind: app.TaskValidationEvidenceSource, ID: "current"}},
		PerformanceComparison: &app.TaskResultPerformanceComparison{Metric: "elapsed", Unit: "ms", Before: 120, After: 90,
			Environment: "same synthetic test fixture", BaselineCallID: childBaselineRefID,
			AfterCallID: taskValidationNamespacedDSHCallID(current.Task.ID, child.SessionID, "missing-call")},
	}}
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, resolveErr := resolveTaskValidationClaims(ctx, tx, current.Task.OwnerID, current.Task.ID,
			current.Task.ConversationID, lease.Fence.AgentSessionID, resultStepID, *current.Task.CurrentRevision, invalidComparison)
		return resolveErr
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("missing performance measurement reference error = %v, want invalid argument", err)
	}
	_, err = f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: resultStepID, Result: result, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: resultStepID, TaskID: current.Task.ID, Sequence: 1, Kind: "result", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
		ValidationClaims: []app.TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRef{
			{Kind: app.TaskValidationEvidenceSource, ID: "current"},
			{Kind: app.TaskValidationEvidenceAction, CallID: callID},
			{Kind: app.TaskValidationEvidenceAction, CallID: childActionRefID},
			{Kind: app.TaskValidationEvidenceResult, ID: "current"},
		}, PerformanceComparison: &app.TaskResultPerformanceComparison{
			Metric: "elapsed time", Unit: "ms", Before: 120, After: 90, Environment: "same synthetic test fixture",
			BaselineCallID: childBaselineRefID, AfterCallID: childActionRefID,
		}}},
	})
	if err != nil {
		t.Fatalf("commit result with delegated and final-result evidence: %v", err)
	}
	details, err := f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil || details.ValidationEvidence == nil || len(details.ValidationEvidence.Claims) != 1 ||
		len(details.ValidationEvidence.Claims[0].EvidenceRefs) != 4 || details.ValidationEvidence.Claims[0].PerformanceComparison == nil {
		t.Fatalf("loaded delegated Task validation evidence = %+v err=%v", details.ValidationEvidence, err)
	}
	refs := details.ValidationEvidence.Claims[0].EvidenceRefs
	if refs[0].Kind != app.TaskValidationEvidenceSource || refs[0].ID != sourceMessageID ||
		refs[1].Kind != app.TaskValidationEvidenceAction || refs[1].ID != child.Step.ID || refs[1].CallID != callID ||
		refs[2].Kind != app.TaskValidationEvidenceAction || refs[2].ID != childActionStepID || refs[2].CallID != childActionRefID ||
		refs[3].Kind != app.TaskValidationEvidenceResult || refs[3].ID != resultStepID ||
		refs[1].TaskRevision != *current.Task.CurrentRevision || refs[2].TaskRevision != *current.Task.CurrentRevision ||
		refs[3].TaskRevision != *current.Task.CurrentRevision {
		t.Fatalf("resolved delegated/final-result references = %+v", refs)
	}
	comparison := details.ValidationEvidence.Claims[0].PerformanceComparison
	if comparison.Metric != "elapsed time" || comparison.Unit != "ms" || comparison.Before != 120 || comparison.After != 90 ||
		comparison.Delta != -30 || comparison.Environment != "same synthetic test fixture" ||
		comparison.BaselineRef.Kind != app.TaskValidationEvidenceBaseline || comparison.BaselineRef.ID != childBaselineStepID ||
		comparison.BaselineRef.CallID != childBaselineRefID || comparison.AfterRef.Kind != app.TaskValidationEvidenceBeforeAfter ||
		comparison.AfterRef.ID != childActionStepID || comparison.AfterRef.CallID != childActionRefID {
		t.Fatalf("resolved performance comparison = %+v", comparison)
	}
}

func TestPostgresTaskValidationEvidenceResolvesArtifactSourceCall(t *testing.T) {
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
	stepID := mustID(t, domain.NewStepID)
	artifactID := mustID(t, domain.NewArtifactID)
	callDigest := sha256.Sum256([]byte("file_write:{}"))
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusRunning, CurrentStep: stepID, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded}},
		ToolCall: &app.TaskToolCallProgress{
			AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
			AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			TaskRevision: *current.Task.CurrentRevision, CallKey: "write-1", ToolName: "file_write",
			RequestDigest: callDigest[:], Phase: "execute", DetailState: "not_recorded",
		},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatalf("commit artifact source step: %v", err)
	}
	resultStepID := mustID(t, domain.NewStepID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status) VALUES ($1,$2,2,'result','succeeded')`, resultStepID, current.Task.ID); err != nil {
		t.Fatalf("insert unversioned result Step: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_artifacts (id,task_id,step_id,label,path,media_type) VALUES ($1,$2,$3,$4,$5,$6)`,
		artifactID, current.Task.ID, resultStepID, "report.txt", "report.txt", "text/plain"); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"artifact_id": artifactID, "step_id": resultStepID, "source_call_id": "write-1", "task_revision": *current.Task.CurrentRevision})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		sequence, err := incrementTaskEventSequence(ctx, tx, current.Task.ID)
		if err != nil {
			return err
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: current.Task.ID, Sequence: sequence, Type: "task.artifact", Payload: payload})
	}); err != nil {
		t.Fatalf("seed artifact source event: %v", err)
	}
	result := "Saved report.txt."
	claims := []app.TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRef{
		{Kind: app.TaskValidationEvidenceSideEffect, CallID: "write-1"},
	}}}
	var resolved []app.TaskValidationEvidenceClaimRecord
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var resolveErr error
		resolved, resolveErr = resolveTaskValidationClaims(ctx, tx, current.Task.OwnerID, current.Task.ID,
			current.Task.ConversationID, lease.Fence.AgentSessionID, resultStepID, *current.Task.CurrentRevision, claims)
		return resolveErr
	}); err != nil {
		t.Fatalf("resolve artifact source reference: %v", err)
	}
	if len(resolved) != 1 || len(resolved[0].EvidenceRefs) != 1 || resolved[0].EvidenceRefs[0].ID != artifactID ||
		resolved[0].EvidenceRefs[0].CallID != "write-1" || resolved[0].EvidenceRefs[0].TaskRevision != *current.Task.CurrentRevision {
		t.Fatalf("resolved artifact source evidence = %+v; want artifact bound to its verified write call", resolved)
	}
}

func TestPostgresTaskValidationArtifactEvidenceTracksCurrentWorkspaceFile(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHostWithoutWorkspace(t, ctx, store, owner.ID, "ai", 0)
	setAgentTestHostRuntime(t, store, hostValue, "validation-artifact-current-file-runtime")
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "validation artifact")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	path := "reports/result.txt"
	original := []byte("right")
	manifest := seedTaskArtifactManifest(t, store, workspaceValue.ID, 0, map[string][]byte{path: original})
	revision := int64(0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "validation-artifact-current-file", "write and verify a result")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("validation artifact input")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "worker-validation-artifact", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: claimed=%t task=%s err=%v", found, claimed.ID, err)
	}
	const supervisorID = "validation-artifact-current-file-supervisor"
	authenticator, err := app.NewAgentStopAuthenticator(supervisorID, bytes.Repeat([]byte{0x4d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
			AgentInstanceID: "agent-validation-artifact", AgentSessionID: "session-validation-artifact",
			SupervisorID: supervisorID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: bytes.Repeat([]byte{0x27}, 32), LeaseDuration: 30 * time.Second,
	})
	if err != nil || replayed {
		t.Fatalf("start Task Agent: lease=%+v replayed=%t err=%v", started.Fence, replayed, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text)
VALUES ('p1234567',NULL,'validation artifact execution model','https://api.openai.com/v1','gpt-6-luna',
decode(repeat('00',29),'hex'),clock_timestamp(),true)
ON CONFLICT (id) DO UPDATE SET base_url=EXCLUDED.base_url,model_name=EXCLUDED.model_name,checked_at=clock_timestamp(),capability_text=true`); err != nil {
		t.Fatalf("seed execution model connection: %v", err)
	}
	executionModel := seedTaskValidationExecutionModelResponse(t, store, started.Fence)
	stepID := mustID(t, domain.NewStepID)
	callID := "write_result_1"
	step := task.Step{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded,
		Title: "Write result", TaskRevision: &revision, ToolCallKey: callID, ToolName: "morde_workspace_write"}
	artifactID := mustID(t, domain.NewArtifactID)
	artifact := addArtifactProof(task.Artifact{ID: artifactID, TaskID: created.Task.ID, StepID: stepID, SourceCallID: callID,
		Label: "Result", Path: path, MediaType: "text/plain"}, manifest[path])
	result := "The result file contains right."
	if err := store.RecordHostReplicaHeartbeat(ctx, hostValue.ID, hostValue.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	resultEvent, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: result, Usage: created.Task.Usage,
		Steps: []task.Step{step}, Artifacts: []task.Artifact{artifact},
		ValidationClaims: []app.TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)),
			EvidenceRefs: []app.TaskValidationEvidenceRef{{Kind: app.TaskValidationEvidenceSideEffect, CallID: callID}}}},
		Event: task.Event{TaskID: created.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
		ToolCall: &app.TaskToolCallProgress{AgentSessionID: started.Fence.AgentSessionID, AgentInstanceID: started.Fence.AgentInstanceID,
			AgentGeneration: started.Fence.AgentGeneration, AuthorizationVersion: started.Fence.AuthorizationVersion,
			TaskRevision: revision, CallKey: callID, ToolName: "morde_workspace_write", RequestDigest: hashBytes("morde_workspace_write:" + callID),
			Phase: "execute", DetailState: "not_recorded"},
	})
	if err != nil {
		t.Fatalf("complete Task with Artifact evidence: %v", err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.ValidationEvidence == nil || len(details.ValidationEvidence.Claims) != 1 || len(details.ValidationEvidence.Claims[0].EvidenceRefs) != 1 {
		t.Fatalf("load completed Task evidence: evidence=%+v err=%v", details.ValidationEvidence, err)
	}
	ref := details.ValidationEvidence.Claims[0].EvidenceRefs[0]
	if ref.Kind != app.TaskValidationEvidenceSideEffect || ref.ID != artifactID || ref.Sequence != 1 || ref.CallID != callID ||
		ref.TaskRevision != revision || details.ValidationEvidence.EventSequence != resultEvent.Sequence {
		t.Fatalf("resolved Artifact evidence ref = %+v; want current source call and result event", ref)
	}
	contentDigest := sha256.Sum256(original)
	contentProof := app.TaskValidationReviewArtifactContentProof{Ref: ref, WorkspaceID: workspaceValue.ID, Revision: 0,
		Path: path, MediaType: "text/plain", SizeBytes: int64(len(original)), SHA256: hex.EncodeToString(contentDigest[:]),
		ReadbackSHA256: hex.EncodeToString(contentDigest[:]), PreviewSHA256: hex.EncodeToString(contentDigest[:]),
		PreviewBytes: int64(len(original)), TextContent: true, ContentComplete: true}
	encodedProofs, err := json.Marshal([]app.TaskValidationReviewArtifactContentProof{contentProof})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_validation_reviews
SET state='reviewed',validation_state='PASS',execution_model_call_id=$5,
    reviewer_session_id='fixture_reviewer',execution_model_id='paaaaaaa',execution_connection_version=clock_timestamp(),
    execution_endpoint='http://fixture',execution_configured_model='fixture-execution',execution_reported_model='fixture-execution',
    execution_model_family='fixture-execution-family',execution_catalog_version='fixture-v1',
    reviewer_model_id='pbbbbbbb',reviewer_connection_version=clock_timestamp(),reviewer_endpoint='http://fixture',
    reviewer_configured_model='fixture-reviewer',reviewer_reported_model='fixture-reviewer',
    reviewer_model_family='fixture-reviewer-family',reviewer_catalog_version='fixture-v1',review_report='{}'::jsonb,
    reviewed_at=clock_timestamp(),artifact_content_evidence=$2::jsonb
	WHERE task_id=$1 AND task_revision=$3 AND result_event_sequence=$4`, created.Task.ID, string(encodedProofs), revision, resultEvent.Sequence, executionModel.CallID); err != nil {
		t.Fatalf("seed reviewed Artifact content proof: %v", err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Validation.State != app.TaskValidationPass {
		t.Fatalf("current committed Workspace file validation = %#v err=%v; want PASS", details.Validation, err)
	}
	changed := []byte("wrong")
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions(workspace_id,revision,file_count,total_bytes)
VALUES ($1,1,1,$2)`, workspaceValue.ID, len(changed)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_revision_files SET valid_to_revision=1
WHERE workspace_id=$1 AND file_path=$2`, workspaceValue.ID, path); err != nil {
		t.Fatal(err)
	}
	seedTaskArtifactManifest(t, store, workspaceValue.ID, 1, map[string][]byte{path: changed})
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=1 WHERE id=$1`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Validation.State != app.TaskValidationPending {
		t.Fatalf("changed current Workspace file validation = %#v err=%v; want pending", details.Validation, err)
	}
}

// TestPostgresTaskValidationFactsResolveWithE09Ledger is the E09
// acceptance against a real database: test replay, correction and exact
// approval-to-operation bindings can be re-derived from stored records.
// A review without content evidence still remains pending.
//
// It also pins migration 00239: the test stamps (test_failed,
// test_passed, test_suite_passed) are inserted into
// task_steps.failure_reason, which migration 00237's check forbade.
func TestPostgresTaskValidationFactsResolveWithE09Ledger(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	submission := newTestTaskSubmission(t, owner.ID, "", "validation-e09-ledger", "fix the login bug")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.CurrentRevision == nil {
		t.Fatalf("load created Task: %+v err=%v", details.Task, err)
	}
	revision := *details.Task.CurrentRevision
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,task_revision)
VALUES ($1,$2,'e09-session',$3)`, created.Task.ID, owner.ID, revision); err != nil {
		t.Fatalf("insert Task agent session: %v", err)
	}

	suiteDigest := sha256.Sum256([]byte("shell_exec go test ./..."))
	targetedDigest := sha256.Sum256([]byte("shell_exec go test -run TestLogin ./..."))
	insertStep := func(sequence int64, status, toolName, callID, reason string, digest []byte) string {
		stepID := mustID(t, domain.NewStepID)
		if digest == nil {
			value := sha256.Sum256([]byte(toolName + " " + callID))
			digest = value[:]
		}
		phase := "execute"
		if status == string(task.StepSucceeded) {
			phase = "succeeded"
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,task_revision,tool_call_key,tool_name,request_digest,phase,detail_state,attempt_no,failure_reason,retryable,agent_session_id)
VALUES ($1,$2,$3,'tool',$4,'test step',CASE WHEN $10 LIKE 'test_%' THEN 'The test command ran; its exit status was recorded.' ELSE 'test step' END,
 $5,$6,$7,$8,$9,'available',1,NULLIF($10,''),false,'e09-session')`,
			stepID, created.Task.ID, sequence, status, revision, callID, toolName, digest, phase, reason); err != nil {
			t.Fatalf("insert Task step %d: %v", sequence, err)
		}
		return stepID
	}
	insertStep(1, string(task.StepFailed), "shell_exec", "call-suite-fail", "test_failed", suiteDigest[:])
	insertStep(2, string(task.StepSucceeded), "shell_exec", "call-suite-pass", "test_suite_passed", suiteDigest[:])
	insertStep(3, string(task.StepFailed), "shell_exec", "call-targeted-1", "test_failed", targetedDigest[:])
	insertStep(4, string(task.StepFailed), "shell_exec", "call-targeted-2", "test_failed", targetedDigest[:])
	insertStep(5, string(task.StepSucceeded), "shell_exec", "call-targeted-pass", "test_passed", targetedDigest[:])
	readStepID := insertStep(6, string(task.StepSucceeded), "file_read", "call-read", "", nil)

	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1`, created.Task.ID, hostValue.ID); err != nil {
		t.Fatalf("bind Task Host: %v", err)
	}
	insertApproval := func(version int64, callID string, digest []byte) {
		approvalID := mustID(t, domain.NewPendingRequestID)
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,task_revision,
 runtime_generation,runtime_instance_id,control_version,status,decision,responded_by_user_id,responded_at)
VALUES ($1,$2,$3,'approval','Approve shell test','Host shell','Run the test command','The command can change Workspace files',$4,'shell_exec',$5,$6,
 0,'test-runtime',0,'approved','approve',$7,clock_timestamp())`,
			approvalID, created.Task.ID, version, callID, digest, revision, owner.ID); err != nil {
			t.Fatalf("insert approved pending request %s: %v", callID, err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_operations
(id,task_id,operation_key,request_hash,host_id,runtime_generation,control_version,lease_version,status,
 task_revision,call_id,tool_name,approval_id,approval_version,approval_request_hash)
VALUES ($1,$2,$3,$4,$5,0,0,1,'succeeded',$6,$7,'shell_exec',$8,$9,$4)`,
			mustID(t, domain.NewOperationID), created.Task.ID, "e09-"+callID, digest, hostValue.ID, revision, callID, approvalID, version); err != nil {
			t.Fatalf("insert approved operation %s: %v", callID, err)
		}
	}
	insertApproval(1, "call-suite-pass", suiteDigest[:])
	insertApproval(2, "call-targeted-pass", targetedDigest[:])

	plan := task.Plan{TaskRevision: revision, Revision: 1, Goal: "Fix the login bug", CostEstimateState: "unknown",
		Steps:   []task.PlanStep{{ID: "step-1", Title: "Reproduce the bug", Status: task.PlanStepPending}},
		FixPlan: &task.FixPlan{ChangedFiles: []string{"auth.go"}, Risks: []string{"regression"}, Alternatives: []string{"revert"}}}
	planCommit := app.CloudDSHTaskPlanCommit{TaskID: created.Task.ID, OwnerID: owner.ID, AgentSessionID: "e09-session",
		AuthorizationVersion: details.Task.Authorization.Version, CallID: "call-plan-1", Plan: plan}
	planCommit.FingerprintSHA256 = postgresCloudDSHTaskPlanFingerprint(planCommit)
	planEvent := appTaskPlanEvent{TaskRevision: revision, PlanRevision: 1, Plan: plan}
	planEvent.Detail.CallID = planCommit.CallID
	planEvent.Detail.AgentSessionID = planCommit.AgentSessionID
	planEvent.Detail.Tool = "morde_task_plan_update"
	planEvent.Detail.AuthorizationVersion = planCommit.AuthorizationVersion
	planEvent.Detail.FingerprintSHA256 = planCommit.FingerprintSHA256
	planPayload, err := json.Marshal(planEvent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_events (task_id,sequence,event_type,payload)
SELECT $1,COALESCE(MAX(sequence),0)+1,'task.plan',$2 FROM task_events WHERE task_id=$1`, created.Task.ID, planPayload); err != nil {
		t.Fatalf("insert Task plan event: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_clarification_requests
(id,task_id,version,model_step_id,call_id,question,status,answer,responded_at)
VALUES ($1,$2,1,$3,'call-clarify-1','Which login flow is broken?','answered','The password flow.',clock_timestamp())`,
		mustID(t, domain.NewPendingRequestID), created.Task.ID, readStepID); err != nil {
		t.Fatalf("insert answered clarification: %v", err)
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	facts, err := loadTaskValidationVerdictFacts(ctx, tx,
		app.TaskValidationReviewClaim{TaskID: created.Task.ID, TaskRevision: revision}, "", nil, app.TaskValidationReviewReport{}, nil)
	if err != nil {
		t.Fatalf("load Task validation facts: %v", err)
	}
	if facts.TaskExecution != app.TaskValidationFactPresent ||
		facts.IrreversibleAction != app.TaskValidationFactPresent || !facts.ApprovalHistoryVerified ||
		facts.RepeatedError != app.TaskValidationFactPresent || !facts.CorrectionRecordVerified ||
		facts.ReplayVerified || !facts.ClarificationHistoryKnown {
		t.Fatalf("E09 ledger facts = %+v; want approval/correction verified and fix replay pending without immutable Workspace proof", facts)
	}

	// The fixture has no independently verified content evidence, so the
	// review must remain pending even though these ledger facts resolve.
	findings := make([]app.TaskValidationReviewFinding, 0, 7)
	for _, id := range []app.TaskHardCheckID{app.TaskHardCheckConclusionFirst, app.TaskHardCheckEvidence,
		app.TaskHardCheckIndependent, app.TaskHardCheckClarification, app.TaskHardCheckApproval,
		app.TaskHardCheckCorrection, app.TaskHardCheckReplay} {
		findings = append(findings, app.TaskValidationReviewFinding{ID: id, Assessment: app.TaskValidationReviewSupports})
	}
	report := app.TaskValidationReviewReport{Recommendation: app.TaskValidationPass, Findings: findings}
	if got := app.SummarizeTaskValidationEvaluations(app.EvaluateTaskValidationReview(report, facts)); got.State != app.TaskValidationPending {
		t.Fatalf("E09 ledger verdict without content evidence = %q, want pending", got.State)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_operations SET approval_request_hash=$3 WHERE task_id=$1 AND call_id=$2`,
		created.Task.ID, "call-suite-pass", targetedDigest[:]); err != nil {
		t.Fatalf("alter approval binding: %v", err)
	}
	if err := loadTaskValidationExecutionFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: created.Task.ID, TaskRevision: revision}, &facts); err != nil {
		t.Fatalf("reload mismatched operation binding: %v", err)
	}
	if facts.ApprovalHistoryVerified {
		t.Fatal("mismatched operation approval hash was accepted")
	}
	if _, err := tx.Exec(ctx, `UPDATE task_operations SET approval_request_hash=request_hash WHERE task_id=$1 AND call_id=$2`,
		created.Task.ID, "call-suite-pass"); err != nil {
		t.Fatalf("restore approval binding: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM task_operations WHERE task_id=$1 AND call_id=$2`, created.Task.ID, "call-targeted-pass"); err != nil {
		t.Fatalf("remove operation receipt: %v", err)
	}
	if err := loadTaskValidationExecutionFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: created.Task.ID, TaskRevision: revision}, &facts); err != nil {
		t.Fatalf("reload missing operation binding: %v", err)
	}
	if facts.ApprovalHistoryVerified {
		t.Fatal("approval without an operation receipt was accepted")
	}
}

func TestPostgresTaskValidationApprovalBindingsAcrossLedgers(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	for _, test := range []struct {
		name string
		tool string
	}{
		{name: "mcp", tool: "morde_mcp_k1234567_send"},
		{name: "workspace", tool: "morde_workspace_write"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := f.createTask(t, f.ownerID, "verify a synthetic operation")
			approval := insertConversationResultTestApproval(t, f, value.ID, 0, "approved", "call-"+test.name, test.tool)
			if _, err := f.store.pool.Exec(ctx, `UPDATE task_pending_requests SET responded_by_user_id=$2 WHERE id=$1`, approval.id, f.ownerID); err != nil {
				t.Fatal(err)
			}
			if test.name == "mcp" {
				insertConversationResultTestOperation(t, f, value.ID, "succeeded", 0, approval.callID, approval.tool, &approval)
			} else {
				insertConversationResultTestWorkspacePublish(t, f, value.ID, "succeeded", 0, approval)
			}
			if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,task_revision)
VALUES ($1,$2,'binding-session',0)`, value.ID, f.ownerID); err != nil {
				t.Fatal(err)
			}
			stepID := mustID(t, domain.NewStepID)
			if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,task_revision,tool_call_key,tool_name,request_digest,agent_session_id)
VALUES ($1,$2,1,'tool','succeeded','operation','synthetic',0,$3,$4,$5,'binding-session')`,
				stepID, value.ID, approval.callID, approval.tool, approval.hash); err != nil {
				t.Fatal(err)
			}
			tx, err := f.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var facts app.TaskValidationVerdictFacts
			if err := loadTaskValidationExecutionFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: value.ID}, &facts); err != nil {
				t.Fatal(err)
			}
			if facts.IrreversibleAction != app.TaskValidationFactPresent || !facts.ApprovalHistoryVerified {
				t.Fatalf("valid %s binding was not verified: %+v", test.name, facts)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM task_steps WHERE task_id=$1 AND id=$2`, value.ID, stepID); err != nil {
				t.Fatal(err)
			}
			if err := loadTaskValidationExecutionFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: value.ID}, &facts); err != nil {
				t.Fatal(err)
			}
			if facts.ApprovalHistoryVerified {
				t.Fatalf("%s operation without its matching step was accepted", test.name)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,task_revision,tool_call_key,tool_name,request_digest,agent_session_id)
VALUES ($1,$2,1,'tool','succeeded','operation','synthetic',0,$3,$4,$5,'binding-session')`,
				stepID, value.ID, approval.callID, approval.tool, approval.hash); err != nil {
				t.Fatal(err)
			}
			wrongHash := sha256.Sum256([]byte("different approval"))
			query := `UPDATE task_mcp_connector_operations SET approval_request_hash=$2 WHERE task_id=$1`
			if test.name == "workspace" {
				query = `UPDATE task_workspace_publish_receipts SET approval_request_hash=$2 WHERE task_id=$1`
			}
			if _, err := tx.Exec(ctx, query, value.ID, wrongHash[:]); err != nil {
				t.Fatal(err)
			}
			if err := loadTaskValidationExecutionFacts(ctx, tx, app.TaskValidationReviewClaim{TaskID: value.ID}, &facts); err != nil {
				t.Fatal(err)
			}
			if facts.ApprovalHistoryVerified {
				t.Fatalf("mismatched %s approval hash was accepted", test.name)
			}
		})
	}
}
