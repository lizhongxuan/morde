package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) ListUnknownModelCalls(ctx context.Context, principal identity.Principal, ownerID string, limit int) ([]app.UnknownModelCall, error) {
	items := make([]app.UnknownModelCall, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT owner_id,source,call_id,model_id,provider_model,COALESCE(task_id,''),
 reserved_tokens,created_at,settled_at FROM model_call_ledger
 WHERE state='unknown' AND ($1='' OR owner_id=$1)
 ORDER BY settled_at,owner_id,source,call_id LIMIT $2`, ownerID, limit)
		if err != nil {
			return fmt.Errorf("list unknown model usage: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var item app.UnknownModelCall
			if err := rows.Scan(&item.OwnerID, &item.Source, &item.CallID, &item.ModelID, &item.ProviderModel,
				&item.TaskID, &item.ReservedTokens, &item.CreatedAt, &item.UnknownAt); err != nil {
				return fmt.Errorf("read unknown model usage: %w", err)
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return items, err
}

func (s *Store) GetModelAccountBudget(ctx context.Context, principal identity.Principal, ownerID string) (app.ModelAccountBudget, error) {
	if !domain.ValidUserID(ownerID) {
		return app.ModelAccountBudget{}, domain.InvalidArgument("owner_id", "invalid owner")
	}
	var budget app.ModelAccountBudget
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		var err error
		budget, err = loadModelAccountBudget(ctx, tx, ownerID)
		return err
	})
	return budget, err
}

func (s *Store) SetModelAccountBudget(ctx context.Context, principal identity.Principal, command app.ModelAccountBudgetUpdate) (app.ModelAccountBudget, error) {
	if !domain.ValidUserID(command.OwnerID) || command.TokenLimit < 1 || command.MaxConcurrent < 1 || command.MaxConcurrent > 100 || len(command.IP) > 64 {
		return app.ModelAccountBudget{}, domain.InvalidArgument("model_budget", "invalid model budget")
	}
	var budget app.ModelAccountBudget
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		var exists string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, command.OwnerID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "user not found")
		} else if err != nil {
			return fmt.Errorf("lock model budget owner: %w", err)
		}
		insert, err := tx.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,$2,$3) ON CONFLICT (owner_id) DO NOTHING`, command.OwnerID, command.TokenLimit, command.MaxConcurrent)
		if err != nil {
			return fmt.Errorf("reserve model budget row: %w", err)
		}
		var oldLimit int64
		var oldConcurrent int32
		if err := tx.QueryRow(ctx, `SELECT token_limit,max_concurrent FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, command.OwnerID).
			Scan(&oldLimit, &oldConcurrent); err != nil {
			return fmt.Errorf("lock model budget for update: %w", err)
		}
		configured := insert.RowsAffected() == 0
		if !configured {
			oldLimit, oldConcurrent = 0, 0
		}
		if _, err := tx.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent)
VALUES ($1,$2,$3) ON CONFLICT (owner_id) DO UPDATE SET token_limit=EXCLUDED.token_limit,
max_concurrent=EXCLUDED.max_concurrent,updated_at=clock_timestamp()`, command.OwnerID, command.TokenLimit, command.MaxConcurrent); err != nil {
			return fmt.Errorf("save model account budget: %w", err)
		}
		entry, err := auditParams(audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionModelBudgetUpdate,
			Target: command.OwnerID, IP: command.IP, Detail: map[string]any{
				"configured": configured, "previous_token_limit": oldLimit, "previous_max_concurrent": oldConcurrent,
				"token_limit": command.TokenLimit, "max_concurrent": command.MaxConcurrent,
			},
		})
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
			return fmt.Errorf("audit model budget update: %w", err)
		}
		budget, err = loadModelAccountBudget(ctx, tx, command.OwnerID)
		return err
	})
	return budget, err
}

func loadModelAccountBudget(ctx context.Context, tx pgx.Tx, ownerID string) (app.ModelAccountBudget, error) {
	var budget app.ModelAccountBudget
	var concurrent int64
	var inFlight int64
	err := tx.QueryRow(ctx, `SELECT u.id,COALESCE(b.token_limit,0),COALESCE(b.max_concurrent,0),b.owner_id IS NOT NULL,
 COALESCE((SELECT SUM(CASE WHEN l.state='settled' THEN l.input_tokens::numeric+l.output_tokens::numeric ELSE l.reserved_tokens::numeric END)
   FROM model_call_ledger l WHERE l.owner_id=u.id),0),
  COALESCE((SELECT count(*) FROM model_call_ledger l WHERE l.owner_id=u.id AND l.state='dispatched'),0),
 COALESCE((SELECT bool_or(l.state='unknown') FROM model_call_ledger l WHERE l.owner_id=u.id),false)
FROM users u LEFT JOIN model_account_budgets b ON b.owner_id=u.id WHERE u.id=$1`, ownerID).
		Scan(&budget.OwnerID, &budget.TokenLimit, &concurrent, &budget.Configured, &budget.UsedTokens, &inFlight, &budget.HasUnknown)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ModelAccountBudget{}, domain.NewError(domain.CodeNotFound, "user not found")
	}
	if err != nil {
		return app.ModelAccountBudget{}, fmt.Errorf("load model account budget: %w", err)
	}
	if inFlight > math.MaxInt32 {
		return app.ModelAccountBudget{}, domain.NewError(domain.CodeInternal, "model in-flight call count is invalid")
	}
	budget.MaxConcurrent = int32(concurrent)
	budget.InFlight = int32(inFlight)
	return budget, nil
}

func (s *Store) ReconcileUnknownModelCall(ctx context.Context, principal identity.Principal, command app.ModelUsageReconcileCommand) (bool, error) {
	replayed := false
	quotaExceeded := false
	// This transaction shares the account budget lock with normal settlement. A
	// project Task also updates its project money ledger in the same transaction.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var budget int64
		if err := tx.QueryRow(ctx, `SELECT token_limit FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, command.OwnerID).Scan(&budget); err != nil {
			return storeError("lock model usage budget", "model budget", err)
		}
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		var state, taskID, modelID string
		var priorInput, priorOutput *int64
		var accountReserved int64
		var accountDigest []byte
		err := tx.QueryRow(ctx, `SELECT state,COALESCE(task_id,''),model_id,reserved_tokens,input_tokens,output_tokens,request_digest FROM model_call_ledger
 WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, command.OwnerID, string(command.Source), command.CallID).
			Scan(&state, &taskID, &modelID, &accountReserved, &priorInput, &priorOutput, &accountDigest)
		if err != nil {
			return storeError("lock model usage", "model call", err)
		}
		var taskOwnerID, taskProjectID string
		if taskID != "" {
			var observedOwner, observedProject, location string
			if err := tx.QueryRow(ctx, `SELECT owner_id,COALESCE(project_id,''),execution_location FROM tasks
 WHERE id=$1 AND initiator_id=$2`, taskID, command.OwnerID).Scan(&observedOwner, &observedProject, &location); err != nil {
				return storeError("lock model usage task", "task", err)
			}
			if location != "server" {
				return domain.NewError(domain.CodeInvalidState, "task usage cannot be rebuilt from this ledger")
			}
			if observedProject == "" && observedOwner != command.OwnerID {
				return domain.NewError(domain.CodeInvalidState, "personal Task actor does not match its owner")
			}
			if observedProject != "" {
				if command.Source != app.ModelCallTask {
					return domain.NewError(domain.CodeInvalidState, "project usage cannot be reconciled from a non-Task ledger")
				}
				if err := lockProjectByIDInTx(ctx, tx, observedProject); err != nil {
					return err
				}
			}
			if err := tx.QueryRow(ctx, `SELECT owner_id,COALESCE(project_id,''),execution_location FROM tasks
 WHERE id=$1 AND initiator_id=$2 FOR UPDATE`, taskID, command.OwnerID).Scan(&taskOwnerID, &taskProjectID, &location); err != nil {
				return storeError("lock model usage task", "task", err)
			}
			if taskOwnerID != observedOwner || taskProjectID != observedProject || location != "server" {
				return domain.NewError(domain.CodeInvalidState, "task attribution changed during model usage reconciliation")
			}
		}
		var projectCall *modelUsageProjectCall
		if taskProjectID != "" {
			call, err := lockProjectModelCallForReconciliation(ctx, tx, taskProjectID, command, taskID, modelID, accountDigest)
			if err != nil {
				return err
			}
			projectCall = &call
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		target := command.OwnerID + "/" + string(command.Source) + "/" + command.CallID
		if state == "settled" {
			if projectCall != nil && (projectCall.Status != "settled" || projectCall.ActualInput == nil || projectCall.ActualOutput == nil ||
				*projectCall.ActualInput != command.InputTokens || *projectCall.ActualOutput != command.OutputTokens) {
				return domain.NewError(domain.CodeInvalidState, "project and account model ledgers do not match")
			}
			if projectCall != nil {
				actualMicros, err := projectCall.Rate.EstimateTokenCost(project.Usage{InputTokens: command.InputTokens, OutputTokens: command.OutputTokens})
				if err != nil || projectCall.ActualMicros == nil || *projectCall.ActualMicros != actualMicros {
					return domain.NewError(domain.CodeInvalidState, "project and account model ledgers do not match")
				}
			}
			var actor string
			var detail []byte
			err := tx.QueryRow(ctx, `SELECT actor_id,detail FROM audit_logs WHERE action=$1 AND target=$2
 ORDER BY id DESC LIMIT 1`, string(audit.ActionModelUsageReconcile), target).Scan(&actor, &detail)
			var previous struct {
				InputCount  int64  `json:"input_count"`
				OutputCount int64  `json:"output_count"`
				Evidence    string `json:"evidence"`
			}
			if err == nil && json.Unmarshal(detail, &previous) == nil && actor == principal.UserID &&
				priorInput != nil && *priorInput == command.InputTokens && priorOutput != nil && *priorOutput == command.OutputTokens &&
				previous.InputCount == command.InputTokens && previous.OutputCount == command.OutputTokens && previous.Evidence == command.Evidence {
				replayed = true
				return nil
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read model reconciliation audit: %w", err)
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "model usage was already settled differently")
		}
		if state != "unknown" {
			return domain.NewError(domain.CodeInvalidState, "model call is still dispatched or unavailable")
		}
		if projectCall != nil {
			if projectCall.Status != "unknown" {
				return domain.NewError(domain.CodeInvalidState, "project and account model ledgers do not match")
			}
			actualMicros, estimateErr := projectCall.Rate.EstimateTokenCost(project.Usage{InputTokens: command.InputTokens, OutputTokens: command.OutputTokens})
			if estimateErr != nil || command.InputTokens+command.OutputTokens > accountReserved ||
				command.InputTokens > projectCall.ReservedInput || command.OutputTokens > projectCall.ReservedOutput ||
				actualMicros > projectCall.ReservedMicros {
				// Keep both holds unknown when any token or money reservation is too
				// small. The evidence audit below still commits with this decision.
				quotaExceeded = true
			} else {
				if err := settleReconciledAccountLedgerInTx(ctx, tx, command); err != nil {
					return err
				}
				result, err := tx.Exec(ctx, `UPDATE project_budget_ledger SET status='settled',actual_input_tokens=$4,
actual_output_tokens=$5,actual_micros=$6,settled_at=clock_timestamp()
WHERE project_id=$1 AND source=$2 AND call_id=$3 AND status='unknown'`, taskProjectID, string(command.Source), command.CallID,
					command.InputTokens, command.OutputTokens, actualMicros)
				if err != nil || result.RowsAffected() != 1 {
					if err != nil {
						return fmt.Errorf("reconcile project model ledger: %w", err)
					}
					return domain.NewError(domain.CodeConflict, "project model usage changed")
				}
			}
		} else if err := settleReconciledAccountLedgerInTx(ctx, tx, command); err != nil {
			return err
		}
		// The reservation path reads totals into int64. Reject a reconciliation
		// that would make the account's settled and reserved total unrepresentable.
		var accountTotalFits bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN state='settled'
 THEN input_tokens::numeric+output_tokens::numeric ELSE reserved_tokens::numeric END),0) <= 9223372036854775807
 FROM model_call_ledger WHERE owner_id=$1`, command.OwnerID).Scan(&accountTotalFits); err != nil {
			return fmt.Errorf("check reconciled model usage total: %w", err)
		}
		if !accountTotalFits {
			quotaExceeded = true
			if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',input_tokens=NULL,output_tokens=NULL,settled_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3`, command.OwnerID, string(command.Source), command.CallID); err != nil {
				return fmt.Errorf("restore unknown account model usage after total overflow: %w", err)
			}
			if projectCall != nil {
				if _, err := tx.Exec(ctx, `UPDATE project_budget_ledger SET status='unknown',actual_input_tokens=NULL,actual_output_tokens=NULL,
actual_micros=NULL,settled_at=clock_timestamp() WHERE project_id=$1 AND source=$2 AND call_id=$3`, taskProjectID,
					string(command.Source), command.CallID); err != nil {
					return fmt.Errorf("restore unknown project usage after account total overflow: %w", err)
				}
			}
		}
		if taskID != "" {
			if err := refreshTaskModelUsageInTx(ctx, tx, taskID, command.OwnerID); err != nil {
				return err
			}
		}
		entry, err := auditParams(audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionModelUsageReconcile,
			Target: target, IP: command.IP, Detail: map[string]any{
				"owner_id": command.OwnerID, "source": string(command.Source), "call_id": command.CallID,
				"input_count": command.InputTokens, "output_count": command.OutputTokens, "evidence": command.Evidence,
				"project_id": taskProjectID, "over_reservation": quotaExceeded,
			},
		})
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
			return fmt.Errorf("audit model usage reconciliation: %w", err)
		}
		return nil
	})
	if err == nil && quotaExceeded {
		return false, domain.NewError(domain.CodeQuotaExceeded, "reconciled usage exceeded a token or project reservation; both holds remain unknown")
	}
	return replayed, err
}

type modelUsageProjectCall struct {
	Status         string
	ReservedInput  int64
	ReservedOutput int64
	ReservedMicros int64
	ActualInput    *int64
	ActualOutput   *int64
	ActualMicros   *int64
	Rate           project.RateSnapshot
}

func lockProjectModelCallForReconciliation(ctx context.Context, tx pgx.Tx, projectID string, command app.ModelUsageReconcileCommand,
	taskID, modelID string, accountDigest []byte) (modelUsageProjectCall, error) {
	var call modelUsageProjectCall
	var actorID, ledgerTaskID, ledgerModelID, currency string
	var provider, model string
	var rateVersion, inputRate, outputRate int64
	var projectDigest []byte
	err := tx.QueryRow(ctx, `SELECT actor_id,task_id,model_id,status,currency,rate_provider,rate_model,rate_version,
input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,
actual_input_tokens,actual_output_tokens,actual_micros,request_digest FROM project_budget_ledger
WHERE project_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, projectID, string(command.Source), command.CallID).
		Scan(&actorID, &ledgerTaskID, &ledgerModelID, &call.Status, &currency, &provider, &model, &rateVersion,
			&inputRate, &outputRate, &call.ReservedInput, &call.ReservedOutput, &call.ReservedMicros,
			&call.ActualInput, &call.ActualOutput, &call.ActualMicros, &projectDigest)
	if err != nil {
		return modelUsageProjectCall{}, storeError("lock project model usage", "project model call", err)
	}
	call.Rate = project.RateSnapshot{Provider: provider, ModelID: ledgerModelID, Model: model, Currency: currency,
		Version: rateVersion, InputMicrosPerMillion: inputRate, OutputMicrosPerMillion: outputRate}
	if actorID != command.OwnerID || ledgerTaskID != taskID || ledgerModelID != modelID || len(projectDigest) != 32 || len(accountDigest) != 32 ||
		call.Rate.Validate() != nil || call.ReservedInput < 0 || call.ReservedOutput < 1 || call.ReservedMicros < 1 {
		return modelUsageProjectCall{}, domain.NewError(domain.CodeInvalidState, "project and account model ledgers do not match")
	}
	return call, nil
}

func settleReconciledAccountLedgerInTx(ctx context.Context, tx pgx.Tx, command app.ModelUsageReconcileCommand) error {
	result, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='settled',input_tokens=$4,output_tokens=$5,settled_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3 AND state='unknown'`, command.OwnerID, string(command.Source), command.CallID,
		command.InputTokens, command.OutputTokens)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return fmt.Errorf("reconcile model ledger: %w", err)
		}
		return domain.NewError(domain.CodeConflict, "model usage changed")
	}
	return nil
}

func lockModelUsageAdminUser(ctx context.Context, tx pgx.Tx, principal identity.Principal) error {
	if principal.Kind != identity.KindSession || principal.ClientInstallationID != "" || !principal.Role.IsAdmin() ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) || principal.HostScope != "" ||
		len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 {
		return domain.NewError(domain.CodeForbidden, "unbound admin session required")
	}
	var status, role string
	if err := tx.QueryRow(ctx, `SELECT status,role FROM users WHERE id=$1 FOR SHARE`, principal.UserID).Scan(&status, &role); err != nil ||
		status != string(identity.StatusApproved) || !identity.Role(role).IsAdmin() {
		return domain.NewError(domain.CodeForbidden, "admin session is unavailable")
	}
	return nil
}

func lockModelUsageAdminToken(ctx context.Context, tx pgx.Tx, principal identity.Principal) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT kind='session' AND revoked_at IS NULL AND expires_at>clock_timestamp()
 AND client_installation_id IS NULL FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, principal.TokenID, principal.UserID).Scan(&active); err != nil || !active {
		return domain.NewError(domain.CodeForbidden, "admin session is unavailable")
	}
	return nil
}

var _ app.ModelUsageAdminStore = (*Store)(nil)
