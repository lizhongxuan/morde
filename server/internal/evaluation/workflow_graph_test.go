package evaluation

import (
	"encoding/json"
	"testing"
	"time"
)

func TestValidateWorkflowTraceKeepsPartialLiveObservationSourceBound(t *testing.T) {
	trace := partialWorkflowTrace()
	if err := ValidateWorkflowTrace(trace); err != nil {
		t.Fatalf("validate partial live workflow trace: %v", err)
	}
	if trace.DataClass != WorkflowDataLiveObserved || trace.Coverage != WorkflowCoveragePartial || len(trace.Gaps) == 0 {
		t.Fatalf("trace lost its evidence class or unresolved coverage: %+v", trace)
	}
}

func TestValidateWorkflowTraceRequiresSourceBoundFactsAndValidHandoffs(t *testing.T) {
	trace := WorkflowTrace{
		Version: 1, TraceID: "run-1", DataClass: WorkflowDataSynthetic,
		CapturedAt: time.Date(2026, time.November, 26, 0, 0, 0, 0, time.UTC), Coverage: WorkflowCoverageDeclaredComplete,
		CoverageSourceIDs: []string{"fixture"},
		Sources:           []WorkflowSource{{ID: "fixture", Kind: WorkflowSourceLocalTest, Locator: "workflow_graph_test.go:fixture"}},
		Nodes: []WorkflowNode{
			{ID: "main-agent", Kind: WorkflowNodeAgent, State: WorkflowNodeCompleted, SourceIDs: []string{"fixture"}},
			{ID: "child-agent", Kind: WorkflowNodeAgent, State: WorkflowNodeCompleted, SourceIDs: []string{"fixture"}},
		},
		Edges: []WorkflowEdge{{ID: "handoff-1", From: "main-agent", To: "child-agent", Kind: WorkflowEdgeHandoff,
			SourceIDs: []string{"fixture"}}},
	}
	if err := ValidateWorkflowTrace(trace); err != nil {
		t.Fatalf("validate source-bound synthetic handoff: %v", err)
	}
}

func TestValidateWorkflowTraceRejectsUnboundOrOverstatedFacts(t *testing.T) {
	tests := map[string]func(*WorkflowTrace){
		"model prose is not a source": func(trace *WorkflowTrace) {
			trace.Sources[0].Kind = WorkflowSourceKind("model_claim")
		},
		"live observation cannot use a test fixture source": func(trace *WorkflowTrace) {
			trace.Sources[0].Kind = WorkflowSourceLocalTest
		},
		"node has no source": func(trace *WorkflowTrace) {
			trace.Nodes[0].SourceIDs = nil
		},
		"node source kind does not match fact": func(trace *WorkflowTrace) {
			trace.Nodes[0].SourceIDs = []string{"model-ledger"}
		},
		"edge endpoint missing": func(trace *WorkflowTrace) {
			trace.Edges[0].To = "missing-node"
		},
		"handoff must connect agents": func(trace *WorkflowTrace) {
			trace.Edges[0].Kind = WorkflowEdgeHandoff
		},
		"absent requires a zero count": func(trace *WorkflowTrace) {
			count := 1
			trace.Gaps[0].Count = &count
		},
		"absent gap requires its ledger kind": func(trace *WorkflowTrace) {
			trace.Gaps[0].SourceIDs = []string{"model-ledger"}
		},
		"unknown cannot claim a zero count": func(trace *WorkflowTrace) {
			count := 0
			trace.Gaps[0].State = WorkflowGapUnknown
			trace.Gaps[0].Count = &count
		},
		"declared complete cannot hide recorded gaps": func(trace *WorkflowTrace) {
			trace.Coverage = WorkflowCoverageDeclaredComplete
		},
		"partial requires explicit gaps": func(trace *WorkflowTrace) {
			trace.Coverage = WorkflowCoverageUnknown
			trace.Gaps = nil
		},
		"duplicate node id": func(trace *WorkflowTrace) {
			trace.Nodes = append(trace.Nodes, trace.Nodes[0])
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			trace := partialWorkflowTrace()
			mutate(&trace)
			if err := ValidateWorkflowTrace(trace); err == nil {
				t.Fatal("expected invalid workflow trace to be rejected")
			}
		})
	}
}

func TestLoadWorkflowTraceRequiresStrictJSONAndValidGraph(t *testing.T) {
	trace := partialWorkflowTrace()
	raw, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("marshal trace fixture: %v", err)
	}
	loaded, err := LoadWorkflowTrace(raw)
	if err != nil {
		t.Fatalf("load valid workflow trace: %v", err)
	}
	if loaded.TraceID != trace.TraceID || loaded.DataClass != WorkflowDataLiveObserved || loaded.Coverage != WorkflowCoveragePartial {
		t.Fatalf("loaded workflow trace identity changed: %+v", loaded)
	}

	for name, input := range map[string][]byte{
		"trailing object": append(append([]byte(nil), raw...), []byte(` {}`)...),
		"unknown field":   []byte(`{"version":1,"unknown":true}`),
		"empty input":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadWorkflowTrace(input); err == nil {
				t.Fatal("expected invalid workflow JSON to be rejected")
			}
		})
	}
}

func partialWorkflowTrace() WorkflowTrace {
	zero := 0
	return WorkflowTrace{
		Version: 1, TraceID: "observed-task-1", DataClass: WorkflowDataLiveObserved,
		CapturedAt: time.Date(2026, time.November, 26, 0, 0, 0, 0, time.UTC), Coverage: WorkflowCoveragePartial,
		CoverageSourceIDs: []string{"task-ledger", "model-ledger"},
		Sources: []WorkflowSource{
			{ID: "task-ledger", Kind: WorkflowSourceTaskLedger, Locator: "task:q1234567"},
			{ID: "model-ledger", Kind: WorkflowSourceModelCallLedger, Locator: "task:q1234567:model_calls"},
			{ID: "operation-ledger", Kind: WorkflowSourceOperationLedger, Locator: "task:q1234567:operations"},
			{ID: "approval-ledger", Kind: WorkflowSourceApprovalLedger, Locator: "task:q1234567:approvals"},
			{ID: "validation-ledger", Kind: WorkflowSourceValidationLedger, Locator: "task:q1234567:validations"},
			{ID: "delivery-ledger", Kind: WorkflowSourceConversationOutput, Locator: "task:q1234567:delivery"},
		},
		Nodes: []WorkflowNode{
			{ID: "task-q1234567", Kind: WorkflowNodeTask, State: WorkflowNodeWaitingUser, SourceIDs: []string{"task-ledger"}},
			{ID: "call-1", Kind: WorkflowNodeModelCall, State: WorkflowNodeSettled, SourceIDs: []string{"model-ledger"}},
		},
		Edges: []WorkflowEdge{{ID: "task-call-1", From: "task-q1234567", To: "call-1", Kind: WorkflowEdgeInvoked,
			SourceIDs: []string{"task-ledger", "model-ledger"}}},
		Gaps: []WorkflowGap{
			{ID: "operation-absent", Stage: WorkflowStageOperation, Scope: "Task operations", State: WorkflowGapAbsent,
				Count: &zero, SourceIDs: []string{"operation-ledger"}},
			{ID: "approval-absent", Stage: WorkflowStageApproval, Scope: "Task approvals", State: WorkflowGapAbsent,
				Count: &zero, SourceIDs: []string{"approval-ledger"}},
			{ID: "verification-absent", Stage: WorkflowStageVerification, Scope: "Task validations", State: WorkflowGapAbsent,
				Count: &zero, SourceIDs: []string{"validation-ledger"}},
			{ID: "delivery-absent", Stage: WorkflowStageDelivery, Scope: "conversation result delivery", State: WorkflowGapAbsent,
				Count: &zero, SourceIDs: []string{"delivery-ledger"}},
		},
	}
}
