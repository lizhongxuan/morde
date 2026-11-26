package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type conversationResultDeliveryFacts struct {
	approvals             []conversationResultApprovalFact
	pendingOperations     int
	unknownOperations     int
	nonSuccessOperations  int
	unversionedOperations int
	hasArtifacts          bool
	validationPlan        *task.Plan
	validationReplay      *app.TaskValidationReplayEvidence
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
	var initiatorID, conversationID string
	if err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision,initiator_id,conversation_id FROM tasks
WHERE id=$1 AND owner_id=$2 FOR SHARE`, source.TaskID, ownerID).Scan(&currentRevision, &desiredRevision, &initiatorID, &conversationID); err != nil {
		return facts, fmt.Errorf("lock Task for result delivery facts: %w", err)
	}
	if currentRevision != source.TaskRevision || desiredRevision != source.TaskRevision {
		return facts, fmt.Errorf("task revision changed while loading result delivery facts")
	}
	if err := loadConversationResultPlanValidationFactsInTx(ctx, tx, ownerID, initiatorID, conversationID, source, &facts); err != nil {
		return facts, err
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

func loadConversationResultPlanValidationFactsInTx(ctx context.Context, tx pgx.Tx, ownerID, initiatorID, conversationID string,
	source app.ConversationTaskResultSource, facts *conversationResultDeliveryFacts) error {
	plan, err := latestTaskPlan(ctx, tx, source.TaskID, source.TaskRevision)
	if err != nil {
		return fmt.Errorf("read current fix plan for result receipt: %w", err)
	}
	if plan == nil || plan.FixPlan == nil {
		return nil
	}
	resultDigest := sha256.Sum256([]byte(source.Content))
	// Replay assembly uses the Task/revision/source identity only. The receipt
	// path does not claim or extend a review lease.
	claim := app.TaskValidationReviewClaim{OwnerID: ownerID, InitiatorID: initiatorID, ConversationID: conversationID,
		TaskID: source.TaskID, TaskRevision: source.TaskRevision, ResultEventSequence: source.EventSequence,
		ResultSHA256: resultDigest[:], ClaimGeneration: 1, LeaseOwner: "conversation-result-receipt"}
	if !app.ValidTaskValidationReviewClaim(claim) {
		return errors.New("task result identity is invalid for validation receipt")
	}
	current, err := readTaskValidationReplayTask(ctx, tx, claim)
	if err != nil {
		return fmt.Errorf("read current Task identity for validation receipt: %w", err)
	}
	replay, err := loadTaskValidationPlanCheckReplay(ctx, tx, claim, *plan, current)
	if err != nil {
		return fmt.Errorf("read current typed check facts for result receipt: %w", err)
	}
	facts.validationPlan = plan
	facts.validationReplay = replay
	return nil
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
	lines := make([]string, 0, len(facts.approvals)+len(facts.validationReplayChecks())+3)
	lines = append(lines, conversationResultValidationFactLines(source, facts)...)
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

func (facts conversationResultDeliveryFacts) validationReplayChecks() []app.TaskValidationReplayCheckEvidence {
	if facts.validationReplay == nil {
		return nil
	}
	return facts.validationReplay.Checks
}

func conversationResultValidationFactLines(source app.ConversationTaskResultSource, facts conversationResultDeliveryFacts) []string {
	plan, replay := facts.validationPlan, facts.validationReplay
	if plan == nil || plan.FixPlan == nil {
		return nil
	}
	state := app.TaskValidationReplayPending
	if replay != nil && replay.State != "" {
		state = replay.State
	}
	lines := []string{fmt.Sprintf("%s r%d 修复验证 Plan r%d：H07 %s；要求集完整性 %s；最终 Workspace 快照 %s",
		source.TaskID, source.TaskRevision, plan.Revision, state,
		conversationResultValidationFactState(replay != nil && replay.RequirementSetVerified),
		conversationResultValidationFactState(replay != nil && replay.FinalWorkspaceSnapshotVerified))}
	if replay == nil || !replay.RequirementSetVerified {
		lines = append(lines, "该计划没有可信的要求集完整性证明；已列检查不能代表所有用户/项目要求均已覆盖")
	}
	checksByID := make(map[string]app.TaskValidationReplayCheckEvidence)
	if replay != nil {
		for _, check := range replay.Checks {
			checksByID[check.CheckID] = check
		}
	}
	for _, step := range plan.Steps {
		check := step.VerificationCheck
		if check == nil {
			continue
		}
		fact, found := checksByID[step.ID]
		status := "无可绑定的执行回执"
		if found {
			switch {
			case fact.OperationID == "":
				status = "无可绑定的执行回执"
			case fact.Status == app.TaskValidationReplayFail:
				status = fmt.Sprintf("执行失败（退出码 %d，超时 %t）", fact.ExitCode, fact.TimedOut)
			case fact.Status == app.TaskValidationReplayPass:
				status = "命令退出码 0；实际覆盖范围 " + conversationResultValidationFactState(fact.ActualCoverageVerified)
			default:
				status = "存在操作记录，但执行结果或身份尚未完整核验"
			}
		}
		basis := "风险选择（非用户/项目明确要求）"
		sourceRef := ""
		if check.Basis == task.PlanVerificationCheckRequired {
			basis = "计划声明为必需"
			requirementIDs := make([]string, 0, len(check.RequirementIDs))
			for _, id := range check.RequirementIDs {
				requirementIDs = append(requirementIDs, string(id))
			}
			sourceRef = "要求 " + strings.Join(requirementIDs, ", ") + " → " +
				conversationResultRequirementSourceSummary(plan, *check, found && fact.RequirementSourceVerified)
		}
		scope := string(check.ScopeMode)
		if scope == "" {
			scope = "范围未记录"
		}
		line := fmt.Sprintf("%s r%d Plan r%d 检查 %s（%s/%s，%s）：%s；范围：%s",
			source.TaskID, source.TaskRevision, plan.Revision, step.ID, check.Kind, basis, scope, status,
			conversationResultValidationScope(check.CoveragePaths, check.Selectors))
		if sourceRef != "" {
			line += "；来源 " + sourceRef
		}
		if found && fact.OperationID != "" {
			line += fmt.Sprintf("；执行记录 %s（步骤序号 %d）", fact.OperationID, fact.RunSequence)
		}
		lines = append(lines, line)
	}
	if replay != nil && len(replay.PlanStepsWithoutChecks) > 0 {
		lines = append(lines, "未绑定检查的计划步骤："+strings.Join(replay.PlanStepsWithoutChecks, ", "))
	}
	return lines
}

func conversationResultRequirementSourceSummary(plan *task.Plan, check task.PlanVerificationCheck, verified bool) string {
	verification := "来源未核验"
	if verified {
		verification = "来源已核验"
	}
	sources := make([]string, 0, len(check.RequirementIDs))
	if plan.RequirementManifest != nil {
		obligations := make(map[task.PlanVerificationRequirementID]task.PlanVerificationObligation,
			len(plan.RequirementManifest.Obligations))
		for _, obligation := range plan.RequirementManifest.Obligations {
			obligations[obligation.ID] = obligation
		}
		for _, id := range check.RequirementIDs {
			if obligation, ok := obligations[id]; ok {
				sources = append(sources, conversationResultRequirementSourceLocation(obligation.Source))
			} else {
				sources = append(sources, string(id)+"（来源缺失）")
			}
		}
	} else if check.RequirementSource != nil {
		sources = append(sources, conversationResultRequirementSourceLocation(*check.RequirementSource))
	}
	if len(sources) == 0 {
		return "用户/项目来源缺失（" + verification + "）"
	}
	return strings.Join(sources, ", ") + "（" + verification + "）"
}

func conversationResultRequirementSourceLocation(source task.PlanVerificationRequirementSource) string {
	switch source.Kind {
	case task.PlanVerificationRequirementUserMessage:
		return "用户消息 " + source.MessageID
	case task.PlanVerificationRequirementProjectFile:
		if source.WorkspaceRevision == nil {
			return "项目文件 " + source.WorkspacePath + "（版本缺失）"
		}
		return fmt.Sprintf("Workspace r%d:%s", *source.WorkspaceRevision, source.WorkspacePath)
	default:
		return "未知来源"
	}
}

func conversationResultValidationScope(paths, selectors []string) string {
	parts := make([]string, 0, 2)
	if len(paths) > 0 {
		parts = append(parts, "路径 "+strings.Join(paths, ", "))
	}
	if len(selectors) > 0 {
		parts = append(parts, "选择器 "+strings.Join(selectors, ", "))
	}
	if len(parts) == 0 {
		return "未记录"
	}
	return strings.Join(parts, "；")
}

func conversationResultValidationFactState(verified bool) string {
	if verified {
		return "已验证"
	}
	return "待验证"
}

func conversationResultUnversionedSuffix(count int) string {
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("（%d 条修订归属不明）", count)
}
