package postgres

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
)

type conversationResultDeliveryFacts struct {
	approvals             []conversationResultApprovalFact
	pendingOperations     int
	unknownOperations     int
	nonSuccessOperations  int
	unversionedOperations int
	hasArtifacts          bool
}

type conversationResultApprovalFact struct {
	id          string
	version     int64
	revision    int64
	revisionSet bool
	callID      string
	toolName    string
	requestHash []byte
	decision    string
	operation   string
}

type conversationResultOperationFact struct {
	rowID           string
	revision        int64
	revisionSet     bool
	callID          string
	toolName        string
	approvalID      string
	approvalVersion int64
	approvalHash    []byte
	status          string
	approvalLinked  bool
}

// conversationResultDeliveryFactsInTx builds the receipt from the Task's durable
// ledgers while the caller's result-publish transaction is still open. Approval
// state and side-effect state remain separate facts; only a unique, exact binding
// can support a successful operation receipt.
func conversationResultDeliveryFactsInTx(ctx context.Context, tx pgx.Tx, ownerID string,
	source app.ConversationTaskResultSource) (conversationResultDeliveryFacts, error) {
	facts := conversationResultDeliveryFacts{approvals: make([]conversationResultApprovalFact, 0)}
	var currentRevision, desiredRevision int64
	if err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision FROM tasks
WHERE id=$1 AND owner_id=$2 FOR SHARE`, source.TaskID, ownerID).Scan(&currentRevision, &desiredRevision); err != nil {
		return facts, fmt.Errorf("lock Task for result delivery facts: %w", err)
	}
	if currentRevision != source.TaskRevision || desiredRevision != source.TaskRevision {
		return facts, fmt.Errorf("task revision changed while loading result delivery facts")
	}

	rows, err := tx.Query(ctx, `SELECT id,version,COALESCE(task_revision,-1),call_id,tool_name,request_hash,status,decision,
COALESCE(expires_at <= clock_timestamp(),false)
FROM task_pending_requests
WHERE task_id=$1 AND (task_revision=$2 OR task_revision IS NULL)
ORDER BY version FOR SHARE`, source.TaskID, source.TaskRevision)
	if err != nil {
		return facts, fmt.Errorf("lock Task approval decisions for result receipt: %w", err)
	}
	for rows.Next() {
		var approval conversationResultApprovalFact
		var status, decision string
		var expired bool
		if err := rows.Scan(&approval.id, &approval.version, &approval.revision, &approval.callID,
			&approval.toolName, &approval.requestHash, &status, &decision, &expired); err != nil {
			rows.Close()
			return facts, fmt.Errorf("read Task approval decision for result receipt: %w", err)
		}
		approval.revisionSet = approval.revision >= 0
		approval.decision = effectiveConversationApprovalDecision(status, decision, expired)
		facts.approvals = append(facts.approvals, approval)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return facts, fmt.Errorf("iterate Task approval decisions for result receipt: %w", rowsErr)
	}

	operations := make([]conversationResultOperationFact, 0)
	rows, err = tx.Query(ctx, `SELECT id,COALESCE(task_revision,-1),COALESCE(call_id,''),COALESCE(tool_name,''),
COALESCE(approval_id,''),COALESCE(approval_version,-1),approval_request_hash,status
FROM task_operations WHERE task_id=$1 ORDER BY created_at,id FOR SHARE`, source.TaskID)
	if err != nil {
		return facts, fmt.Errorf("lock Task operation receipts for result receipt: %w", err)
	}
	for rows.Next() {
		var operation conversationResultOperationFact
		if err := rows.Scan(&operation.rowID, &operation.revision, &operation.callID, &operation.toolName,
			&operation.approvalID, &operation.approvalVersion, &operation.approvalHash, &operation.status); err != nil {
			rows.Close()
			return facts, fmt.Errorf("read Task operation receipt for result receipt: %w", err)
		}
		operation.revisionSet = operation.revision >= 0
		operations = append(operations, operation)
	}
	rowsErr = rows.Err()
	rows.Close()
	if rowsErr != nil {
		return facts, fmt.Errorf("iterate Task operation receipts for result receipt: %w", rowsErr)
	}

	rows, err = tx.Query(ctx, `SELECT id,COALESCE(task_revision,-1),COALESCE(call_id,''),tool_name,
COALESCE(approval_id,''),COALESCE(approval_version,-1),approval_request_hash,status
FROM task_mcp_connector_operations WHERE task_id=$1 AND owner_id=$2 ORDER BY created_at,id FOR SHARE`, source.TaskID, ownerID)
	if err != nil {
		return facts, fmt.Errorf("lock Task connector receipts for result receipt: %w", err)
	}
	for rows.Next() {
		var operation conversationResultOperationFact
		if err := rows.Scan(&operation.rowID, &operation.revision, &operation.callID, &operation.toolName,
			&operation.approvalID, &operation.approvalVersion, &operation.approvalHash, &operation.status); err != nil {
			rows.Close()
			return facts, fmt.Errorf("read Task connector receipt for result receipt: %w", err)
		}
		operation.revisionSet = operation.revision >= 0
		operations = append(operations, operation)
	}
	rowsErr = rows.Err()
	rows.Close()
	if rowsErr != nil {
		return facts, fmt.Errorf("iterate Task connector receipts for result receipt: %w", rowsErr)
	}

	rows, err = tx.Query(ctx, `SELECT operation_key,COALESCE(task_revision,-1),COALESCE(call_id,''),
COALESCE(tool_name,''),COALESCE(approval_id,''),COALESCE(approval_version,-1),approval_request_hash,state
FROM task_workspace_publish_receipts WHERE task_id=$1 AND owner_id=$2 ORDER BY created_at,operation_key FOR SHARE`, source.TaskID, ownerID)
	if err != nil {
		return facts, fmt.Errorf("lock Task workspace publish receipts for result receipt: %w", err)
	}
	for rows.Next() {
		var operation conversationResultOperationFact
		if err := rows.Scan(&operation.rowID, &operation.revision, &operation.callID, &operation.toolName,
			&operation.approvalID, &operation.approvalVersion, &operation.approvalHash, &operation.status); err != nil {
			rows.Close()
			return facts, fmt.Errorf("read Task workspace publish receipt for result receipt: %w", err)
		}
		operation.revisionSet = operation.revision >= 0
		operations = append(operations, operation)
	}
	rowsErr = rows.Err()
	rows.Close()
	if rowsErr != nil {
		return facts, fmt.Errorf("iterate Task workspace publish receipts for result receipt: %w", rowsErr)
	}

	for operationIndex := range operations {
		operation := &operations[operationIndex]
		for _, approval := range facts.approvals {
			if operation.approvalID != approval.id && (operation.callID == "" || operation.callID != approval.callID) {
				continue
			}
			operation.approvalLinked = true
		}
	}
	for approvalIndex := range facts.approvals {
		approval := &facts.approvals[approvalIndex]
		var candidates []conversationResultOperationFact
		for _, operation := range operations {
			if operation.approvalID == approval.id || operation.callID != "" && operation.callID == approval.callID {
				candidates = append(candidates, operation)
			}
		}
		if len(candidates) == 0 {
			if approval.decision == "批准" {
				approval.operation = "无执行回执；批准不代表执行成功"
			} else {
				approval.operation = "无执行回执"
			}
			continue
		}
		if len(candidates) != 1 || !conversationResultOperationExactlyBindsApproval(candidates[0], *approval, source.TaskRevision) {
			approval.operation = "未知（回执绑定不精确或存在冲突）"
			continue
		}
		approval.operation = conversationResultOperationStatus(candidates[0].status, approval.decision)
	}

	for _, operation := range operations {
		if operation.approvalLinked {
			continue
		}
		status := operation.status
		if operation.approvalID != "" || operation.approvalVersion >= 0 || len(operation.approvalHash) > 0 {
			if status != "prepared" && status != "dispatched" && status != "pending" {
				status = "unknown"
			}
		}
		conversationCountGenericOperation(&facts, status, operation.revisionSet, operation.revision == source.TaskRevision)
	}
	if err := loadConversationResultHubOperationFactsInTx(ctx, tx, source, ownerID, &facts); err != nil {
		return facts, err
	}
	if err := loadConversationResultSandboxCallFactsInTx(ctx, tx, source, ownerID, &facts); err != nil {
		return facts, err
	}
	if err := loadConversationResultFileFetchFactsInTx(ctx, tx, source, ownerID, &facts); err != nil {
		return facts, err
	}
	if err := loadConversationResultMediaFactsInTx(ctx, tx, source, ownerID, &facts); err != nil {
		return facts, err
	}
	if err := loadConversationResultBatchFactsInTx(ctx, tx, source, &facts); err != nil {
		return facts, err
	}
	if err := loadConversationResultStepFactsInTx(ctx, tx, source, &facts); err != nil {
		return facts, err
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM task_artifacts artifact JOIN task_steps step
ON step.task_id=artifact.task_id AND step.id=artifact.step_id
WHERE artifact.task_id=$1 FOR SHARE OF artifact,step)`, source.TaskID).Scan(&facts.hasArtifacts); err != nil {
		return facts, fmt.Errorf("lock Task artifact references for result receipt: %w", err)
	}
	return facts, nil
}

func effectiveConversationApprovalDecision(status, decision string, expired bool) string {
	if status == "pending" && expired {
		return "过期"
	}
	switch {
	case status == "pending" && decision == "":
		return "待决定"
	case status == "approved" && decision == "approve":
		return "批准"
	case status == "rejected" && decision == "reject":
		return "拒绝"
	case status == "expired" && decision == "reject":
		return "过期"
	case status == "cancelled" && decision == "":
		return "取消"
	default:
		return "未知"
	}
}

func conversationResultOperationExactlyBindsApproval(operation conversationResultOperationFact,
	approval conversationResultApprovalFact, currentRevision int64) bool {
	return approval.revisionSet && approval.revision == currentRevision && operation.revisionSet &&
		operation.revision == currentRevision && operation.callID == approval.callID &&
		operation.toolName == approval.toolName && operation.approvalID == approval.id &&
		operation.approvalVersion == approval.version && len(approval.requestHash) == 32 &&
		bytes.Equal(operation.approvalHash, approval.requestHash)
}

func conversationResultOperationStatus(status, decision string) string {
	switch status {
	case "prepared", "pending":
		return "待处理（未执行完成）"
	case "dispatched":
		return "已派发，结果待确认"
	case "unknown":
		return "未知，待对账"
	case "succeeded":
		if decision == "批准" {
			return "成功回执（精确绑定）"
		}
		return "未知（审批未批准却存在成功回执）"
	case "failed":
		return "失败回执"
	case "cancelled":
		return "已取消"
	case "conflict":
		return "冲突"
	case "quota_rejected":
		return "配额拒绝"
	case "superseded":
		return "已取代"
	default:
		return "未知，待对账"
	}
}

func conversationCountGenericOperation(facts *conversationResultDeliveryFacts, status string, revisionSet, currentRevision bool) {
	switch status {
	case "unknown":
		facts.unknownOperations++
	case "prepared", "dispatched", "pending", "queued", "waiting_user", "waiting_device", "transferring", "verifying", "cancelling", "preview", "saving", "running":
		facts.pendingOperations++
	case "failed", "cancelled", "conflict", "quota_rejected", "superseded":
		facts.nonSuccessOperations++
	default:
		return
	}
	if !revisionSet || !currentRevision {
		facts.unversionedOperations++
	}
}

func loadConversationResultHubOperationFactsInTx(ctx context.Context, tx pgx.Tx, source app.ConversationTaskResultSource,
	ownerID string, facts *conversationResultDeliveryFacts) error {
	rows, err := tx.Query(ctx, `SELECT hub_op.operation_key,hub_op.action,hub_op.task_revision,hub_op.status
FROM task_hub_operations hub_op
WHERE hub_op.task_id=$1 AND hub_op.owner_id=$2
AND (hub_op.action<>'file.fetch' OR NOT EXISTS (
  SELECT 1 FROM task_file_fetches file_fetch
  WHERE file_fetch.task_id=hub_op.task_id AND file_fetch.owner_id=hub_op.owner_id AND file_fetch.operation_key=hub_op.operation_key
))
ORDER BY hub_op.operation_key FOR SHARE OF hub_op`, source.TaskID, ownerID)
	if err != nil {
		return fmt.Errorf("lock Task hub operation facts for result receipt: %w", err)
	}
	for rows.Next() {
		var key, action, status string
		var revision int64
		if err := rows.Scan(&key, &action, &revision, &status); err != nil {
			rows.Close()
			return fmt.Errorf("read Task hub operation fact for result receipt: %w", err)
		}
		revisionBound := action == "file.fetch"
		conversationCountGenericOperation(facts, status, revisionBound, revisionBound && revision == source.TaskRevision)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Task hub operation facts for result receipt: %w", err)
	}
	rows.Close()
	return nil
}

func loadConversationResultSandboxCallFactsInTx(ctx context.Context, tx pgx.Tx, source app.ConversationTaskResultSource,
	ownerID string, facts *conversationResultDeliveryFacts) error {
	rows, err := tx.Query(ctx, `SELECT call_id,status FROM task_sandbox_call_receipts
WHERE task_id=$1 AND owner_id=$2 ORDER BY created_at,call_id FOR SHARE`, source.TaskID, ownerID)
	if err != nil {
		return fmt.Errorf("lock Task sandbox call facts for result receipt: %w", err)
	}
	for rows.Next() {
		var callID, status string
		if err := rows.Scan(&callID, &status); err != nil {
			rows.Close()
			return fmt.Errorf("read Task sandbox call fact for result receipt: %w", err)
		}
		conversationCountGenericOperation(facts, status, false, false)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Task sandbox call facts for result receipt: %w", err)
	}
	rows.Close()
	return nil
}

func loadConversationResultFileFetchFactsInTx(ctx context.Context, tx pgx.Tx, source app.ConversationTaskResultSource,
	ownerID string, facts *conversationResultDeliveryFacts) error {
	rows, err := tx.Query(ctx, `SELECT fetch_id,task_revision,status FROM task_file_fetches
WHERE task_id=$1 AND owner_id=$2 ORDER BY created_at,fetch_id FOR SHARE`, source.TaskID, ownerID)
	if err != nil {
		return fmt.Errorf("lock Task file fetch facts for result receipt: %w", err)
	}
	for rows.Next() {
		var fetchID, status string
		var revision int64
		if err := rows.Scan(&fetchID, &revision, &status); err != nil {
			rows.Close()
			return fmt.Errorf("read Task file fetch fact for result receipt: %w", err)
		}
		conversationCountGenericOperation(facts, status, true, revision == source.TaskRevision)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Task file fetch facts for result receipt: %w", err)
	}
	rows.Close()
	return nil
}

func loadConversationResultMediaFactsInTx(ctx context.Context, tx pgx.Tx, source app.ConversationTaskResultSource,
	ownerID string, facts *conversationResultDeliveryFacts) error {
	rows, err := tx.Query(ctx, `SELECT staged.operation_id,staged.state,COALESCE(operation.task_revision,-1)
FROM task_media_staged_objects staged JOIN task_operations operation
ON operation.task_id=staged.task_id AND operation.id=staged.operation_id
WHERE staged.task_id=$1 AND staged.owner_id=$2 ORDER BY staged.created_at,staged.operation_id FOR SHARE OF staged,operation`,
		source.TaskID, ownerID)
	if err != nil {
		return fmt.Errorf("lock Task media staging facts for result receipt: %w", err)
	}
	for rows.Next() {
		var operationID, status string
		var revision int64
		if err := rows.Scan(&operationID, &status, &revision); err != nil {
			rows.Close()
			return fmt.Errorf("read Task media staging fact for result receipt: %w", err)
		}
		conversationCountGenericOperation(facts, status, revision >= 0, revision == source.TaskRevision)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Task media staging facts for result receipt: %w", err)
	}
	rows.Close()
	return nil
}

func loadConversationResultBatchFactsInTx(ctx context.Context, tx pgx.Tx, source app.ConversationTaskResultSource,
	facts *conversationResultDeliveryFacts) error {
	rows, err := tx.Query(ctx, `SELECT item_key,status FROM task_batch_items
WHERE task_id=$1 ORDER BY created_at,item_key FOR SHARE`, source.TaskID)
	if err != nil {
		return fmt.Errorf("lock Task batch item facts for result receipt: %w", err)
	}
	for rows.Next() {
		var itemKey, status string
		if err := rows.Scan(&itemKey, &status); err != nil {
			rows.Close()
			return fmt.Errorf("read Task batch item fact for result receipt: %w", err)
		}
		conversationCountGenericOperation(facts, status, false, false)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Task batch item facts for result receipt: %w", err)
	}
	rows.Close()
	return nil
}

func loadConversationResultStepFactsInTx(ctx context.Context, tx pgx.Tx, source app.ConversationTaskResultSource,
	facts *conversationResultDeliveryFacts) error {
	rows, err := tx.Query(ctx, `SELECT id,COALESCE(task_revision,-1),status FROM task_steps
WHERE task_id=$1 AND status IN ('queued','running','waiting_user','unknown') ORDER BY sequence,id FOR SHARE`, source.TaskID)
	if err != nil {
		return fmt.Errorf("lock Task step facts for result receipt: %w", err)
	}
	for rows.Next() {
		var stepID, status string
		var revision int64
		if err := rows.Scan(&stepID, &revision, &status); err != nil {
			rows.Close()
			return fmt.Errorf("read Task step fact for result receipt: %w", err)
		}
		conversationCountGenericOperation(facts, status, revision >= 0, revision == source.TaskRevision)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Task step facts for result receipt: %w", err)
	}
	rows.Close()
	return nil
}

func conversationResultDeliveryFactLines(source app.ConversationTaskResultSource, facts conversationResultDeliveryFacts) []string {
	lines := make([]string, 0, len(facts.approvals)+2)
	for index, approval := range facts.approvals {
		toolName := strings.Join(strings.Fields(approval.toolName), " ")
		if toolName == "" {
			toolName = "未知工具"
		}
		toolRunes := []rune(toolName)
		if len(toolRunes) > 32 {
			toolName = string(toolRunes[:31]) + "…"
		}
		decision := approval.decision
		if !approval.revisionSet {
			decision = "修订未知，" + decision
		}
		lines = append(lines, fmt.Sprintf("%s r%d 审批#%d %s：%s；操作回执：%s",
			source.TaskID, source.TaskRevision, index+1, toolName, decision, approval.operation))
	}
	if facts.pendingOperations+facts.unknownOperations+facts.nonSuccessOperations > 0 {
		lines = append(lines, fmt.Sprintf("%s r%d 其他操作账本：%d 条待处理、%d 条未知待对账、%d 条已结束非成功操作%s",
			source.TaskID, source.TaskRevision, facts.pendingOperations, facts.unknownOperations, facts.nonSuccessOperations,
			conversationResultUnversionedSuffix(facts.unversionedOperations)))
	}
	if facts.hasArtifacts {
		lines = append(lines, "产物请在 Task 详情打开；授权下载接口会重新校验权限和内容可用性")
	}
	return lines
}

func conversationResultUnversionedSuffix(count int) string {
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("（%d 条修订归属不明）", count)
}
