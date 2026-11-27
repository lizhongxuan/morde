package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

var modelBudgetCapacityCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

const taskModelBudgetCapacityInvalidatedReason = "model_budget_capacity_invalidated"

type modelBudgetCapacityWaitRow struct {
	OwnerID               string
	TaskID                string
	TaskOwnerID           string
	Fence                 app.ModelTaskFence
	ModelID               string
	ProjectID             string
	DSHCallID             string
	LedgerCallID          string
	BudgetCallID          string
	RequestDigest         []byte
	CheckpointDigest      []byte
	InputTokenReservation int64
	MinimumOutputTokens   int64
	MinimumProjectMicros  int64
	ProjectBudgetCurrency string
	CreatedAt             time.Time
	ExpiresAt             time.Time
}

type modelBudgetCapacityCallDeadlineRow struct {
	OwnerID, TaskID, TaskOwnerID                                     string
	TaskRevision, AuthorizationVersion                               int64
	LeaseOwner, AgentInstanceID, AgentSessionID, AgentRootSessionID  string
	LeaseVersion, AgentGeneration, ThroughSequence                   int64
	EngineVersion, ProfileVersion, RecoveryCallID, RecoveryDSHCallID string
	ModelID, ProjectID, DSHCallID, LedgerCallID, BudgetCallID        string
	RequestDigest, CheckpointDigest                                  []byte
	FirstWaitAt, DeadlineAt                                          time.Time
}

type modelBudgetCapacityCallDeadline struct {
	FirstWaitAt time.Time
	DeadlineAt  time.Time
}

func validTaskModelBudgetCapacityWaitRequest(request app.TaskModelBudgetCapacityWaitRequest) bool {
	fence := request.Fence
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) ||
		fence.TaskRevision < 0 || fence.LeaseOwner == "" || len(fence.LeaseOwner) > 128 || fence.LeaseVersion < 1 ||
		!domain.ValidModelConnectionID(request.ModelID) || !validModelBudgetPreparationID(request.BudgetCallID) ||
		!app.ValidModelBudgetRequestDigest(request.RequestDigest) || request.InputTokenReservation < 0 || request.InputTokenReservation > 2_000_000 ||
		request.MinimumOutputTokens < 1 || request.MinimumOutputTokens > 2_000_000 ||
		request.InputTokenReservation > math.MaxInt64-request.MinimumOutputTokens ||
		request.MinimumProjectMicros < 0 || request.ProjectID != "" && !project.ValidID(request.ProjectID) ||
		request.MinimumProjectMicros > 0 && (!modelBudgetCapacityCurrencyPattern.MatchString(request.ProjectBudgetCurrency) || request.ProjectID == "") ||
		request.MinimumProjectMicros == 0 && (request.ProjectBudgetCurrency != "" || request.ProjectID != "") ||
		request.ProjectID == "" && request.MinimumProjectMicros != 0 {
		return false
	}
	if !validModelBudgetPreparationID(request.DSHCallID) || !validModelBudgetPreparationID(request.LedgerCallID) ||
		!validModelBudgetCapacityFence(fence) {
		return false
	}
	failedCallID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		return false
	}
	if fence.RecoveryCallID == "" && fence.RecoveryDSHCallID == "" {
		return request.DSHCallID == failedCallID && request.LedgerCallID == failedCallID
	}
	expectedDSHCallID, dshErr := app.TaskModelRecoveryDSHCallID(fence.TaskID, fence.TaskRevision, failedCallID, fence.ThroughSequence)
	expectedLedgerCallID, ledgerErr := app.TaskModelRecoveryCallID(fence.TaskID, fence.TaskRevision, failedCallID, fence.ThroughSequence)
	return dshErr == nil && ledgerErr == nil && fence.RecoveryDSHCallID == expectedDSHCallID &&
		fence.RecoveryCallID == expectedLedgerCallID && request.DSHCallID == expectedDSHCallID &&
		request.LedgerCallID == expectedLedgerCallID && request.DSHCallID != request.LedgerCallID
}

func validModelBudgetCapacityFence(fence app.ModelTaskFence) bool {
	return fence.LeaseOwner != "" && fence.LeaseVersion > 0 && fence.AgentInstanceID != "" && len(fence.AgentInstanceID) <= 128 &&
		fence.AgentGeneration > 0 && fence.AgentSessionID != "" && len(fence.AgentSessionID) <= 128 &&
		fence.AgentRootSessionID != "" && len(fence.AgentRootSessionID) <= 128 && fence.AuthorizationVersion > 0 &&
		fence.ThroughSequence >= 0 && fence.EngineVersion != "" && len(fence.EngineVersion) <= 128 &&
		fence.ProfileVersion != "" && len(fence.ProfileVersion) <= 128
}

func (s *Store) WaitForTaskModelBudgetCapacity(ctx context.Context, request app.TaskModelBudgetCapacityWaitRequest) error {
	if ctx == nil || !validTaskModelBudgetCapacityWaitRequest(request) {
		return domain.InvalidArgument("model_budget_capacity_wait", "model budget capacity wait identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		fence := request.Fence
		if err := lockModelLedgerOwner(ctx, tx, fence.InitiatorID); err != nil {
			return err
		}
		if _, _, err := lockPlatformModelBudget(ctx, tx, fence.InitiatorID); err != nil {
			return err
		}
		var sharedModel bool
		var providerModel string
		var connectionVersion time.Time
		if err := tx.QueryRow(ctx, `SELECT owner_id IS NULL,model_name,updated_at FROM model_connections WHERE id=$1`, request.ModelID).
			Scan(&sharedModel, &providerModel, &connectionVersion); err != nil || !sharedModel {
			return domain.NewError(domain.CodeConflict, "capacity wait requires the same shared platform model route")
		}

		if _, found, err := readModelBudgetCapacityWait(ctx, tx, `owner_id=$1 AND task_id=$2 AND ledger_call_id=$3`,
			fence.InitiatorID, fence.TaskID, request.LedgerCallID); err != nil {
			return err
		} else if found {
			var status, reason, leaseOwner string
			var leaseExpired bool
			var revision, desiredRevision, authorizationVersion, leaseVersion, generation int64
			var agentID, agentSession, engineVersion, profileVersion, currentProjectID string
			if err := tx.QueryRow(ctx, `SELECT status,waiting_reason,lease_owner,lease_expires_at IS NULL,current_revision,desired_revision,
auth_version,lease_version,agent_instance_id,agent_generation,agent_session_id,agent_engine_version,agent_profile_version,COALESCE(project_id,'')
FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 FOR UPDATE`, fence.TaskID, fence.OwnerID, fence.InitiatorID).Scan(&status, &reason, &leaseOwner, &leaseExpired,
				&revision, &desiredRevision, &authorizationVersion, &leaseVersion, &agentID, &generation, &agentSession, &engineVersion, &profileVersion,
				&currentProjectID); err != nil ||
				status != string(task.StatusWaitingUser) || reason != app.TaskModelBudgetCapacityWaitReason || leaseOwner != "" || !leaseExpired {
				return domain.NewError(domain.CodeConflict, "model budget capacity wait Task state changed")
			}
			if revision != fence.TaskRevision || desiredRevision != fence.TaskRevision || authorizationVersion != fence.AuthorizationVersion ||
				leaseVersion != fence.LeaseVersion || agentID != fence.AgentInstanceID || generation != fence.AgentGeneration ||
				agentSession != fence.AgentRootSessionID || engineVersion != fence.EngineVersion || profileVersion != fence.ProfileVersion {
				return domain.NewError(domain.CodeConflict, "model budget capacity wait Task fence changed")
			}
			if currentProjectID != request.ProjectID {
				return domain.NewError(domain.CodeConflict, "model budget capacity wait Task project changed")
			}
			lockedExisting, found, err := readModelBudgetCapacityWait(ctx, tx,
				`owner_id=$1 AND task_id=$2 AND ledger_call_id=$3 FOR UPDATE`, fence.InitiatorID, fence.TaskID, request.LedgerCallID)
			if err != nil {
				return err
			}
			if !found || !sameModelBudgetCapacityWaitRequest(lockedExisting, request) {
				return domain.NewError(domain.CodeIdempotencyConflict, "Task already has a different model budget capacity wait")
			}
			_, created, err := ensureModelBudgetCapacityCallDeadline(ctx, tx, request,
				lockedExisting.CheckpointDigest, false)
			if err != nil {
				return err
			}
			if created {
				return domain.NewError(domain.CodeIdempotencyConflict, "model budget capacity deadline identity changed")
			}
			return nil
		} else if err != nil {
			return err
		}

		call := app.ModelCallReservation{Source: app.ModelCallTask, Task: &fence, OwnerID: fence.InitiatorID,
			ProjectID: request.ProjectID, ModelID: request.ModelID, CallID: request.LedgerCallID,
			DSHCallID: request.DSHCallID, BudgetCallID: request.BudgetCallID, ProviderModel: providerModel,
			ConnectionVersion: connectionVersion, PersonalModel: false}
		if err := checkModelCaller(ctx, tx, call); err != nil {
			return err
		}
		if err := verifyTaskModelRecoveryPreparationForReservation(ctx, tx, call); err != nil {
			return err
		}
		var currentProjectID string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM tasks WHERE id=$1 AND owner_id=$2`, fence.TaskID, fence.OwnerID).
			Scan(&currentProjectID); err != nil || currentProjectID != request.ProjectID {
			return domain.NewError(domain.CodeConflict, "Task project scope changed before capacity wait")
		}
		var checkpointDigest []byte
		if err := tx.QueryRow(ctx, `SELECT checkpoint_digest FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fence.TaskID, fence.OwnerID, fence.AgentSessionID).Scan(&checkpointDigest); err != nil ||
			len(checkpointDigest) != 32 {
			return domain.NewError(domain.CodeConflict, "Task Agent checkpoint changed before capacity wait")
		}
		var existingCall bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2
  UNION ALL SELECT 1 FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND (call_id=$2 OR dsh_call_id=$3 OR budget_call_id=$5)
  UNION ALL SELECT 1 FROM task_model_call_responses WHERE owner_id=$1 AND source='task' AND call_id=$2
  UNION ALL SELECT 1 FROM project_budget_ledger WHERE project_id=NULLIF($4::text,'') AND source='task' AND call_id=$2
)`, fence.InitiatorID, request.LedgerCallID, request.DSHCallID, request.ProjectID, request.BudgetCallID).Scan(&existingCall); err != nil {
			return fmt.Errorf("check capacity wait call dispatch identity: %w", err)
		}
		if existingCall {
			return domain.NewError(domain.CodeConflict, "model call identity already has a reservation or result")
		}
		deadline, createdDeadline, err := ensureModelBudgetCapacityCallDeadline(ctx, tx, request, checkpointDigest, true)
		if err != nil {
			return err
		}
		var recoveryCallID, recoveryDSHCallID string
		if fence.RecoveryCallID != "" {
			recoveryCallID, recoveryDSHCallID = fence.RecoveryCallID, fence.RecoveryDSHCallID
		}
		if !task.CanTransition(task.StatusRunning, task.StatusWaitingUser) {
			return domain.NewError(domain.CodeInvalidState, "model budget capacity Task transition is not permitted")
		}
		var eventSequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='waiting_user',waiting_reason=$7,lease_owner='',lease_expires_at=NULL,
finished_at=NULL,event_seq=event_seq+1,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND status='running' AND current_revision=$4 AND desired_revision=$4
 AND auth_version=$5 AND lease_owner=$6 AND lease_version=$8 AND agent_instance_id=$9 AND agent_generation=$10
 AND agent_session_id=$11 AND agent_engine_version=$12 AND agent_profile_version=$13
 AND project_id IS NOT DISTINCT FROM NULLIF($14::text,'') AND lease_expires_at>clock_timestamp()
RETURNING event_seq`, fence.TaskID, fence.OwnerID, fence.InitiatorID, fence.TaskRevision, fence.AuthorizationVersion,
			fence.LeaseOwner, app.TaskModelBudgetCapacityWaitReason, fence.LeaseVersion, fence.AgentInstanceID,
			fence.AgentGeneration, fence.AgentRootSessionID, fence.EngineVersion, fence.ProfileVersion, request.ProjectID).Scan(&eventSequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "Task lease changed before model budget capacity wait")
		}
		if err != nil {
			return fmt.Errorf("move Task to model budget capacity wait: %w", err)
		}
		var projectID any
		if request.ProjectID != "" {
			projectID = request.ProjectID
		}
		_, err = tx.Exec(ctx, `INSERT INTO model_budget_capacity_waits
(owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,agent_instance_id,agent_generation,
agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,recovery_call_id,recovery_dsh_call_id,
model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,checkpoint_digest,input_token_reservation,
minimum_output_tokens,minimum_project_micros,project_budget_currency,created_at,last_reconciled_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,decode($22,'hex'),$23,$24,$25,$26,$27,
$28,CASE WHEN $29 THEN NULL ELSE clock_timestamp() END,$30)`, fence.InitiatorID, fence.TaskID, fence.OwnerID, fence.TaskRevision,
			fence.AuthorizationVersion, fence.LeaseOwner, fence.LeaseVersion, fence.AgentInstanceID, fence.AgentGeneration,
			fence.AgentSessionID, fence.AgentRootSessionID, fence.EngineVersion, fence.ProfileVersion, fence.ThroughSequence,
			recoveryCallID, recoveryDSHCallID, request.ModelID, projectID, request.DSHCallID, request.LedgerCallID,
			request.BudgetCallID, request.RequestDigest, checkpointDigest, request.InputTokenReservation,
			request.MinimumOutputTokens, request.MinimumProjectMicros, request.ProjectBudgetCurrency, deadline.FirstWaitAt,
			createdDeadline, deadline.DeadlineAt)
		if err != nil {
			return fmt.Errorf("persist Task model budget capacity wait: %w", err)
		}
		payload, err := json.Marshal(map[string]any{"status": string(task.StatusWaitingUser), "reason": app.TaskModelBudgetCapacityWaitReason,
			"waiting_reason": app.TaskModelBudgetCapacityWaitReason, "message": "模型调用并发额度暂满，任务将在额度释放后自动继续。"})
		if err != nil {
			return fmt.Errorf("encode model budget capacity wait event: %w", err)
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: eventSequence, Type: "task.status", Payload: payload})
	})
}

func readModelBudgetCapacityWait(ctx context.Context, tx pgx.Tx, predicate string, args ...any) (modelBudgetCapacityWaitRow, bool, error) {
	var row modelBudgetCapacityWaitRow
	var projectID pgtype.Text
	err := tx.QueryRow(ctx, `SELECT owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,
agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,
recovery_call_id,recovery_dsh_call_id,model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,
checkpoint_digest,input_token_reservation,minimum_output_tokens,minimum_project_micros,project_budget_currency,created_at,expires_at
FROM model_budget_capacity_waits WHERE `+predicate, args...).Scan(
		&row.OwnerID, &row.TaskID, &row.TaskOwnerID, &row.Fence.TaskRevision, &row.Fence.AuthorizationVersion,
		&row.Fence.LeaseOwner, &row.Fence.LeaseVersion, &row.Fence.AgentInstanceID, &row.Fence.AgentGeneration,
		&row.Fence.AgentSessionID, &row.Fence.AgentRootSessionID, &row.Fence.EngineVersion, &row.Fence.ProfileVersion,
		&row.Fence.ThroughSequence, &row.Fence.RecoveryCallID, &row.Fence.RecoveryDSHCallID, &row.ModelID, &projectID,
		&row.DSHCallID, &row.LedgerCallID, &row.BudgetCallID, &row.RequestDigest, &row.CheckpointDigest,
		&row.InputTokenReservation, &row.MinimumOutputTokens, &row.MinimumProjectMicros, &row.ProjectBudgetCurrency,
		&row.CreatedAt, &row.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return modelBudgetCapacityWaitRow{}, false, nil
	}
	if err != nil {
		return modelBudgetCapacityWaitRow{}, false, fmt.Errorf("read model budget capacity wait: %w", err)
	}
	row.Fence.TaskID, row.Fence.OwnerID, row.Fence.InitiatorID = row.TaskID, row.TaskOwnerID, row.OwnerID
	if projectID.Valid {
		row.ProjectID = projectID.String
	}
	return row, true, nil
}

func sameModelBudgetCapacityWaitRequest(row modelBudgetCapacityWaitRow, request app.TaskModelBudgetCapacityWaitRequest) bool {
	fence := request.Fence
	return row.TaskID == fence.TaskID && row.TaskOwnerID == fence.OwnerID && row.OwnerID == fence.InitiatorID &&
		row.Fence.TaskRevision == fence.TaskRevision && row.Fence.AuthorizationVersion == fence.AuthorizationVersion &&
		row.Fence.LeaseOwner == fence.LeaseOwner && row.Fence.LeaseVersion == fence.LeaseVersion &&
		row.Fence.AgentInstanceID == fence.AgentInstanceID && row.Fence.AgentGeneration == fence.AgentGeneration &&
		row.Fence.AgentSessionID == fence.AgentSessionID && row.Fence.AgentRootSessionID == fence.AgentRootSessionID &&
		row.Fence.EngineVersion == fence.EngineVersion && row.Fence.ProfileVersion == fence.ProfileVersion &&
		row.Fence.ThroughSequence == fence.ThroughSequence && row.Fence.RecoveryCallID == fence.RecoveryCallID &&
		row.Fence.RecoveryDSHCallID == fence.RecoveryDSHCallID && row.ModelID == request.ModelID && row.ProjectID == request.ProjectID &&
		row.DSHCallID == request.DSHCallID && row.LedgerCallID == request.LedgerCallID && row.BudgetCallID == request.BudgetCallID &&
		bytes.Equal(row.RequestDigest, mustDecodeModelBudgetDigest(request.RequestDigest)) &&
		row.InputTokenReservation == request.InputTokenReservation && row.MinimumOutputTokens == request.MinimumOutputTokens &&
		row.MinimumProjectMicros == request.MinimumProjectMicros && row.ProjectBudgetCurrency == request.ProjectBudgetCurrency
}

func ensureModelBudgetCapacityCallDeadline(ctx context.Context, tx pgx.Tx, request app.TaskModelBudgetCapacityWaitRequest,
	checkpointDigest []byte, allowAgentAdvance bool) (modelBudgetCapacityCallDeadline, bool, error) {
	fence := request.Fence
	var row modelBudgetCapacityCallDeadlineRow
	err := tx.QueryRow(ctx, `SELECT owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,
agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,
recovery_call_id,recovery_dsh_call_id,model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,
checkpoint_digest,first_wait_at,deadline_at
FROM model_budget_capacity_call_deadlines WHERE owner_id=$1 AND budget_call_id=$2 FOR UPDATE`,
		fence.InitiatorID, request.BudgetCallID).Scan(&row.OwnerID, &row.TaskID, &row.TaskOwnerID, &row.TaskRevision,
		&row.AuthorizationVersion, &row.LeaseOwner, &row.LeaseVersion, &row.AgentInstanceID, &row.AgentGeneration,
		&row.AgentSessionID, &row.AgentRootSessionID, &row.EngineVersion, &row.ProfileVersion, &row.ThroughSequence,
		&row.RecoveryCallID, &row.RecoveryDSHCallID, &row.ModelID, &row.ProjectID, &row.DSHCallID, &row.LedgerCallID,
		&row.BudgetCallID, &row.RequestDigest, &row.CheckpointDigest, &row.FirstWaitAt, &row.DeadlineAt)
	if err == nil {
		if !modelBudgetCapacityDeadlineIdentityMatches(row, request, checkpointDigest) {
			return modelBudgetCapacityCallDeadline{}, false, domain.NewError(domain.CodeIdempotencyConflict,
				"model budget capacity call deadline identity changed")
		}
		if allowAgentAdvance {
			if fence.AgentGeneration <= row.AgentGeneration || fence.LeaseVersion <= row.LeaseVersion ||
				fence.AgentInstanceID == row.AgentInstanceID {
				return modelBudgetCapacityCallDeadline{}, false, domain.NewError(domain.CodeIdempotencyConflict,
					"model budget capacity call Agent fence did not advance")
			}
			updated, err := tx.Exec(ctx, `UPDATE model_budget_capacity_call_deadlines SET lease_owner=$3,lease_version=$4,
agent_instance_id=$5,agent_generation=$6 WHERE owner_id=$1 AND budget_call_id=$2`, fence.InitiatorID,
				request.BudgetCallID, fence.LeaseOwner, fence.LeaseVersion, fence.AgentInstanceID, fence.AgentGeneration)
			if err != nil || updated.RowsAffected() != 1 {
				if err != nil {
					return modelBudgetCapacityCallDeadline{}, false, fmt.Errorf("advance model budget capacity deadline Agent fence: %w", err)
				}
				return modelBudgetCapacityCallDeadline{}, false, domain.NewError(domain.CodeConflict,
					"model budget capacity call deadline changed before Agent fence update")
			}
		} else if fence.AgentInstanceID != row.AgentInstanceID || fence.AgentGeneration != row.AgentGeneration ||
			fence.LeaseOwner != row.LeaseOwner || fence.LeaseVersion != row.LeaseVersion {
			return modelBudgetCapacityCallDeadline{}, false, domain.NewError(domain.CodeIdempotencyConflict,
				"model budget capacity wait Agent fence changed")
		}
		return modelBudgetCapacityCallDeadline{FirstWaitAt: row.FirstWaitAt, DeadlineAt: row.DeadlineAt}, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return modelBudgetCapacityCallDeadline{}, false, fmt.Errorf("read model budget capacity call deadline: %w", err)
	}
	var conflictingIdentity bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM model_budget_capacity_call_deadlines WHERE owner_id=$1 AND (ledger_call_id=$2 OR dsh_call_id=$3))`,
		fence.InitiatorID, request.LedgerCallID, request.DSHCallID).Scan(&conflictingIdentity); err != nil {
		return modelBudgetCapacityCallDeadline{}, false, fmt.Errorf("check model budget capacity deadline identity: %w", err)
	}
	if conflictingIdentity {
		return modelBudgetCapacityCallDeadline{}, false, domain.NewError(domain.CodeIdempotencyConflict,
			"model budget capacity call identity is already bound to another deadline")
	}
	var result modelBudgetCapacityCallDeadline
	err = tx.QueryRow(ctx, `INSERT INTO model_budget_capacity_call_deadlines
(owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,agent_instance_id,agent_generation,
agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,recovery_call_id,recovery_dsh_call_id,
model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,checkpoint_digest,first_wait_at,deadline_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,decode($22,'hex'),$23,
clock.started_at,clock.started_at+interval '5 minutes' FROM (SELECT clock_timestamp() AS started_at) clock
RETURNING first_wait_at,deadline_at`, fence.InitiatorID, fence.TaskID, fence.OwnerID, fence.TaskRevision,
		fence.AuthorizationVersion, fence.LeaseOwner, fence.LeaseVersion, fence.AgentInstanceID, fence.AgentGeneration,
		fence.AgentSessionID, fence.AgentRootSessionID, fence.EngineVersion, fence.ProfileVersion, fence.ThroughSequence,
		fence.RecoveryCallID, fence.RecoveryDSHCallID, request.ModelID, request.ProjectID, request.DSHCallID,
		request.LedgerCallID, request.BudgetCallID, request.RequestDigest, checkpointDigest).Scan(&result.FirstWaitAt, &result.DeadlineAt)
	if err != nil {
		return modelBudgetCapacityCallDeadline{}, false, fmt.Errorf("create model budget capacity call deadline: %w", err)
	}
	return result, true, nil
}

func modelBudgetCapacityDeadlineIdentityMatches(row modelBudgetCapacityCallDeadlineRow,
	request app.TaskModelBudgetCapacityWaitRequest, checkpointDigest []byte) bool {
	fence := request.Fence
	return row.OwnerID == fence.InitiatorID && row.TaskID == fence.TaskID && row.TaskOwnerID == fence.OwnerID &&
		row.TaskRevision == fence.TaskRevision && row.AuthorizationVersion == fence.AuthorizationVersion &&
		row.AgentSessionID == fence.AgentSessionID && row.AgentRootSessionID == fence.AgentRootSessionID &&
		row.EngineVersion == fence.EngineVersion && row.ProfileVersion == fence.ProfileVersion &&
		row.ThroughSequence == fence.ThroughSequence && row.RecoveryCallID == fence.RecoveryCallID &&
		row.RecoveryDSHCallID == fence.RecoveryDSHCallID && row.ModelID == request.ModelID && row.ProjectID == request.ProjectID &&
		row.DSHCallID == request.DSHCallID && row.LedgerCallID == request.LedgerCallID && row.BudgetCallID == request.BudgetCallID &&
		bytes.Equal(row.RequestDigest, mustDecodeModelBudgetDigest(request.RequestDigest)) &&
		bytes.Equal(row.CheckpointDigest, checkpointDigest)
}

func clearModelBudgetCapacityCallDeadlineAfterPrepare(ctx context.Context, tx pgx.Tx,
	preparation app.ModelBudgetReservationPreparation) error {
	call := preparation.Reservation
	if call.Task == nil {
		return nil
	}
	dshCallID := call.DSHCallID
	if dshCallID == "" && call.Task.RecoveryDSHCallID == "" {
		dshCallID = call.CallID
	}
	request := app.TaskModelBudgetCapacityWaitRequest{Fence: *call.Task, ModelID: call.ModelID, ProjectID: call.ProjectID,
		DSHCallID: dshCallID, LedgerCallID: call.CallID, BudgetCallID: preparation.BudgetCallID,
		RequestDigest: preparation.ProviderRequestDigest}
	var deadline modelBudgetCapacityCallDeadlineRow
	err := tx.QueryRow(ctx, `SELECT owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,
agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,
recovery_call_id,recovery_dsh_call_id,model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,
checkpoint_digest,first_wait_at,deadline_at
FROM model_budget_capacity_call_deadlines WHERE owner_id=$1 AND budget_call_id=$2 FOR UPDATE`, call.OwnerID, preparation.BudgetCallID).
		Scan(&deadline.OwnerID, &deadline.TaskID, &deadline.TaskOwnerID, &deadline.TaskRevision,
			&deadline.AuthorizationVersion, &deadline.LeaseOwner, &deadline.LeaseVersion, &deadline.AgentInstanceID,
			&deadline.AgentGeneration, &deadline.AgentSessionID, &deadline.AgentRootSessionID, &deadline.EngineVersion,
			&deadline.ProfileVersion, &deadline.ThroughSequence, &deadline.RecoveryCallID, &deadline.RecoveryDSHCallID,
			&deadline.ModelID, &deadline.ProjectID, &deadline.DSHCallID, &deadline.LedgerCallID, &deadline.BudgetCallID,
			&deadline.RequestDigest, &deadline.CheckpointDigest, &deadline.FirstWaitAt, &deadline.DeadlineAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock model budget capacity deadline after prepare: %w", err)
	}
	var checkpointDigest []byte
	if err := tx.QueryRow(ctx, `SELECT checkpoint_digest FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, call.Task.TaskID, call.Task.OwnerID, call.Task.AgentSessionID).
		Scan(&checkpointDigest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "prepared model budget call checkpoint changed before deadline cleanup")
		}
		return fmt.Errorf("read Agent checkpoint while clearing model budget capacity deadline: %w", err)
	}
	if !modelBudgetCapacityDeadlineIdentityMatches(deadline, request, checkpointDigest) {
		return domain.NewError(domain.CodeIdempotencyConflict, "prepared model budget call conflicts with its capacity deadline identity")
	}
	command, err := tx.Exec(ctx, `DELETE FROM model_budget_capacity_call_deadlines
WHERE owner_id=$1 AND budget_call_id=$2 AND task_id=$3 AND ledger_call_id=$4`,
		call.OwnerID, preparation.BudgetCallID, call.Task.TaskID, call.CallID)
	if err != nil {
		return fmt.Errorf("clear prepared model budget capacity deadline: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "model budget capacity deadline changed before prepare completion")
	}
	return nil
}

func mustDecodeModelBudgetDigest(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil
	}
	return decoded
}

func (s *Store) ReconcileTaskModelBudgetCapacityWaits(ctx context.Context, limit int32) (app.TaskModelBudgetCapacityReconcileResult, error) {
	if ctx == nil || limit < 1 || limit > app.TaskModelBudgetCapacityReconcileBatchSize {
		return app.TaskModelBudgetCapacityReconcileResult{}, domain.InvalidArgument("model_budget_capacity_wait", "reconcile batch size is invalid")
	}
	var result app.TaskModelBudgetCapacityReconcileResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		candidates, err := listOldestModelBudgetCapacityWaitPerOwner(ctx, tx, limit)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			locked, err := tryLockModelBudgetLedgerOwner(ctx, tx, candidate.OwnerID)
			if err != nil {
				return err
			}
			if !locked {
				result.Deferred++
				continue
			}
			accountLimit, maxConcurrent, budgetErr := lockPlatformModelBudget(ctx, tx, candidate.OwnerID)
			if budgetErr != nil && domain.CodeOf(budgetErr) != domain.CodeQuotaExceeded {
				return budgetErr
			}
			projectLocked := false
			if candidate.ProjectID != "" {
				if _, _, err := lockProjectForActorInTx(ctx, tx, candidate.OwnerID, candidate.ProjectID, project.RoleMember, false); err != nil {
					if domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden ||
						domain.CodeOf(err) == domain.CodeUnauthenticated || domain.CodeOf(err) == domain.CodeConflict {
						outcome, err := invalidateModelBudgetCapacityWaitCandidate(ctx, tx, candidate)
						if err != nil {
							return err
						}
						applyModelBudgetCapacityOutcome(&result, outcome)
						continue
					}
					return err
				}
				projectLocked = true
			}
			current, found, err := lockModelBudgetCapacityWaitTask(ctx, tx, candidate)
			if err != nil {
				return err
			}
			if !found {
				if err := discardModelBudgetCapacityWait(ctx, tx, candidate); err != nil {
					return err
				}
				result.Discarded++
				continue
			}
			wait, found, err := readModelBudgetCapacityWait(ctx, tx, `owner_id=$1 AND task_id=$2 FOR UPDATE SKIP LOCKED`, candidate.OwnerID, candidate.TaskID)
			if err != nil {
				return err
			}
			if !found {
				result.Deferred++
				continue
			}
			if !modelBudgetCapacityTaskCanInvalidate(current, wait) {
				if err := deleteModelBudgetCapacityWait(ctx, tx, wait); err != nil {
					return err
				}
				result.Discarded++
				continue
			}
			if !modelBudgetCapacityTaskIdentityMatchesWait(current, wait) || !current.AuthValid {
				outcome, err := pauseOrDeferInvalidatedModelBudgetCapacityWait(ctx, tx, current, wait)
				if err != nil {
					return err
				}
				applyModelBudgetCapacityOutcome(&result, outcome)
				continue
			}
			stopped, err := modelBudgetCapacityWaitAgentStopped(ctx, tx, wait)
			if err != nil {
				return err
			}
			// Keep overdue waiters that lack a verified receipt. Pausing or
			// requeueing while the old DSH tree may still run is unsafe.
			if !stopped {
				if err := deferModelBudgetCapacityWait(ctx, tx, wait); err != nil {
					return err
				}
				result.Deferred++
				continue
			}
			recoveryMatches, err := modelBudgetCapacityWaitRecoveryMatches(ctx, tx, wait)
			if err != nil {
				return err
			}
			if !recoveryMatches {
				if err := pauseModelBudgetCapacityTask(ctx, tx, wait, taskModelBudgetCapacityInvalidatedReason); err != nil {
					return err
				}
				result.Paused++
				continue
			}
			if err := verifyModelBudgetCapacityWaitCheckpoint(ctx, tx, wait); err != nil {
				if domain.CodeOf(err) == domain.CodeConflict || domain.CodeOf(err) == domain.CodeInvalidState {
					if err := pauseModelBudgetCapacityTask(ctx, tx, wait, taskModelBudgetCapacityInvalidatedReason); err != nil {
						return err
					}
					result.Paused++
					continue
				}
				return err
			}
			var expired bool
			if err := tx.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM model_budget_capacity_waits
WHERE owner_id=$1 AND task_id=$2`, wait.OwnerID, wait.TaskID).Scan(&expired); err != nil {
				return fmt.Errorf("check model budget capacity wait deadline: %w", err)
			}
			if expired {
				if err := pauseModelBudgetCapacityTask(ctx, tx, wait, app.TaskModelBudgetCapacityExpiredReason); err != nil {
					return err
				}
				result.Paused++
				continue
			}
			if budgetErr != nil {
				if err := deferModelBudgetCapacityWait(ctx, tx, wait); err != nil {
					return err
				}
				result.Deferred++
				continue
			}
			availability, err := modelBudgetCapacityAvailable(ctx, tx, wait, accountLimit, maxConcurrent, projectLocked)
			if err != nil {
				return err
			}
			switch availability {
			case modelBudgetCapacityHardExhausted:
				if err := pauseModelBudgetCapacityTask(ctx, tx, wait, app.TaskModelBudgetCapacityExhaustedReason); err != nil {
					return err
				}
				result.Paused++
			case modelBudgetCapacityDeferred:
				if err := deferModelBudgetCapacityWait(ctx, tx, wait); err != nil {
					return err
				}
				result.Deferred++
			case modelBudgetCapacityReady:
				if err := requeueModelBudgetCapacityTask(ctx, tx, wait); err != nil {
					return err
				}
				result.Requeued++
			}
		}
		return cleanupInactiveModelBudgetCapacityCallDeadlines(ctx, tx, limit)
	})
	return result, err
}

type modelBudgetCapacityCallDeadlineCandidate struct {
	OwnerID, TaskID, ProjectID, BudgetCallID string
}

func cleanupInactiveModelBudgetCapacityCallDeadlines(ctx context.Context, tx pgx.Tx, limit int32) error {
	rows, err := tx.Query(ctx, `SELECT deadline.owner_id,deadline.task_id,deadline.project_id,deadline.budget_call_id
FROM model_budget_capacity_call_deadlines deadline JOIN tasks task
 ON task.id=deadline.task_id AND task.owner_id=deadline.task_owner_id
WHERE NOT (
  task.current_revision=deadline.task_revision AND task.desired_revision=deadline.task_revision AND
  (task.status IN ('queued','running') OR (task.status='waiting_user' AND task.waiting_reason='model_budget_capacity'))
)
ORDER BY deadline.first_wait_at,deadline.owner_id,deadline.budget_call_id LIMIT $1`, limit)
	if err != nil {
		return fmt.Errorf("list inactive model budget capacity call deadlines: %w", err)
	}
	var candidates []modelBudgetCapacityCallDeadlineCandidate
	for rows.Next() {
		var candidate modelBudgetCapacityCallDeadlineCandidate
		if err := rows.Scan(&candidate.OwnerID, &candidate.TaskID, &candidate.ProjectID, &candidate.BudgetCallID); err != nil {
			rows.Close()
			return fmt.Errorf("scan inactive model budget capacity call deadline: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate inactive model budget capacity call deadlines: %w", err)
	}
	rows.Close()
	for _, candidate := range candidates {
		locked, err := tryLockModelBudgetLedgerOwner(ctx, tx, candidate.OwnerID)
		if err != nil {
			return err
		}
		if !locked {
			continue
		}
		if _, _, err := lockPlatformModelBudget(ctx, tx, candidate.OwnerID); err != nil && domain.CodeOf(err) != domain.CodeQuotaExceeded {
			return err
		}
		if candidate.ProjectID != "" {
			if _, _, err := lockProjectForActorInTx(ctx, tx, candidate.OwnerID, candidate.ProjectID, project.RoleMember, false); err != nil &&
				domain.CodeOf(err) != domain.CodeNotFound && domain.CodeOf(err) != domain.CodeForbidden &&
				domain.CodeOf(err) != domain.CodeUnauthenticated && domain.CodeOf(err) != domain.CodeConflict {
				return err
			}
		}
		taskCandidate := modelBudgetCapacityCandidate{OwnerID: candidate.OwnerID, TaskID: candidate.TaskID, ProjectID: candidate.ProjectID}
		current, found, err := lockModelBudgetCapacityWaitTask(ctx, tx, taskCandidate)
		if err != nil {
			return err
		}
		var taskRevision int64
		err = tx.QueryRow(ctx, `SELECT task_revision FROM model_budget_capacity_call_deadlines
WHERE owner_id=$1 AND budget_call_id=$2 FOR UPDATE`, candidate.OwnerID, candidate.BudgetCallID).Scan(&taskRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("lock inactive model budget capacity call deadline: %w", err)
		}
		active := found && current.Revision == taskRevision && current.DesiredRevision == taskRevision &&
			(current.Status == string(task.StatusQueued) || current.Status == string(task.StatusRunning) ||
				current.Status == string(task.StatusWaitingUser) && current.WaitingReason == app.TaskModelBudgetCapacityWaitReason)
		if active {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM model_budget_capacity_call_deadlines WHERE owner_id=$1 AND budget_call_id=$2`,
			candidate.OwnerID, candidate.BudgetCallID); err != nil {
			return fmt.Errorf("delete inactive model budget capacity call deadline: %w", err)
		}
	}
	return nil
}

func listOldestModelBudgetCapacityWaitPerOwner(ctx context.Context, tx pgx.Tx, limit int32) ([]modelBudgetCapacityCandidate, error) {
	rows, err := tx.Query(ctx, `SELECT owner_id,task_id,COALESCE(project_id,'') FROM (
  SELECT DISTINCT ON (owner_id) owner_id,task_id,project_id,created_at,last_reconciled_at
  FROM model_budget_capacity_waits
  ORDER BY owner_id,COALESCE(last_reconciled_at,created_at),created_at,task_id
) oldest ORDER BY COALESCE(last_reconciled_at,created_at),created_at,owner_id,task_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list FIFO model budget capacity waiters: %w", err)
	}
	defer rows.Close()
	values := make([]modelBudgetCapacityCandidate, 0, limit)
	for rows.Next() {
		var value modelBudgetCapacityCandidate
		if err := rows.Scan(&value.OwnerID, &value.TaskID, &value.ProjectID); err != nil {
			return nil, fmt.Errorf("scan FIFO model budget capacity waiter: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model budget capacity waiters: %w", err)
	}
	return values, nil
}

type modelBudgetCapacityCandidate struct{ OwnerID, TaskID, ProjectID string }

func tryLockModelBudgetLedgerOwner(ctx context.Context, tx pgx.Tx, ownerID string) (bool, error) {
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('morde:model-ledger:' || $1,0))`, ownerID).Scan(&locked); err != nil {
		return false, fmt.Errorf("try lock model ledger owner for capacity wait: %w", err)
	}
	return locked, nil
}

type modelBudgetCapacityTask struct {
	Status, WaitingReason, InitiatorID, ModelID, ProjectID string
	Revision, DesiredRevision, AuthorizationVersion        int64
	LeaseOwner, AgentInstanceID, AgentSessionID            string
	LeaseVersion, AgentGeneration                          int64
	AgentEngineVersion, AgentProfileVersion                string
	LeaseClear, AuthValid                                  bool
}

func lockModelBudgetCapacityWaitTask(ctx context.Context, tx pgx.Tx, candidate modelBudgetCapacityCandidate) (modelBudgetCapacityTask, bool, error) {
	var current modelBudgetCapacityTask
	var leaseOwner, agentInstanceID, agentSessionID, engineVersion, profileVersion string
	var leaseVersion, generation int64
	err := tx.QueryRow(ctx, `SELECT status,waiting_reason,initiator_id,model_id,COALESCE(project_id,''),current_revision,desired_revision,
auth_version,lease_owner,lease_version,agent_instance_id,agent_generation,agent_session_id,agent_engine_version,agent_profile_version,
lease_expires_at IS NULL,(auth_kind='schedule_task' OR auth_expires_at>clock_timestamp())
FROM tasks WHERE id=$1 AND initiator_id=$2 FOR UPDATE`, candidate.TaskID, candidate.OwnerID).Scan(
		&current.Status, &current.WaitingReason, &current.InitiatorID, &current.ModelID, &current.ProjectID,
		&current.Revision, &current.DesiredRevision, &current.AuthorizationVersion, &leaseOwner, &leaseVersion,
		&agentInstanceID, &generation, &agentSessionID, &engineVersion, &profileVersion, &current.LeaseClear, &current.AuthValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return modelBudgetCapacityTask{}, false, nil
	}
	if err != nil {
		return modelBudgetCapacityTask{}, false, fmt.Errorf("lock Task for model budget capacity reconciliation: %w", err)
	}
	current.LeaseOwner, current.LeaseVersion, current.AgentInstanceID, current.AgentGeneration = leaseOwner, leaseVersion, agentInstanceID, generation
	current.AgentSessionID, current.AgentEngineVersion, current.AgentProfileVersion = agentSessionID, engineVersion, profileVersion
	return current, true, nil
}

func modelBudgetCapacityTaskCanInvalidate(current modelBudgetCapacityTask, wait modelBudgetCapacityWaitRow) bool {
	return current.Status == string(task.StatusWaitingUser) && current.WaitingReason == app.TaskModelBudgetCapacityWaitReason &&
		current.Revision == wait.Fence.TaskRevision && current.DesiredRevision == wait.Fence.TaskRevision &&
		current.LeaseOwner == "" && current.LeaseClear
}

func modelBudgetCapacityTaskIdentityMatchesWait(current modelBudgetCapacityTask, wait modelBudgetCapacityWaitRow) bool {
	return modelBudgetCapacityTaskCanInvalidate(current, wait) && current.InitiatorID == wait.OwnerID &&
		current.ModelID == wait.ModelID && current.ProjectID == wait.ProjectID &&
		current.AuthorizationVersion == wait.Fence.AuthorizationVersion && current.LeaseOwner == "" && current.LeaseClear &&
		current.LeaseVersion == wait.Fence.LeaseVersion && current.AgentInstanceID == wait.Fence.AgentInstanceID &&
		current.AgentGeneration == wait.Fence.AgentGeneration && current.AgentSessionID == wait.Fence.AgentRootSessionID &&
		current.AgentEngineVersion == wait.Fence.EngineVersion && current.AgentProfileVersion == wait.Fence.ProfileVersion
}

type modelBudgetCapacityOutcome uint8

const (
	modelBudgetCapacityOutcomeDeferred modelBudgetCapacityOutcome = iota
	modelBudgetCapacityOutcomePaused
	modelBudgetCapacityOutcomeDiscarded
)

func applyModelBudgetCapacityOutcome(result *app.TaskModelBudgetCapacityReconcileResult, outcome modelBudgetCapacityOutcome) {
	switch outcome {
	case modelBudgetCapacityOutcomeDeferred:
		result.Deferred++
	case modelBudgetCapacityOutcomePaused:
		result.Paused++
	case modelBudgetCapacityOutcomeDiscarded:
		result.Discarded++
	}
}

func invalidateModelBudgetCapacityWaitCandidate(ctx context.Context, tx pgx.Tx,
	candidate modelBudgetCapacityCandidate) (modelBudgetCapacityOutcome, error) {
	current, found, err := lockModelBudgetCapacityWaitTask(ctx, tx, candidate)
	if err != nil {
		return modelBudgetCapacityOutcomeDeferred, err
	}
	if !found {
		if err := discardModelBudgetCapacityWait(ctx, tx, candidate); err != nil {
			return modelBudgetCapacityOutcomeDeferred, err
		}
		return modelBudgetCapacityOutcomeDiscarded, nil
	}
	wait, found, err := readModelBudgetCapacityWait(ctx, tx, `owner_id=$1 AND task_id=$2 FOR UPDATE SKIP LOCKED`,
		candidate.OwnerID, candidate.TaskID)
	if err != nil {
		return modelBudgetCapacityOutcomeDeferred, err
	}
	if !found {
		return modelBudgetCapacityOutcomeDeferred, nil
	}
	if !modelBudgetCapacityTaskCanInvalidate(current, wait) {
		if err := deleteModelBudgetCapacityWait(ctx, tx, wait); err != nil {
			return modelBudgetCapacityOutcomeDeferred, err
		}
		return modelBudgetCapacityOutcomeDiscarded, nil
	}
	return pauseOrDeferInvalidatedModelBudgetCapacityWait(ctx, tx, current, wait)
}

func pauseOrDeferInvalidatedModelBudgetCapacityWait(ctx context.Context, tx pgx.Tx,
	current modelBudgetCapacityTask, wait modelBudgetCapacityWaitRow) (modelBudgetCapacityOutcome, error) {
	if !modelBudgetCapacityTaskCanInvalidate(current, wait) {
		if err := deleteModelBudgetCapacityWait(ctx, tx, wait); err != nil {
			return modelBudgetCapacityOutcomeDeferred, err
		}
		return modelBudgetCapacityOutcomeDiscarded, nil
	}
	stopped, err := modelBudgetCapacityWaitAgentStopped(ctx, tx, wait)
	if err != nil {
		return modelBudgetCapacityOutcomeDeferred, err
	}
	if !stopped {
		if err := deferModelBudgetCapacityWait(ctx, tx, wait); err != nil {
			return modelBudgetCapacityOutcomeDeferred, err
		}
		return modelBudgetCapacityOutcomeDeferred, nil
	}
	if err := pauseModelBudgetCapacityTask(ctx, tx, wait, taskModelBudgetCapacityInvalidatedReason); err != nil {
		return modelBudgetCapacityOutcomeDeferred, err
	}
	return modelBudgetCapacityOutcomePaused, nil
}

func modelBudgetCapacityWaitAgentStopped(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow) (bool, error) {
	var receipt, digest []byte
	err := tx.QueryRow(ctx, `SELECT stop_receipt,stop_receipt_digest FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_instance_id=$3 AND agent_generation=$4 AND agent_session_id=$5
 AND engine_version=$6 AND profile_version=$7 AND lease_owner=$8 AND lease_version=$9 AND auth_version=$10
	 AND state='stopped'`, wait.TaskID, wait.TaskOwnerID,
		wait.Fence.AgentInstanceID, wait.Fence.AgentGeneration, wait.Fence.AgentRootSessionID, wait.Fence.EngineVersion,
		wait.Fence.ProfileVersion, wait.Fence.LeaseOwner, wait.Fence.LeaseVersion, wait.Fence.AuthorizationVersion).Scan(&receipt, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check verified Agent stop receipt for model budget wait: %w", err)
	}
	if len(receipt) < 1 || len(receipt) > 4096 || len(digest) != sha256.Size {
		return false, nil
	}
	want := sha256.Sum256(receipt)
	return bytes.Equal(digest, want[:]), nil
}

func modelBudgetCapacityWaitRecoveryMatches(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow) (bool, error) {
	if wait.Fence.RecoveryCallID == "" {
		return true, nil
	}
	failedCallID, err := app.DSHModelCallID(wait.TaskID, wait.Fence.AgentSessionID, wait.Fence.ThroughSequence)
	if err != nil {
		return false, domain.NewError(domain.CodeConflict, "Task model recovery source identity is invalid")
	}
	var matches bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_model_recovery_schedules recovery
 JOIN tasks current ON current.id=recovery.task_id AND current.owner_id=recovery.owner_id AND current.initiator_id=recovery.initiator_id
 WHERE recovery.task_id=$1 AND recovery.owner_id=$2 AND recovery.initiator_id=$3 AND recovery.task_revision=$4
   AND recovery.authorization_version=$5 AND recovery.agent_session_id=$6 AND recovery.failed_call_id=$7
   AND recovery.recovery_call_id=$8 AND recovery.recovery_dsh_call_id=$9 AND recovery.failed_model_id=$10
   AND recovery.failed_through_seq=$11 AND recovery.checkpoint_through_seq=$11 AND recovery.checkpoint_digest=$12
   AND recovery.engine_version=$13 AND recovery.profile_version=$14 AND recovery.state='attempting'
   AND recovery.attempt_started_at IS NOT NULL AND recovery.agent_generation<$15 AND recovery.agent_instance_id<>$16
   AND current.status='waiting_user' AND current.waiting_reason=$17 AND current.current_revision=$4
   AND current.desired_revision=$4 AND current.auth_version=$5 AND current.agent_instance_id=$16
   AND current.agent_generation=$15 AND current.agent_session_id=$6 AND current.agent_engine_version=$13
   AND current.agent_profile_version=$14 AND current.checkpoint_through_seq=$11 AND current.checkpoint_digest=$12
)`, wait.TaskID, wait.TaskOwnerID, wait.OwnerID, wait.Fence.TaskRevision, wait.Fence.AuthorizationVersion,
		wait.Fence.AgentRootSessionID, failedCallID, wait.Fence.RecoveryCallID, wait.Fence.RecoveryDSHCallID, wait.ModelID,
		wait.Fence.ThroughSequence, wait.CheckpointDigest, wait.Fence.EngineVersion, wait.Fence.ProfileVersion,
		wait.Fence.AgentGeneration, wait.Fence.AgentInstanceID, app.TaskModelBudgetCapacityWaitReason).Scan(&matches)
	if err != nil {
		return false, fmt.Errorf("verify Task recovery identity for model capacity wait: %w", err)
	}
	return matches, nil
}

func verifyModelBudgetCapacityWaitCheckpoint(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow) error {
	var through int64
	var digest []byte
	if err := tx.QueryRow(ctx, `SELECT checkpoint_through_seq,checkpoint_digest FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR SHARE`, wait.TaskID, wait.TaskOwnerID, wait.Fence.AgentSessionID).
		Scan(&through, &digest); err != nil {
		return domain.NewError(domain.CodeConflict, "waiting Agent Session checkpoint is unavailable")
	}
	if through != wait.Fence.ThroughSequence || !bytes.Equal(digest, wait.CheckpointDigest) {
		return domain.NewError(domain.CodeConflict, "waiting Agent Session checkpoint changed")
	}
	var checkpoint bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_checkpoints c
JOIN task_agent_instances i ON i.task_id=c.task_id AND i.owner_id=c.owner_id AND i.agent_generation=c.agent_generation
 AND i.agent_instance_id=c.agent_instance_id AND i.agent_session_id=c.agent_root_session_id
WHERE c.task_id=$1 AND c.owner_id=$2 AND c.agent_session_id=$3 AND c.agent_root_session_id=$4 AND c.through_seq=$5
 AND c.digest=$6 AND c.auth_version=$7 AND c.engine_version=$8 AND c.profile_version=$9 AND c.lease_version=$10
	 AND c.agent_instance_id=$11 AND c.agent_generation=$12
 AND i.state='stopped' AND i.stop_receipt IS NOT NULL AND i.stop_receipt_digest=sha256(i.stop_receipt))`, wait.TaskID, wait.TaskOwnerID,
		wait.Fence.AgentSessionID, wait.Fence.AgentRootSessionID, wait.Fence.ThroughSequence, wait.CheckpointDigest,
		wait.Fence.AuthorizationVersion, wait.Fence.EngineVersion, wait.Fence.ProfileVersion, wait.Fence.LeaseVersion,
		wait.Fence.AgentInstanceID, wait.Fence.AgentGeneration).Scan(&checkpoint); err != nil {
		return fmt.Errorf("verify stopped Agent checkpoint for model budget wait: %w", err)
	}
	if !checkpoint {
		return domain.NewError(domain.CodeInvalidState, "waiting Agent checkpoint is not committed")
	}
	return nil
}

type modelBudgetCapacityAvailability uint8

const (
	modelBudgetCapacityDeferred modelBudgetCapacityAvailability = iota
	modelBudgetCapacityReady
	modelBudgetCapacityHardExhausted
)

func modelBudgetCapacityAvailable(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow,
	accountLimit int64, maxConcurrent int32, projectLocked bool) (modelBudgetCapacityAvailability, error) {
	minimumTokens := wait.InputTokenReservation + wait.MinimumOutputTokens
	var accountSpent, accountReserved int64
	var active int32
	var accountUnknown bool
	if err := tx.QueryRow(ctx, `SELECT
COALESCE(SUM(input_tokens+output_tokens) FILTER (WHERE state='settled'),0)::bigint,
COALESCE(SUM(reserved_tokens) FILTER (WHERE state IN ('prepared','dispatched')),0)::bigint,
count(*) FILTER (WHERE state IN ('prepared','dispatched'))::integer,
COALESCE(bool_or(state='unknown'),false)
FROM model_call_ledger WHERE owner_id=$1 AND NOT personal_model`, wait.OwnerID).
		Scan(&accountSpent, &accountReserved, &active, &accountUnknown); err != nil {
		return modelBudgetCapacityDeferred, fmt.Errorf("read account model capacity for wait: %w", err)
	}
	if accountUnknown || accountLimit <= 0 || maxConcurrent <= 0 {
		return modelBudgetCapacityDeferred, nil
	}
	if accountSpent >= accountLimit {
		return modelBudgetCapacityHardExhausted, nil
	}
	accountRemaining := accountLimit - accountSpent
	if active >= maxConcurrent || accountReserved >= accountRemaining ||
		accountReserved > 0 && accountRemaining-accountReserved < minimumTokens {
		return modelBudgetCapacityDeferred, nil
	}
	var taskUsage pgtype.Int8
	var taskLimit int64
	var taskProjectID string
	if err := tx.QueryRow(ctx, `SELECT usage_model_tokens,budget_model_tokens,COALESCE(project_id,'')
FROM tasks WHERE id=$1 AND owner_id=$2`, wait.TaskID, wait.TaskOwnerID).Scan(&taskUsage, &taskLimit, &taskProjectID); err != nil {
		return modelBudgetCapacityDeferred, domain.NewError(domain.CodeConflict, "Task model budget is unavailable during capacity reconciliation")
	}
	if !taskUsage.Valid || taskUsage.Int64 < 0 || taskLimit <= 0 || taskProjectID != wait.ProjectID {
		return modelBudgetCapacityDeferred, nil
	}
	if taskUsage.Int64 >= taskLimit {
		return modelBudgetCapacityHardExhausted, nil
	}
	var taskReserved int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_tokens),0)::bigint FROM model_call_ledger
WHERE task_id=$1 AND source='task' AND state IN ('prepared','dispatched')`, wait.TaskID).Scan(&taskReserved); err != nil {
		return modelBudgetCapacityDeferred, fmt.Errorf("read Task active model reservations: %w", err)
	}
	taskRemaining := taskLimit - taskUsage.Int64
	if taskReserved >= taskRemaining || taskReserved > 0 && taskRemaining-taskReserved < minimumTokens {
		return modelBudgetCapacityDeferred, nil
	}
	if wait.MinimumProjectMicros > 0 {
		if !projectLocked {
			return modelBudgetCapacityDeferred, domain.NewError(domain.CodeInvalidState, "project model budget lock is missing")
		}
		var currency string
		var projectLimit int64
		if err := tx.QueryRow(ctx, `SELECT currency,limit_micros FROM project_budgets
WHERE project_id=$1 ORDER BY version DESC LIMIT 1 FOR UPDATE`, wait.ProjectID).Scan(&currency, &projectLimit); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return modelBudgetCapacityDeferred, nil
			}
			return modelBudgetCapacityDeferred, fmt.Errorf("lock project model budget for capacity wait: %w", err)
		}
		if currency == "" || projectLimit < 0 {
			return modelBudgetCapacityDeferred, nil
		}
		totals, err := loadProjectBudgetTotalsInTx(ctx, tx, wait.ProjectID)
		if err != nil {
			return modelBudgetCapacityDeferred, err
		}
		if totals.HasUnknown {
			return modelBudgetCapacityDeferred, nil
		}
		if totals.LedgerCount > 0 && totals.LedgerCurrency != currency {
			return modelBudgetCapacityDeferred, nil
		}
		if totals.SpentMicros >= projectLimit {
			return modelBudgetCapacityHardExhausted, nil
		}
		projectRemaining := projectLimit - totals.SpentMicros
		if totals.ReservedMicros >= projectRemaining || totals.ReservedMicros > 0 &&
			projectRemaining-totals.ReservedMicros < wait.MinimumProjectMicros {
			return modelBudgetCapacityDeferred, nil
		}
	}
	return modelBudgetCapacityReady, nil
}

func requeueModelBudgetCapacityTask(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow) error {
	if !task.CanTransitionWithReason(task.StatusWaitingUser, task.StatusQueued, app.TaskModelBudgetCapacityWaitReason) {
		return domain.NewError(domain.CodeInvalidState, "model budget capacity Task transition is not permitted")
	}
	waitingReason := ""
	if wait.Fence.RecoveryCallID != "" {
		waitingReason = taskModelRecoveryScheduledReason
	}
	var sequence int64
	err := tx.QueryRow(ctx, `UPDATE tasks SET status='queued',waiting_reason=$4,finished_at=NULL,event_seq=event_seq+1,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='waiting_user' AND waiting_reason=$3 AND lease_owner='' AND lease_expires_at IS NULL
 AND current_revision=$5 AND desired_revision=$5 AND auth_version=$6 AND lease_version=$7
RETURNING event_seq`, wait.TaskID, wait.TaskOwnerID, app.TaskModelBudgetCapacityWaitReason, waitingReason,
		wait.Fence.TaskRevision, wait.Fence.AuthorizationVersion, wait.Fence.LeaseVersion).Scan(&sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "Task changed before model budget capacity requeue")
	}
	if err != nil {
		return fmt.Errorf("requeue Task after model budget capacity release: %w", err)
	}
	if wait.Fence.RecoveryCallID != "" {
		failedCallID, err := app.DSHModelCallID(wait.TaskID, wait.Fence.AgentSessionID, wait.Fence.ThroughSequence)
		if err != nil {
			return domain.NewError(domain.CodeConflict, "Task model recovery source identity is invalid")
		}
		updated, err := tx.Exec(ctx, `UPDATE task_model_recovery_schedules SET state='scheduled',attempt_started_at=NULL,
event_sequence=$3,updated_at=clock_timestamp() WHERE task_id=$1 AND owner_id=$4 AND initiator_id=$5 AND task_revision=$2
 AND state='attempting' AND authorization_version=$6 AND agent_session_id=$7 AND failed_call_id=$8 AND failed_model_id=$9
 AND agent_generation<$10 AND agent_instance_id<>$11 AND recovery_call_id=$12 AND recovery_dsh_call_id=$13
 AND failed_through_seq=$14 AND checkpoint_through_seq=$14 AND checkpoint_digest=$15 AND engine_version=$16 AND profile_version=$17`,
			wait.TaskID, wait.Fence.TaskRevision, sequence, wait.TaskOwnerID, wait.OwnerID, wait.Fence.AuthorizationVersion,
			wait.Fence.AgentRootSessionID, failedCallID, wait.ModelID, wait.Fence.AgentGeneration, wait.Fence.AgentInstanceID,
			wait.Fence.RecoveryCallID, wait.Fence.RecoveryDSHCallID, wait.Fence.ThroughSequence, wait.CheckpointDigest,
			wait.Fence.EngineVersion, wait.Fence.ProfileVersion)
		if err != nil || updated.RowsAffected() != 1 {
			if err != nil {
				return fmt.Errorf("reschedule recovery attempt after model budget wait: %w", err)
			}
			return domain.NewError(domain.CodeConflict, "Task model recovery identity changed before requeue")
		}
	}
	if err := deleteModelBudgetCapacityWait(ctx, tx, wait); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"status": string(task.StatusQueued), "reason": app.TaskModelBudgetCapacityWaitReason})
	if err != nil {
		return fmt.Errorf("encode queued model budget capacity event: %w", err)
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: wait.TaskID, Sequence: sequence, Type: "task.status", Payload: payload})
}

func pauseModelBudgetCapacityTask(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow, reason string) error {
	if !task.CanTransitionWithReason(task.StatusWaitingUser, task.StatusPaused, app.TaskModelBudgetCapacityWaitReason) {
		return domain.NewError(domain.CodeInvalidState, "model budget capacity Task transition is not permitted")
	}
	var sequence int64
	err := tx.QueryRow(ctx, `UPDATE tasks SET status='paused',waiting_reason=$4,lease_owner='',lease_expires_at=NULL,
finished_at=NULL,event_seq=event_seq+1,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='waiting_user' AND waiting_reason=$3 AND lease_owner='' AND lease_expires_at IS NULL
 AND current_revision=$5 AND desired_revision=$5 AND auth_version=$6 AND lease_version=$7
RETURNING event_seq`, wait.TaskID, wait.TaskOwnerID, app.TaskModelBudgetCapacityWaitReason, reason,
		wait.Fence.TaskRevision, wait.Fence.AuthorizationVersion, wait.Fence.LeaseVersion).Scan(&sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "Task changed before model budget capacity pause")
	}
	if err != nil {
		return fmt.Errorf("pause Task after model budget capacity wait: %w", err)
	}
	if wait.Fence.RecoveryCallID != "" {
		if reason == taskModelBudgetCapacityInvalidatedReason {
			updated, err := tx.Exec(ctx, `UPDATE task_model_recovery_schedules SET state=CASE WHEN state='attempting' THEN 'spent' ELSE 'superseded' END,
updated_at=clock_timestamp() WHERE task_id=$1 AND owner_id=$2 AND task_revision=$3 AND recovery_call_id=$4
 AND recovery_dsh_call_id=$5 AND state IN ('attempting','scheduled')`, wait.TaskID, wait.TaskOwnerID, wait.Fence.TaskRevision,
				wait.Fence.RecoveryCallID, wait.Fence.RecoveryDSHCallID)
			if err != nil {
				return fmt.Errorf("invalidate recovery schedule after model budget wait: %w", err)
			}
			_ = updated
		} else {
			failedCallID, err := app.DSHModelCallID(wait.TaskID, wait.Fence.AgentSessionID, wait.Fence.ThroughSequence)
			if err != nil {
				return domain.NewError(domain.CodeConflict, "Task model recovery source identity is invalid")
			}
			updated, err := tx.Exec(ctx, `UPDATE task_model_recovery_schedules SET state='spent',updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND initiator_id=$3 AND task_revision=$4 AND state='attempting'
 AND authorization_version=$5 AND agent_session_id=$6 AND failed_call_id=$7 AND failed_model_id=$8
 AND agent_generation<$9 AND agent_instance_id<>$10 AND recovery_call_id=$11 AND recovery_dsh_call_id=$12
 AND failed_through_seq=$13 AND checkpoint_through_seq=$13 AND checkpoint_digest=$14 AND engine_version=$15 AND profile_version=$16`,
				wait.TaskID, wait.TaskOwnerID, wait.OwnerID, wait.Fence.TaskRevision, wait.Fence.AuthorizationVersion,
				wait.Fence.AgentRootSessionID, failedCallID, wait.ModelID, wait.Fence.AgentGeneration, wait.Fence.AgentInstanceID,
				wait.Fence.RecoveryCallID, wait.Fence.RecoveryDSHCallID, wait.Fence.ThroughSequence, wait.CheckpointDigest,
				wait.Fence.EngineVersion, wait.Fence.ProfileVersion)
			if err != nil {
				return fmt.Errorf("spend model recovery attempt after budget pause: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "Task model recovery identity changed before budget pause")
			}
		}
	}
	if err := deleteModelBudgetCapacityWait(ctx, tx, wait); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"status": string(task.StatusPaused), "reason": reason})
	if err != nil {
		return fmt.Errorf("encode paused model budget capacity event: %w", err)
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: wait.TaskID, Sequence: sequence, Type: "task.status", Payload: payload})
}

func discardModelBudgetCapacityWait(ctx context.Context, tx pgx.Tx, candidate modelBudgetCapacityCandidate) error {
	_, err := tx.Exec(ctx, `DELETE FROM model_budget_capacity_waits WHERE owner_id=$1 AND task_id=$2`, candidate.OwnerID, candidate.TaskID)
	if err != nil {
		return fmt.Errorf("discard stale model budget capacity wait: %w", err)
	}
	return nil
}

func deleteModelBudgetCapacityWait(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow) error {
	command, err := tx.Exec(ctx, `DELETE FROM model_budget_capacity_waits WHERE owner_id=$1 AND task_id=$2 AND ledger_call_id=$3`,
		wait.OwnerID, wait.TaskID, wait.LedgerCallID)
	if err != nil {
		return fmt.Errorf("delete completed model budget capacity wait: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "model budget capacity wait changed before deletion")
	}
	return nil
}

func deferModelBudgetCapacityWait(ctx context.Context, tx pgx.Tx, wait modelBudgetCapacityWaitRow) error {
	command, err := tx.Exec(ctx, `UPDATE model_budget_capacity_waits SET last_reconciled_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND ledger_call_id=$3`, wait.OwnerID, wait.TaskID, wait.LedgerCallID)
	if err != nil {
		return fmt.Errorf("rotate deferred model budget capacity waiter: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "model budget capacity wait changed before rotation")
	}
	return nil
}
