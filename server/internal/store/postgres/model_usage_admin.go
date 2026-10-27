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
	// This transaction shares the budget lock with normal settlement: the ledger,
	// rebuilt task usage, and administrator audit record either all commit or none do.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var budget int64
		if err := tx.QueryRow(ctx, `SELECT token_limit FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, command.OwnerID).Scan(&budget); err != nil {
			return storeError("lock model usage budget", "model budget", err)
		}
		if err := lockModelUsageAdminUser(ctx, tx, principal); err != nil {
			return err
		}
		var state, taskID string
		var priorInput, priorOutput *int64
		err := tx.QueryRow(ctx, `SELECT state,COALESCE(task_id,''),input_tokens,output_tokens FROM model_call_ledger
 WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, command.OwnerID, string(command.Source), command.CallID).
			Scan(&state, &taskID, &priorInput, &priorOutput)
		if err != nil {
			return storeError("lock model usage", "model call", err)
		}
		if taskID != "" {
			var location string
			if err := tx.QueryRow(ctx, `SELECT execution_location FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, command.OwnerID).Scan(&location); err != nil {
				return storeError("lock model usage task", "task", err)
			}
			if location != "server" {
				return domain.NewError(domain.CodeInvalidState, "task usage cannot be rebuilt from this ledger")
			}
		}
		if err := lockModelUsageAdminToken(ctx, tx, principal); err != nil {
			return err
		}
		target := command.OwnerID + "/" + string(command.Source) + "/" + command.CallID
		if state == "settled" {
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
		result, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='settled',input_tokens=$4,output_tokens=$5,settled_at=now()
 WHERE owner_id=$1 AND source=$2 AND call_id=$3 AND state='unknown'`, command.OwnerID, string(command.Source), command.CallID,
			command.InputTokens, command.OutputTokens)
		if err != nil || result.RowsAffected() != 1 {
			if err != nil {
				return fmt.Errorf("reconcile model ledger: %w", err)
			}
			return domain.NewError(domain.CodeConflict, "model usage changed")
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
			return domain.InvalidArgument("model_usage", "reconciled account usage is too large")
		}
		if taskID != "" {
			var unknown bool
			var total int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(bool_or(state='unknown'),false),
 COALESCE(SUM(input_tokens::numeric+output_tokens::numeric) FILTER (WHERE state='settled'),0)
 FROM model_call_ledger WHERE task_id=$1 AND owner_id=$2`, taskID, command.OwnerID).Scan(&unknown, &total); err != nil {
				return fmt.Errorf("rebuild task model usage: %w", err)
			}
			var usage any
			if !unknown {
				usage = total
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=$3,updated_at=now() WHERE id=$1 AND owner_id=$2`, taskID, command.OwnerID, usage); err != nil {
				return fmt.Errorf("update reconciled task model usage: %w", err)
			}
		}
		entry, err := auditParams(audit.Entry{
			ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionModelUsageReconcile,
			Target: target, IP: command.IP, Detail: map[string]any{
				"owner_id": command.OwnerID, "source": string(command.Source), "call_id": command.CallID,
				"input_count": command.InputTokens, "output_count": command.OutputTokens, "evidence": command.Evidence,
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
	return replayed, err
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
