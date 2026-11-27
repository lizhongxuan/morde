package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

const modelBudgetPreparationTTL = 2 * time.Minute

func (s *Store) PrepareTaskModelBudget(ctx context.Context, preparation app.ModelBudgetReservationPreparation) (app.ModelBudgetReservationResult, error) {
	if ctx == nil || !validModelBudgetPreparation(preparation) {
		return app.ModelBudgetReservationResult{}, domain.InvalidArgument("model_budget", "model budget preparation is invalid")
	}
	var result app.ModelBudgetReservationResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		call := preparation.Reservation
		if err := lockModelLedgerOwner(ctx, tx, call.OwnerID); err != nil {
			return err
		}
		if err := releaseExpiredRouteReservationsInTx(ctx, tx, call.OwnerID); err != nil {
			return err
		}
		if existing, found, err := loadExistingModelBudgetPreparation(ctx, tx, preparation); err != nil {
			return err
		} else if found {
			if err := clearModelBudgetCapacityCallDeadlineAfterPrepare(ctx, tx, preparation); err != nil {
				return err
			}
			result = existing
			return nil
		}
		if err := discardExpiredModelBudgetPreparationForCall(ctx, tx, call); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, call); err != nil {
			return err
		}
		if err := verifyTaskModelRecoveryPreparationForReservation(ctx, tx, call); err != nil {
			return err
		}
		safeTokens, projectID, err := adaptiveModelBudgetSafeTokensInTx(ctx, tx, preparation)
		if err != nil {
			return err
		}
		sendTokens := min(preparation.GoalTokens, safeTokens)
		if sendTokens < preparation.MinimumTokens {
			budgetErr := domain.NewError(domain.CodeQuotaExceeded, "model budget is below the selected stage minimum")
			budgetErr.Details["reason"] = "model_budget_below_stage_minimum"
			return budgetErr
		}
		requestDigest, err := app.ModelBudgetFinalRequestDigest(preparation.ProviderRequestDigest, sendTokens)
		if err != nil {
			return err
		}
		call.RequestDigest = requestDigest
		call.ReservedTokens = preparation.InputTokenReservation + sendTokens
		call.BudgetPlanID = preparation.PlanID
		call.BudgetCallID = preparation.BudgetCallID
		call.BudgetRequestDigest = preparation.ProviderRequestDigest
		if call.DSHCallID == "" {
			// Normal Task calls retain the historical one-to-one ledger/DSH ID.
			// Recovery calls must carry the separately derived DSH identity.
			if call.Task.RecoveryDSHCallID != "" {
				return domain.InvalidArgument("model_budget", "recovery DSH call identity is required")
			}
			call.DSHCallID = call.CallID
		}
		var projectReservation *app.ProjectModelReservation
		if projectID != "" && !call.PersonalModel {
			if preparation.ProjectRate == nil {
				return domain.NewError(domain.CodeInvalidState, "project model rate snapshot is unavailable")
			}
			usage := project.Usage{InputTokens: preparation.InputTokenReservation, OutputTokens: sendTokens}
			reservedMicros, err := preparation.ProjectRate.EstimateTokenCost(usage)
			if err != nil || reservedMicros < 1 {
				return domain.NewError(domain.CodeInvalidState, "project model rate snapshot is unavailable")
			}
			projectDigest, err := app.ProjectModelCallRequestDigest(projectID, call.OwnerID, call.Task.TaskID,
				call.Source, call.CallID, call.ModelID, usage, call.RequestDigest)
			if err != nil {
				return err
			}
			projectReservation = &app.ProjectModelReservation{
				ProjectID: projectID, ActorID: call.OwnerID, TaskID: call.Task.TaskID, Source: call.Source,
				CallID: call.CallID, ModelID: call.ModelID, Usage: usage, ReservedTokens: call.ReservedTokens,
				Rate: *preparation.ProjectRate, ReservedMicros: reservedMicros, RequestDigest: projectDigest, AccountCall: call,
			}
		}
		if err := reserveModelCallInTxWithState(ctx, tx, call, projectReservation, "prepared"); err != nil {
			return err
		}
		provider := modelBudgetProvider(call.ProviderEndpoint, preparation.ProjectRate)
		fence := call.Task
		_, err = tx.Exec(ctx, `INSERT INTO model_budget_preparations
(owner_id,source,call_id,task_id,task_revision,lease_owner,lease_version,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,authorization_version,engine_version,profile_version,through_sequence,turn,step,
plan_id,budget_call_id,dsh_call_id,request_digest,model_id,provider,provider_model,endpoint,connection_version,capability_version,
 stage,strategy_version,model_max_output_tokens,context_window_tokens,context_input_upper_bound_tokens,
 context_safety_margin_tokens,goal_tokens,minimum_tokens,safe_tokens,send_tokens,input_reserved_tokens,expires_at)
VALUES ($1,'task',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,
 $29,$30,$31,$32,$33,$34,$35,$36,$37,clock_timestamp()+($38 * interval '1 second'))`,
			call.OwnerID, call.CallID, fence.TaskID, fence.TaskRevision, fence.LeaseOwner, fence.LeaseVersion, fence.AgentInstanceID,
			fence.AgentGeneration, fence.AgentSessionID, fence.AgentRootSessionID, fence.AuthorizationVersion, fence.EngineVersion,
			fence.ProfileVersion, fence.ThroughSequence, preparation.Turn, preparation.Step, preparation.PlanID,
			preparation.BudgetCallID, call.DSHCallID, call.RequestDigest, call.ModelID, provider, call.ProviderModel, call.ProviderEndpoint,
			call.ConnectionVersion, preparation.CapabilityVersion, preparation.Stage, preparation.StrategyVersion,
			preparation.ModelMaxOutputTokens, preparation.ContextWindowTokens, preparation.InputContextUpperBound,
			preparation.ContextSafetyMargin, preparation.GoalTokens, preparation.MinimumTokens, safeTokens, sendTokens,
			preparation.InputTokenReservation, modelBudgetPreparationTTL.Seconds())
		if err != nil {
			return fmt.Errorf("persist adaptive model budget preparation: %w", err)
		}
		if err := clearModelBudgetCapacityCallDeadlineAfterPrepare(ctx, tx, preparation); err != nil {
			return err
		}
		result = app.ModelBudgetReservationResult{PlanID: preparation.PlanID, MaxOutputTokens: sendTokens}
		return nil
	})
	if err != nil {
		return app.ModelBudgetReservationResult{}, err
	}
	return result, nil
}

func validModelBudgetPreparation(value app.ModelBudgetReservationPreparation) bool {
	call := value.Reservation
	return call.Source == app.ModelCallTask && call.Task != nil && call.OwnerID == call.Task.InitiatorID &&
		domain.ValidUserID(call.OwnerID) && domain.ValidTaskID(call.Task.TaskID) && domain.ValidModelConnectionID(call.ModelID) &&
		validModelBudgetPreparationID(value.PlanID) && validModelBudgetPreparationID(value.BudgetCallID) &&
		call.CallID != "" && call.BudgetCallID == value.BudgetCallID && call.BudgetRequestDigest == value.ProviderRequestDigest &&
		call.BudgetCapabilityVersion == value.CapabilityVersion &&
		call.ProviderEndpoint != "" && call.ProviderModel != "" && len(call.ProviderModel) <= 256 && !call.ConnectionVersion.IsZero() &&
		len(call.RequestDigest) == 0 && value.Turn >= 0 && value.Step >= 0 &&
		len(value.ProviderRequestDigest) == 64 && value.Stage != "" && len(value.Stage) <= 64 &&
		value.StrategyVersion != "" && len(value.StrategyVersion) <= 64 &&
		value.CapabilityVersion != "" && len(value.CapabilityVersion) <= 128 &&
		value.GoalTokens >= value.MinimumTokens && value.MinimumTokens > 0 &&
		value.StaticSafeTokens >= value.MinimumTokens &&
		value.ModelMaxOutputTokens > 0 && value.ModelMaxOutputTokens <= app.MaxModelOutputTokens &&
		value.ContextWindowTokens >= 4096 && value.ContextWindowTokens <= app.MaxModelContextWindowTokens &&
		value.InputContextUpperBound >= 0 && value.InputTokenReservation == value.InputContextUpperBound &&
		value.ContextSafetyMargin >= 0 && value.ProtocolMaxOutputTokens > 0 &&
		value.StaticSafeTokens <= min(value.ModelMaxOutputTokens, value.ProtocolMaxOutputTokens,
			max(int64(0), value.ContextWindowTokens-value.InputContextUpperBound-value.ContextSafetyMargin)) &&
		((call.ProjectID != "" && !call.PersonalModel) == (value.ProjectRate != nil)) &&
		(value.ProjectRate == nil || value.ProjectRate.Validate() == nil && value.ProjectRate.ModelID == call.ModelID && value.ProjectRate.Model == call.ProviderModel)
}

func validModelBudgetPreparationID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func adaptiveModelBudgetSafeTokensInTx(ctx context.Context, tx pgx.Tx,
	preparation app.ModelBudgetReservationPreparation) (int64, string, error) {
	call := preparation.Reservation
	limitTokens := preparation.StaticSafeTokens
	if limitTokens <= 0 || preparation.InputTokenReservation < 0 || preparation.InputTokenReservation > math.MaxInt64-limitTokens {
		return 0, "", domain.NewError(domain.CodeInvalidState, "adaptive model token calculation is invalid")
	}
	var accountLimit int64
	var maxConcurrent int32
	if !call.PersonalModel {
		var err error
		accountLimit, maxConcurrent, err = lockPlatformModelBudget(ctx, tx, call.OwnerID)
		if err != nil {
			return 0, "", err
		}
		var used int64
		var running int32
		var unknown bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN state='settled' THEN input_tokens+output_tokens
 WHEN state='rejected' THEN 0 ELSE reserved_tokens END),0),
 count(*) FILTER (WHERE state IN ('prepared','dispatched')),COALESCE(bool_or(state='unknown'),false)
FROM model_call_ledger WHERE owner_id=$1 AND NOT personal_model`, call.OwnerID).
			Scan(&used, &running, &unknown); err != nil {
			return 0, "", fmt.Errorf("read adaptive account model budget: %w", err)
		}
		if unknown {
			return 0, "", domain.NewError(domain.CodeInvalidState, "model usage is unknown and requires reconciliation")
		}
		if running >= maxConcurrent {
			limited := domain.NewError(domain.CodeRateLimited, "model concurrency limit reached")
			limited.Details["reason"] = "model_concurrency_limit"
			limited.Details["stage"] = "reservation"
			return 0, "", limited
		}
		accountRemaining := int64(0)
		if used <= accountLimit && preparation.InputTokenReservation <= accountLimit-used {
			accountRemaining = accountLimit - used - preparation.InputTokenReservation
		}
		limitTokens = min(limitTokens, accountRemaining)
	}
	projectBudgetRequired := call.ProjectID != "" && !call.PersonalModel
	if projectBudgetRequired {
		if preparation.ProjectRate == nil || preparation.ProjectRate.Validate() != nil ||
			preparation.ProjectRate.ModelID != call.ModelID || preparation.ProjectRate.Model != call.ProviderModel {
			return 0, "", domain.NewError(domain.CodeInvalidState, "project model rate snapshot is unavailable")
		}
		// Match the existing project-ledger lock order: account -> project/membership
		// -> Task -> project budget. This prevents budget-row/Task-row lock cycles
		// with ordinary project model reservations.
		if _, _, err := lockProjectForActorInTx(ctx, tx, call.OwnerID, call.ProjectID, project.RoleMember, false); err != nil {
			return 0, "", err
		}
	}
	var taskUsed pgtype.Int8
	var taskLimit int64
	var taskProjectID string
	if err := tx.QueryRow(ctx, `SELECT usage_model_tokens,budget_model_tokens,COALESCE(project_id,'')
FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 FOR UPDATE`, call.Task.TaskID, call.Task.OwnerID,
		call.Task.InitiatorID).Scan(&taskUsed, &taskLimit, &taskProjectID); err != nil {
		return 0, "", domain.NewError(domain.CodeInvalidState, "task model usage is unavailable")
	}
	if !taskUsed.Valid || taskUsed.Int64 < 0 || taskLimit <= 0 || taskProjectID != call.ProjectID {
		return 0, "", domain.NewError(domain.CodeInvalidState, "task model usage or project scope is unavailable")
	}
	var pending int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_tokens),0) FROM model_call_ledger
WHERE task_id=$1 AND source='task' AND state IN ('prepared','dispatched')`, call.Task.TaskID).Scan(&pending); err != nil {
		return 0, "", fmt.Errorf("read adaptive Task model reservations: %w", err)
	}
	taskRemaining := int64(0)
	if taskUsed.Int64 <= taskLimit && pending <= taskLimit-taskUsed.Int64 && preparation.InputTokenReservation <= taskLimit-taskUsed.Int64-pending {
		taskRemaining = taskLimit - taskUsed.Int64 - pending - preparation.InputTokenReservation
	}
	limitTokens = min(limitTokens, taskRemaining)
	if projectBudgetRequired {
		var budget project.Budget
		if err := tx.QueryRow(ctx, `SELECT currency,limit_micros,version FROM project_budgets
WHERE project_id=$1 ORDER BY version DESC LIMIT 1 FOR UPDATE`, call.ProjectID).
			Scan(&budget.Currency, &budget.LimitMicros, &budget.Version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, "", domain.NewError(domain.CodeQuotaExceeded, "project model budget is not configured")
			}
			return 0, "", fmt.Errorf("lock adaptive project budget: %w", err)
		}
		if budget.Validate() != nil || budget.Currency != preparation.ProjectRate.Currency {
			return 0, "", domain.NewError(domain.CodeInvalidState, "project model rate currency does not match the budget")
		}
		totals, err := loadProjectBudgetTotalsInTx(ctx, tx, call.ProjectID)
		if err != nil {
			return 0, "", err
		}
		if totals.HasUnknown {
			return 0, "", domain.NewError(domain.CodeInvalidState, "project model usage is unknown and requires reconciliation")
		}
		if totals.LedgerCount > 0 && totals.LedgerCurrency != budget.Currency {
			return 0, "", domain.NewError(domain.CodeInternal, "project budget currency does not match its ledger")
		}
		remainingMicros := int64(0)
		if totals.SpentMicros <= budget.LimitMicros && totals.ReservedMicros <= budget.LimitMicros-totals.SpentMicros {
			remainingMicros = budget.LimitMicros - totals.SpentMicros - totals.ReservedMicros
		}
		limitTokens = maxAffordableProjectOutputTokens(*preparation.ProjectRate, preparation.InputTokenReservation, limitTokens, remainingMicros)
	}
	return limitTokens, taskProjectID, nil
}

func maxAffordableProjectOutputTokens(rate project.RateSnapshot, inputTokens, upperTokens, remainingMicros int64) int64 {
	if upperTokens <= 0 || remainingMicros < 0 {
		return 0
	}
	low, high := int64(0), upperTokens
	for low < high {
		middle := low + (high-low+1)/2
		cost, err := rate.EstimateTokenCost(project.Usage{InputTokens: inputTokens, OutputTokens: middle})
		if err == nil && cost <= remainingMicros {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

func modelBudgetProvider(_ string, rate *project.RateSnapshot) string {
	if rate != nil && strings.TrimSpace(rate.Provider) != "" {
		return rate.Provider
	}
	return "custom"
}

func (s *Store) ValidateTaskModelBudgetPreparation(ctx context.Context, request app.ModelCallReservation) error {
	if ctx == nil || request.Source != app.ModelCallTask || request.Task == nil || request.BudgetPlanID == "" ||
		request.BudgetCallID == "" || len(request.RequestDigest) != 32 || !validTaskModelCallReservation(request) {
		return domain.InvalidArgument("model_budget", "adaptive model budget identity is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		return verifyTaskModelBudgetPreparationInTx(ctx, tx, request, map[string]bool{"prepared": true, "settled": true}, true)
	})
}

func verifyTaskModelBudgetPreparationInTx(ctx context.Context, tx pgx.Tx, request app.ModelCallReservation,
	allowedStates map[string]bool, requireUnexpired bool) error {
	if request.Task == nil {
		return domain.NewError(domain.CodeInvalidState, "adaptive model budget Task fence is unavailable")
	}
	var state string
	var planID, budgetCallID, providerModel, endpoint, stage, strategyVersion, capabilityVersion string
	var dshCallID string
	var modelID, taskID, taskLeaseOwner, taskAgentID, taskSessionID, taskRootSessionID, engineVersion, profileVersion string
	var requestDigest []byte
	var taskRevision, leaseVersion, generation, authorizationVersion, throughSequence, sendTokens, inputReserved, reservedTokens int64
	var connectionVersion time.Time
	var consumedAt pgtype.Timestamptz
	var unexpired bool
	if err := tx.QueryRow(ctx, `SELECT ledger.state,preparation.plan_id,preparation.budget_call_id,preparation.dsh_call_id,preparation.request_digest,
	 preparation.model_id,preparation.provider_model,preparation.endpoint,preparation.connection_version,preparation.capability_version,
 preparation.stage,preparation.strategy_version,preparation.task_id,preparation.task_revision,preparation.lease_owner,
 preparation.lease_version,preparation.agent_instance_id,preparation.agent_generation,preparation.agent_session_id,
 preparation.agent_root_session_id,preparation.authorization_version,preparation.engine_version,preparation.profile_version,
 preparation.through_sequence,preparation.send_tokens,preparation.input_reserved_tokens,preparation.expires_at > clock_timestamp(),preparation.consumed_at,
 ledger.reserved_tokens
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.owner_id=$1 AND preparation.source='task' AND preparation.call_id=$2 FOR UPDATE OF preparation,ledger`,
		request.OwnerID, request.CallID).Scan(&state, &planID, &budgetCallID, &dshCallID, &requestDigest, &modelID, &providerModel, &endpoint,
		&connectionVersion, &capabilityVersion, &stage, &strategyVersion, &taskID, &taskRevision, &taskLeaseOwner,
		&leaseVersion, &taskAgentID, &generation, &taskSessionID, &taskRootSessionID, &authorizationVersion,
		&engineVersion, &profileVersion, &throughSequence, &sendTokens, &inputReserved, &unexpired, &consumedAt, &reservedTokens); err != nil {
		return storeError("read adaptive model budget plan", "model budget plan", err)
	}
	fence := request.Task
	expectedDigest, digestErr := app.ModelBudgetFinalRequestDigest(request.BudgetRequestDigest, sendTokens)
	preparedDSHCallID := ""
	throughMatches := throughSequence <= fence.ThroughSequence
	if fence.RecoveryDSHCallID != "" || fence.RecoveryCallID != "" {
		preparedDSHCallID = fence.RecoveryDSHCallID
		throughMatches = throughSequence == fence.ThroughSequence
		throughMatches = throughMatches && request.DSHCallID == fence.RecoveryDSHCallID
	} else {
		preparedDSHCallID, _ = app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, throughSequence)
		finalDSHCallID, finalIDErr := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
		throughMatches = throughMatches && finalIDErr == nil && request.DSHCallID == finalDSHCallID
	}
	if digestErr != nil || planID != request.BudgetPlanID || budgetCallID != request.BudgetCallID || dshCallID != preparedDSHCallID || modelID != request.ModelID ||
		!bytes.Equal(requestDigest, expectedDigest) || providerModel != request.ProviderModel || endpoint != request.ProviderEndpoint ||
		!connectionVersion.Equal(request.ConnectionVersion) || capabilityVersion != request.BudgetCapabilityVersion ||
		taskID != fence.TaskID || taskRevision != fence.TaskRevision ||
		taskLeaseOwner != fence.LeaseOwner || leaseVersion != fence.LeaseVersion || taskAgentID != fence.AgentInstanceID ||
		generation != fence.AgentGeneration || taskSessionID != fence.AgentSessionID || taskRootSessionID != fence.AgentRootSessionID ||
		authorizationVersion != fence.AuthorizationVersion || engineVersion != fence.EngineVersion || profileVersion != fence.ProfileVersion ||
		!throughMatches || inputReserved+sendTokens != request.ReservedTokens || reservedTokens != request.ReservedTokens ||
		!validTaskModelBudgetPlanVersion(stage, strategyVersion, capabilityVersion) {
		return domain.NewError(domain.CodeConflict, "adaptive model budget plan does not match the final model request")
	}
	if (state == "prepared" && consumedAt.Valid) || (state != "prepared" && !consumedAt.Valid) {
		return domain.NewError(domain.CodeInvalidState, "adaptive model budget plan state is inconsistent")
	}
	if requireUnexpired && state == "prepared" && !unexpired {
		return domain.NewError(domain.CodeConflict, "adaptive model budget plan expired before dispatch")
	}
	if !allowedStates[state] {
		return domain.NewError(domain.CodeInvalidState, "adaptive model budget plan is not available for dispatch")
	}
	return nil
}

func validTaskModelBudgetPlanVersion(stage, strategyVersion, capabilityVersion string) bool {
	return len(stage) > 0 && len(stage) <= 64 && len(strategyVersion) > 0 && len(strategyVersion) <= 64 &&
		len(capabilityVersion) > 0 && len(capabilityVersion) <= 128
}

func validTaskModelCallReservation(request app.ModelCallReservation) bool {
	return request.Task != nil && request.OwnerID == request.Task.InitiatorID &&
		domain.ValidUserID(request.OwnerID) && domain.ValidTaskID(request.Task.TaskID) && domain.ValidModelConnectionID(request.ModelID) &&
		request.CallID != "" && request.ProviderEndpoint != "" && request.ProviderModel != "" &&
		!request.ConnectionVersion.IsZero() && len(request.RequestDigest) == 32 && request.ReservedTokens > 0
}

func loadExistingModelBudgetPreparation(ctx context.Context, tx pgx.Tx,
	preparation app.ModelBudgetReservationPreparation) (app.ModelBudgetReservationResult, bool, error) {
	call := preparation.Reservation
	var storedPlanID, storedBudgetCallID, storedDSHCallID string
	var providerDigest []byte
	var sendTokens int64
	var consumedAt pgtype.Timestamptz
	var state string
	var unexpired bool
	var storedModelID, storedProviderModel, storedEndpoint, storedCapabilityVersion string
	var storedStage, storedStrategyVersion, storedTaskID, storedLeaseOwner string
	var storedAgentID, storedAgentSessionID, storedAgentRootSessionID, storedEngineVersion, storedProfileVersion string
	var storedConnectionVersion time.Time
	var storedTaskRevision, storedLeaseVersion, storedGeneration, storedAuthorizationVersion, storedThroughSequence int64
	var storedTurn, storedStep, storedMaxOutput, storedContextWindow, storedInputContextUpperBound int64
	var storedContextSafetyMargin, storedGoal, storedMinimum, storedInputReserved int64
	err := tx.QueryRow(ctx, `SELECT preparation.plan_id,preparation.budget_call_id,preparation.dsh_call_id,preparation.request_digest,
 preparation.send_tokens,preparation.expires_at > clock_timestamp(),preparation.consumed_at,ledger.state,
 preparation.model_id,preparation.provider_model,preparation.endpoint,preparation.connection_version,preparation.capability_version,
 preparation.stage,preparation.strategy_version,preparation.task_id,preparation.task_revision,preparation.lease_owner,
 preparation.lease_version,preparation.agent_instance_id,preparation.agent_generation,preparation.agent_session_id,
 preparation.agent_root_session_id,preparation.authorization_version,preparation.engine_version,preparation.profile_version,
 preparation.through_sequence,preparation.turn,preparation.step,preparation.model_max_output_tokens,
 preparation.context_window_tokens,preparation.context_input_upper_bound_tokens,preparation.context_safety_margin_tokens,
 preparation.goal_tokens,preparation.minimum_tokens,preparation.input_reserved_tokens
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.owner_id=$1 AND preparation.source='task' AND preparation.call_id=$2 FOR UPDATE OF preparation,ledger`,
		call.OwnerID, call.CallID).Scan(&storedPlanID, &storedBudgetCallID, &storedDSHCallID, &providerDigest, &sendTokens, &unexpired, &consumedAt, &state,
		&storedModelID, &storedProviderModel, &storedEndpoint, &storedConnectionVersion, &storedCapabilityVersion,
		&storedStage, &storedStrategyVersion, &storedTaskID, &storedTaskRevision, &storedLeaseOwner, &storedLeaseVersion,
		&storedAgentID, &storedGeneration, &storedAgentSessionID, &storedAgentRootSessionID, &storedAuthorizationVersion,
		&storedEngineVersion, &storedProfileVersion, &storedThroughSequence, &storedTurn, &storedStep, &storedMaxOutput,
		&storedContextWindow, &storedInputContextUpperBound, &storedContextSafetyMargin, &storedGoal, &storedMinimum,
		&storedInputReserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ModelBudgetReservationResult{}, false, nil
	}
	if err != nil {
		return app.ModelBudgetReservationResult{}, false, fmt.Errorf("find existing adaptive model budget plan: %w", err)
	}
	expected, digestErr := app.ModelBudgetFinalRequestDigest(preparation.ProviderRequestDigest, sendTokens)
	if digestErr != nil || !bytesEqual(providerDigest, expected) {
		return app.ModelBudgetReservationResult{}, false, domain.NewError(domain.CodeIdempotencyConflict, "model call key was used with different budget input")
	}
	fence := call.Task
	expectedDSHCallID := call.DSHCallID
	if expectedDSHCallID == "" && fence.RecoveryDSHCallID == "" {
		expectedDSHCallID = call.CallID
	}
	snapshotMatches := fence != nil && storedBudgetCallID == preparation.BudgetCallID &&
		storedDSHCallID == expectedDSHCallID &&
		storedModelID == call.ModelID && storedProviderModel == call.ProviderModel && storedEndpoint == call.ProviderEndpoint &&
		storedConnectionVersion.Equal(call.ConnectionVersion) && storedCapabilityVersion == preparation.CapabilityVersion &&
		storedStage == preparation.Stage && storedStrategyVersion == preparation.StrategyVersion &&
		storedTaskID == fence.TaskID && storedTaskRevision == fence.TaskRevision && storedLeaseOwner == fence.LeaseOwner &&
		storedLeaseVersion == fence.LeaseVersion && storedAgentID == fence.AgentInstanceID &&
		storedGeneration == fence.AgentGeneration && storedAgentSessionID == fence.AgentSessionID &&
		storedAgentRootSessionID == fence.AgentRootSessionID && storedAuthorizationVersion == fence.AuthorizationVersion &&
		storedEngineVersion == fence.EngineVersion && storedProfileVersion == fence.ProfileVersion &&
		storedThroughSequence == fence.ThroughSequence && storedTurn == preparation.Turn && storedStep == preparation.Step &&
		storedMaxOutput == preparation.ModelMaxOutputTokens && storedContextWindow == preparation.ContextWindowTokens &&
		storedInputContextUpperBound == preparation.InputContextUpperBound &&
		storedContextSafetyMargin == preparation.ContextSafetyMargin && storedGoal == preparation.GoalTokens &&
		storedMinimum == preparation.MinimumTokens && storedInputReserved == preparation.InputTokenReservation
	if state == "prepared" && !consumedAt.Valid && unexpired && snapshotMatches {
		return app.ModelBudgetReservationResult{PlanID: storedPlanID, MaxOutputTokens: sendTokens}, true, nil
	}
	if state == "prepared" && !consumedAt.Valid {
		if err := releasePreparedModelCallInTx(ctx, tx, call.OwnerID, call.CallID, call.Task.TaskID); err != nil {
			return app.ModelBudgetReservationResult{}, false, err
		}
		return app.ModelBudgetReservationResult{}, false, nil
	}
	if state == "settled" && consumedAt.Valid {
		if _, err := tx.Exec(ctx, `UPDATE model_budget_preparations SET plan_id=$3,budget_call_id=$4,
prepared_at=clock_timestamp(),expires_at=clock_timestamp()+($5 * interval '1 second'),consumed_at=clock_timestamp()
WHERE owner_id=$1 AND source='task' AND call_id=$2`, call.OwnerID, call.CallID, preparation.PlanID,
			preparation.BudgetCallID, modelBudgetPreparationTTL.Seconds()); err != nil {
			return app.ModelBudgetReservationResult{}, false, fmt.Errorf("bind replay of settled adaptive model budget: %w", err)
		}
		return app.ModelBudgetReservationResult{PlanID: preparation.PlanID, MaxOutputTokens: sendTokens}, true, nil
	}
	return app.ModelBudgetReservationResult{}, false, domain.NewError(domain.CodeInvalidState, "model budget call is already dispatched or requires reconciliation")
}

func discardExpiredModelBudgetPreparationForCall(ctx context.Context, tx pgx.Tx, call app.ModelCallReservation) error {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`,
		call.OwnerID, call.CallID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing model call before preparation: %w", err)
	}
	if state == "prepared" {
		return releasePreparedModelCallInTx(ctx, tx, call.OwnerID, call.CallID, call.Task.TaskID)
	}
	return domain.NewError(domain.CodeConflict, "model call already has a ledger reservation")
}

func releasePreparedModelCallInTx(ctx context.Context, tx pgx.Tx, ownerID, callID, taskID string) error {
	var personal bool
	if err := tx.QueryRow(ctx, `SELECT personal_model FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2 AND state='prepared' FOR UPDATE`, ownerID, callID).Scan(&personal); err != nil {
		return storeError("read prepared model call", "model call", err)
	}
	if !personal {
		if _, _, err := lockPlatformModelBudget(ctx, tx, ownerID); err != nil {
			return err
		}
	}
	var projectIDs []string
	rows, err := tx.Query(ctx, `SELECT DISTINCT project_id FROM project_budget_ledger
WHERE actor_id=$1 AND source='task' AND call_id=$2 AND task_id=$3 ORDER BY project_id`, ownerID, callID, taskID)
	if err != nil {
		return fmt.Errorf("read prepared project budget holds: %w", err)
	}
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			rows.Close()
			return fmt.Errorf("read prepared project budget hold: %w", err)
		}
		projectIDs = append(projectIDs, projectID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate prepared project budget holds: %w", err)
	}
	rows.Close()
	sort.Strings(projectIDs)
	for _, projectID := range projectIDs {
		if err := lockProjectByIDInTx(ctx, tx, projectID); err != nil {
			return err
		}
	}
	if len(projectIDs) > 1 {
		return domain.NewError(domain.CodeInvalidState, "prepared model call has multiple project holds")
	}
	if len(projectIDs) == 1 {
		var projectState string
		if err := tx.QueryRow(ctx, `SELECT status FROM project_budget_ledger
WHERE project_id=$1 AND source='task' AND call_id=$2 AND actor_id=$3 AND task_id=$4 FOR UPDATE`,
			projectIDs[0], callID, ownerID, taskID).Scan(&projectState); err != nil || projectState != "reserved" {
			return domain.NewError(domain.CodeConflict, "prepared project model budget is no longer releasable")
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2`, ownerID, callID); err != nil {
		return fmt.Errorf("delete prepared model budget plan: %w", err)
	}
	if len(projectIDs) == 1 {
		result, err := tx.Exec(ctx, `DELETE FROM project_budget_ledger WHERE project_id=$1 AND source='task' AND call_id=$2 AND task_id=$3 AND status='reserved'`,
			projectIDs[0], callID, taskID)
		if err != nil {
			return fmt.Errorf("release prepared project model budget: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "prepared project model budget changed before release")
		}
	}
	result, err := tx.Exec(ctx, `DELETE FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2 AND task_id=$3 AND state='prepared'`, ownerID, callID, taskID)
	if err != nil {
		return fmt.Errorf("release prepared model token reservation: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "prepared model token reservation changed before release")
	}
	return nil
}

func (s *Store) CleanupExpiredTaskModelBudgetPreparations(ctx context.Context, limit int32) (int32, error) {
	if ctx == nil || limit < 1 || limit > 1000 {
		return 0, domain.InvalidArgument("model_budget", "expired model budget cleanup request is invalid")
	}
	var released int32
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		ownersRows, err := tx.Query(ctx, `SELECT DISTINCT preparation.owner_id
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.consumed_at IS NULL AND preparation.expires_at <= clock_timestamp() AND ledger.state='prepared'
ORDER BY preparation.owner_id LIMIT $1`, limit)
		if err != nil {
			return fmt.Errorf("list expired adaptive model budget owners: %w", err)
		}
		owners := make([]string, 0, limit)
		for ownersRows.Next() {
			var ownerID string
			if err := ownersRows.Scan(&ownerID); err != nil {
				ownersRows.Close()
				return fmt.Errorf("read expired adaptive model budget owner: %w", err)
			}
			owners = append(owners, ownerID)
		}
		if err := ownersRows.Err(); err != nil {
			ownersRows.Close()
			return fmt.Errorf("iterate expired adaptive model budget owners: %w", err)
		}
		ownersRows.Close()
		for _, ownerID := range owners {
			if err := lockModelLedgerOwner(ctx, tx, ownerID); err != nil {
				return err
			}
			var platform bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_budget_preparations preparation
JOIN model_call_ledger ledger ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.owner_id=$1 AND preparation.consumed_at IS NULL AND preparation.expires_at <= clock_timestamp() AND
 ledger.state='prepared' AND NOT ledger.personal_model)`, ownerID).Scan(&platform); err != nil {
				return fmt.Errorf("inspect expired adaptive account reservations: %w", err)
			}
			if platform {
				if _, _, err := lockPlatformModelBudget(ctx, tx, ownerID); err != nil {
					return err
				}
			}
			rows, err := tx.Query(ctx, `SELECT preparation.call_id,preparation.task_id,ledger.personal_model
FROM model_budget_preparations preparation JOIN model_call_ledger ledger
 ON ledger.owner_id=preparation.owner_id AND ledger.source=preparation.source AND ledger.call_id=preparation.call_id
WHERE preparation.owner_id=$1 AND preparation.consumed_at IS NULL AND preparation.expires_at <= clock_timestamp() AND ledger.state='prepared'
ORDER BY preparation.call_id LIMIT $2 FOR UPDATE OF preparation,ledger SKIP LOCKED`, ownerID, limit)
			if err != nil {
				return fmt.Errorf("lock expired adaptive model budget plans: %w", err)
			}
			type expiredCall struct{ callID, taskID string }
			calls := make([]expiredCall, 0, limit)
			for rows.Next() {
				var call expiredCall
				var personal bool
				if err := rows.Scan(&call.callID, &call.taskID, &personal); err != nil {
					rows.Close()
					return fmt.Errorf("read expired adaptive model budget plan: %w", err)
				}
				calls = append(calls, call)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return fmt.Errorf("iterate expired adaptive model budget plans: %w", err)
			}
			rows.Close()
			for _, call := range calls {
				if err := releasePreparedModelCallInTx(ctx, tx, ownerID, call.callID, call.taskID); err != nil {
					return err
				}
				released++
				if released >= limit {
					return nil
				}
			}
		}
		return nil
	})
	return released, err
}
