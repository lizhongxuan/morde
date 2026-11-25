package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) GetCloudDSHTaskPlanReceipt(ctx context.Context, taskID, ownerID, sessionID, callID string) (task.Plan, error) {
	var result task.Plan
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		_, err := taskOwnerIDForActor(ctx, tx, taskID, ownerID, false, false)
		if err != nil {
			return err
		}
		var payload []byte
		err = tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.plan'
AND payload #>> '{detail,call_id}'=$2 ORDER BY sequence DESC LIMIT 1`, taskID, callID).Scan(&payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task plan receipt not found")
		}
		if err != nil {
			return fmt.Errorf("read task plan receipt: %w", err)
		}
		result, err = app.ReadCloudDSHTaskPlanReceipt(payload, taskID, ownerID, sessionID, callID)
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func latestTaskPlan(ctx context.Context, tx pgx.Tx, taskID string, taskRevision int64) (*task.Plan, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.plan'
AND payload #>> '{plan,task_revision}'=$2 ORDER BY sequence DESC LIMIT 1`, taskID, fmt.Sprint(taskRevision)).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest task plan: %w", err)
	}
	var event appTaskPlanEvent
	if len(payload) > 64<<10 || json.Unmarshal(payload, &event) != nil || event.Plan.Validate() != nil ||
		event.Plan.TaskRevision != taskRevision || event.PlanRevision != event.Plan.Revision || event.TaskRevision != taskRevision {
		return nil, domain.NewError(domain.CodeInvalidState, "stored task plan is invalid")
	}
	return &event.Plan, nil
}

type appTaskPlanEvent struct {
	StepID       string    `json:"step_id"`
	Sequence     int64     `json:"sequence"`
	TaskRevision int64     `json:"task_revision"`
	PlanRevision int64     `json:"plan_revision"`
	Plan         task.Plan `json:"plan"`
	Detail       struct {
		CallID               string `json:"call_id"`
		AgentSessionID       string `json:"agent_session_id"`
		Tool                 string `json:"tool"`
		AuthorizationVersion int64  `json:"authorization_version"`
		FingerprintSHA256    string `json:"fingerprint_sha256"`
	} `json:"detail"`
}

const (
	planStepExecutionOperation = "operation"
	planStepExecutionMCP       = "mcp_connector"
	planStepExecutionChild     = "child_session"
	planStepExecutionReadTool  = "read_tool"

	planStepReadinessTaskRevisionStale = "task_revision_stale"
	planStepReadinessPlanRevisionStale = "plan_revision_stale"
	planStepReadinessTaskNotRunning    = "task_not_running"
	planStepReadinessAuthorization     = "authorization_unavailable"
	planStepReadinessPlanMissing       = "plan_missing"
	planStepReadinessStepMissing       = "plan_step_missing"
	planStepReadinessMissingBinding    = "dependency_missing_binding"
	planStepReadinessEvidenceMissing   = "execution_evidence_missing"
	planStepReadinessPending           = "execution_not_settled"
	planStepReadinessFailed            = "execution_failed"
	planStepReadinessCancelled         = "execution_cancelled"
	planStepReadinessUnknown           = "execution_unknown"
)

type taskPlanStepExecutionBinding struct {
	kind                 string
	executionID          string
	agentSessionID       string
	callID               string
	toolName             string
	connectorID          string
	childStepID          string
	parentStepID         string
	requestDigest        []byte
	resultDigest         []byte
	authorizationVersion int64
}

type taskPlanStepBindingFence struct {
	ownerID              string
	taskID               string
	taskRevision         int64
	planRevision         int64
	authorizationVersion int64
	planStepID           string
	plan                 task.Plan
	planTaskStepSequence int64
}

type taskPlanStepBindingRow struct {
	planStepID string
	binding    taskPlanStepExecutionBinding
}

func findCloudDSHTaskPlanEvent(ctx context.Context, tx pgx.Tx, taskID, callID string) ([]byte, bool, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.plan'
AND payload #>> '{detail,call_id}'=$2 ORDER BY sequence LIMIT 1`, taskID, callID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find previous task plan call: %w", err)
	}
	return payload, true, nil
}

func validateCloudDSHTaskPlanProgress(ctx context.Context, tx pgx.Tx, current task.Task, progress app.TaskProgress, commit app.CloudDSHTaskPlanCommit) (task.Event, bool, error) {
	if current.ID != commit.TaskID || current.OwnerID != commit.OwnerID || current.ExecutionLocation != "server" ||
		current.AgentSessionID != commit.AgentSessionID || current.Authorization.Version != commit.AuthorizationVersion ||
		progress.ToolCall == nil || progress.ToolCall.TaskRevision != commit.Plan.TaskRevision {
		return task.Event{}, false, domain.NewError(domain.CodeInvalidState, "task plan revision or authorization changed")
	}
	var authorizationValid bool
	if err := tx.QueryRow(ctx, `SELECT auth_kind='schedule_task' OR auth_expires_at > clock_timestamp() FROM tasks WHERE id=$1`, current.ID).Scan(&authorizationValid); err != nil {
		return task.Event{}, false, fmt.Errorf("check task plan authorization expiry: %w", err)
	}
	if !authorizationValid {
		return task.Event{}, false, domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
	if len(progress.Steps) != 1 || app.ValidateCloudDSHTaskPlanEventForStep(progress.Event.Payload, commit,
		progress.Steps[0].ID, progress.Steps[0].Sequence) != nil {
		return task.Event{}, false, domain.NewError(domain.CodeInvalidState, "task plan event step identity changed")
	}
	if priorPayload, found, err := findCloudDSHTaskPlanEvent(ctx, tx, current.ID, commit.CallID); err != nil {
		return task.Event{}, false, err
	} else if found {
		if app.ValidateCloudDSHTaskPlanEvent(priorPayload, commit) != nil {
			return task.Event{}, false, domain.NewError(domain.CodeIdempotencyConflict, "task plan call identity was reused")
		}
		var prior task.Event
		if err := tx.QueryRow(ctx, `SELECT task_id,sequence,event_type,payload,created_at FROM task_events
WHERE task_id=$1 AND event_type='task.plan' AND payload #>> '{detail,call_id}'=$2 ORDER BY sequence LIMIT 1`, current.ID, commit.CallID).
			Scan(&prior.TaskID, &prior.Sequence, &prior.Type, &prior.Payload, &prior.CreatedAt); err != nil {
			return task.Event{}, false, fmt.Errorf("read replayed task plan event: %w", err)
		}
		return prior, true, nil
	}
	var previousRevision int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(MAX((payload #>> '{plan,revision}')::bigint),0) FROM task_events
WHERE task_id=$1 AND event_type='task.plan' AND payload #>> '{plan,task_revision}'=$2`, current.ID, fmt.Sprint(commit.Plan.TaskRevision)).Scan(&previousRevision)
	if err != nil {
		return task.Event{}, false, fmt.Errorf("read prior task plan revision: %w", err)
	}
	if commit.Plan.Revision != previousRevision+1 {
		return task.Event{}, false, domain.NewError(domain.CodeConflict, "task plan revision is stale")
	}
	return task.Event{}, false, nil
}

// BindCloudDSHTaskPlanStepOperation records an existing task_operations row
// against one step in the current persisted plan. Host operation dispatch
// admission uses the same binding logic in its prepared-to-dispatched
// transaction below.
func (s *Store) BindCloudDSHTaskPlanStepOperation(ctx context.Context, current task.Task, planRevision int64,
	planStepID string, toolCall app.TaskToolCallProgress, operation task.Operation) error {
	if ctx == nil {
		return domain.InvalidArgument("task_plan_step.binding", "task plan execution context is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return bindCloudDSHTaskPlanStepOperationInTx(ctx, tx, current, planRevision, planStepID, toolCall, operation)
	})
}

func bindCloudDSHTaskPlanStepOperationInTx(ctx context.Context, tx pgx.Tx, current task.Task, planRevision int64,
	planStepID string, toolCall app.TaskToolCallProgress, operation task.Operation) error {
	if !validPlanStepToolCall(current, planRevision, planStepID, toolCall) ||
		!domain.ValidOperationID(operation.ID) || operation.TaskID != current.ID || operation.TaskRevision == nil ||
		*operation.TaskRevision != toolCall.TaskRevision || operation.CallID != toolCall.CallKey ||
		operation.ToolName != toolCall.ToolName || operation.Key != app.AgentOperationKey(operation.CallID) ||
		len(operation.RequestHash) != 32 {
		return domain.InvalidArgument("task_plan_step.binding", "task plan operation identity is invalid")
	}
	fence, blocked, err := taskPlanStepBindingFenceInTx(ctx, tx, current, planRevision, planStepID,
		toolCall.AuthorizationVersion, true)
	if err != nil {
		return err
	}
	if blocked != "" {
		return taskPlanStepBindingConflict(blocked)
	}
	binding := taskPlanStepExecutionBinding{kind: planStepExecutionOperation, executionID: operation.ID,
		agentSessionID: toolCall.AgentSessionID, callID: toolCall.CallKey, toolName: toolCall.ToolName,
		requestDigest: append([]byte(nil), toolCall.RequestDigest...), authorizationVersion: toolCall.AuthorizationVersion}
	if err := verifyTaskPlanStepExecutionSource(ctx, tx, fence, &binding); err != nil {
		return err
	}
	var existingStepID string
	err = tx.QueryRow(ctx, `SELECT plan_step_id FROM task_plan_step_execution_bindings
WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4 AND execution_kind=$5 AND execution_id=$6`,
		fence.ownerID, fence.taskID, fence.taskRevision, fence.planRevision, binding.kind, binding.executionID).Scan(&existingStepID)
	if err == nil {
		return requireExistingTaskPlanStepOperationBindingInTx(ctx, tx, fence, planStepID, binding)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read task plan operation binding before replay: %w", err)
	}
	ready, reason, err := taskPlanStepDependenciesReadyInTx(ctx, tx, fence, planStepID)
	if err != nil {
		return err
	}
	if !ready {
		return taskPlanStepBindingConflict(reason)
	}
	return persistTaskPlanStepExecutionBindingInTx(ctx, tx, fence, planStepID, binding)
}

// BindCloudDSHTaskPlanStepMCPConnectorOperation records an existing
// task_mcp_connector_operations row against one step in the current persisted
// plan. MCP operation settlement is always derived from that existing row.
// MCP dispatch now creates the same binding atomically at its prepared to
// dispatched boundary; this adapter remains available for evidence recovery.
func (s *Store) BindCloudDSHTaskPlanStepMCPConnectorOperation(ctx context.Context, current task.Task, planRevision int64,
	planStepID string, toolCall app.TaskToolCallProgress, operation app.MCPConnectorOperation) error {
	if !validPlanStepToolCall(current, planRevision, planStepID, toolCall) ||
		!domain.ValidOperationID(operation.ID) || operation.TaskID != current.ID || operation.OwnerID != current.OwnerID ||
		operation.TaskRevision == nil || *operation.TaskRevision != toolCall.TaskRevision ||
		operation.CallID != toolCall.CallKey || operation.ToolName != toolCall.ToolName ||
		operation.ConnectorID == "" || len(operation.RequestHash) != 32 {
		return domain.InvalidArgument("task_plan_step.binding", "task plan MCP operation identity is invalid")
	}
	binding := taskPlanStepExecutionBinding{kind: planStepExecutionMCP, executionID: operation.ID,
		agentSessionID: toolCall.AgentSessionID, callID: toolCall.CallKey, toolName: toolCall.ToolName,
		connectorID: operation.ConnectorID, requestDigest: append([]byte(nil), toolCall.RequestDigest...),
		authorizationVersion: toolCall.AuthorizationVersion}
	return s.bindCloudDSHTaskPlanStepExecution(ctx, current, planRevision, planStepID, binding)
}

// BindCloudDSHTaskPlanStepChildSession records an existing child Agent Session
// and its spawn identity against one step in the current persisted plan. Child
// creation and binding still need to be wired in one transaction by its caller.
func (s *Store) BindCloudDSHTaskPlanStepChildSession(ctx context.Context, current task.Task, planRevision int64,
	planStepID string, child app.AgentChildSession) error {
	if !validPlanStepIdentity(current, planRevision, planStepID) || child.TaskID != current.ID ||
		child.SessionID == "" || child.ParentSessionID == "" || child.SpawnKey == "" ||
		!domain.ValidStepID(child.Step.ID) || child.Step.TaskID != current.ID || current.Authorization.Version < 1 {
		return domain.InvalidArgument("task_plan_step.binding", "task plan child session identity is invalid")
	}
	binding := taskPlanStepExecutionBinding{kind: planStepExecutionChild, executionID: child.SessionID,
		agentSessionID: child.ParentSessionID, callID: child.SpawnKey, childStepID: child.Step.ID,
		authorizationVersion: current.Authorization.Version}
	return s.bindCloudDSHTaskPlanStepExecution(ctx, current, planRevision, planStepID, binding)
}

// AdmitCloudDSHTaskPlanStepRead records a pending direct read against the
// current plan step before the broker invokes the read tool. Its result digest
// stays NULL until the broker has a successful result and the matching
// task_steps call is durably successful.
func (s *Store) AdmitCloudDSHTaskPlanStepRead(ctx context.Context, current task.Task, planRevision int64,
	planStepID string, toolCall app.TaskToolCallProgress) error {
	if ctx == nil || !validPlanStepToolCall(current, planRevision, planStepID, toolCall) ||
		current.AgentSessionID == "" || toolCall.AgentSessionID != current.AgentSessionID ||
		current.ExecutionLocation != "server" {
		return domain.InvalidArgument("task_plan_step.read", "task plan read identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		fence, blocked, err := taskPlanStepBindingFenceInTx(ctx, tx, current, planRevision, planStepID,
			toolCall.AuthorizationVersion, true)
		if err != nil {
			return err
		}
		if blocked != "" {
			return taskPlanStepBindingConflict(blocked)
		}
		ready, reason, err := taskPlanStepDependenciesReadyInTx(ctx, tx, fence, planStepID)
		if err != nil {
			return err
		}
		if !ready {
			return taskPlanStepBindingConflict(reason)
		}
		binding := taskPlanStepExecutionBinding{kind: planStepExecutionReadTool,
			executionID: taskPlanReadToolExecutionID(toolCall.AgentSessionID, toolCall.CallKey),
			agentSessionID: toolCall.AgentSessionID, callID: toolCall.CallKey, toolName: toolCall.ToolName,
			requestDigest: append([]byte(nil), toolCall.RequestDigest...), authorizationVersion: toolCall.AuthorizationVersion}
		var stored taskPlanStepBindingRow
		err = tx.QueryRow(ctx, `SELECT plan_step_id,execution_kind,execution_id,agent_session_id,call_id,tool_name,
connector_id,child_step_id,parent_step_id,request_digest,authorization_version,result_digest
FROM task_plan_step_execution_bindings WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4
 AND execution_kind=$5 AND execution_id=$6 FOR UPDATE`, fence.ownerID, fence.taskID, fence.taskRevision,
			fence.planRevision, binding.kind, binding.executionID).Scan(&stored.planStepID, &stored.binding.kind,
			&stored.binding.executionID, &stored.binding.agentSessionID, &stored.binding.callID, &stored.binding.toolName,
			&stored.binding.connectorID, &stored.binding.childStepID, &stored.binding.parentStepID,
			&stored.binding.requestDigest, &stored.binding.authorizationVersion, &stored.binding.resultDigest)
		if err == nil {
			if stored.planStepID != planStepID || !sameTaskPlanStepExecutionBindingIgnoringResult(stored.binding, binding) ||
				(len(stored.binding.resultDigest) != 0 && len(stored.binding.resultDigest) != 32) {
				return domain.NewError(domain.CodeIdempotencyConflict, "task plan read call identity was reused")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing task plan read binding: %w", err)
		}
		return persistTaskPlanStepExecutionBindingInTx(ctx, tx, fence, planStepID, binding)
	})
}

// SettleCloudDSHTaskPlanStepRead records the digest of the exact successful
// result returned by a direct read. It rechecks the current plan and
// dependencies and verifies the existing TaskStep identity in the same
// transaction that stores the result digest.
func (s *Store) SettleCloudDSHTaskPlanStepRead(ctx context.Context, current task.Task, planRevision int64,
	planStepID string, toolCall app.TaskToolCallProgress, resultDigest []byte) error {
	if ctx == nil || !validPlanStepToolCall(current, planRevision, planStepID, toolCall) ||
		current.AgentSessionID == "" || toolCall.AgentSessionID != current.AgentSessionID ||
		current.ExecutionLocation != "server" || len(resultDigest) != 32 {
		return domain.InvalidArgument("task_plan_step.read", "task plan read settlement is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		fence, blocked, err := taskPlanStepBindingFenceInTx(ctx, tx, current, planRevision, planStepID,
			toolCall.AuthorizationVersion, true)
		if err != nil {
			return err
		}
		if blocked != "" {
			return taskPlanStepBindingConflict(blocked)
		}
		ready, reason, err := taskPlanStepDependenciesReadyInTx(ctx, tx, fence, planStepID)
		if err != nil {
			return err
		}
		if !ready {
			return taskPlanStepBindingConflict(reason)
		}
		binding := taskPlanStepExecutionBinding{kind: planStepExecutionReadTool,
			executionID: taskPlanReadToolExecutionID(toolCall.AgentSessionID, toolCall.CallKey),
			agentSessionID: toolCall.AgentSessionID, callID: toolCall.CallKey, toolName: toolCall.ToolName,
			requestDigest: append([]byte(nil), toolCall.RequestDigest...), resultDigest: append([]byte(nil), resultDigest...),
			authorizationVersion: toolCall.AuthorizationVersion}
		if err := verifyTaskPlanStepReadSource(ctx, tx, fence, binding, true); err != nil {
			return err
		}
	var storedPlanStepID string
		var stored taskPlanStepExecutionBinding
		err = tx.QueryRow(ctx, `SELECT plan_step_id,execution_kind,execution_id,agent_session_id,call_id,tool_name,
connector_id,child_step_id,parent_step_id,request_digest,authorization_version,result_digest
FROM task_plan_step_execution_bindings WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4
 AND execution_kind=$5 AND execution_id=$6 FOR UPDATE`, fence.ownerID, fence.taskID, fence.taskRevision,
			fence.planRevision, binding.kind, binding.executionID).Scan(&storedPlanStepID, &stored.kind,
			&stored.executionID, &stored.agentSessionID, &stored.callID, &stored.toolName, &stored.connectorID,
			&stored.childStepID, &stored.parentStepID, &stored.requestDigest, &stored.authorizationVersion, &stored.resultDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if err != nil {
			return fmt.Errorf("read pending task plan read binding: %w", err)
		}
		if storedPlanStepID != planStepID || !sameTaskPlanStepExecutionBindingIgnoringResult(stored, binding) {
			return domain.NewError(domain.CodeIdempotencyConflict, "task plan read call was bound differently")
		}
		if len(stored.resultDigest) > 0 {
			if bytes.Equal(stored.resultDigest, resultDigest) {
				return nil
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "task plan read result changed after settlement")
		}
		command, err := tx.Exec(ctx, `UPDATE task_plan_step_execution_bindings SET result_digest=$7
WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4 AND execution_kind=$5 AND execution_id=$6
 AND plan_step_id=$8 AND result_digest IS NULL`, fence.ownerID, fence.taskID, fence.taskRevision,
			fence.planRevision, binding.kind, binding.executionID, resultDigest, planStepID)
		if err != nil {
			return fmt.Errorf("settle task plan read binding: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeIdempotencyConflict, "task plan read settlement changed concurrently")
		}
		return nil
	})
}

// CheckCloudDSHTaskPlanStepReadiness derives readiness from durable execution
// rows. PlanStep.Status is intentionally ignored; missing or non-successful
// evidence returns false with a stable reason. It is not a complete execution
// gate until operation admission and child spawn call sites bind atomically.
func (s *Store) CheckCloudDSHTaskPlanStepReadiness(ctx context.Context, current task.Task,
	planRevision int64, planStepID string) (bool, string, error) {
	if ctx == nil || !validPlanStepIdentity(current, planRevision, planStepID) || current.Authorization.Version < 1 {
		return false, "", domain.InvalidArgument("task_plan_step", "task plan step identity is invalid")
	}
	ready := false
	reason := ""
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		fence, blocked, err := taskPlanStepBindingFenceInTx(ctx, tx, current, planRevision, planStepID,
			current.Authorization.Version, false)
		if err != nil {
			return err
		}
		if blocked != "" {
			reason = blocked
			return nil
		}
		ready, reason, err = taskPlanStepDependenciesReadyInTx(ctx, tx, fence, planStepID)
		if err != nil {
			return err
		}
		return nil
	})
	return ready, reason, err
}

func taskPlanStepDependenciesReadyInTx(ctx context.Context, tx pgx.Tx, fence taskPlanStepBindingFence,
	planStepID string) (bool, string, error) {
	for _, dependencyID := range planStepDependencies(fence.plan, planStepID) {
		bindings, err := loadTaskPlanStepBindings(ctx, tx, fence, dependencyID)
		if err != nil {
			return false, "", err
		}
		if len(bindings) == 0 {
			return false, planStepReadinessMissingBinding, nil
		}
		for _, binding := range bindings {
			if binding.authorizationVersion != fence.authorizationVersion {
				return false, planStepReadinessAuthorization, nil
			}
			reason, err := taskPlanStepExecutionSettlementReason(ctx, tx, fence, binding)
			if err != nil {
				return false, "", err
			}
			if reason != "" {
				return false, reason, nil
			}
		}
	}
	return true, "", nil
}

func validPlanStepIdentity(current task.Task, planRevision int64, planStepID string) bool {
	return domain.ValidTaskID(current.ID) && domain.ValidUserID(current.OwnerID) && current.CurrentRevision != nil &&
		current.DesiredRevision != nil && *current.CurrentRevision >= 0 && *current.DesiredRevision == *current.CurrentRevision &&
		planRevision > 0 && validTaskPlanStepID(planStepID)
}

func validTaskPlanStepID(value string) bool {
	if value == "" || len(value) > 40 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') ||
			(index > 0 && (char == '-' || char == '_')) {
			continue
		}
		return false
	}
	return true
}

func taskPlanReadToolExecutionID(agentSessionID, callID string) string {
	digest := sha256.Sum256([]byte(agentSessionID + "\x00" + callID))
	return fmt.Sprintf("read:%x", digest)
}

func sameTaskPlanStepExecutionBindingIgnoringResult(a, b taskPlanStepExecutionBinding) bool {
	a.resultDigest, b.resultDigest = nil, nil
	return sameTaskPlanStepExecutionBinding(a, b)
}

func verifyTaskPlanStepReadSource(ctx context.Context, tx pgx.Tx, fence taskPlanStepBindingFence,
	binding taskPlanStepExecutionBinding, requireSuccess bool) error {
	var taskRevision, sequence int64
	var status, agentSessionID, callID, toolName string
	var requestDigest []byte
	err := tx.QueryRow(ctx, `SELECT COALESCE(task_revision,-1),sequence,status,COALESCE(agent_session_id,''),
COALESCE(tool_call_key,''),COALESCE(tool_name,''),COALESCE(request_digest,''::bytea)
FROM task_steps WHERE task_id=$1 AND agent_session_id=$2 AND task_revision=$3 AND tool_call_key=$4 AND tool_name=$5
FOR SHARE`, fence.taskID, binding.agentSessionID, fence.taskRevision, binding.callID, binding.toolName).
		Scan(&taskRevision, &sequence, &status, &agentSessionID, &callID, &toolName, &requestDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
	}
	if err != nil {
		return fmt.Errorf("verify task plan read TaskStep identity: %w", err)
	}
	if taskRevision != fence.taskRevision || sequence <= fence.planTaskStepSequence ||
		agentSessionID != binding.agentSessionID || callID != binding.callID || toolName != binding.toolName ||
		!bytes.Equal(requestDigest, binding.requestDigest) || !validTaskPlanStepStatus(status) {
		return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
	}
	if requireSuccess && status != string(task.StepSucceeded) {
		return taskPlanStepBindingConflict(taskPlanChildSettlementReason(status))
	}
	return nil
}

func validPlanStepToolCall(current task.Task, planRevision int64, planStepID string, call app.TaskToolCallProgress) bool {
	return validPlanStepIdentity(current, planRevision, planStepID) && current.ExecutionLocation == "server" &&
		call.AgentSessionID != "" && len(call.AgentSessionID) <= 128 && call.TaskRevision == *current.CurrentRevision &&
		call.AuthorizationVersion > 0 && call.AuthorizationVersion == current.Authorization.Version &&
		call.CallKey != "" && len(call.CallKey) <= 256 && call.ToolName != "" && len(call.ToolName) <= 128 &&
		len(call.RequestDigest) == 32
}

func (s *Store) bindCloudDSHTaskPlanStepExecution(ctx context.Context, current task.Task, planRevision int64,
	planStepID string, binding taskPlanStepExecutionBinding) error {
	if ctx == nil {
		return domain.InvalidArgument("task_plan_step.binding", "task plan execution context is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		fence, blocked, err := taskPlanStepBindingFenceInTx(ctx, tx, current, planRevision, planStepID,
			binding.authorizationVersion, true)
		if err != nil {
			return err
		}
		if blocked != "" {
			return taskPlanStepBindingConflict(blocked)
		}
		if err := verifyTaskPlanStepExecutionSource(ctx, tx, fence, &binding); err != nil {
			return err
		}
		return persistTaskPlanStepExecutionBindingInTx(ctx, tx, fence, planStepID, binding)
	})
}

// admitCloudDSHTaskPlanStepOperationDispatchInTx gates the side-effect
// boundary. The caller holds the Task row lock and invokes this before
// changing a prepared operation to dispatched, so binding and dispatch are
// committed atomically. Model-provided IDs are only selectors: plan identity,
// operation identity, and dependency settlement are verified from Store rows.
func admitCloudDSHTaskPlanStepOperationDispatchInTx(ctx context.Context, tx pgx.Tx, current task.Task,
	operation task.Operation, planRevision int64, planStepID string) error {
	if current.CurrentRevision == nil || *current.CurrentRevision < 0 || current.DesiredRevision == nil ||
		*current.DesiredRevision != *current.CurrentRevision {
		return taskPlanStepBindingConflict(planStepReadinessTaskRevisionStale)
	}
	if (planRevision == 0) != (planStepID == "") {
		return domain.InvalidArgument("task_plan_step", "task plan step metadata is incomplete")
	}
	plan, err := latestTaskPlan(ctx, tx, current.ID, *current.CurrentRevision)
	if err != nil {
		return err
	}
	if plan == nil {
		if planRevision != 0 || planStepID != "" {
			return taskPlanStepBindingConflict(planStepReadinessPlanMissing)
		}
		return nil
	}
	if planRevision < 1 || planStepID == "" {
		return taskPlanStepBindingConflict(planStepReadinessStepMissing)
	}
	if planRevision != plan.Revision {
		return taskPlanStepBindingConflict(planStepReadinessPlanRevisionStale)
	}
	if operation.TaskRevision == nil || *operation.TaskRevision != *current.CurrentRevision ||
		operation.TaskID != current.ID || operation.CallID == "" ||
		operation.ToolName == "" || len(operation.RequestHash) != 32 {
		return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
	}
	toolCall := app.TaskToolCallProgress{AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version, TaskRevision: *operation.TaskRevision,
		CallKey: operation.CallID, ToolName: operation.ToolName, RequestDigest: append([]byte(nil), operation.RequestHash...)}
	if !validPlanStepToolCall(current, planRevision, planStepID, toolCall) {
		return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
	}
	fence, blocked, err := taskPlanStepBindingFenceInTx(ctx, tx, current, planRevision, planStepID,
		toolCall.AuthorizationVersion, true)
	if err != nil {
		return err
	}
	if blocked != "" {
		return taskPlanStepBindingConflict(blocked)
	}
	binding := taskPlanStepExecutionBinding{kind: planStepExecutionOperation, executionID: operation.ID,
		agentSessionID: toolCall.AgentSessionID, callID: toolCall.CallKey, toolName: toolCall.ToolName,
		requestDigest: toolCall.RequestDigest, authorizationVersion: toolCall.AuthorizationVersion}
	if err := verifyTaskPlanStepExecutionSource(ctx, tx, fence, &binding); err != nil {
		return err
	}
	if operation.Status == task.OperationPrepared {
		ready, reason, err := taskPlanStepDependenciesReadyInTx(ctx, tx, fence, planStepID)
		if err != nil {
			return err
		}
		if !ready {
			return taskPlanStepBindingConflict(reason)
		}
		return persistTaskPlanStepExecutionBindingInTx(ctx, tx, fence, planStepID, binding)
	}
	// Idempotent dispatch retries must find the exact binding committed with
	// the original dispatch. Never create a late binding for an already
	// dispatched operation.
	return requireExistingTaskPlanStepOperationBindingInTx(ctx, tx, fence, planStepID, binding)
}

// admitCloudDSHInternalFileEvidenceReadDispatchInTx admits the Runtime's
// read-only Workspace verification operation only when it names exact durable
// successful file_write sources. If a plan exists, every source must already
// be bound and settled under the current plan revision. This operation itself
// is not a model-selected plan step and receives no plan binding.
func admitCloudDSHInternalFileEvidenceReadDispatchInTx(ctx context.Context, tx pgx.Tx, current task.Task,
	operation task.Operation, sources []app.OperationEvidenceSource) error {
	if current.CurrentRevision == nil || *current.CurrentRevision < 0 || current.DesiredRevision == nil ||
		*current.DesiredRevision != *current.CurrentRevision {
		return taskPlanStepBindingConflict(planStepReadinessTaskRevisionStale)
	}
	if operation.Status != task.OperationPrepared && operation.Status != task.OperationDispatched {
		return domain.NewError(domain.CodeInvalidState, "internal file evidence operation is not prepared or replayable")
	}
	if !validInternalFileEvidenceOperationKey(operation.Key, operation.WorkspaceID, operation.BindingVersion) ||
		operation.HostID != current.HostID || current.WorkspaceID != operation.WorkspaceID ||
		operation.WorkspaceID == "" || operation.BindingVersion < 1 || current.InputRevision == nil ||
		current.WorkspaceCheckpointRevision == nil ||
		!sameTaskInputRevision(operation.InputRevision, current.InputRevision) ||
		!sameTaskInputRevision(operation.WorkspaceCheckpointRevision, current.WorkspaceCheckpointRevision) {
		return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
	}
	requestHash, err := app.FileEvidenceReadRequestHash(operation.Key, sources)
	if err != nil || !bytes.Equal(operation.RequestHash, requestHash[:]) {
		return idempotencyConflict()
	}
	plan, err := latestTaskPlan(ctx, tx, current.ID, *current.CurrentRevision)
	if err != nil {
		return err
	}
	seenSourceKeys := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if source.WorkspaceID != operation.WorkspaceID || source.BindingVersion != operation.BindingVersion {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if _, exists := seenSourceKeys[source.OperationKey]; exists {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		seenSourceKeys[source.OperationKey] = struct{}{}
		sourceOperation, err := loadOperationByKey(ctx, tx, current.ID, source.OperationKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if err != nil {
			return fmt.Errorf("read file evidence source operation: %w", err)
		}
		if sourceOperation.TaskID != current.ID || sourceOperation.HostID != current.HostID ||
			sourceOperation.TaskRevision == nil || *sourceOperation.TaskRevision != *current.CurrentRevision ||
			sourceOperation.CallID != source.CallID || sourceOperation.ToolName != "file_write" ||
			sourceOperation.WorkspaceID != source.WorkspaceID || sourceOperation.BindingVersion != source.BindingVersion ||
			!bytes.Equal(sourceOperation.RequestHash, source.RequestHash) || sourceOperation.Result != source.Result ||
			sourceOperation.Status != task.OperationSucceeded ||
			!sameTaskInputRevision(sourceOperation.InputRevision, current.InputRevision) ||
			!sameTaskInputRevision(sourceOperation.WorkspaceCheckpointRevision, current.WorkspaceCheckpointRevision) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if plan == nil {
			continue
		}
		var binding taskPlanStepExecutionBinding
		var planStepID string
		err = tx.QueryRow(ctx, `SELECT plan_step_id,agent_session_id,call_id,tool_name,request_digest,authorization_version
FROM task_plan_step_execution_bindings
WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4
 AND execution_kind=$5 AND execution_id=$6`, current.OwnerID, current.ID, *current.CurrentRevision, plan.Revision,
			planStepExecutionOperation, sourceOperation.ID).Scan(&planStepID, &binding.agentSessionID, &binding.callID,
			&binding.toolName, &binding.requestDigest, &binding.authorizationVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if err != nil {
			return fmt.Errorf("read current plan binding for file evidence source: %w", err)
		}
		binding.kind = planStepExecutionOperation
		binding.executionID = sourceOperation.ID
		if binding.agentSessionID != current.AgentSessionID || binding.callID != source.CallID ||
			binding.toolName != "file_write" || binding.authorizationVersion != current.Authorization.Version ||
			!bytes.Equal(binding.requestDigest, source.RequestHash) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		fence, blocked, err := taskPlanStepBindingFenceInTx(ctx, tx, current, plan.Revision, planStepID,
			binding.authorizationVersion, true)
		if err != nil {
			return err
		}
		if blocked != "" {
			return taskPlanStepBindingConflict(blocked)
		}
		if err := verifyTaskPlanStepExecutionSource(ctx, tx, fence, &binding); err != nil {
			return err
		}
		settlement, err := taskPlanStepExecutionSettlementReason(ctx, tx, fence, binding)
		if err != nil {
			return err
		}
		if settlement != "" {
			return taskPlanStepBindingConflict(settlement)
		}
	}
	return nil
}

func validInternalFileEvidenceOperationKey(operationKey, workspaceID string, bindingVersion int64) bool {
	if !strings.HasPrefix(operationKey, "agent:verify:v2:") || !domain.ValidWorkspaceID(workspaceID) || bindingVersion < 1 {
		return false
	}
	suffix := fmt.Sprintf(":%s:%d", workspaceID, bindingVersion)
	if !strings.HasSuffix(operationKey, suffix) {
		return false
	}
	stepID := strings.TrimSuffix(strings.TrimPrefix(operationKey, "agent:verify:v2:"), suffix)
	return domain.ValidStepID(stepID)
}

func requireExistingTaskPlanStepOperationBindingInTx(ctx context.Context, tx pgx.Tx, fence taskPlanStepBindingFence,
	planStepID string, binding taskPlanStepExecutionBinding) error {
	var stored taskPlanStepBindingRow
	err := tx.QueryRow(ctx, `SELECT plan_step_id,execution_kind,execution_id,agent_session_id,call_id,tool_name,
connector_id,child_step_id,parent_step_id,request_digest,authorization_version,result_digest
FROM task_plan_step_execution_bindings WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4
 AND execution_kind=$5 AND execution_id=$6`, fence.ownerID, fence.taskID, fence.taskRevision, fence.planRevision,
		binding.kind, binding.executionID).Scan(&stored.planStepID, &stored.binding.kind, &stored.binding.executionID,
		&stored.binding.agentSessionID, &stored.binding.callID, &stored.binding.toolName, &stored.binding.connectorID,
		&stored.binding.childStepID, &stored.binding.parentStepID, &stored.binding.requestDigest,
		&stored.binding.authorizationVersion, &stored.binding.resultDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
	}
	if err != nil {
		return fmt.Errorf("read existing task plan step operation binding: %w", err)
	}
	if stored.planStepID != planStepID || !sameTaskPlanStepExecutionBinding(stored.binding, binding) {
		return domain.NewError(domain.CodeIdempotencyConflict, "task plan step operation was bound differently")
	}
	return nil
}

func persistTaskPlanStepExecutionBindingInTx(ctx context.Context, tx pgx.Tx, fence taskPlanStepBindingFence,
	planStepID string, binding taskPlanStepExecutionBinding) error {
	_, err := tx.Exec(ctx, `INSERT INTO task_plan_step_execution_bindings
(owner_id,task_id,task_revision,plan_revision,plan_step_id,execution_kind,execution_id,agent_session_id,call_id,
 tool_name,connector_id,child_step_id,parent_step_id,request_digest,authorization_version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT DO NOTHING`,
		fence.ownerID, fence.taskID, fence.taskRevision, fence.planRevision, fence.planStepID,
		binding.kind, binding.executionID, binding.agentSessionID, binding.callID, binding.toolName,
		binding.connectorID, binding.childStepID, binding.parentStepID, binding.requestDigest, binding.authorizationVersion)
	if err != nil {
		return fmt.Errorf("persist task plan step execution binding: %w", err)
	}
	var stored taskPlanStepBindingRow
	err = tx.QueryRow(ctx, `SELECT plan_step_id,execution_kind,execution_id,agent_session_id,call_id,tool_name,
connector_id,child_step_id,parent_step_id,request_digest,authorization_version,result_digest
FROM task_plan_step_execution_bindings WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4
 AND execution_kind=$5 AND execution_id=$6`, fence.ownerID, fence.taskID, fence.taskRevision, fence.planRevision,
		binding.kind, binding.executionID).Scan(&stored.planStepID, &stored.binding.kind, &stored.binding.executionID,
		&stored.binding.agentSessionID, &stored.binding.callID, &stored.binding.toolName, &stored.binding.connectorID,
		&stored.binding.childStepID, &stored.binding.parentStepID, &stored.binding.requestDigest,
		&stored.binding.authorizationVersion, &stored.binding.resultDigest)
	if err != nil {
		return fmt.Errorf("read persisted task plan step execution binding: %w", err)
	}
	if stored.planStepID != planStepID || !sameTaskPlanStepExecutionBinding(stored.binding, binding) {
		return domain.NewError(domain.CodeIdempotencyConflict, "task plan step execution was bound differently")
	}
	return nil
}

func taskPlanStepBindingConflict(reason string) error {
	err := domain.NewError(domain.CodeConflict, "task plan step execution evidence is stale or incomplete")
	err.Details["reason"] = reason
	return err
}

func taskPlanStepBindingFenceInTx(ctx context.Context, tx pgx.Tx, current task.Task, planRevision int64,
	planStepID string, authorizationVersion int64, forUpdate bool) (taskPlanStepBindingFence, string, error) {
	if !validPlanStepIdentity(current, planRevision, planStepID) || authorizationVersion < 1 {
		return taskPlanStepBindingFence{}, "", domain.InvalidArgument("task_plan_step", "task plan step identity is invalid")
	}
	lock := "FOR SHARE"
	if forUpdate {
		lock = "FOR UPDATE"
	}
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 `+lock, current.ID, current.OwnerID).Scan(&lockedID); errors.Is(err, pgx.ErrNoRows) {
		return taskPlanStepBindingFence{}, "", missingTaskError()
	} else if err != nil {
		return taskPlanStepBindingFence{}, "", fmt.Errorf("lock task for plan step binding: %w", err)
	}
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: current.ID, OwnerID: current.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return taskPlanStepBindingFence{}, "", missingTaskError()
	}
	if err != nil {
		return taskPlanStepBindingFence{}, "", fmt.Errorf("load task for plan step binding: %w", err)
	}
	if row.CurrentRevision != *current.CurrentRevision || row.DesiredRevision != *current.DesiredRevision ||
		row.CurrentRevision != row.DesiredRevision {
		return taskPlanStepBindingFence{}, planStepReadinessTaskRevisionStale, nil
	}
	if row.Status != string(task.StatusRunning) || current.Status != task.StatusRunning {
		return taskPlanStepBindingFence{}, planStepReadinessTaskNotRunning, nil
	}
	if row.AuthVersion != authorizationVersion || current.Authorization.Version != authorizationVersion {
		return taskPlanStepBindingFence{}, planStepReadinessAuthorization, nil
	}
	if err := requireCurrentLease(ctx, tx, row, current.Lease.Owner, current.Lease.Version); err != nil {
		return taskPlanStepBindingFence{}, planStepReadinessTaskNotRunning, nil
	}
	authorized, err := requireCurrentTaskAuthorization(ctx, tx, row)
	if err != nil {
		return taskPlanStepBindingFence{}, "", err
	}
	if !authorized {
		return taskPlanStepBindingFence{}, planStepReadinessAuthorization, nil
	}
	plan, planTaskStepSequence, err := currentCloudDSHTaskPlanForStep(ctx, tx, current.ID, current.OwnerID,
		row.CurrentRevision, planRevision, current.AgentSessionID, authorizationVersion)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return taskPlanStepBindingFence{}, planStepReadinessPlanMissing, nil
	}
	if domain.CodeOf(err) == domain.CodeConflict {
		return taskPlanStepBindingFence{}, planStepReadinessPlanRevisionStale, nil
	}
	if err != nil {
		return taskPlanStepBindingFence{}, "", err
	}
	if !planHasStep(plan, planStepID) {
		return taskPlanStepBindingFence{}, planStepReadinessStepMissing, nil
	}
	return taskPlanStepBindingFence{ownerID: current.OwnerID, taskID: current.ID,
		taskRevision: row.CurrentRevision, planRevision: planRevision, authorizationVersion: authorizationVersion,
		planStepID: planStepID, plan: plan, planTaskStepSequence: planTaskStepSequence}, "", nil
}

func currentCloudDSHTaskPlanForStep(ctx context.Context, tx pgx.Tx, taskID, ownerID string, taskRevision, planRevision int64,
	agentSessionID string, authorizationVersion int64) (task.Plan, int64, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.plan'
 AND payload #>> '{plan,task_revision}'=$2 ORDER BY sequence DESC LIMIT 1`, taskID, fmt.Sprint(taskRevision)).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return task.Plan{}, 0, domain.NewError(domain.CodeNotFound, "task plan not found")
	}
	if err != nil {
		return task.Plan{}, 0, fmt.Errorf("read current task plan for step binding: %w", err)
	}
	var event appTaskPlanEvent
	if len(payload) > 64<<10 || json.Unmarshal(payload, &event) != nil || event.Plan.Validate() != nil ||
		event.StepID == "" || event.Sequence < 1 || event.TaskRevision != taskRevision ||
		event.Plan.TaskRevision != taskRevision || event.PlanRevision != event.Plan.Revision {
		return task.Plan{}, 0, domain.NewError(domain.CodeInvalidState, "stored task plan is invalid")
	}
	if event.PlanRevision != planRevision {
		return task.Plan{}, 0, domain.NewError(domain.CodeConflict, "task plan revision is stale")
	}
	commit := app.CloudDSHTaskPlanCommit{TaskID: taskID, OwnerID: ownerID, AgentSessionID: event.Detail.AgentSessionID,
		AuthorizationVersion: event.Detail.AuthorizationVersion, CallID: event.Detail.CallID, Plan: event.Plan,
		FingerprintSHA256: event.Detail.FingerprintSHA256}
	if event.Detail.Tool != "morde_task_plan_update" || event.Detail.AgentSessionID != agentSessionID ||
		event.Detail.AuthorizationVersion != authorizationVersion ||
		app.ValidateCloudDSHTaskPlanEventForStep(payload, commit, event.StepID, event.Sequence) != nil {
		return task.Plan{}, 0, domain.NewError(domain.CodeInvalidState, "stored task plan event identity is invalid")
	}
	var sequence, stepTaskRevision int64
	var stepAgentSessionID, stepCallID, stepToolName string
	var stepRequestDigest []byte
	if err := tx.QueryRow(ctx, `SELECT sequence,COALESCE(agent_session_id,''),COALESCE(task_revision,-1),
COALESCE(tool_call_key,''),COALESCE(tool_name,''),COALESCE(request_digest,''::bytea)
FROM task_steps WHERE task_id=$1 AND id=$2`, taskID, event.StepID).Scan(&sequence, &stepAgentSessionID,
		&stepTaskRevision, &stepCallID, &stepToolName, &stepRequestDigest); errors.Is(err, pgx.ErrNoRows) {
		return task.Plan{}, 0, domain.NewError(domain.CodeInvalidState, "stored task plan step event is missing")
	} else if err != nil {
		return task.Plan{}, 0, fmt.Errorf("read task plan event step sequence: %w", err)
	}
	if sequence != event.Sequence || stepAgentSessionID != event.Detail.AgentSessionID || stepTaskRevision != taskRevision ||
		stepCallID != event.Detail.CallID || stepToolName != event.Detail.Tool || len(stepRequestDigest) != 32 {
		return task.Plan{}, 0, domain.NewError(domain.CodeInvalidState, "stored task plan event step identity changed")
	}
	return event.Plan, sequence, nil
}

func planHasStep(plan task.Plan, stepID string) bool {
	for _, step := range plan.Steps {
		if step.ID == stepID {
			return true
		}
	}
	return false
}

func planStepDependencies(plan task.Plan, stepID string) []string {
	for _, step := range plan.Steps {
		if step.ID == stepID {
			return step.DependsOn
		}
	}
	return nil
}

func verifyTaskPlanStepExecutionSource(ctx context.Context, tx pgx.Tx, fence taskPlanStepBindingFence,
	binding *taskPlanStepExecutionBinding) error {
	switch binding.kind {
	case planStepExecutionOperation:
		var status, callID, toolName, agentSessionID, stepCallID, stepToolName, stepStatus string
		var taskRevision, stepTaskRevision, stepSequence int64
		var requestDigest, operationRequestDigest []byte
		err := tx.QueryRow(ctx, `SELECT op.status,COALESCE(op.task_revision,-1),COALESCE(op.call_id,''),
COALESCE(op.tool_name,''),COALESCE(step.agent_session_id,''),COALESCE(step.task_revision,-1),
COALESCE(step.tool_call_key,''),COALESCE(step.tool_name,''),COALESCE(step.request_digest,''::bytea),op.request_hash,step.sequence,step.status
FROM task_operations op JOIN tasks task ON task.id=op.task_id AND task.owner_id=$2
JOIN task_steps step ON step.task_id=op.task_id AND step.agent_session_id=$3
 AND step.task_revision=op.task_revision AND step.tool_call_key=op.call_id AND step.tool_name=op.tool_name
WHERE op.id=$1 AND op.task_id=$4
FOR SHARE OF op,step`, binding.executionID, fence.ownerID, binding.agentSessionID, fence.taskID).
			Scan(&status, &taskRevision, &callID, &toolName, &agentSessionID, &stepTaskRevision, &stepCallID,
				&stepToolName, &requestDigest, &operationRequestDigest, &stepSequence, &stepStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if err != nil {
			return fmt.Errorf("verify task plan operation identity: %w", err)
		}
		if taskRevision != fence.taskRevision || stepTaskRevision != fence.taskRevision || callID != binding.callID ||
			toolName != binding.toolName || agentSessionID != binding.agentSessionID || stepCallID != binding.callID ||
			stepToolName != binding.toolName || !bytes.Equal(requestDigest, binding.requestDigest) ||
			!bytes.Equal(operationRequestDigest, binding.requestDigest) ||
			stepSequence <= fence.planTaskStepSequence || !validTaskPlanOperationStatus(status) || !validTaskPlanStepStatus(stepStatus) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
	case planStepExecutionMCP:
		var status, callID, toolName, connectorID, agentSessionID, stepCallID, stepToolName, stepStatus string
		var taskRevision, stepTaskRevision, stepSequence int64
		var requestDigest, operationRequestDigest []byte
		err := tx.QueryRow(ctx, `SELECT op.status,COALESCE(op.task_revision,-1),COALESCE(op.call_id,''),op.tool_name,
	COALESCE(op.connector_id,'builtin:' || op.builtin_service),COALESCE(step.agent_session_id,''),COALESCE(step.task_revision,-1),COALESCE(step.tool_call_key,''),
COALESCE(step.tool_name,''),COALESCE(step.request_digest,''::bytea),op.request_hash,step.sequence,step.status
FROM task_mcp_connector_operations op JOIN tasks task ON task.id=op.task_id AND task.owner_id=op.owner_id
JOIN task_steps step ON step.task_id=op.task_id AND step.agent_session_id=$3
 AND step.task_revision=op.task_revision AND step.tool_call_key=op.call_id AND step.tool_name=op.tool_name
WHERE op.id=$1 AND op.task_id=$4 AND op.owner_id=$2
FOR SHARE OF op,step`, binding.executionID, fence.ownerID, binding.agentSessionID, fence.taskID).
			Scan(&status, &taskRevision, &callID, &toolName, &connectorID, &agentSessionID, &stepTaskRevision,
				&stepCallID, &stepToolName, &requestDigest, &operationRequestDigest, &stepSequence, &stepStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if err != nil {
			return fmt.Errorf("verify task plan MCP operation identity: %w", err)
		}
		if taskRevision != fence.taskRevision || stepTaskRevision != fence.taskRevision || callID != binding.callID ||
			toolName != binding.toolName || connectorID != binding.connectorID || agentSessionID != binding.agentSessionID ||
			stepCallID != binding.callID || stepToolName != binding.toolName || !bytes.Equal(requestDigest, binding.requestDigest) ||
			!bytes.Equal(operationRequestDigest, binding.requestDigest) ||
			stepSequence <= fence.planTaskStepSequence || !validTaskPlanOperationStatus(status) || !validTaskPlanStepStatus(stepStatus) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
	case planStepExecutionChild:
		var taskRevision, stepSequence int64
		var parentSessionID, spawnKey, childStepID, parentStepID, stepStatus string
		var requestDigest []byte
		err := tx.QueryRow(ctx, `SELECT child.task_revision,child.parent_session_id,child.spawn_key,child.step_id,
COALESCE(parent.step_id,''),child.request_hash,step.sequence,step.status
FROM task_agent_sessions child JOIN task_agent_sessions parent ON parent.task_id=child.task_id
 AND parent.owner_id=child.owner_id AND parent.session_id=child.parent_session_id
JOIN task_steps step ON step.task_id=child.task_id AND step.id=child.step_id
WHERE child.task_id=$1 AND child.owner_id=$2 AND child.session_id=$3 AND child.parent_session_id=$4
 AND child.spawn_key=$5
FOR SHARE OF child,parent,step`, fence.taskID, fence.ownerID, binding.executionID, binding.agentSessionID, binding.callID).
			Scan(&taskRevision, &parentSessionID, &spawnKey, &childStepID, &parentStepID, &requestDigest,
				&stepSequence, &stepStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if err != nil {
			return fmt.Errorf("verify task plan child identity: %w", err)
		}
		if taskRevision != fence.taskRevision || parentSessionID != binding.agentSessionID || spawnKey != binding.callID ||
			childStepID != binding.childStepID || len(requestDigest) != 32 ||
			stepSequence <= fence.planTaskStepSequence || !validTaskPlanStepStatus(stepStatus) {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		binding.parentStepID = parentStepID
		binding.requestDigest = requestDigest
	case planStepExecutionReadTool:
		if binding.executionID != binding.callID || binding.connectorID != "" || binding.childStepID != "" ||
			binding.parentStepID != "" || binding.toolName == "" || len(binding.requestDigest) != 32 ||
			len(binding.resultDigest) != 32 {
			return taskPlanStepBindingConflict(planStepReadinessEvidenceMissing)
		}
		if err := verifyTaskPlanStepReadSource(ctx, tx, fence, *binding, true); err != nil {
			return err
		}
	default:
		return domain.InvalidArgument("task_plan_step.binding", "task plan execution kind is invalid")
	}
	return nil
}

func validTaskPlanOperationStatus(status string) bool {
	switch status {
	case string(task.OperationPrepared), string(task.OperationDispatched), string(task.OperationSucceeded),
		string(task.OperationFailed), string(task.OperationCancelled), string(task.OperationUnknown):
		return true
	default:
		return false
	}
}

func validTaskPlanStepStatus(status string) bool {
	switch status {
	case string(task.StepQueued), string(task.StepRunning), string(task.StepWaitingUser), string(task.StepSucceeded),
		string(task.StepFailed), string(task.StepUnknown), string(task.StepCancelled):
		return true
	default:
		return false
	}
}

func sameTaskPlanStepExecutionBinding(a, b taskPlanStepExecutionBinding) bool {
	return a.kind == b.kind && a.executionID == b.executionID && a.agentSessionID == b.agentSessionID &&
		a.callID == b.callID && a.toolName == b.toolName && a.connectorID == b.connectorID &&
		a.childStepID == b.childStepID && a.parentStepID == b.parentStepID &&
		bytes.Equal(a.requestDigest, b.requestDigest) && bytes.Equal(a.resultDigest, b.resultDigest) &&
		a.authorizationVersion == b.authorizationVersion
}

func loadTaskPlanStepBindings(ctx context.Context, tx pgx.Tx, fence taskPlanStepBindingFence,
	planStepID string) ([]taskPlanStepExecutionBinding, error) {
	rows, err := tx.Query(ctx, `SELECT execution_kind,execution_id,agent_session_id,call_id,tool_name,connector_id,
child_step_id,parent_step_id,request_digest,authorization_version,result_digest
FROM task_plan_step_execution_bindings WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4
 AND plan_step_id=$5 ORDER BY execution_kind,execution_id`, fence.ownerID, fence.taskID, fence.taskRevision,
		fence.planRevision, planStepID)
	if err != nil {
		return nil, fmt.Errorf("load task plan step execution bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]taskPlanStepExecutionBinding, 0)
	for rows.Next() {
		var value taskPlanStepExecutionBinding
		if err := rows.Scan(&value.kind, &value.executionID, &value.agentSessionID, &value.callID, &value.toolName,
			&value.connectorID, &value.childStepID, &value.parentStepID, &value.requestDigest,
			&value.authorizationVersion, &value.resultDigest); err != nil {
			return nil, fmt.Errorf("scan task plan step execution binding: %w", err)
		}
		bindings = append(bindings, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task plan step execution bindings: %w", err)
	}
	return bindings, nil
}

func taskPlanStepExecutionSettlementReason(ctx context.Context, tx pgx.Tx, fence taskPlanStepBindingFence,
	binding taskPlanStepExecutionBinding) (string, error) {
	switch binding.kind {
	case planStepExecutionOperation:
		var operationStatus, stepStatus, callID, toolName, agentSessionID, stepCallID, stepToolName string
		var taskRevision, stepTaskRevision, stepSequence int64
		var requestDigest, operationRequestDigest []byte
		err := tx.QueryRow(ctx, `SELECT op.status,COALESCE(op.task_revision,-1),COALESCE(op.call_id,''),
COALESCE(op.tool_name,''),COALESCE(step.agent_session_id,''),COALESCE(step.task_revision,-1),
COALESCE(step.tool_call_key,''),COALESCE(step.tool_name,''),COALESCE(step.request_digest,''::bytea),op.request_hash,step.sequence,step.status
FROM task_operations op JOIN tasks task ON task.id=op.task_id AND task.owner_id=$2
JOIN task_steps step ON step.task_id=op.task_id AND step.agent_session_id=$3
 AND step.task_revision=op.task_revision AND step.tool_call_key=op.call_id AND step.tool_name=op.tool_name
WHERE op.id=$1 AND op.task_id=$4`, binding.executionID, fence.ownerID, binding.agentSessionID,
			fence.taskID).Scan(&operationStatus, &taskRevision, &callID, &toolName, &agentSessionID,
			&stepTaskRevision, &stepCallID, &stepToolName, &requestDigest, &operationRequestDigest, &stepSequence, &stepStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return planStepReadinessEvidenceMissing, nil
		}
		if err != nil {
			return "", fmt.Errorf("read task plan operation settlement: %w", err)
		}
		if taskRevision != fence.taskRevision || stepTaskRevision != fence.taskRevision || callID != binding.callID ||
			toolName != binding.toolName || agentSessionID != binding.agentSessionID || stepCallID != binding.callID ||
			stepToolName != binding.toolName || !bytes.Equal(requestDigest, binding.requestDigest) ||
			!bytes.Equal(operationRequestDigest, binding.requestDigest) ||
			stepSequence <= fence.planTaskStepSequence || !validTaskPlanOperationStatus(operationStatus) || !validTaskPlanStepStatus(stepStatus) {
			return planStepReadinessEvidenceMissing, nil
		}
		return taskPlanOperationSettlementReason(operationStatus, stepStatus), nil
	case planStepExecutionMCP:
		var operationStatus, callID, toolName, connectorID, agentSessionID, stepCallID, stepToolName, stepStatus string
		var taskRevision, stepTaskRevision, stepSequence int64
		var requestDigest, operationRequestDigest []byte
		err := tx.QueryRow(ctx, `SELECT op.status,COALESCE(op.task_revision,-1),COALESCE(op.call_id,''),op.tool_name,
	COALESCE(op.connector_id,'builtin:' || op.builtin_service),COALESCE(step.agent_session_id,''),COALESCE(step.task_revision,-1),COALESCE(step.tool_call_key,''),
COALESCE(step.tool_name,''),COALESCE(step.request_digest,''::bytea),op.request_hash,step.sequence,step.status
FROM task_mcp_connector_operations op JOIN tasks task ON task.id=op.task_id AND task.owner_id=op.owner_id
JOIN task_steps step ON step.task_id=op.task_id AND step.agent_session_id=$3
 AND step.task_revision=op.task_revision AND step.tool_call_key=op.call_id AND step.tool_name=op.tool_name
WHERE op.id=$1 AND op.task_id=$4 AND op.owner_id=$2`, binding.executionID, fence.ownerID,
			binding.agentSessionID, fence.taskID).Scan(&operationStatus, &taskRevision, &callID, &toolName,
			&connectorID, &agentSessionID, &stepTaskRevision, &stepCallID, &stepToolName, &requestDigest,
			&operationRequestDigest, &stepSequence, &stepStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return planStepReadinessEvidenceMissing, nil
		}
		if err != nil {
			return "", fmt.Errorf("read task plan MCP settlement: %w", err)
		}
		if taskRevision != fence.taskRevision || stepTaskRevision != fence.taskRevision || callID != binding.callID ||
			toolName != binding.toolName || connectorID != binding.connectorID || agentSessionID != binding.agentSessionID ||
			stepCallID != binding.callID || stepToolName != binding.toolName || !bytes.Equal(requestDigest, binding.requestDigest) ||
			!bytes.Equal(operationRequestDigest, binding.requestDigest) ||
			stepSequence <= fence.planTaskStepSequence || !validTaskPlanOperationStatus(operationStatus) || !validTaskPlanStepStatus(stepStatus) {
			return planStepReadinessEvidenceMissing, nil
		}
		return taskPlanOperationSettlementReason(operationStatus, stepStatus), nil
	case planStepExecutionChild:
		var taskRevision, stepSequence int64
		var parentSessionID, spawnKey, childStepID, parentStepID, stepStatus string
		var requestDigest []byte
		var cancellationPending bool
		err := tx.QueryRow(ctx, `SELECT child.task_revision,child.parent_session_id,child.spawn_key,child.step_id,
COALESCE(parent.step_id,''),child.request_hash,step.sequence,step.status,child.cancel_requested_at IS NOT NULL
FROM task_agent_sessions child JOIN tasks task ON task.id=child.task_id AND task.owner_id=child.owner_id
JOIN task_agent_sessions parent ON parent.task_id=child.task_id AND parent.owner_id=child.owner_id
 AND parent.session_id=child.parent_session_id
JOIN task_steps step ON step.task_id=child.task_id AND step.id=child.step_id
WHERE child.task_id=$1 AND child.owner_id=$2 AND child.session_id=$3 AND child.parent_session_id=$4
 AND child.spawn_key=$5`, fence.taskID, fence.ownerID, binding.executionID, binding.agentSessionID,
			binding.callID).Scan(&taskRevision, &parentSessionID, &spawnKey, &childStepID, &parentStepID,
			&requestDigest, &stepSequence, &stepStatus, &cancellationPending)
		if errors.Is(err, pgx.ErrNoRows) {
			return planStepReadinessEvidenceMissing, nil
		}
		if err != nil {
			return "", fmt.Errorf("read task plan child settlement: %w", err)
		}
		if taskRevision != fence.taskRevision || parentSessionID != binding.agentSessionID || spawnKey != binding.callID ||
			childStepID != binding.childStepID || parentStepID != binding.parentStepID ||
			!bytes.Equal(requestDigest, binding.requestDigest) || stepSequence <= fence.planTaskStepSequence ||
			!validTaskPlanStepStatus(stepStatus) {
			return planStepReadinessEvidenceMissing, nil
		}
		if cancellationPending {
			return planStepReadinessCancelled, nil
		}
		return taskPlanChildSettlementReason(stepStatus), nil
	default:
		return planStepReadinessEvidenceMissing, nil
	}
}

func taskPlanOperationSettlementReason(operationStatus, stepStatus string) string {
	if operationStatus == string(task.OperationFailed) || stepStatus == string(task.StepFailed) {
		return planStepReadinessFailed
	}
	if operationStatus == string(task.OperationCancelled) || stepStatus == string(task.StepCancelled) {
		return planStepReadinessCancelled
	}
	if operationStatus == string(task.OperationUnknown) || stepStatus == string(task.StepUnknown) {
		return planStepReadinessUnknown
	}
	if operationStatus == string(task.OperationSucceeded) && stepStatus == string(task.StepSucceeded) {
		return ""
	}
	return planStepReadinessPending
}

func taskPlanChildSettlementReason(stepStatus string) string {
	switch stepStatus {
	case string(task.StepSucceeded):
		return ""
	case string(task.StepFailed):
		return planStepReadinessFailed
	case string(task.StepCancelled):
		return planStepReadinessCancelled
	case string(task.StepUnknown):
		return planStepReadinessUnknown
	default:
		return planStepReadinessPending
	}
}
