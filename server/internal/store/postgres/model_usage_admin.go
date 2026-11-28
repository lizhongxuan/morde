package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
		rows, err := tx.Query(ctx, `SELECT l.owner_id,l.source,l.call_id,l.model_id,l.provider_model,COALESCE(l.task_id,''),
 l.reserved_tokens,l.created_at,COALESCE(l.settled_at,c.updated_at,s.updated_at),
 o.call_id IS NOT NULL,o.reconciliation_status,o.reason_code,o.reserved_input_tokens,o.reserved_output_tokens,
 o.actual_input_tokens,o.actual_output_tokens,o.input_overage_tokens,o.output_overage_tokens,o.project_id,o.currency,
 o.reserved_micros,o.actual_micros,o.money_overage_micros FROM model_call_ledger l
 LEFT JOIN conversation_route_model_calls c ON c.actor_id=l.owner_id AND c.source=l.source AND c.call_id=l.call_id
 LEFT JOIN conversation_message_routes r ON r.message_id=c.message_id
 LEFT JOIN conversation_compaction_operations s ON s.actor_id=l.owner_id AND s.model_call_id=l.call_id
 LEFT JOIN model_budget_overage_reconciliations o ON o.owner_id=l.owner_id AND o.source=l.source AND o.call_id=l.call_id
   AND o.reconciliation_status='open'
 WHERE (l.state='unknown' OR (l.source='compaction' AND l.state='dispatched' AND s.state='checkpointed'
   AND s.lease_expires_at IS NOT NULL AND s.lease_expires_at<=clock_timestamp()) OR
   (l.source='route' AND l.state='dispatched' AND c.state='dispatched'
   AND (r.state<>'processing' OR r.lease_expires_at IS NULL OR r.lease_expires_at<=clock_timestamp()
     OR r.lease_version<>c.dispatch_lease_version OR r.lease_owner<>c.dispatch_lease_owner)))
 AND ($1='' OR l.owner_id=$1)
 ORDER BY COALESCE(l.settled_at,c.updated_at),l.owner_id,l.source,l.call_id LIMIT $2`, ownerID, limit)
		if err != nil {
			return fmt.Errorf("list unknown model usage: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var item app.UnknownModelCall
			var overagePresent bool
			var overageStatus, overageReason, overageProjectID, overageCurrency pgtype.Text
			var reservedInput, reservedOutput, reportedInput, reportedOutput pgtype.Int8
			var inputOverage, outputOverage, reservedMicros, reportedMicros, moneyOverage pgtype.Int8
			if err := rows.Scan(&item.OwnerID, &item.Source, &item.CallID, &item.ModelID, &item.ProviderModel,
				&item.TaskID, &item.ReservedTokens, &item.CreatedAt, &item.UnknownAt,
				&overagePresent, &overageStatus, &overageReason, &reservedInput, &reservedOutput,
				&reportedInput, &reportedOutput, &inputOverage, &outputOverage, &overageProjectID, &overageCurrency,
				&reservedMicros, &reportedMicros, &moneyOverage); err != nil {
				return fmt.Errorf("read unknown model usage: %w", err)
			}
			if overagePresent {
				if !overageStatus.Valid || overageStatus.String != "open" || !overageReason.Valid ||
					!reservedInput.Valid || !reservedOutput.Valid || !reportedInput.Valid || !reportedOutput.Valid ||
					!inputOverage.Valid || !outputOverage.Valid {
					return domain.NewError(domain.CodeInternal, "adaptive model overage record is incomplete")
				}
				overage := &app.AdaptiveModelBudgetOverage{
					Status: overageStatus.String, ReasonCode: overageReason.String,
					ReservedInputTokens: reservedInput.Int64, ReservedOutputTokens: reservedOutput.Int64,
					ReportedInputTokens: reportedInput.Int64, ReportedOutputTokens: reportedOutput.Int64,
					InputOverageTokens: inputOverage.Int64, OutputOverageTokens: outputOverage.Int64,
				}
				if overageProjectID.Valid {
					overage.ProjectID = &overageProjectID.String
				}
				if overageCurrency.Valid {
					overage.Currency = &overageCurrency.String
				}
				if reservedMicros.Valid {
					overage.ReservedMicros = &reservedMicros.Int64
				}
				if reportedMicros.Valid {
					overage.ReportedMicros = &reportedMicros.Int64
				}
				if moneyOverage.Valid {
					overage.MoneyOverageMicros = &moneyOverage.Int64
				}
				item.AdaptiveBudgetOverage = overage
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
 COALESCE((SELECT SUM(CASE WHEN l.state='settled' THEN l.input_tokens::numeric+l.output_tokens::numeric WHEN l.state='rejected' THEN 0 ELSE l.reserved_tokens::numeric END)
   FROM model_call_ledger l WHERE l.owner_id=u.id AND NOT l.personal_model),0),
  COALESCE((SELECT count(*) FROM model_call_ledger l WHERE l.owner_id=u.id AND NOT l.personal_model AND l.state='dispatched'),0),
 COALESCE((SELECT bool_or(l.state='unknown') FROM model_call_ledger l WHERE l.owner_id=u.id AND NOT l.personal_model),false)
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
	// Project charges, when present, settle together with the model ledger.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, command.OwnerID); err != nil {
			return err
		}
		personal, err := modelCallIsPersonal(ctx, tx, command.OwnerID, command.Source, command.CallID)
		if err != nil {
			return err
		}
		if !personal {
			if _, _, err := lockPlatformModelBudget(ctx, tx, command.OwnerID); err != nil {
				return err
			}
		}
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		var state, taskID, modelID string
		var priorInput, priorOutput *int64
		var accountReserved int64
		var accountDigest []byte
		var reviewTaskRevision, reviewResultSequence, reviewClaimGeneration *int64
		var reviewResultSHA256 []byte
		var terminalDeliveryID, terminalDeliveryClaimGeneration *int64
		var terminalDeliveryDispatchStarted bool
		err = tx.QueryRow(ctx, `SELECT state,COALESCE(task_id,''),model_id,reserved_tokens,input_tokens,output_tokens,request_digest,
review_task_revision,review_result_event_sequence,review_result_sha256,review_claim_generation,
terminal_delivery_id,terminal_delivery_claim_generation,terminal_delivery_dispatch_started FROM model_call_ledger
 WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, command.OwnerID, string(command.Source), command.CallID).
			Scan(&state, &taskID, &modelID, &accountReserved, &priorInput, &priorOutput, &accountDigest,
				&reviewTaskRevision, &reviewResultSequence, &reviewResultSHA256, &reviewClaimGeneration,
				&terminalDeliveryID, &terminalDeliveryClaimGeneration, &terminalDeliveryDispatchStarted)
		if err != nil {
			return storeError("lock model usage", "model call", err)
		}
		if command.Source == app.ModelCallReview && (taskID == "" || reviewTaskRevision == nil || reviewResultSequence == nil ||
			reviewClaimGeneration == nil || *reviewTaskRevision < 0 || *reviewResultSequence <= 0 || *reviewClaimGeneration <= 0 ||
			len(reviewResultSHA256) != 32) {
			return domain.NewError(domain.CodeInvalidState, "Task validation review model attribution is unavailable")
		}
		if command.Source == app.ModelCallTerminalDelivery {
			matches, err := taskTerminalDeliveryUnknownUsageAttributionMatches(ctx, tx, command, taskID,
				terminalDeliveryID, terminalDeliveryClaimGeneration, terminalDeliveryDispatchStarted)
			if err != nil {
				return err
			}
			if !matches {
				return domain.NewError(domain.CodeInvalidState, "Task terminal delivery model call attribution is unavailable")
			}
		}
		var taskOwnerID, taskProjectID string
		switch command.Source {
		case app.ModelCallRoute:
			var routeState, routeMessageID string
			var abandoned bool
			if taskID != "" {
				return domain.NewError(domain.CodeInvalidState, "route model call has unexpected task attribution")
			}
			if err := tx.QueryRow(ctx, `SELECT COALESCE(c.project_id,''),c.state,c.message_id,
 (r.state<>'processing' OR r.lease_expires_at IS NULL OR r.lease_expires_at<=clock_timestamp()
  OR r.lease_version<>c.dispatch_lease_version OR r.lease_owner<>c.dispatch_lease_owner)
FROM conversation_route_model_calls c JOIN conversation_message_routes r ON r.message_id=c.message_id
WHERE c.actor_id=$1 AND c.source='route' AND c.call_id=$2 FOR UPDATE OF c,r`, command.OwnerID, command.CallID).
				Scan(&taskProjectID, &routeState, &routeMessageID, &abandoned); err != nil ||
				(routeState != "unknown" && (routeState != "dispatched" || !abandoned)) {
				return domain.NewError(domain.CodeInvalidState, "unknown route model attribution is unavailable")
			}
			if taskProjectID != "" {
				if err := lockProjectByIDInTx(ctx, tx, taskProjectID); err != nil {
					return err
				}
			}
			if routeState == "dispatched" {
				if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
					return err
				}
				if err := markAbandonedRouteCallUnknown(ctx, tx, routeMessageID, command.OwnerID, taskProjectID, command.CallID); err != nil {
					return err
				}
				state = "unknown"
			}
		case app.ModelCallCompaction:
			var scopeKind, scopeID, operationState string
			var leaseExpired bool
			if taskID != "" {
				return domain.NewError(domain.CodeInvalidState, "unknown conversation summary model attribution is unavailable")
			}
			if err := tx.QueryRow(ctx, `SELECT scope_kind,scope_id,state,
lease_expires_at IS NOT NULL AND lease_expires_at<=clock_timestamp() FROM conversation_compaction_operations
WHERE actor_id=$1 AND model_call_id=$2 FOR UPDATE`, command.OwnerID, command.CallID).
				Scan(&scopeKind, &scopeID, &operationState, &leaseExpired); err != nil {
				return domain.NewError(domain.CodeInvalidState, "unknown conversation summary model attribution is unavailable")
			}
			if scopeKind == app.ConversationSummaryProjectScope {
				if !project.ValidID(scopeID) {
					return domain.NewError(domain.CodeInvalidState, "conversation summary project attribution is invalid")
				}
				taskProjectID = scopeID
				if err := lockProjectByIDInTx(ctx, tx, taskProjectID); err != nil {
					return err
				}
			} else if scopeKind != app.ConversationSummaryPersonalScope || scopeID != "" {
				return domain.NewError(domain.CodeInvalidState, "conversation summary scope attribution is invalid")
			}
			if state == "dispatched" && operationState == "checkpointed" && leaseExpired {
				if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
					return err
				}
				var hasProjectCharge bool
				if taskProjectID != "" {
					if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_budget_ledger
WHERE project_id=$1 AND source='compaction' AND call_id=$2)`, taskProjectID, command.CallID).Scan(&hasProjectCharge); err != nil {
						return fmt.Errorf("check stale conversation summary project charge: %w", err)
					}
				}
				if hasProjectCharge {
					if err := markProjectModelCallUnknown(ctx, tx, taskProjectID, command.OwnerID, string(command.Source), command.CallID, ""); err != nil {
						return err
					}
				} else if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source='compaction' AND call_id=$2 AND state='dispatched'`, command.OwnerID, command.CallID); err != nil {
					return fmt.Errorf("mark stale conversation summary model usage unknown: %w", err)
				}
				if _, err := tx.Exec(ctx, `UPDATE conversation_compaction_operations SET state='unknown',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE actor_id=$1 AND model_call_id=$2 AND state='checkpointed'`, command.OwnerID, command.CallID); err != nil {
					return fmt.Errorf("mark stale conversation summary operation unknown: %w", err)
				}
				if _, err := tx.Exec(ctx, `UPDATE conversation_summary_heads h SET lease_operation_id='',lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() FROM conversation_compaction_operations o
WHERE o.actor_id=$1 AND o.model_call_id=$2 AND h.service_instance_id=o.service_instance_id AND h.owner_id=o.owner_id
 AND h.actor_id=o.actor_id AND h.conversation_id=o.conversation_id AND h.scope_kind=o.scope_kind AND h.scope_id=o.scope_id
 AND h.lease_operation_id=o.operation_id`, command.OwnerID, command.CallID); err != nil {
					return fmt.Errorf("release stale conversation summary lease: %w", err)
				}
				state, operationState = "unknown", "unknown"
			}
			if (state != "unknown" || operationState != "unknown") && (state != "settled" || operationState != "failed") {
				return domain.NewError(domain.CodeInvalidState, "conversation summary model call is still active")
			}
		}
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
				if command.Source != app.ModelCallTask && command.Source != app.ModelCallReview &&
					command.Source != app.ModelCallTerminalDelivery {
					return domain.NewError(domain.CodeInvalidState, "project usage cannot be reconciled from this ledger")
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
			if command.Source == app.ModelCallReview {
				var reviewOwnerID string
				var reviewRevision, reviewSequence, reviewGeneration int64
				var reviewDigest []byte
				if err := tx.QueryRow(ctx, `SELECT owner_id,task_revision,result_event_sequence,result_sha256,claim_generation
FROM task_validation_reviews WHERE task_id=$1 AND task_revision=$2 FOR SHARE`, taskID, *reviewTaskRevision).
					Scan(&reviewOwnerID, &reviewRevision, &reviewSequence, &reviewDigest, &reviewGeneration); err != nil ||
					reviewOwnerID != taskOwnerID || reviewRevision != *reviewTaskRevision || reviewSequence != *reviewResultSequence ||
					!bytes.Equal(reviewDigest, reviewResultSHA256) || reviewGeneration < *reviewClaimGeneration {
					return domain.NewError(domain.CodeInvalidState, "Task validation review ledger attribution does not match its Task")
				}
			}
		}
		var projectCall *modelUsageProjectCall
		if taskProjectID != "" {
			var hasProjectCharge bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3)`,
				taskProjectID, string(command.Source), command.CallID).Scan(&hasProjectCharge); err != nil {
				return fmt.Errorf("check project model charge: %w", err)
			}
			if !personal || hasProjectCharge {
				call, err := lockProjectModelCallForReconciliation(ctx, tx, taskProjectID, command, taskID, modelID, accountDigest)
				if err != nil {
					return err
				}
				projectCall = &call
			}
		}
		overage, err := lockAdaptiveModelBudgetOverageForReconciliation(ctx, tx, command, taskID, taskProjectID, accountReserved, projectCall)
		if err != nil {
			return err
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		target := command.OwnerID + "/" + string(command.Source) + "/" + command.CallID
		if state == "settled" {
			if overage != nil && (overage.Status != "resolved" || overage.ResolvedBy != principal.UserID ||
				overage.ResolutionEvidence != command.Evidence || overage.VerifiedInputTokens == nil ||
				overage.VerifiedOutputTokens == nil || *overage.VerifiedInputTokens != command.InputTokens ||
				*overage.VerifiedOutputTokens != command.OutputTokens) {
				return domain.NewError(domain.CodeIdempotencyConflict, "adaptive model overage was already resolved differently")
			}
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
		resolveOverage := overage != nil && overage.Status == "open"
		var verifiedProjectMicros any
		if projectCall != nil {
			if projectCall.Status != "unknown" {
				return domain.NewError(domain.CodeInvalidState, "project and account model ledgers do not match")
			}
			actualMicros, estimateErr := projectCall.Rate.EstimateTokenCost(project.Usage{InputTokens: command.InputTokens, OutputTokens: command.OutputTokens})
			if estimateErr != nil && resolveOverage {
				return domain.NewError(domain.CodeInvalidState, "verified overage cost cannot be represented")
			}
			if estimateErr == nil {
				verifiedProjectMicros = actualMicros
			}
			if estimateErr != nil || !resolveOverage && (command.InputTokens+command.OutputTokens > accountReserved ||
				command.InputTokens > projectCall.ReservedInput || command.OutputTokens > projectCall.ReservedOutput ||
				actualMicros > projectCall.ReservedMicros) {
				// Keep both holds unknown when any token or money reservation is too
				// small. The evidence audit below still commits with this decision,
				// except for an adaptive Task overage with an explicit invoice-backed
				// reconciliation record.
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
 THEN input_tokens::numeric+output_tokens::numeric WHEN state='rejected' THEN 0 ELSE reserved_tokens::numeric END),0) <= 9223372036854775807
 FROM model_call_ledger WHERE owner_id=$1 AND NOT personal_model`, command.OwnerID).Scan(&accountTotalFits); err != nil {
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
		if resolveOverage && !quotaExceeded {
			result, err := tx.Exec(ctx, `UPDATE model_budget_overage_reconciliations
SET reconciliation_status='resolved',resolved_at=clock_timestamp(),resolved_by=$4,resolution_evidence=$5,
    verified_input_tokens=$6,verified_output_tokens=$7,verified_micros=$8
WHERE owner_id=$1 AND source='task' AND call_id=$2 AND reconciliation_status='open'
 AND task_id=$3`, command.OwnerID, command.CallID, taskID, principal.UserID, command.Evidence,
				command.InputTokens, command.OutputTokens, verifiedProjectMicros)
			if err != nil {
				return fmt.Errorf("resolve adaptive model overage record: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "adaptive model overage changed before reconciliation")
			}
		}
		if command.Source == app.ModelCallCompaction && !quotaExceeded {
			if err := finishReconciledConversationSummaryModelCallInTx(ctx, tx, command.OwnerID, command.CallID); err != nil {
				return err
			}
		}
		if taskID != "" && command.Source == app.ModelCallTask {
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
				"adaptive_budget_overage_resolved": resolveOverage && !quotaExceeded,
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

type adaptiveModelBudgetOverageAdminRecord struct {
	Status               string
	PlanID               string
	BudgetCallID         string
	TaskID               string
	ReasonCode           string
	ReservedInputTokens  int64
	ReservedOutputTokens int64
	ReportedInputTokens  int64
	ReportedOutputTokens int64
	ProjectID            string
	Currency             *string
	ReservedMicros       *int64
	ReportedMicros       *int64
	MoneyOverageMicros   *int64
	ResolvedBy           string
	ResolutionEvidence   string
	VerifiedInputTokens  *int64
	VerifiedOutputTokens *int64
	VerifiedMicros       *int64
}

func lockAdaptiveModelBudgetOverageForReconciliation(ctx context.Context, tx pgx.Tx,
	command app.ModelUsageReconcileCommand, taskID, taskProjectID string, accountReserved int64,
	projectCall *modelUsageProjectCall) (*adaptiveModelBudgetOverageAdminRecord, error) {
	var record adaptiveModelBudgetOverageAdminRecord
	var currency, resolvedBy, evidence pgtype.Text
	var reservedMicros, reportedMicros, moneyOverage pgtype.Int8
	var verifiedInput, verifiedOutput, verifiedMicros pgtype.Int8
	err := tx.QueryRow(ctx, `SELECT reconciliation_status,plan_id,budget_call_id,task_id,reason_code,
reserved_input_tokens,reserved_output_tokens,actual_input_tokens,actual_output_tokens,COALESCE(project_id,''),currency,
reserved_micros,actual_micros,money_overage_micros,resolved_by,resolution_evidence,
verified_input_tokens,verified_output_tokens,verified_micros
FROM model_budget_overage_reconciliations WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`,
		command.OwnerID, string(command.Source), command.CallID).Scan(
		&record.Status, &record.PlanID, &record.BudgetCallID, &record.TaskID, &record.ReasonCode,
		&record.ReservedInputTokens, &record.ReservedOutputTokens, &record.ReportedInputTokens, &record.ReportedOutputTokens,
		&record.ProjectID, &currency, &reservedMicros, &reportedMicros, &moneyOverage,
		&resolvedBy, &evidence, &verifiedInput, &verifiedOutput, &verifiedMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock adaptive model overage for reconciliation: %w", err)
	}
	if command.Source != app.ModelCallTask || taskID == "" || record.TaskID != taskID ||
		record.Status != "open" && record.Status != "resolved" ||
		record.ReservedInputTokens > math.MaxInt64-record.ReservedOutputTokens ||
		record.ReservedInputTokens+record.ReservedOutputTokens != accountReserved ||
		record.ReasonCode != modelBudgetOverageReason(record.ReportedInputTokens, record.ReportedOutputTokens,
			record.ReservedInputTokens, record.ReservedOutputTokens) {
		return nil, domain.NewError(domain.CodeInvalidState, "adaptive model overage does not match its unknown Task call")
	}
	var planID, budgetCallID string
	var inputReserved, outputReserved int64
	if err := tx.QueryRow(ctx, `SELECT plan_id,budget_call_id,input_reserved_tokens,send_tokens
FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2 FOR SHARE`,
		command.OwnerID, command.CallID).Scan(&planID, &budgetCallID, &inputReserved, &outputReserved); err != nil ||
		planID != record.PlanID || budgetCallID != record.BudgetCallID ||
		inputReserved != record.ReservedInputTokens || outputReserved != record.ReservedOutputTokens {
		return nil, domain.NewError(domain.CodeConflict, "adaptive model overage no longer matches its signed plan")
	}
	if projectCall == nil {
		if record.ProjectID != "" || currency.Valid || reservedMicros.Valid || reportedMicros.Valid || moneyOverage.Valid {
			return nil, domain.NewError(domain.CodeInvalidState, "adaptive model overage has unexpected project charges")
		}
	} else {
		if record.ProjectID != taskProjectID || !currency.Valid || currency.String != projectCall.Rate.Currency ||
			!reservedMicros.Valid || reservedMicros.Int64 != projectCall.ReservedMicros {
			return nil, domain.NewError(domain.CodeInvalidState, "adaptive model overage project attribution changed")
		}
		reportedCost, estimateErr := projectCall.Rate.EstimateTokenCost(project.Usage{
			InputTokens: record.ReportedInputTokens, OutputTokens: record.ReportedOutputTokens,
		})
		if estimateErr != nil || !reportedMicros.Valid || reportedMicros.Int64 != reportedCost || !moneyOverage.Valid ||
			moneyOverage.Int64 != max(reportedCost-projectCall.ReservedMicros, 0) {
			return nil, domain.NewError(domain.CodeInvalidState, "adaptive model overage project amounts changed")
		}
		record.Currency = &currency.String
		record.ReservedMicros = &reservedMicros.Int64
		record.ReportedMicros = &reportedMicros.Int64
		record.MoneyOverageMicros = &moneyOverage.Int64
	}
	if record.Status == "open" {
		if resolvedBy.Valid || evidence.Valid || verifiedInput.Valid || verifiedOutput.Valid || verifiedMicros.Valid {
			return nil, domain.NewError(domain.CodeInvalidState, "open adaptive model overage already has resolution details")
		}
	} else {
		if !resolvedBy.Valid || !evidence.Valid || !verifiedInput.Valid || !verifiedOutput.Valid ||
			(record.ProjectID == "") != !verifiedMicros.Valid {
			return nil, domain.NewError(domain.CodeInvalidState, "resolved adaptive model overage is missing audit details")
		}
		record.ResolvedBy, record.ResolutionEvidence = resolvedBy.String, evidence.String
		record.VerifiedInputTokens, record.VerifiedOutputTokens = &verifiedInput.Int64, &verifiedOutput.Int64
		if verifiedMicros.Valid {
			record.VerifiedMicros = &verifiedMicros.Int64
		}
	}
	return &record, nil
}

// taskTerminalDeliveryUnknownUsageAttributionMatches binds an admin usage
// reconciliation to the durable source row that owns the stable model call ID.
// It intentionally does not require the source to remain publishable: an
// already-dispatched provider charge still needs reconciliation after a newer
// user message or authorization change invalidates the reply. The source read
// is non-locking so it does not invert the terminal worker's outbox-to-ledger
// lock order.
func taskTerminalDeliveryUnknownUsageAttributionMatches(ctx context.Context, tx pgx.Tx,
	command app.ModelUsageReconcileCommand, taskID string, deliveryID, dispatchGeneration *int64,
	dispatchStarted bool) (bool, error) {
	if command.Source != app.ModelCallTerminalDelivery || !domain.ValidTaskID(taskID) || deliveryID == nil || *deliveryID < 1 ||
		dispatchGeneration == nil || *dispatchGeneration < 1 || !dispatchStarted ||
		command.CallID != "terminal_delivery_"+strconv.FormatInt(*deliveryID, 10) {
		return false, nil
	}
	var storedTaskID, initiatorID, modelCallID string
	var currentGeneration int64
	err := tx.QueryRow(ctx, `SELECT delivery.task_id,delivery.initiator_id,delivery.model_call_id,delivery.claim_generation
FROM task_terminal_delivery_outbox delivery
JOIN tasks task ON task.id=delivery.task_id AND task.owner_id=delivery.owner_id
WHERE delivery.id=$1 AND task.initiator_id=delivery.initiator_id`, *deliveryID).
		Scan(&storedTaskID, &initiatorID, &modelCallID, &currentGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verify Task terminal delivery model usage source: %w", err)
	}
	return storedTaskID == taskID && initiatorID == command.OwnerID && modelCallID == command.CallID &&
		currentGeneration >= *dispatchGeneration, nil
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
	err := tx.QueryRow(ctx, `SELECT actor_id,COALESCE(task_id,''),model_id,status,currency,rate_provider,rate_model,rate_version,
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
