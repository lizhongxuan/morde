package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/evaluation"
)

func TestGetTaskWorkflowTraceIsOwnerScopedAndBindsLedgerDigests(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "workflow-trace-source", true)
	ctx := context.Background()

	const objectiveCanary = "workflow-trace-objective-private-canary"
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET objective=$2 WHERE id=$1 AND owner_id=$3`,
		fixture.task.ID, objectiveCanary, fixture.task.OwnerID); err != nil {
		t.Fatalf("set objective redaction canary: %v", err)
	}

	messageID := mustID(t, domain.NewMessageID)
	const firstContent = "workflow-trace-assistant-private-canary-one"
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,actor_id,role,content,task_id,sequence,delivery_status,output_status,output_task_revision)
VALUES ($1,$2,$3,$3,'assistant',$4,$5,2,'applied','interrupted',$6)`, messageID,
		fixture.task.ConversationID, fixture.task.OwnerID, firstContent, fixture.task.ID, fixture.task.CurrentRevision); err != nil {
		t.Fatalf("insert interrupted assistant output: %v", err)
	}

	approvalID := mustID(t, domain.NewPendingRequestID)
	operationID := mustID(t, domain.NewOperationID)
	const callID = "call_workflow_trace_approval"
	const toolName = "host_exec"
	approvalHash := sha256.Sum256([]byte("workflow-trace-approval-binding"))
	operationHash := sha256.Sum256([]byte("workflow-trace-operation-request"))
	var hostGeneration, hostControlVersion int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT runtime_generation,control_version FROM hosts WHERE id=$1 AND owner_id=$2`,
		fixture.task.HostID, fixture.task.OwnerID).Scan(&hostGeneration, &hostControlVersion); err != nil {
		t.Fatalf("read workflow test Host fence: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,runtime_generation,runtime_instance_id,
control_version,task_revision,status,decision)
VALUES ($1,$2,1,'approval','Review action','host command','requested action','external effect',$3,$4,$5,$6,'workflow-trace',$7,$8,'pending','')`,
		approvalID, fixture.task.ID, callID, toolName, approvalHash[:], hostGeneration, hostControlVersion, fixture.task.CurrentRevision); err != nil {
		t.Fatalf("insert approval ledger row: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO task_operations
(id,task_id,operation_key,request_hash,host_id,runtime_generation,control_version,lease_version,status,task_revision,
call_id,tool_name,approval_id,approval_version,approval_request_hash)
VALUES ($1,$2,'workflow-trace-operation',$3,$4,$5,$6,$7,'prepared',$8,$9,$10,$11,1,$12)`,
		operationID, fixture.task.ID, operationHash[:], fixture.task.HostID, hostGeneration, hostControlVersion,
		fixture.task.Lease.Version, fixture.task.CurrentRevision, callID, toolName, approvalID, approvalHash[:]); err != nil {
		t.Fatalf("insert legacy operation ledger row: %v", err)
	}

	trace, err := fixture.route.store.GetTaskWorkflowTrace(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatalf("load workflow trace: %v", err)
	}
	if err := evaluation.ValidateWorkflowTrace(trace); err != nil {
		t.Fatalf("validate workflow trace: %v", err)
	}
	if trace.DataClass != evaluation.WorkflowDataLiveObserved || trace.Coverage == evaluation.WorkflowCoverageDeclaredComplete {
		t.Fatalf("workflow trace overstated coverage: data_class=%s coverage=%s", trace.DataClass, trace.Coverage)
	}

	serialized, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("marshal workflow trace: %v", err)
	}
	if strings.Contains(string(serialized), objectiveCanary) || strings.Contains(string(serialized), firstContent) {
		t.Fatal("workflow trace exposed raw task objective or assistant content")
	}

	kinds := make(map[evaluation.WorkflowNodeKind]bool)
	for _, node := range trace.Nodes {
		kinds[node.Kind] = true
	}
	for _, kind := range []evaluation.WorkflowNodeKind{
		evaluation.WorkflowNodeTask,
		evaluation.WorkflowNodeRoute,
		evaluation.WorkflowNodeAgent,
		evaluation.WorkflowNodeOperation,
		evaluation.WorkflowNodeApproval,
		evaluation.WorkflowNodeDelivery,
		evaluation.WorkflowNodeHost,
	} {
		if !kinds[kind] {
			t.Errorf("workflow trace lacks %q node", kind)
		}
	}
	approvalRequestEdgeFound := false
	for _, edge := range trace.Edges {
		if edge.Kind == evaluation.WorkflowEdgeApprovalRequested {
			approvalRequestEdgeFound = true
			break
		}
	}
	if !approvalRequestEdgeFound {
		t.Fatal("workflow trace omitted the source-bound operation-to-approval request edge")
	}

	var firstSource evaluation.WorkflowSource
	for _, source := range trace.Sources {
		if source.Locator == "db://conversation_messages/"+fixture.task.ID+"/"+messageID {
			firstSource = source
		}
		stableLedgerLocator := strings.HasPrefix(source.Locator, "db://") ||
			source.Kind == evaluation.WorkflowSourceRuntimeSnapshot && strings.HasPrefix(source.Locator, "guardian://task-processes/")
		if !stableLedgerLocator {
			t.Errorf("source locator is not a stable ledger locator: %q", source.Locator)
		}
		if len(source.SHA256) != sha256.Size*2 {
			t.Errorf("source %q has malformed digest %q", source.ID, source.SHA256)
		} else if _, err := hex.DecodeString(source.SHA256); err != nil {
			t.Errorf("source %q digest is not hexadecimal: %v", source.ID, err)
		}
	}
	if firstSource.ID == "" {
		t.Fatalf("workflow trace has no source for assistant message %s", messageID)
	}

	pcAdapterUnavailableGapFound := false
	for _, gap := range trace.Gaps {
		if gap.Stage == evaluation.WorkflowStageProvenance && gap.Scope == "pc_observation_adapter" &&
			gap.State == evaluation.WorkflowGapUnavailable {
			pcAdapterUnavailableGapFound = true
			break
		}
	}
	if pcAdapterUnavailableGapFound {
		t.Fatal("workflow trace still marks the PC observation adapter unavailable")
	}

	wrongOwnerID := mustID(t, domain.NewUserID)
	if _, err := fixture.route.store.GetTaskWorkflowTrace(ctx, fixture.task.ID, wrongOwnerID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("wrong-owner trace lookup error=%v, want not-found", err)
	}

	const secondContent = "workflow-trace-assistant-private-canary-two"
	if len(secondContent) != len(firstContent) {
		t.Fatal("digest mutation fixture must preserve content length")
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE conversation_messages SET content=$2 WHERE id=$1`, messageID, secondContent); err != nil {
		t.Fatalf("mutate assistant output at stable locator: %v", err)
	}
	secondTrace, err := fixture.route.store.GetTaskWorkflowTrace(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatalf("reload workflow trace after source mutation: %v", err)
	}
	var secondSource evaluation.WorkflowSource
	for _, source := range secondTrace.Sources {
		if source.Locator == firstSource.Locator {
			secondSource = source
			break
		}
	}
	if secondSource.ID == "" || secondSource.SHA256 == firstSource.SHA256 {
		t.Fatal("assistant source digest did not change when content changed at the same ledger locator")
	}
	secondJSON, err := json.Marshal(secondTrace)
	if err != nil {
		t.Fatalf("marshal updated workflow trace: %v", err)
	}
	if strings.Contains(string(secondJSON), objectiveCanary) || strings.Contains(string(secondJSON), secondContent) {
		t.Fatal("workflow trace exposed raw task objective or mutated assistant content")
	}

	completedMessageID := mustID(t, domain.NewMessageID)
	const completedContent = "workflow-trace-completed-result"
	resultDigest := sha256.Sum256([]byte(completedContent))
	var resultEventSequence, messageSequence int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_events WHERE task_id=$1`, fixture.task.ID).Scan(&resultEventSequence); err != nil {
		t.Fatalf("allocate result event sequence: %v", err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM conversation_messages WHERE conversation_id=$1`, fixture.task.ConversationID).Scan(&messageSequence); err != nil {
		t.Fatalf("allocate assistant message sequence: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO task_events
(task_id,sequence,event_type,payload,task_revision,result_content_sha256)
VALUES ($1,$2,'task.result','{"status":"completed"}'::jsonb,$3,$4)`, fixture.task.ID, resultEventSequence, fixture.task.CurrentRevision, resultDigest[:]); err != nil {
		t.Fatalf("insert completed Task result event: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET status='completed',current_revision=$2,desired_revision=$2,result=$3 WHERE id=$1`,
		fixture.task.ID, fixture.task.CurrentRevision, completedContent); err != nil {
		t.Fatalf("complete workflow trace Task: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,actor_id,role,content,task_id,sequence,delivery_status,output_status,output_task_revision,output_result_event_sequence)
VALUES ($1,$2,$3,$3,'assistant',$4,$5,$6,'applied','completed',$7,$8)`, completedMessageID,
		fixture.task.ConversationID, fixture.task.OwnerID, completedContent, fixture.task.ID, messageSequence,
		fixture.task.CurrentRevision, resultEventSequence); err != nil {
		t.Fatalf("insert completed assistant result: %v", err)
	}
	completedTrace, err := fixture.route.store.GetTaskWorkflowTrace(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatalf("load workflow trace with unobserved completed reply: %v", err)
	}
	pcObservationGapFound := false
	for _, gap := range completedTrace.Gaps {
		if gap.Stage == evaluation.WorkflowStageDelivery && gap.State == evaluation.WorkflowGapUnknown &&
			strings.Contains(gap.Scope, "PC visible-area observation is absent for published reply: "+completedMessageID) {
			pcObservationGapFound = true
			break
		}
	}
	if !pcObservationGapFound {
		t.Fatal("workflow trace omitted the PC visible-area gap for an unobserved completed reply")
	}
}
