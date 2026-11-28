package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

// RecordTaskModelBudgetOverage stores provider-reported usage above a signed
// input/output hold and moves both applicable budget ledgers to unknown. The
// unknown states freeze further admission while the reconciliation table
// retains the exact reported token and project-money differences.
func (s *Store) RecordTaskModelBudgetOverage(ctx context.Context, request app.ModelCallReservation, usage app.ModelUsage) error {
	if ctx == nil || request.Source != app.ModelCallTask || request.Task == nil ||
		request.OwnerID != request.Task.InitiatorID || request.BudgetPlanID == "" || request.BudgetCallID == "" ||
		!app.ValidModelBudgetRequestDigest(request.BudgetRequestDigest) || request.BudgetCapabilityVersion == "" ||
		!validTaskModelCallReservation(request) || !usage.Known || usage.InputTokens < 0 || usage.OutputTokens < 0 ||
		usage.InputTokens > math.MaxInt64-usage.OutputTokens {
		return domain.InvalidArgument("model_budget", "adaptive model overage identity or usage is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		if !request.PersonalModel {
			if _, _, err := lockPlatformModelBudget(ctx, tx, request.OwnerID); err != nil {
				return err
			}
		}
		var existingPlanID, existingBudgetCallID, existingTaskID, existingReason string
		var existingReservedInput, existingReservedOutput, existingActualInput, existingActualOutput int64
		existingErr := tx.QueryRow(ctx, `SELECT plan_id,budget_call_id,task_id,reason_code,reserved_input_tokens,
reserved_output_tokens,actual_input_tokens,actual_output_tokens
FROM model_budget_overage_reconciliations WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`,
			request.OwnerID, request.CallID).Scan(&existingPlanID, &existingBudgetCallID, &existingTaskID, &existingReason,
			&existingReservedInput, &existingReservedOutput, &existingActualInput, &existingActualOutput)
		if existingErr == nil {
			reason := modelBudgetOverageReason(usage.InputTokens, usage.OutputTokens, existingReservedInput, existingReservedOutput)
			if existingPlanID == request.BudgetPlanID && existingBudgetCallID == request.BudgetCallID &&
				existingTaskID == request.Task.TaskID && existingReason == reason &&
				existingActualInput == usage.InputTokens && existingActualOutput == usage.OutputTokens {
				return nil
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "adaptive model overage call was recorded with different usage")
		}
		if !errors.Is(existingErr, pgx.ErrNoRows) {
			return fmt.Errorf("read existing adaptive model overage: %w", existingErr)
		}
		if request.ProjectID != "" && !request.PersonalModel {
			var projectID string
			if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 FOR SHARE`, request.ProjectID).Scan(&projectID); err != nil {
				return storeError("lock adaptive overage project", "project", err)
			}
		}
		var taskOwnerID, taskInitiatorID, taskProjectID string
		if err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id,COALESCE(project_id,'') FROM tasks WHERE id=$1 FOR UPDATE`,
			request.Task.TaskID).Scan(&taskOwnerID, &taskInitiatorID, &taskProjectID); err != nil ||
			taskOwnerID != request.Task.OwnerID || taskInitiatorID != request.OwnerID || taskProjectID != request.ProjectID {
			return domain.NewError(domain.CodeConflict, "adaptive model overage Task attribution changed")
		}
		if err := verifyTaskModelBudgetPreparationInTx(ctx, tx, request, map[string]bool{"dispatched": true}, false); err != nil {
			return err
		}

		var inputReserved, outputReserved int64
		var planID, budgetCallID string
		if err := tx.QueryRow(ctx, `SELECT plan_id,budget_call_id,input_reserved_tokens,send_tokens
FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`,
			request.OwnerID, request.CallID).Scan(&planID, &budgetCallID, &inputReserved, &outputReserved); err != nil {
			return storeError("read adaptive model overage plan", "model budget plan", err)
		}
		inputExceeded := usage.InputTokens > inputReserved
		outputExceeded := usage.OutputTokens > outputReserved
		if !inputExceeded && !outputExceeded {
			return domain.NewError(domain.CodeConflict, "provider usage is within the signed model budget")
		}
		reason := modelBudgetOverageReason(usage.InputTokens, usage.OutputTokens, inputReserved, outputReserved)

		var accountState, accountModelID, accountProviderModel string
		var accountTaskID *string
		var accountReserved int64
		var accountDigest []byte
		if err := tx.QueryRow(ctx, `SELECT state,model_id,provider_model,task_id,reserved_tokens,request_digest
FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`,
			request.OwnerID, request.CallID).Scan(&accountState, &accountModelID, &accountProviderModel, &accountTaskID,
			&accountReserved, &accountDigest); err != nil {
			return storeError("lock adaptive model overage account hold", "model call", err)
		}
		if accountState != "dispatched" || accountModelID != request.ModelID || accountProviderModel != request.ProviderModel ||
			accountTaskID == nil || *accountTaskID != request.Task.TaskID || accountReserved != request.ReservedTokens ||
			len(accountDigest) != 32 || !bytes.Equal(accountDigest, request.RequestDigest) ||
			inputReserved > math.MaxInt64-outputReserved || inputReserved+outputReserved != accountReserved {
			return domain.NewError(domain.CodeConflict, "adaptive model overage account hold changed")
		}

		var currency any
		var reservedMicros, actualMicros any
		if request.ProjectID != "" && !request.PersonalModel {
			// Keep the established project order: project reference -> Task ->
			// project budget -> project ledger.
			var budgetCurrency string
			var budgetLimit, budgetVersion int64
			if err := tx.QueryRow(ctx, `SELECT currency,limit_micros,version FROM project_budgets
WHERE project_id=$1 ORDER BY version DESC LIMIT 1 FOR UPDATE`, request.ProjectID).
				Scan(&budgetCurrency, &budgetLimit, &budgetVersion); err != nil {
				return storeError("lock adaptive overage project budget", "project budget", err)
			}
			var status, actorID, modelID, storedCurrency string
			var taskID string
			var rateProvider, rateModel string
			var projectRequestDigest []byte
			var rateVersion, inputRate, outputRate, projectInputReserved, projectOutputReserved, projectReservedMicros int64
			if err := tx.QueryRow(ctx, `SELECT status,actor_id,COALESCE(task_id,''),model_id,rate_provider,rate_model,rate_version,
currency,input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,request_digest
FROM project_budget_ledger WHERE project_id=$1 AND source='task' AND call_id=$2 FOR UPDATE`, request.ProjectID, request.CallID).
				Scan(&status, &actorID, &taskID, &modelID, &rateProvider, &rateModel, &rateVersion, &storedCurrency,
					&inputRate, &outputRate, &projectInputReserved, &projectOutputReserved, &projectReservedMicros, &projectRequestDigest); err != nil {
				return storeError("lock adaptive overage project hold", "project model call", err)
			}
			if status != "reserved" || actorID != request.OwnerID || taskID != request.Task.TaskID || modelID != request.ModelID ||
				storedCurrency != budgetCurrency || budgetLimit <= 0 || budgetVersion < 1 || rateModel != request.ProviderModel ||
				projectInputReserved != inputReserved || projectOutputReserved != outputReserved {
				return domain.NewError(domain.CodeConflict, "adaptive model overage project hold changed")
			}
			rate := project.RateSnapshot{Provider: rateProvider, ModelID: modelID, Model: rateModel, Currency: storedCurrency,
				Version: rateVersion, InputMicrosPerMillion: inputRate, OutputMicrosPerMillion: outputRate}
			reservedCost, err := rate.EstimateTokenCost(project.Usage{InputTokens: inputReserved, OutputTokens: outputReserved})
			if err != nil || reservedCost != projectReservedMicros {
				return domain.NewError(domain.CodeInvalidState, "adaptive model overage project reservation cost changed")
			}
			expectedProjectDigest, err := app.ProjectModelCallRequestDigest(request.ProjectID, request.OwnerID, request.Task.TaskID,
				request.Source, request.CallID, request.ModelID, project.Usage{InputTokens: inputReserved, OutputTokens: outputReserved}, request.RequestDigest)
			if err != nil || !bytes.Equal(projectRequestDigest, expectedProjectDigest) {
				return domain.NewError(domain.CodeConflict, "adaptive model overage project request identity changed")
			}
			actualCost, err := rate.EstimateTokenCost(project.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens})
			if err != nil {
				return domain.NewError(domain.CodeInvalidState, "adaptive model overage project rate is invalid")
			}
			currency, reservedMicros, actualMicros = storedCurrency, projectReservedMicros, actualCost
		}

		if _, err := tx.Exec(ctx, `INSERT INTO model_budget_overage_reconciliations
(owner_id,source,call_id,plan_id,budget_call_id,task_id,reason_code,reserved_input_tokens,reserved_output_tokens,
 actual_input_tokens,actual_output_tokens,project_id,currency,reserved_micros,actual_micros)
VALUES ($1,'task',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, request.OwnerID, request.CallID, planID,
			budgetCallID, request.Task.TaskID, reason, inputReserved, outputReserved, usage.InputTokens, usage.OutputTokens,
			overageProjectID(request.ProjectID, !request.PersonalModel), currency, reservedMicros, actualMicros); err != nil {
			return fmt.Errorf("record adaptive model overage reconciliation: %w", err)
		}
		accountResult, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source='task' AND call_id=$2 AND state='dispatched'`, request.OwnerID, request.CallID)
		if err != nil {
			return fmt.Errorf("freeze adaptive model account budget after overage: %w", err)
		}
		if accountResult.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "adaptive model account hold changed before overage freeze")
		}
		if request.ProjectID != "" && !request.PersonalModel {
			projectResult, err := tx.Exec(ctx, `UPDATE project_budget_ledger SET status='unknown',settled_at=clock_timestamp()
WHERE project_id=$1 AND source='task' AND call_id=$2 AND status='reserved'`, request.ProjectID, request.CallID)
			if err != nil {
				return fmt.Errorf("freeze adaptive project budget after overage: %w", err)
			}
			if projectResult.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "adaptive model project hold changed before overage freeze")
			}
		}
		if err := refreshTaskModelUsageInTx(ctx, tx, request.Task.TaskID, request.OwnerID); err != nil {
			return err
		}
		return nil
	})
}

func overageProjectID(value string, set bool) any {
	if !set || value == "" {
		return nil
	}
	return value
}

var _ app.ModelBudgetOverageRecorder = (*Store)(nil)

func modelBudgetOverageReason(input, output, inputReserved, outputReserved int64) string {
	inputExceeded := input > inputReserved
	outputExceeded := output > outputReserved
	if inputExceeded && outputExceeded {
		return "input_and_output_exceeded"
	}
	if inputExceeded {
		return "input_bound_exceeded"
	}
	return "output_cap_exceeded"
}
