package app

import (
	"reflect"
	"testing"
	"time"

	"morde/server/internal/domain/task"
)

func taskEventChainTestEvent(sequence int64, eventType, payload string) task.Event {
	return task.Event{TaskID: "q1234567", Sequence: sequence, Type: eventType,
		Payload: []byte(payload), CreatedAt: time.Unix(1000+sequence, 0).UTC()}
}

func taskEventChainKinds(chain TaskEventChain) []TaskEventKind {
	kinds := make([]TaskEventKind, 0, len(chain.Nodes))
	for _, node := range chain.Nodes {
		kinds = append(kinds, node.Kind)
	}
	return kinds
}

func TestProjectTaskEventChainReplaysCompleteFixTask(t *testing.T) {
	events := []task.Event{
		taskEventChainTestEvent(1, "task.status", `{"status":"queued"}`),
		taskEventChainTestEvent(2, "task.plan", `{"step_id":"s0000001","sequence":1,"task_revision":0,"plan_revision":1}`),
		taskEventChainTestEvent(3, "task.message", `{"role":"user","message_id":"m0000001","turn_id":"s0000001","done":true}`),
		taskEventChainTestEvent(4, "task.operation", `{"operation_id":"op000001","status":"dispatched"}`),
		taskEventChainTestEvent(5, "task.step", `{"step_id":"s0000002","status":"running","kind":"tool","tool":"go_test"}`),
		taskEventChainTestEvent(6, "task.step", `{"step_id":"s0000002","status":"failed","kind":"tool","tool":"go_test"}`),
		taskEventChainTestEvent(7, "task.operation", `{"operation_id":"op000001","status":"unknown","requires_reconciliation":true}`),
		taskEventChainTestEvent(8, "task.step", `{"step_id":"s0000003","status":"succeeded","kind":"tool","tool":"go_test"}`),
		taskEventChainTestEvent(9, "task.waiting_user", `{"status":"waiting_user","reason":"approval_required"}`),
		taskEventChainTestEvent(10, "task.response", `{"pending_id":"p0000001","decision":"approve","status":"approved"}`),
		taskEventChainTestEvent(11, "task.artifact", `{"artifact_id":"a0000001","step_id":"s0000003"}`),
		taskEventChainTestEvent(12, "task.priority", `{"priority":"pinned","previous_priority":"normal"}`),
		taskEventChainTestEvent(13, "task.status", `{"status":"completed"}`),
		taskEventChainTestEvent(14, "task.result", `{"status":"completed"}`),
	}
	facts := TaskEventChainFacts{ValidationState: TaskValidationPass, ResultEventSequence: 14,
		ReceiptPublished: true, ReceiptEventSequence: 14}
	chain, err := ProjectTaskEventChain(events, facts)
	if err != nil {
		t.Fatalf("project fix Task chain: %v", err)
	}
	wantKinds := []TaskEventKind{TaskEventKindCreated, TaskEventKindSplit, TaskEventKindDispatched,
		TaskEventKindExecution, TaskEventKindFailure, TaskEventKindRecovery, TaskEventKindExecution,
		TaskEventKindApproval, TaskEventKindApproval, TaskEventKindVerification, TaskEventKindDelivery}
	if kinds := taskEventChainKinds(chain); !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("chain kinds = %v, want %v", kinds, wantKinds)
	}
	if chain.Nodes[0].Kind != TaskEventKindCreated || chain.Nodes[0].SourceSequence != 1 {
		t.Fatalf("chain head = %+v, want created at sequence 1", chain.Nodes[0])
	}
	// Recovery sits after the failure it recovers from, not before it.
	failureAt, recoveryAt := -1, -1
	for index, node := range chain.Nodes {
		if node.Kind == TaskEventKindFailure {
			failureAt = index
		}
		if node.Kind == TaskEventKindRecovery {
			recoveryAt = index
		}
	}
	if failureAt < 0 || recoveryAt <= failureAt {
		t.Fatalf("failure at %d, recovery at %d; want recovery after failure", failureAt, recoveryAt)
	}
	// Completed status is not delivery, and no event is silently dropped:
	// message, artifact, priority, completed status and the result event are
	// all listed as unmapped.
	wantUnmapped := []int64{3, 11, 12, 13, 14}
	if !reflect.DeepEqual(chain.UnmappedSequences, wantUnmapped) {
		t.Fatalf("unmapped = %v, want %v", chain.UnmappedSequences, wantUnmapped)
	}
	verification := chain.Nodes[len(chain.Nodes)-2]
	if verification.Kind != TaskEventKindVerification || verification.Detail != "PASS" || verification.SourceSequence != 14 {
		t.Fatalf("verification node = %+v, want PASS at sequence 14", verification)
	}
	delivery := chain.Nodes[len(chain.Nodes)-1]
	if delivery.Kind != TaskEventKindDelivery || delivery.SourceSequence != 14 || delivery.SourceType != "" {
		t.Fatalf("delivery node = %+v, want fact-derived delivery at sequence 14", delivery)
	}
}

func TestProjectTaskEventChainDeliveryNeedsVerificationAndReceipt(t *testing.T) {
	events := []task.Event{
		taskEventChainTestEvent(1, "task.status", `{"status":"queued"}`),
		taskEventChainTestEvent(2, "task.step", `{"step_id":"s0000001","status":"succeeded","kind":"tool"}`),
		taskEventChainTestEvent(3, "task.status", `{"status":"completed"}`),
		taskEventChainTestEvent(4, "task.result", `{"status":"completed"}`),
	}
	cases := []struct {
		name  string
		facts TaskEventChainFacts
		want  bool
	}{
		{"pass without published receipt", TaskEventChainFacts{ValidationState: TaskValidationPass,
			ResultEventSequence: 4, ReceiptPublished: false}, false},
		{"fail with published receipt", TaskEventChainFacts{ValidationState: TaskValidationFail,
			ResultEventSequence: 4, ReceiptPublished: true, ReceiptEventSequence: 4}, false},
		{"pending with published receipt", TaskEventChainFacts{ValidationState: TaskValidationPending,
			ResultEventSequence: 4, ReceiptPublished: true, ReceiptEventSequence: 4}, false},
		{"no verdict with published receipt", TaskEventChainFacts{ReceiptPublished: true,
			ReceiptEventSequence: 4}, false},
		{"pass with notes and published receipt", TaskEventChainFacts{ValidationState: TaskValidationPassNotes,
			ResultEventSequence: 4, ReceiptPublished: true, ReceiptEventSequence: 4}, true},
		{"pass and published receipt", TaskEventChainFacts{ValidationState: TaskValidationPass,
			ResultEventSequence: 4, ReceiptPublished: true, ReceiptEventSequence: 4}, true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			chain, err := ProjectTaskEventChain(events, item.facts)
			if err != nil {
				t.Fatalf("project chain: %v", err)
			}
			found := false
			for _, node := range chain.Nodes {
				if node.Kind == TaskEventKindDelivery {
					found = true
				}
			}
			if found != item.want {
				t.Fatalf("delivery present = %t, want %t (kinds %v)", found, item.want, taskEventChainKinds(chain))
			}
			// A completed status event never becomes delivery on its own.
			for _, node := range chain.Nodes {
				if node.SourceSequence == 3 && node.Kind == TaskEventKindDelivery {
					t.Fatalf("completed status event projected as delivery: %+v", node)
				}
			}
		})
	}
}

func TestProjectTaskEventChainMappingRules(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
		want      TaskEventKind
		mapped    bool
	}{
		{"plan splits", "task.plan", `{"plan_revision":1}`, TaskEventKindSplit, true},
		{"operation dispatched", "task.operation", `{"status":"dispatched"}`, TaskEventKindDispatched, true},
		{"operation failed", "task.operation", `{"status":"failed"}`, TaskEventKindFailure, true},
		{"operation unknown without reconciliation is execution", "task.operation", `{"status":"unknown"}`,
			TaskEventKindExecution, true},
		{"operation unknown with reconciliation is recovery", "task.operation",
			`{"status":"unknown","requires_reconciliation":true}`, TaskEventKindRecovery, true},
		{"operation succeeded is execution", "task.operation", `{"status":"succeeded"}`, TaskEventKindExecution, true},
		{"batch item failed", "task.batch_item", `{"status":"failed"}`, TaskEventKindFailure, true},
		{"batch item running is execution", "task.batch_item", `{"status":"running"}`, TaskEventKindExecution, true},
		{"status running dispatches", "task.status", `{"status":"running"}`, TaskEventKindDispatched, true},
		{"status failed", "task.status", `{"status":"failed"}`, TaskEventKindFailure, true},
		{"status completed is unmapped", "task.status", `{"status":"completed"}`, "", false},
		{"status cancelled is unmapped", "task.status", `{"status":"cancelled"}`, "", false},
		{"response to approval", "task.response", `{"pending_id":"p0000001","decision":"approve"}`,
			TaskEventKindApproval, true},
		{"clarification answer is unmapped", "task.response", `{"clarification_id":"c0000001","decision":"answer"}`,
			"", false},
		{"message is unmapped", "task.message", `{"role":"assistant"}`, "", false},
		{"artifact is unmapped", "task.artifact", `{"artifact_id":"a0000001"}`, "", false},
		{"priority is unmapped", "task.priority", `{"priority":"pinned"}`, "", false},
		{"unknown task type is unmapped", "task.custom", `{}`, "", false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			events := []task.Event{
				taskEventChainTestEvent(1, "task.status", `{"status":"queued"}`),
				taskEventChainTestEvent(2, item.eventType, item.payload),
			}
			chain, err := ProjectTaskEventChain(events, TaskEventChainFacts{})
			if err != nil {
				t.Fatalf("project chain: %v", err)
			}
			if !item.mapped {
				if len(chain.Nodes) != 1 || !reflect.DeepEqual(chain.UnmappedSequences, []int64{2}) {
					t.Fatalf("nodes = %v unmapped = %v, want only the created head and sequence 2 unmapped",
						taskEventChainKinds(chain), chain.UnmappedSequences)
				}
				return
			}
			if len(chain.Nodes) != 2 || chain.Nodes[1].Kind != item.want || chain.Nodes[1].SourceSequence != 2 {
				t.Fatalf("nodes = %v, want created then %s at sequence 2", taskEventChainKinds(chain), item.want)
			}
			if len(chain.UnmappedSequences) != 0 {
				t.Fatalf("unmapped = %v, want none", chain.UnmappedSequences)
			}
		})
	}
}

func TestProjectTaskEventChainRejectsBadInput(t *testing.T) {
	head := taskEventChainTestEvent(1, "task.status", `{"status":"queued"}`)
	cases := []struct {
		name   string
		events []task.Event
		facts  TaskEventChainFacts
	}{
		{"empty events", nil, TaskEventChainFacts{}},
		{"mixed tasks", []task.Event{head,
			{TaskID: "q7654321", Sequence: 2, Type: "task.step", Payload: []byte(`{}`)}}, TaskEventChainFacts{}},
		{"duplicate sequence", []task.Event{head,
			taskEventChainTestEvent(1, "task.step", `{"status":"running"}`)}, TaskEventChainFacts{}},
		{"invalid verdict", []task.Event{head},
			TaskEventChainFacts{ValidationState: "MAYBE", ResultEventSequence: 1}},
		{"verdict without result event", []task.Event{head},
			TaskEventChainFacts{ValidationState: TaskValidationPass}},
		{"published receipt without receipt event", []task.Event{head},
			TaskEventChainFacts{ValidationState: TaskValidationPass, ResultEventSequence: 1, ReceiptPublished: true}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if _, err := ProjectTaskEventChain(item.events, item.facts); err == nil {
				t.Fatalf("project chain accepted %s", item.name)
			}
		})
	}
}

func TestProjectTaskEventChainHeadIsCreatedFromFirstEvent(t *testing.T) {
	// A task opened on an unclear file-delivery intent starts with
	// task.waiting_user, not task.status/queued. The head is still created.
	events := []task.Event{
		taskEventChainTestEvent(1, "task.waiting_user", `{"status":"waiting_user","reason":"clarification_required"}`),
		taskEventChainTestEvent(2, "task.response", `{"clarification_id":"c0000001"}`),
	}
	chain, err := ProjectTaskEventChain(events, TaskEventChainFacts{})
	if err != nil {
		t.Fatalf("project chain: %v", err)
	}
	if chain.Nodes[0].Kind != TaskEventKindCreated || chain.Nodes[0].SourceType != "task.waiting_user" {
		t.Fatalf("head = %+v, want created from the first waiting_user event", chain.Nodes[0])
	}
	if !reflect.DeepEqual(chain.UnmappedSequences, []int64{2}) {
		t.Fatalf("unmapped = %v, want [2]", chain.UnmappedSequences)
	}
}
