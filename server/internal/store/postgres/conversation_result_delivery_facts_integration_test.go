package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type conversationResultTestApproval struct {
	id       string
	version  int64
	revision int64
	callID   string
	tool     string
	hash     []byte
}

func insertConversationResultTestApproval(t *testing.T, f resultAggregationFixture, taskID string,
	revision int64, status, callID, tool string) conversationResultTestApproval {
	t.Helper()
	id := mustID(t, domain.NewPendingRequestID)
	hash := sha256.Sum256([]byte(taskID + ":" + callID + ":" + tool))
	decision := ""
	switch status {
	case "approved":
		decision = "approve"
	case "rejected":
		decision = "reject"
	case "expired":
		decision = "reject"
	default:
		t.Fatalf("unsupported approval test status %q", status)
	}
	if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,runtime_generation,
runtime_instance_id,control_version,task_revision,status,decision,responded_at,created_at,expires_at)
VALUES ($1,$2,1,'approval','A side effect','approved operation','test request','test impact',$3,$4,$5,0,'result-test',0,$6,$7,$8,
			CASE WHEN $7='pending' THEN NULL ELSE clock_timestamp() END,
			CASE WHEN $9::boolean THEN clock_timestamp()-interval '3 minutes' ELSE clock_timestamp() END,
			CASE WHEN $9::boolean THEN clock_timestamp()-interval '2 minutes' ELSE NULL END)`,
		id, taskID, callID, tool, hash[:], revision, status, decision, status == "expired"); err != nil {
		t.Fatalf("insert Task approval test row: %v", err)
	}
	return conversationResultTestApproval{id: id, version: 1, revision: revision, callID: callID, tool: tool, hash: hash[:]}
}

func insertConversationResultTestOperation(t *testing.T, f resultAggregationFixture, taskID, status string,
	revision int64, callID, tool string, approval *conversationResultTestApproval) {
	t.Helper()
	id := mustID(t, domain.NewOperationID)
	var approvalID any
	var approvalVersion any
	var approvalHash any
	if approval != nil {
		approvalID, approvalVersion, approvalHash = approval.id, approval.version, approval.hash
	}
	var taskRevision any = revision
	if revision < 0 {
		taskRevision = nil
	}
	if tool == "" {
		tool = "test.unknown"
	}
	operationHash := sha256.Sum256([]byte("operation:" + id))
	if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
host_id,workspace_id,binding_version,task_revision,call_id,operation_key,tool_name,request_hash,status,
approval_id,approval_version,approval_request_hash)
VALUES ($1,$2,$3,'k1234567',NULL,1,0,1,1,'','',0,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11,$12)`,
		id, taskID, f.ownerID, taskRevision, callID, "result-facts-"+id, tool, operationHash[:], status,
		approvalID, approvalVersion, approvalHash); err != nil {
		t.Fatalf("insert Task operation test row: %v", err)
	}
}

func insertConversationResultTestHostOperation(t *testing.T, f resultAggregationFixture, taskID string,
	status string, revision int64, approval conversationResultTestApproval) {
	t.Helper()
	ctx := context.Background()
	hostValue := createRunningControlHost(t, ctx, f.store, f.ownerID, "ai", 0)
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversations SET host_id=$2 WHERE id=$1`, f.conversation.ID, hostValue.ID); err != nil {
		t.Fatalf("bind Host to result conversation: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1`, taskID, hostValue.ID); err != nil {
		t.Fatalf("bind Host to result Task: %v", err)
	}
	id := mustID(t, domain.NewOperationID)
	operationHash := sha256.Sum256([]byte("host-operation:" + id))
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_operations
(id,task_id,operation_key,request_hash,host_id,runtime_generation,control_version,lease_version,status,
task_revision,call_id,tool_name,approval_id,approval_version,approval_request_hash)
SELECT $2,task.id,$3,$4,task.host_id,0,0,GREATEST(task.lease_version,1),$5,$6,$7,$8,$9,$10,$11
FROM tasks task WHERE task.id=$1`, taskID, id, "result-host-facts-"+id, operationHash[:], status, revision,
		approval.callID, approval.tool, approval.id, approval.version, approval.hash); err != nil {
		t.Fatalf("insert Host operation test row: %v", err)
	}
}

func insertConversationResultTestWorkspacePublish(t *testing.T, f resultAggregationFixture, taskID string,
	status string, revision int64, approval conversationResultTestApproval) {
	t.Helper()
	ctx := context.Background()
	workspaceValue := createTestWorkspace(t, ctx, f.store, f.ownerID, "result-facts-"+taskID)
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2,workspace_owner_id=$4,
input_revision=$3,workspace_checkpoint_revision=$3 WHERE id=$1`, taskID, workspaceValue.ID, workspaceValue.Revision, f.ownerID); err != nil {
		t.Fatalf("bind Workspace to Task result fixture: %v", err)
	}
	requestHash := sha256.Sum256([]byte("workspace-publish:" + taskID))
	manifestHash := sha256.Sum256([]byte("workspace-publish-manifest:" + taskID))
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_workspace_publish_receipts
(task_id,owner_id,workspace_id,operation_key,request_hash,base_revision,state,manifest_sha256,published_revision,
task_revision,call_id,tool_name,approval_id,approval_version,approval_request_hash)
VALUES ($1,$2,$3,$4,$5,$6,$7,CASE WHEN $7='succeeded' THEN $8 ELSE NULL END,
CASE WHEN $7='succeeded' THEN $6::bigint+1 ELSE NULL END,$9,$10,$11,$12,$13,$14)`,
		taskID, f.ownerID, workspaceValue.ID, "result-workspace-facts-"+taskID, requestHash[:], workspaceValue.Revision,
		status, hex.EncodeToString(manifestHash[:]), revision, approval.callID, approval.tool, approval.id,
		approval.version, approval.hash); err != nil {
		t.Fatalf("insert Workspace publish receipt test row: %v", err)
	}
}

func conversationResultTestFacts(t *testing.T, f resultAggregationFixture, value resultAggregationTask) conversationResultDeliveryFacts {
	t.Helper()
	ctx := context.Background()
	tx, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	facts, err := conversationResultDeliveryFactsInTx(ctx, tx, f.ownerID, app.ConversationTaskResultSource{
		TaskID: value.ID, TaskRevision: value.TaskRevision, EventSequence: value.EventSequence, Content: value.Content,
	})
	if err != nil {
		t.Fatalf("load Task result delivery facts: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Task result delivery fact read: %v", err)
	}
	return facts
}

func conversationResultTestReceipt(value resultAggregationTask, facts conversationResultDeliveryFacts) string {
	return conversationTaskResultReceiptWithDeliveryFacts("result summary", []app.ConversationTaskResultSource{{
		TaskID: value.ID, TaskRevision: value.TaskRevision,
	}}, []app.TaskValidationState{app.TaskValidationPending}, []conversationResultDeliveryFacts{facts})
}

func advanceConversationResultTestTaskRevision(t *testing.T, f resultAggregationFixture, value resultAggregationTask) {
	t.Helper()
	ctx := context.Background()
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, testSessionTokenID(f.ownerID))
	if err != nil {
		t.Fatal(err)
	}
	messageID := mustID(t, domain.NewMessageID)
	digest := sha256.Sum256([]byte("advance result fixture revision:" + value.ID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: messageID, OwnerID: f.ownerID, ActorID: value.ActorID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "advance fixture revision"},
		ServiceInstanceID: instanceID, ClientID: "result-facts-revision-advance-" + value.ID, RequestHash: digest[:],
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID,
			Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatalf("accept revision-advance message: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state)
VALUES ($1,$2,$3,$3,$4,$5,0,'applied')`, message.ID, value.ID, f.ownerID, f.conversation.ID, message.Sequence); err != nil {
		t.Fatalf("insert revision-advance intent: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET task_id=$1,delivery_status='applied' WHERE id=$2`, value.ID, message.ID); err != nil {
		t.Fatalf("bind revision-advance message to Task: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET state='routed' WHERE message_id=$1`, message.ID); err != nil {
		t.Fatalf("finish revision-advance message route: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=1,desired_revision=1 WHERE id=$1`, value.ID); err != nil {
		t.Fatalf("advance Task revision: %v", err)
	}
}

func TestConversationResultDeliveryFactsSeparateApprovalFromExactOperation(t *testing.T) {
	f := newResultAggregationFixture(t)

	approvedWithoutReceipt := f.createTask(t, f.ownerID, "approved without receipt")
	if !f.complete(t, &approvedWithoutReceipt, "completed task") {
		t.Fatal("approved no-receipt Task result was not queued")
	}
	insertConversationResultTestApproval(t, f, approvedWithoutReceipt.ID, approvedWithoutReceipt.TaskRevision,
		"approved", "call-approved-without-receipt", "fs.write")
	noReceipt := conversationResultTestReceipt(approvedWithoutReceipt, conversationResultTestFacts(t, f, approvedWithoutReceipt))
	if !strings.Contains(noReceipt, "审批#1 fs.write：批准；操作回执：无执行回执；批准不代表执行成功") ||
		strings.Contains(noReceipt, "成功回执（精确绑定）") {
		t.Fatalf("approval without an operation receipt was presented as execution success: %q", noReceipt)
	}

	for _, status := range []string{"rejected", "expired"} {
		t.Run(status+" cannot prove success", func(t *testing.T) {
			value := f.createTask(t, f.ownerID, status+" approval")
			if !f.complete(t, &value, "completed task") {
				t.Fatal("rejected or expired Task result was not queued")
			}
			approval := insertConversationResultTestApproval(t, f, value.ID, value.TaskRevision,
				status, "call-"+status, "fs.write")
			insertConversationResultTestOperation(t, f, value.ID, "succeeded", value.TaskRevision,
				approval.callID, approval.tool, &approval)
			receipt := conversationResultTestReceipt(value, conversationResultTestFacts(t, f, value))
			if !strings.Contains(receipt, "审批#1 fs.write："+map[string]string{"rejected": "拒绝", "expired": "过期"}[status]) ||
				strings.Contains(receipt, "成功回执（精确绑定）") ||
				!strings.Contains(receipt, "未知（审批未批准却存在成功回执）") {
				t.Fatalf("%s approval was treated as execution success: %q", status, receipt)
			}
		})
	}

	for _, operationStatus := range []string{"prepared", "dispatched", "unknown"} {
		t.Run(operationStatus+" stays unresolved", func(t *testing.T) {
			value := f.createTask(t, f.ownerID, operationStatus+" operation")
			if !f.complete(t, &value, "completed task") {
				t.Fatal("unresolved-operation Task result was not queued")
			}
			approval := insertConversationResultTestApproval(t, f, value.ID, value.TaskRevision,
				"approved", "call-"+operationStatus, "fs.write")
			insertConversationResultTestOperation(t, f, value.ID, operationStatus, value.TaskRevision,
				approval.callID, approval.tool, &approval)
			receipt := conversationResultTestReceipt(value, conversationResultTestFacts(t, f, value))
			want := map[string]string{
				"prepared":   "待处理（未执行完成）",
				"dispatched": "已派发，结果待确认",
				"unknown":    "未知，待对账",
			}[operationStatus]
			if !strings.Contains(receipt, want) || strings.Contains(receipt, "成功回执（精确绑定）") {
				t.Fatalf("%s operation was not kept unresolved: %q", operationStatus, receipt)
			}
		})
	}

	exact := f.createTask(t, f.ownerID, "exactly bound operation")
	if !f.complete(t, &exact, "completed task") {
		t.Fatal("exact-operation Task result was not queued")
	}
	exactApproval := insertConversationResultTestApproval(t, f, exact.ID, exact.TaskRevision,
		"approved", "call-exact-success", "fs.write")
	insertConversationResultTestOperation(t, f, exact.ID, "succeeded", exact.TaskRevision,
		exactApproval.callID, exactApproval.tool, &exactApproval)
	exactReceipt := conversationResultTestReceipt(exact, conversationResultTestFacts(t, f, exact))
	if !strings.Contains(exactReceipt, "审批#1 fs.write：批准；操作回执：成功回执（精确绑定）") {
		t.Fatalf("exactly bound successful operation was not identified: %q", exactReceipt)
	}
}

func TestConversationResultDeliveryFactsKeepCrossLedgerConflictUnknown(t *testing.T) {
	f := newResultAggregationFixture(t)
	value := f.createTask(t, f.ownerID, "conflicting operation receipts across ledgers")
	if !f.complete(t, &value, "completed task") {
		t.Fatal("conflicting-receipt Task result was not queued")
	}
	approval := insertConversationResultTestApproval(t, f, value.ID, value.TaskRevision,
		"approved", "call-cross-ledger-conflict", "fs.write")
	insertConversationResultTestOperation(t, f, value.ID, "succeeded", value.TaskRevision,
		approval.callID, approval.tool, &approval)
	insertConversationResultTestHostOperation(t, f, value.ID, "succeeded", value.TaskRevision, approval)

	facts := conversationResultTestFacts(t, f, value)
	receipt := conversationResultTestReceipt(value, facts)
	if len(facts.approvals) != 1 ||
		facts.approvals[0].operation != "未知（回执绑定不精确或存在冲突）" ||
		!strings.Contains(receipt, "未知（回执绑定不精确或存在冲突）") ||
		strings.Contains(receipt, "成功回执（精确绑定）") {
		t.Fatalf("conflicting cross-ledger receipts were not kept unknown: facts=%+v receipt=%q", facts, receipt)
	}
}

func TestConversationResultDeliveryFactsCaptureAuxiliaryLedgers(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	installationID := createTestClientInstallation(t, f.store.pool, f.ownerID, nil)
	requestHash := sha256.Sum256([]byte("auxiliary operation request"))
	contextHash := sha256.Sum256([]byte("sandbox context"))

	completeTask := func(objective string, revision int64) resultAggregationTask {
		t.Helper()
		value := f.createTask(t, f.ownerID, objective)
		if revision > 0 {
			advanceConversationResultTestTaskRevision(t, f, value)
		}
		if !f.complete(t, &value, "completed task") {
			t.Fatalf("%s result was not queued", objective)
		}
		return value
	}
	assertCounts := func(name string, value resultAggregationTask, pending, unknown, nonSuccess, unversioned int) {
		t.Helper()
		facts := conversationResultTestFacts(t, f, value)
		if facts.pendingOperations != pending || facts.unknownOperations != unknown ||
			facts.nonSuccessOperations != nonSuccess || facts.unversionedOperations != unversioned {
			t.Fatalf("%s delivery facts = pending %d, unknown %d, non-success %d, unversioned %d; want %d/%d/%d/%d",
				name, facts.pendingOperations, facts.unknownOperations, facts.nonSuccessOperations, facts.unversionedOperations,
				pending, unknown, nonSuccess, unversioned)
		}
		receipt := conversationResultTestReceipt(value, facts)
		if pending+unknown+nonSuccess > 0 && !strings.Contains(receipt, "其他操作账本：") {
			t.Fatalf("%s result receipt omitted operation ledger facts: %q", name, receipt)
		}
	}

	hub := completeTask("hub operation outcome", 0)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_hub_operations
(task_id,owner_id,operation_key,tool_name,target_installation_id,action,task_authorization_version,
target_grant_version,lease_version,request_hash,status)
VALUES ($1,$2,'hub-unknown','morde_hub_command',$3,'command',1,1,1,$4,'unknown')`,
		hub.ID, f.ownerID, installationID, requestHash[:]); err != nil {
		t.Fatalf("insert unknown Hub operation: %v", err)
	}
	assertCounts("Hub unknown", hub, 0, 1, 0, 1)

	sandbox := completeTask("sandbox operation outcome", 0)
	workspaceValue := createTestWorkspace(t, ctx, f.store, f.ownerID, "result-facts-sandbox")
	sandboxID := mustID(t, domain.NewOperationID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_sandbox_call_receipts
(id,task_id,owner_id,initiator_id,workspace_id,workspace_owner_id,input_revision,workspace_checkpoint,
context_digest,authorization_version,lease_owner,lease_version,call_id,request_hash,sandbox_id,sandbox_generation,status,unknown_code)
VALUES ($1,$2,$3,$3,$4,$3,$5,$5,$6,1,'result-facts-test',1,'sandbox-call-123',$7,'sandbox-local',1,'unknown','runtime_lost')`,
		sandboxID, sandbox.ID, f.ownerID, workspaceValue.ID, workspaceValue.Revision, contextHash[:], requestHash[:]); err != nil {
		t.Fatalf("insert unknown sandbox call: %v", err)
	}
	assertCounts("sandbox unknown", sandbox, 0, 1, 0, 1)

	media := completeTask("media staging outcome", 0)
	hostValue := createRunningControlHost(t, ctx, f.store, f.ownerID, "ai", 0)
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversations SET host_id=$2 WHERE id=$1`, f.conversation.ID, hostValue.ID); err != nil {
		t.Fatalf("bind media fixture Host to conversation: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1`, media.ID, hostValue.ID); err != nil {
		t.Fatalf("bind media fixture Host to Task: %v", err)
	}
	mediaOperationID := mustID(t, domain.NewOperationID)
	mediaCallID := "media-call-" + media.ID
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_operations
(id,task_id,operation_key,request_hash,host_id,runtime_generation,control_version,lease_version,status,external_id)
VALUES ($1,$2,$3,$4,$5,0,0,1,'succeeded','synthetic-media-operation')`,
		mediaOperationID, media.ID, "media:"+mediaOperationID, requestHash[:], hostValue.ID); err != nil {
		t.Fatalf("insert completed media operation: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,input_tokens,output_tokens,settled_at)
VALUES ($1,'task',$2,$3,'p1234567','media-fixture',$4,100,'settled',12,2,clock_timestamp())`,
		f.ownerID, mediaCallID, requestHash[:], media.ID); err != nil {
		t.Fatalf("insert settled media model call: %v", err)
	}
	mediaDigest := sha256.Sum256([]byte("synthetic staged media"))
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_media_staged_objects
(operation_id,task_id,owner_id,initiator_id,runtime_call_id,media_type,size_bytes,sha256,expires_at,state)
VALUES ($1,$2,$3,$3,$4,'image/png',1,$5,clock_timestamp()+interval '1 hour','saving')`,
		mediaOperationID, media.ID, f.ownerID, mediaCallID, mediaDigest[:]); err != nil {
		t.Fatalf("insert media object being saved: %v", err)
	}
	assertCounts("media saving", media, 1, 0, 0, 1)

	fileFetch := completeTask("file fetch outcome", 1)
	fileFetchKey := "call:file-fetch-" + fileFetch.ID
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_hub_operations
(task_id,owner_id,operation_key,tool_name,target_installation_id,action,task_authorization_version,
target_grant_version,lease_version,request_hash,status,task_revision)
VALUES ($1,$2,$3,'morde_hub_file_fetch',$4,'file.fetch',1,1,1,$5,'prepared',1)`,
		fileFetch.ID, f.ownerID, fileFetchKey, installationID, requestHash[:]); err != nil {
		t.Fatalf("insert file-fetch Hub operation: %v", err)
	}
	fetchID := mustID(t, domain.NewOperationID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_file_fetches
(fetch_id,task_id,owner_id,operation_key,source_installation_id,target_grant_version,task_authorization_version,
task_revision,relative_path,request_hash,status,waiting_reason)
VALUES ($1,$2,$3,$4,$5,1,1,1,'reports/result.txt',$6,'waiting_device','device_offline')`,
		fetchID, fileFetch.ID, f.ownerID, fileFetchKey, installationID, requestHash[:]); err != nil {
		t.Fatalf("insert waiting file fetch: %v", err)
	}
	assertCounts("file fetch waiting for device", fileFetch, 1, 0, 0, 0)

	fileFetchWithoutRecord := completeTask("file fetch operation without tracker", 1)
	fileFetchWithoutRecordKey := "call:file-fetch-without-record-" + fileFetchWithoutRecord.ID
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_hub_operations
(task_id,owner_id,operation_key,tool_name,target_installation_id,action,task_authorization_version,
target_grant_version,lease_version,request_hash,status,task_revision)
VALUES ($1,$2,$3,'morde_hub_file_fetch',$4,'file.fetch',1,1,1,$5,'dispatched',1)`,
		fileFetchWithoutRecord.ID, f.ownerID, fileFetchWithoutRecordKey, installationID, requestHash[:]); err != nil {
		t.Fatalf("insert file-fetch Hub operation without tracker: %v", err)
	}
	assertCounts("file fetch without tracker", fileFetchWithoutRecord, 1, 0, 0, 0)

	batch := completeTask("batch item outcome", 0)
	batchID := mustID(t, domain.NewOperationID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_batch_items (id,task_id,item_key,input,status)
VALUES ($1,$2,'item-unknown','synthetic input','unknown')`, batchID, batch.ID); err != nil {
		t.Fatalf("insert unknown batch item: %v", err)
	}
	assertCounts("batch unknown", batch, 0, 1, 0, 1)

	step := completeTask("unknown work item outcome", 0)
	stepID := mustID(t, domain.NewStepID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,1,'progress','unknown','Unresolved work item','Execution outcome could not be confirmed')`, stepID, step.ID); err != nil {
		t.Fatalf("insert unknown Task step: %v", err)
	}
	assertCounts("unknown Task step", step, 0, 1, 0, 1)
}

func TestConversationResultDeliveryFactsAcceptExactHostAndWorkspaceReceipts(t *testing.T) {
	f := newResultAggregationFixture(t)
	for _, ledger := range []struct {
		name   string
		insert func(*testing.T, resultAggregationFixture, string, int64, conversationResultTestApproval)
	}{
		{name: "Host operation", insert: func(t *testing.T, f resultAggregationFixture, taskID string,
			revision int64, approval conversationResultTestApproval) {
			insertConversationResultTestHostOperation(t, f, taskID, "succeeded", revision, approval)
		}},
		{name: "Workspace publish", insert: func(t *testing.T, f resultAggregationFixture, taskID string,
			revision int64, approval conversationResultTestApproval) {
			insertConversationResultTestWorkspacePublish(t, f, taskID, "succeeded", revision, approval)
		}},
	} {
		t.Run(ledger.name, func(t *testing.T) {
			value := f.createTask(t, f.ownerID, "exact "+ledger.name+" operation")
			if !f.complete(t, &value, "completed task") {
				t.Fatal("exact-receipt Task result was not queued")
			}
			approval := insertConversationResultTestApproval(t, f, value.ID, value.TaskRevision,
				"approved", "call-exact-"+strings.ReplaceAll(strings.ToLower(ledger.name), " ", "-"), "fs.write")
			ledger.insert(t, f, value.ID, value.TaskRevision, approval)
			receipt := conversationResultTestReceipt(value, conversationResultTestFacts(t, f, value))
			if !strings.Contains(receipt, "审批#1 fs.write：批准；操作回执：成功回执（精确绑定）") {
				t.Fatalf("exact %s success was not identified: %q", ledger.name, receipt)
			}
		})
	}
}

func TestConversationResultDeliveryFactsRejectWrongRevisionAndBinding(t *testing.T) {
	f := newResultAggregationFixture(t)

	wrongBinding := f.createTask(t, f.ownerID, "wrong operation binding")
	if !f.complete(t, &wrongBinding, "completed task") {
		t.Fatal("wrong-binding Task result was not queued")
	}
	approval := insertConversationResultTestApproval(t, f, wrongBinding.ID, wrongBinding.TaskRevision,
		"approved", "call-current", "fs.write")
	insertConversationResultTestOperation(t, f, wrongBinding.ID, "succeeded", wrongBinding.TaskRevision+1,
		"call-other", "fs.delete", &approval)
	wrongReceipt := conversationResultTestReceipt(wrongBinding, conversationResultTestFacts(t, f, wrongBinding))
	if !strings.Contains(wrongReceipt, "未知（回执绑定不精确或存在冲突）") ||
		strings.Contains(wrongReceipt, "成功回执（精确绑定）") {
		t.Fatalf("wrongly bound operation was treated as current success: %q", wrongReceipt)
	}

	oldRevision := f.createTask(t, f.ownerID, "old revision approval")
	advanceConversationResultTestTaskRevision(t, f, oldRevision)
	if !f.complete(t, &oldRevision, "revision one result") {
		t.Fatal("old-revision Task result was not queued")
	}
	oldApproval := insertConversationResultTestApproval(t, f, oldRevision.ID, 0,
		"approved", "call-old-revision", "fs.write")
	insertConversationResultTestOperation(t, f, oldRevision.ID, "succeeded", 0,
		oldApproval.callID, oldApproval.tool, &oldApproval)
	oldFacts := conversationResultTestFacts(t, f, oldRevision)
	oldReceipt := conversationResultTestReceipt(oldRevision, oldFacts)
	if len(oldFacts.approvals) != 0 || strings.Contains(oldReceipt, "成功回执（精确绑定）") ||
		!strings.Contains(oldReceipt, "其他操作账本：0 条待处理、1 条未知待对账") {
		t.Fatalf("old-revision approval was used as current evidence: facts=%+v receipt=%q", oldFacts, oldReceipt)
	}

	unknown := f.createTask(t, f.ownerID, "unrelated generic unknown operation")
	if !f.complete(t, &unknown, "completed task") {
		t.Fatal("generic-unknown Task result was not queued")
	}
	insertConversationResultTestOperation(t, f, unknown.ID, "unknown", -1, "", "", nil)
	unknownReceipt := conversationResultTestReceipt(unknown, conversationResultTestFacts(t, f, unknown))
	if !strings.Contains(unknownReceipt, "其他操作账本：0 条待处理、1 条未知待对账") ||
		!strings.Contains(unknownReceipt, "修订归属不明") {
		t.Fatalf("unbound generic unknown operation was not reported: %q", unknownReceipt)
	}
}

func TestConversationResultDeliveryFactsArtifactPromptDoesNotExposePath(t *testing.T) {
	f := newResultAggregationFixture(t)
	value := f.createTask(t, f.ownerID, "artifact access prompt")
	if !f.complete(t, &value, "completed task") {
		t.Fatal("artifact Task result was not queued")
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_steps(id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,1,'tool','succeeded','artifact step','test artifact')`, stepID, value.ID); err != nil {
		t.Fatalf("insert artifact step: %v", err)
	}
	path := "secret/private/never-include.txt"
	if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_artifacts(id,task_id,step_id,label,path,media_type)
VALUES ($1,$2,$3,'artifact',$4,'text/plain')`, mustID(t, domain.NewArtifactID), value.ID, stepID, path); err != nil {
		t.Fatalf("insert legacy Task artifact reference: %v", err)
	}
	receipt := conversationResultTestReceipt(value, conversationResultTestFacts(t, f, value))
	if !strings.Contains(receipt, "产物请在 Task 详情打开；授权下载接口会重新校验权限和内容可用性") || strings.Contains(receipt, path) {
		t.Fatalf("artifact receipt leaked a path or overstated current readability: %q", receipt)
	}
}

func TestConversationResultReceiptPublishesExactApprovalFacts(t *testing.T) {
	f := newResultAggregationFixture(t)
	value := f.createTask(t, f.ownerID, "publish exact approval receipt")
	if !f.complete(t, &value, "completed task") {
		t.Fatal("exact-operation Task result was not queued")
	}
	approval := insertConversationResultTestApproval(t, f, value.ID, value.TaskRevision,
		"approved", "call-published-exact-success", "fs.write")
	insertConversationResultTestOperation(t, f, value.ID, "succeeded", value.TaskRevision,
		approval.callID, approval.tool, &approval)
	f.runNextReview(t)

	var messageID, receipt string
	if err := f.store.pool.QueryRow(context.Background(), `SELECT batch.published_message_id,message.content
FROM conversation_result_batches batch JOIN conversation_messages message ON message.id=batch.published_message_id
WHERE batch.owner_id=$1 AND batch.state='published' AND message.role='assistant'`, f.ownerID).Scan(&messageID, &receipt); err != nil {
		t.Fatalf("read published result receipt %s: %v", messageID, err)
	}
	if !strings.Contains(receipt, "审批#1 fs.write：批准；操作回执：成功回执（精确绑定）") {
		t.Fatalf("published receipt omitted authoritative approval and operation facts: %q", receipt)
	}
}

func TestConversationResultReceiptBoundsDeliveryFactFooter(t *testing.T) {
	facts := conversationResultDeliveryFacts{approvals: make([]conversationResultApprovalFact, 100)}
	for index := range facts.approvals {
		facts.approvals[index] = conversationResultApprovalFact{
			toolName: strings.Repeat("long-tool-name-", 8), decision: "批准", operation: "未知，待对账",
		}
	}
	receipt := conversationTaskResultReceiptWithDeliveryFacts(strings.Repeat("摘要", app.MaxConversationRouteReplyRunes),
		[]app.ConversationTaskResultSource{{TaskID: "task1234", TaskRevision: 3}},
		[]app.TaskValidationState{app.TaskValidationPending}, []conversationResultDeliveryFacts{facts})
	if utf8.RuneCountInString(receipt) > app.MaxConversationRouteReplyRunes || !strings.Contains(receipt, "部分验证或操作事实未展开") {
		t.Fatalf("delivery facts exceeded the durable reply limit or hid truncation: runes=%d receipt=%q",
			utf8.RuneCountInString(receipt), receipt)
	}
}
