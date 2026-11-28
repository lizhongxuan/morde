package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

const (
	modelBudgetShadowReasonAccountBudgetMissing    = "account_budget_missing"
	modelBudgetShadowReasonAccountUsageUnknown     = "account_usage_unknown"
	modelBudgetShadowReasonTaskUsageUnknown        = "task_usage_unknown"
	modelBudgetShadowReasonTaskBudgetMissing       = "task_budget_missing"
	modelBudgetShadowReasonProjectBudgetMissing    = "project_budget_missing"
	modelBudgetShadowReasonProjectUsageUnknown     = "project_usage_unknown"
	modelBudgetShadowReasonProjectRateUnavailable  = "project_rate_unavailable"
	modelBudgetShadowReasonProjectRateMismatch     = "project_rate_mismatch"
	modelBudgetShadowReasonProjectCurrencyMismatch = "project_currency_mismatch"
)

// PreviewTaskModelBudget reads one consistent, read-only view of the account,
// Task, and project budgets. It intentionally does not share the reservation
// path: preview traffic must never acquire budget locks or mutate a ledger.
func (s *Store) PreviewTaskModelBudget(ctx context.Context,
	request app.ModelTaskBudgetShadowPreviewRequest) (app.ModelTaskBudgetShadowPreview, error) {
	if ctx == nil || !validModelTaskBudgetShadowPreviewRequest(request) {
		return app.ModelTaskBudgetShadowPreview{}, domain.InvalidArgument("model_budget_preview", "model budget preview request is invalid")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return app.ModelTaskBudgetShadowPreview{}, fmt.Errorf("begin model budget shadow snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var preview app.ModelTaskBudgetShadowPreview
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&preview.CapturedAt); err != nil {
		return app.ModelTaskBudgetShadowPreview{}, fmt.Errorf("capture model budget shadow snapshot time: %w", err)
	}
	preview.AccountApplicable = !request.PersonalModel
	preview.TaskApplicable = true
	preview.ProjectApplicable = request.ProjectID != "" && !request.PersonalModel
	providerModel, personal, err := readModelBudgetShadowConnection(ctx, tx, request)
	if err != nil {
		return app.ModelTaskBudgetShadowPreview{}, err
	}

	taskSnapshot, err := readModelBudgetShadowTaskSnapshot(ctx, tx, request, preview.CapturedAt)
	if err != nil {
		return app.ModelTaskBudgetShadowPreview{}, err
	}

	if preview.AccountApplicable {
		limit, configured, err := readModelBudgetShadowAccountLimit(ctx, tx, request.OwnerID)
		if err != nil {
			return app.ModelTaskBudgetShadowPreview{}, err
		}
		if !configured {
			preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonAccountBudgetMissing)
		} else {
			spent, reserved, unknown, err := readModelBudgetShadowAccountUsage(ctx, tx, request.OwnerID)
			if err != nil {
				return app.ModelTaskBudgetShadowPreview{}, err
			}
			if unknown {
				preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonAccountUsageUnknown)
			} else {
				value := remainingModelBudgetTokens(limit, spent, reserved, request.InputTokenReservation)
				preview.AccountOutputLimit = &value
			}
		}
	}

	if !taskSnapshot.usage.Valid {
		preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonTaskUsageUnknown)
	} else if taskSnapshot.limit <= 0 {
		preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonTaskBudgetMissing)
	} else {
		reserved, unknown, err := readModelBudgetShadowTaskUsage(ctx, tx, request.Fence.TaskID)
		if err != nil {
			return app.ModelTaskBudgetShadowPreview{}, err
		}
		if unknown {
			preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonTaskUsageUnknown)
		} else {
			value := remainingModelBudgetTokens(taskSnapshot.limit, taskSnapshot.usage.Int64, reserved, request.InputTokenReservation)
			preview.TaskOutputLimit = &value
		}
	}

	if preview.ProjectApplicable {
		projectLimit, currency, configured, err := readModelBudgetShadowProjectLimit(ctx, tx, request.ProjectID)
		if err != nil {
			return app.ModelTaskBudgetShadowPreview{}, err
		}
		if !configured {
			preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonProjectBudgetMissing)
		} else if request.ProjectRate == nil || request.ProjectRate.Validate() != nil {
			preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonProjectRateUnavailable)
		} else if request.ProjectRate.ModelID != request.ModelID || request.ProjectRate.Model != providerModel ||
			request.ProjectRate.Currency != currency || personal {
			preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonProjectRateMismatch)
		} else if request.ProjectRate.OutputMicrosPerMillion <= 0 {
			// A zero output price cannot produce a finite project token limit.
			preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonProjectRateUnavailable)
		} else {
			spent, reserved, ledgerCurrency, unknown, currencyMismatch, err := readModelBudgetShadowProjectUsage(ctx, tx, request.ProjectID)
			if err != nil {
				return app.ModelTaskBudgetShadowPreview{}, err
			}
			if unknown {
				preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonProjectUsageUnknown)
			} else if currencyMismatch || ledgerCurrency != "" && ledgerCurrency != currency {
				preview.UnknownReasons = append(preview.UnknownReasons, modelBudgetShadowReasonProjectCurrencyMismatch)
			} else {
				remainingMicros := remainingModelBudgetTokens(projectLimit, spent, reserved, 0)
				value := modelBudgetShadowAffordableOutputTokens(*request.ProjectRate,
					request.InputTokenReservation, remainingMicros)
				preview.ProjectOutputLimit = &value
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return app.ModelTaskBudgetShadowPreview{}, fmt.Errorf("finish model budget shadow snapshot: %w", err)
	}
	return preview, nil
}

func readModelBudgetShadowConnection(ctx context.Context, tx pgx.Tx,
	request app.ModelTaskBudgetShadowPreviewRequest) (string, bool, error) {
	var endpoint, providerModel, ownerID string
	var connectionVersion time.Time
	var personal bool
	err := tx.QueryRow(ctx, `SELECT base_url,model_name,updated_at,COALESCE(owner_id,''),owner_id IS NOT NULL
FROM model_connections WHERE id=$1`, request.ModelID).Scan(&endpoint, &providerModel, &connectionVersion, &ownerID, &personal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, domain.NewError(domain.CodeConflict, "model budget model connection is stale")
	}
	if err != nil {
		return "", false, fmt.Errorf("read model budget connection snapshot: %w", err)
	}
	if endpoint != request.ProviderEndpoint || providerModel != request.ProviderModel ||
		!connectionVersion.Equal(request.ConnectionVersion) || personal != request.PersonalModel ||
		personal && ownerID != request.OwnerID || !personal && ownerID != "" {
		return "", false, domain.NewError(domain.CodeConflict, "model budget model connection is stale")
	}
	return providerModel, personal, nil
}

func validModelTaskBudgetShadowPreviewRequest(request app.ModelTaskBudgetShadowPreviewRequest) bool {
	fence := request.Fence
	return domain.ValidTaskID(fence.TaskID) && domain.ValidUserID(fence.OwnerID) &&
		domain.ValidUserID(fence.InitiatorID) && request.OwnerID == fence.InitiatorID &&
		domain.ValidModelConnectionID(request.ModelID) && request.ProviderEndpoint != "" &&
		request.ProviderModel != "" && !request.ConnectionVersion.IsZero() && request.InputTokenReservation >= 0 &&
		(request.ProjectID == "" || project.ValidID(request.ProjectID)) &&
		((fence.RecoveryCallID == "") == (fence.RecoveryDSHCallID == "")) &&
		fence.TaskRevision >= 0 && fence.LeaseOwner != "" && fence.LeaseVersion > 0 &&
		fence.AgentInstanceID != "" && fence.AgentGeneration > 0 && fence.AgentSessionID != "" &&
		fence.AuthorizationVersion > 0 && fence.ThroughSequence >= 0 &&
		fence.EngineVersion != "" && fence.ProfileVersion != ""
}

type modelBudgetShadowTaskSnapshot struct {
	usage pgtype.Int8
	limit int64
}

func readModelBudgetShadowTaskSnapshot(ctx context.Context, tx pgx.Tx,
	request app.ModelTaskBudgetShadowPreviewRequest, capturedAt time.Time) (modelBudgetShadowTaskSnapshot, error) {
	fence := request.Fence
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = fence.AgentSessionID
	}
	var snapshot modelBudgetShadowTaskSnapshot
	var initiatorID, projectID, status, location, leaseOwner, agentID, rootSession, engineVersion, profileVersion, modelID string
	var leaseVersion, generation, authorizationVersion, through, currentRevision, desiredRevision, executionEpoch, ownerEpoch int64
	var authValid, leaseValid, ownerEnabled bool
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT t.initiator_id,COALESCE(t.project_id,''),t.status,t.execution_location,
t.lease_owner,t.lease_version,t.agent_instance_id,t.agent_generation,t.agent_session_id,
t.agent_engine_version,t.agent_profile_version,t.auth_version,t.checkpoint_through_seq,t.checkpoint_digest,
t.model_id,t.current_revision,t.desired_revision,t.execution_epoch,
COALESCE(t.lease_expires_at>$3,false),COALESCE(t.auth_kind='schedule_task' OR t.auth_expires_at>$3,false),
t.usage_model_tokens,t.budget_model_tokens,COALESCE(control.epoch,-1),COALESCE(control.state='enabled',false)
FROM tasks t LEFT JOIN owner_execution_controls control ON control.owner_id=t.initiator_id
WHERE t.id=$1 AND t.owner_id=$2`,
		fence.TaskID, fence.OwnerID, capturedAt).Scan(
		&initiatorID, &projectID, &status, &location, &leaseOwner, &leaseVersion, &agentID, &generation,
		&rootSession, &engineVersion, &profileVersion, &authorizationVersion, &through, &digest, &modelID,
		&currentRevision, &desiredRevision, &executionEpoch, &leaseValid, &authValid, &snapshot.usage, &snapshot.limit,
		&ownerEpoch, &ownerEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return modelBudgetShadowTaskSnapshot{}, domain.NewError(domain.CodeConflict, "model budget Task fence is stale")
	}
	if err != nil {
		return modelBudgetShadowTaskSnapshot{}, fmt.Errorf("read model budget Task snapshot: %w", err)
	}
	recoveryCall := fence.RecoveryCallID != ""
	if initiatorID != fence.InitiatorID || projectID != request.ProjectID || status != "running" || location != "server" ||
		!leaseValid || !authValid || currentRevision != desiredRevision || currentRevision != fence.TaskRevision ||
		leaseOwner != fence.LeaseOwner || leaseVersion != fence.LeaseVersion || agentID != fence.AgentInstanceID ||
		generation != fence.AgentGeneration || rootSession != rootSessionID || authorizationVersion != fence.AuthorizationVersion ||
		engineVersion != fence.EngineVersion || profileVersion != fence.ProfileVersion || modelID != request.ModelID ||
		len(digest) != 32 || !ownerEnabled || ownerEpoch != executionEpoch {
		return modelBudgetShadowTaskSnapshot{}, domain.NewError(domain.CodeConflict, "model budget Task fence is stale")
	}
	if fence.AgentSessionID == rootSessionID && ((!recoveryCall && through != fence.ThroughSequence) ||
		(recoveryCall && through < fence.ThroughSequence)) {
		return modelBudgetShadowTaskSnapshot{}, domain.NewError(domain.CodeConflict, "model budget Task fence is stale")
	}
	if snapshot.usage.Valid && snapshot.usage.Int64 < 0 {
		return modelBudgetShadowTaskSnapshot{}, domain.NewError(domain.CodeInvalidState, "model budget Task usage is invalid")
	}
	if err := verifyModelBudgetShadowCheckpoint(ctx, tx, request, capturedAt, rootSessionID, through, digest); err != nil {
		return modelBudgetShadowTaskSnapshot{}, err
	}
	return snapshot, nil
}

func verifyModelBudgetShadowCheckpoint(ctx context.Context, tx pgx.Tx,
	request app.ModelTaskBudgetShadowPreviewRequest, capturedAt time.Time, rootSessionID string,
	taskThrough int64, taskDigest []byte) error {
	fence := request.Fence
	var parentSessionID, stepID string
	var sessionThrough int64
	var sessionDigest []byte
	if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_session_id,''),COALESCE(step_id,''),checkpoint_through_seq,checkpoint_digest
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`,
		fence.TaskID, fence.OwnerID, fence.AgentSessionID).Scan(&parentSessionID, &stepID, &sessionThrough, &sessionDigest); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "model budget Task checkpoint is stale")
	} else if err != nil {
		return fmt.Errorf("read model budget Task checkpoint session: %w", err)
	}
	recoveryCall := fence.RecoveryCallID != ""
	if len(sessionDigest) != 32 || (!recoveryCall && sessionThrough != fence.ThroughSequence) ||
		(recoveryCall && sessionThrough < fence.ThroughSequence) {
		return domain.NewError(domain.CodeConflict, "model budget Task checkpoint is stale")
	}
	if fence.AgentSessionID == rootSessionID {
		if parentSessionID != "" || stepID != "" || taskThrough != sessionThrough || string(taskDigest) != string(sessionDigest) {
			return domain.NewError(domain.CodeConflict, "model budget Task checkpoint is stale")
		}
	} else {
		if parentSessionID == "" || stepID == "" {
			return domain.NewError(domain.CodeConflict, "model budget Task checkpoint is stale")
		}
		var linked bool
		err := tx.QueryRow(ctx, `WITH RECURSIVE ancestry(session_id,parent_session_id,depth) AS (
 SELECT session_id,parent_session_id,0 FROM task_agent_sessions
 WHERE task_id=$1 AND owner_id=$2 AND session_id=$3
 UNION ALL
 SELECT parent.session_id,parent.parent_session_id,ancestry.depth+1
 FROM task_agent_sessions parent JOIN ancestry ON parent.session_id=ancestry.parent_session_id
 WHERE parent.task_id=$1 AND parent.owner_id=$2 AND ancestry.depth<64
)
SELECT EXISTS(SELECT 1 FROM ancestry WHERE session_id=$4) AND EXISTS(
 SELECT 1 FROM task_steps WHERE task_id=$1 AND id=$5 AND status='running')`,
			fence.TaskID, fence.OwnerID, fence.AgentSessionID, rootSessionID, stepID).Scan(&linked)
		if err != nil {
			return fmt.Errorf("verify model budget Task Session ancestry: %w", err)
		}
		if !linked {
			return domain.NewError(domain.CodeConflict, "model budget Task checkpoint is stale")
		}
	}
	var checkpoint bool
	var checkpointErr error
	if fence.RecoveryCallID != "" {
		checkpointErr = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_model_recovery_schedules recovery
JOIN task_agent_checkpoints c ON c.task_id=recovery.task_id AND c.owner_id=recovery.owner_id
JOIN task_agent_instances i ON i.task_id=c.task_id AND i.owner_id=c.owner_id
 AND i.agent_generation=c.agent_generation AND i.agent_instance_id=c.agent_instance_id
 AND i.agent_session_id=c.agent_root_session_id AND i.engine_version=c.engine_version
 AND i.profile_version=c.profile_version AND i.auth_version=c.auth_version AND i.lease_version=c.lease_version
WHERE recovery.task_id=$1 AND recovery.owner_id=$2 AND recovery.recovery_call_id=$10
 AND recovery.recovery_dsh_call_id=$11 AND recovery.state='attempting'
 AND c.agent_session_id=$3 AND c.agent_root_session_id=$4 AND c.through_seq=$5
 AND c.digest=recovery.checkpoint_digest AND octet_length($6::bytea)=32
 AND c.auth_version=$7 AND c.engine_version=$8 AND c.profile_version=$9
 AND c.agent_generation<=recovery.agent_generation AND c.lease_version<=recovery.failed_lease_version
 AND i.state='stopped' AND ((i.stop_receipt IS NOT NULL AND i.stop_receipt_digest=sha256(i.stop_receipt))
   OR (i.guardian_stop_attestation IS NOT NULL AND i.guardian_stop_attestation_digest=sha256(i.guardian_stop_attestation))))`,
			fence.TaskID, fence.OwnerID, fence.AgentSessionID, rootSessionID, fence.ThroughSequence, sessionDigest,
			fence.AuthorizationVersion, fence.EngineVersion, fence.ProfileVersion,
			fence.RecoveryCallID, fence.RecoveryDSHCallID).Scan(&checkpoint)
	} else {
		checkpointErr = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM task_agent_checkpoints c JOIN task_agent_instances i
 ON i.task_id=c.task_id AND i.owner_id=c.owner_id AND i.agent_generation=c.agent_generation
 AND i.agent_instance_id=c.agent_instance_id AND i.agent_session_id=c.agent_root_session_id
 AND i.engine_version=c.engine_version AND i.profile_version=c.profile_version
 AND i.auth_version=c.auth_version AND i.lease_version=c.lease_version
 WHERE c.task_id=$1 AND c.owner_id=$2 AND c.agent_instance_id=$3 AND c.agent_generation=$4
 AND c.agent_session_id=$5 AND c.agent_root_session_id=$6 AND c.through_seq=$7 AND c.digest=$8
 AND c.auth_version=$9 AND c.engine_version=$10 AND c.profile_version=$11 AND c.lease_version=$12
 AND i.state='active' AND i.lease_expires_at>$13)`,
			fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID,
			rootSessionID, fence.ThroughSequence, sessionDigest, fence.AuthorizationVersion,
			fence.EngineVersion, fence.ProfileVersion, fence.LeaseVersion, capturedAt).Scan(&checkpoint)
	}
	if checkpointErr != nil {
		return fmt.Errorf("verify model budget Task checkpoint: %w", checkpointErr)
	}
	if !checkpoint {
		return domain.NewError(domain.CodeConflict, "model budget Task checkpoint is stale")
	}
	return nil
}

func readModelBudgetShadowAccountLimit(ctx context.Context, tx pgx.Tx, ownerID string) (int64, bool, error) {
	var limit int64
	err := tx.QueryRow(ctx, `SELECT token_limit FROM model_account_budgets WHERE owner_id=$1`, ownerID).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read model account budget snapshot: %w", err)
	}
	return limit, true, nil
}

func readModelBudgetShadowAccountUsage(ctx context.Context, tx pgx.Tx, ownerID string) (spent, reserved int64, unknown bool, err error) {
	err = tx.QueryRow(ctx, `SELECT
COALESCE(SUM(input_tokens::numeric+output_tokens::numeric) FILTER (WHERE state='settled'),0)::bigint,
COALESCE(SUM(reserved_tokens) FILTER (WHERE state IN ('prepared','dispatched')),0)::bigint,
COALESCE(bool_or(state='unknown'),false)
FROM model_call_ledger WHERE owner_id=$1 AND NOT personal_model`, ownerID).Scan(&spent, &reserved, &unknown)
	if err != nil {
		err = fmt.Errorf("read model account usage snapshot: %w", err)
	}
	return
}

func readModelBudgetShadowTaskUsage(ctx context.Context, tx pgx.Tx, taskID string) (reserved int64, unknown bool, err error) {
	err = tx.QueryRow(ctx, `SELECT
COALESCE(SUM(reserved_tokens) FILTER (WHERE state IN ('prepared','dispatched')),0)::bigint,
COALESCE(bool_or(state='unknown'),false)
FROM model_call_ledger WHERE task_id=$1 AND source='task'`, taskID).Scan(&reserved, &unknown)
	if err != nil {
		err = fmt.Errorf("read Task model reservations snapshot: %w", err)
	}
	return
}

func readModelBudgetShadowProjectLimit(ctx context.Context, tx pgx.Tx, projectID string) (limit int64, currency string, configured bool, err error) {
	err = tx.QueryRow(ctx, `SELECT limit_micros,currency FROM project_budgets WHERE project_id=$1 ORDER BY version DESC LIMIT 1`, projectID).
		Scan(&limit, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		err = fmt.Errorf("read project budget snapshot: %w", err)
		return
	}
	return limit, currency, true, nil
}

func readModelBudgetShadowProjectUsage(ctx context.Context, tx pgx.Tx, projectID string) (spent, reserved int64,
	ledgerCurrency string, unknown, currencyMismatch bool, err error) {
	var minCurrency, maxCurrency pgtype.Text
	err = tx.QueryRow(ctx, `SELECT
COALESCE(SUM(actual_micros) FILTER (WHERE status='settled'),0)::bigint,
COALESCE(SUM(reserved_micros) FILTER (WHERE status IN ('reserved','unknown')),0)::bigint,
COALESCE(bool_or(status='unknown'),false),min(currency),max(currency)
FROM project_budget_ledger WHERE project_id=$1`, projectID).
		Scan(&spent, &reserved, &unknown, &minCurrency, &maxCurrency)
	if err != nil {
		err = fmt.Errorf("read project model usage snapshot: %w", err)
		return
	}
	if minCurrency.Valid != maxCurrency.Valid || minCurrency.Valid && minCurrency.String != maxCurrency.String {
		currencyMismatch = true
		return
	}
	if minCurrency.Valid {
		ledgerCurrency = minCurrency.String
	}
	return
}

func remainingModelBudgetTokens(limit, spent, reserved, input int64) int64 {
	if limit <= 0 || spent < 0 || reserved < 0 || input < 0 || spent > limit || reserved > limit-spent || input > limit-spent-reserved {
		return 0
	}
	return limit - spent - reserved - input
}

func modelBudgetShadowAffordableOutputTokens(rate project.RateSnapshot, input, remainingMicros int64) int64 {
	if rate.OutputMicrosPerMillion <= 0 || input < 0 || remainingMicros < 0 {
		return 0
	}
	low, high := int64(0), int64(math.MaxInt64)
	for low < high {
		middle := low + (high-low)/2 + 1
		cost := int64(0)
		var err error
		if input != 0 || middle != 0 {
			cost, err = rate.EstimateTokenCost(project.Usage{InputTokens: input, OutputTokens: middle})
		}
		if err == nil && cost <= remainingMicros {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}
