package postgres

import (
	"context"
	"crypto/sha256"
	"reflect"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

// insertTaskEventChainTestEvent appends one stored event with a fixed
// sequence. The caller owns the task's event_seq counter: set it to the last
// inserted sequence before completing the task so the result event lands on
// the next sequence.
func insertTaskEventChainTestEvent(t *testing.T, f resultAggregationFixture, taskID string,
	sequence int64, eventType, payload string) {
	t.Helper()
	if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_events(task_id,sequence,event_type,payload)
VALUES ($1,$2,$3,$4::jsonb)`, taskID, sequence, eventType, payload); err != nil {
		t.Fatalf("insert Task event %d %s: %v", sequence, eventType, err)
	}
}

func setTaskEventChainTestEventSeq(t *testing.T, f resultAggregationFixture, taskID string, sequence int64) {
	t.Helper()
	if _, err := f.store.pool.Exec(context.Background(), `UPDATE tasks SET event_seq=$2 WHERE id=$1`, taskID, sequence); err != nil {
		t.Fatalf("set Task event_seq: %v", err)
	}
}

func taskEventChainTestKinds(chain app.TaskEventChain) []app.TaskEventKind {
	kinds := make([]app.TaskEventKind, 0, len(chain.Nodes))
	for _, node := range chain.Nodes {
		kinds = append(kinds, node.Kind)
	}
	return kinds
}

func TestPostgresTaskEventChainReplaysCompleteFixTask(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	value := f.createTask(t, f.ownerID, "fix the failing test")
	// A complete fix-task chain, written in replay order. The completed
	// status event (13) must stay unmapped: completion is not delivery.
	for _, event := range []struct {
		sequence  int64
		eventType string
		payload   string
	}{
		{1, "task.status", `{"status":"queued"}`},
		{2, "task.plan", `{"step_id":"s0000001","sequence":1,"task_revision":0,"plan_revision":1}`},
		{3, "task.message", `{"role":"user","message_id":"m0000001","turn_id":"s0000001","done":true}`},
		{4, "task.operation", `{"operation_id":"op000001","status":"dispatched"}`},
		{5, "task.step", `{"step_id":"s0000002","status":"running","kind":"tool","tool":"go_test"}`},
		{6, "task.step", `{"step_id":"s0000002","status":"failed","kind":"tool","tool":"go_test"}`},
		{7, "task.operation", `{"operation_id":"op000001","status":"unknown","requires_reconciliation":true}`},
		{8, "task.step", `{"step_id":"s0000003","status":"succeeded","kind":"tool","tool":"go_test"}`},
		{9, "task.waiting_user", `{"status":"waiting_user","reason":"approval_required"}`},
		{10, "task.response", `{"pending_id":"p0000001","decision":"approve","status":"approved"}`},
		{11, "task.artifact", `{"artifact_id":"a0000001","step_id":"s0000003"}`},
		{12, "task.priority", `{"priority":"pinned","previous_priority":"normal"}`},
		{13, "task.status", `{"status":"completed"}`},
	} {
		insertTaskEventChainTestEvent(t, f, value.ID, event.sequence, event.eventType, event.payload)
	}
	setTaskEventChainTestEventSeq(t, f, value.ID, 13)
	if !f.complete(t, &value, "fixed the bug and verified with tests") {
		t.Fatal("task result was not queued")
	}
	digest := sha256.Sum256([]byte(value.Content))
	seedConversationResultValidationReview(t, f, value, value.TaskRevision, value.EventSequence, digest[:], app.TaskValidationPass)
	// complete() already enqueued the review (no other active tasks), so the
	// route worker can run it straight away.
	f.runNextReview(t)
	var batchState string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_result_batches WHERE state='published'`).Scan(&batchState); err != nil {
		t.Fatalf("result receipt was not published: %v", err)
	}
	chain, err := f.store.GetTaskEventChain(ctx, f.ownerID, value.ID)
	if err != nil {
		t.Fatalf("read Task event chain: %v", err)
	}
	wantKinds := []app.TaskEventKind{app.TaskEventKindCreated, app.TaskEventKindSplit, app.TaskEventKindDispatched,
		app.TaskEventKindExecution, app.TaskEventKindFailure, app.TaskEventKindRecovery, app.TaskEventKindExecution,
		app.TaskEventKindApproval, app.TaskEventKindApproval, app.TaskEventKindVerification, app.TaskEventKindDelivery}
	if kinds := taskEventChainTestKinds(chain); !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("chain kinds = %v, want %v", kinds, wantKinds)
	}
	if chain.Nodes[0].Kind != app.TaskEventKindCreated || chain.Nodes[0].SourceSequence != 1 {
		t.Fatalf("chain head = %+v, want created at sequence 1", chain.Nodes[0])
	}
	wantUnmapped := []int64{3, 11, 12, 13, 14}
	if !reflect.DeepEqual(chain.UnmappedSequences, wantUnmapped) {
		t.Fatalf("unmapped = %v, want %v", chain.UnmappedSequences, wantUnmapped)
	}
	verification := chain.Nodes[len(chain.Nodes)-2]
	if verification.Kind != app.TaskEventKindVerification || verification.Detail != "PASS" {
		t.Fatalf("verification node = %+v, want PASS", verification)
	}
	delivery := chain.Nodes[len(chain.Nodes)-1]
	if delivery.Kind != app.TaskEventKindDelivery || delivery.SourceSequence != value.EventSequence {
		t.Fatalf("delivery node = %+v, want delivery at result sequence %d", delivery, value.EventSequence)
	}
}

func TestPostgresTaskEventChainOmitsDeliveryWhenVerificationFails(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	value := f.createTask(t, f.ownerID, "fix the failing test")
	insertTaskEventChainTestEvent(t, f, value.ID, 1, "task.status", `{"status":"queued"}`)
	setTaskEventChainTestEventSeq(t, f, value.ID, 1)
	if !f.complete(t, &value, "unverified fix attempt") {
		t.Fatal("task result was not queued")
	}
	digest := sha256.Sum256([]byte(value.Content))
	seedConversationResultValidationReview(t, f, value, value.TaskRevision, value.EventSequence, digest[:], app.TaskValidationFail)
	chain, err := f.store.GetTaskEventChain(ctx, f.ownerID, value.ID)
	if err != nil {
		t.Fatalf("read Task event chain: %v", err)
	}
	wantKinds := []app.TaskEventKind{app.TaskEventKindCreated, app.TaskEventKindVerification}
	if kinds := taskEventChainTestKinds(chain); !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("chain kinds = %v, want %v", kinds, wantKinds)
	}
	verification := chain.Nodes[1]
	if verification.Detail != "FAIL" || verification.SourceSequence != value.EventSequence {
		t.Fatalf("verification node = %+v, want FAIL at result sequence %d", verification, value.EventSequence)
	}
	if !reflect.DeepEqual(chain.UnmappedSequences, []int64{value.EventSequence}) {
		t.Fatalf("unmapped = %v, want [%d]", chain.UnmappedSequences, value.EventSequence)
	}
}

func TestPostgresTaskEventChainRequiresExistingTask(t *testing.T) {
	f := newResultAggregationFixture(t)
	if _, err := f.store.GetTaskEventChain(context.Background(), f.ownerID, "q0000000"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing Task event chain error = %v, want not_found", err)
	}
}
