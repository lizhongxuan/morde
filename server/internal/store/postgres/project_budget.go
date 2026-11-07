package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

const (
	projectBudgetDefaultPageSize int32 = 50
	projectBudgetMaxPageSize     int32 = 100
)

type projectBudgetLedgerCursor struct {
	ProjectID string    `json:"project_id"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source"`
	CallID    string    `json:"call_id"`
}

type projectBudgetTotals struct {
	SpentMicros    int64
	ReservedMicros int64
	HasUnknown     bool
	LedgerCount    int64
	LedgerCurrency string
}

var _ app.ProjectBudgetStore = (*Store)(nil)

// GetProjectBudget uses a transaction so that membership and the returned
// budget snapshot are checked against one fresh database view.
func (s *Store) GetProjectBudget(ctx context.Context, actorID, projectID string) (app.ProjectBudgetView, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return app.ProjectBudgetView{}, projectNotFound()
	}
	var result app.ProjectBudgetView
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		budget, configured, err := loadCurrentProjectBudgetInTx(ctx, tx, projectID)
		if err != nil {
			return err
		}
		result, err = projectBudgetViewInTx(ctx, tx, projectID, budget, configured)
		return err
	})
	if err != nil {
		return app.ProjectBudgetView{}, err
	}
	return result, nil
}

// SetProjectBudget serializes project budget writers on the project row and
// writes both the version row and idempotency receipt in the same transaction.
func (s *Store) SetProjectBudget(ctx context.Context, actorID, projectID string, budget project.Budget, expectedVersion int64, idempotencyKey string) (app.ProjectBudgetView, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return app.ProjectBudgetView{}, projectNotFound()
	}
	if expectedVersion < 0 || expectedVersion == int64(^uint64(0)>>1) || budget.Version != expectedVersion+1 ||
		budget.Validate() != nil || !validProjectIdempotencyKey(idempotencyKey) {
		return app.ProjectBudgetView{}, domain.InvalidArgument("budget", "project budget fields are invalid")
	}
	digest, err := projectRequestDigest(struct {
		ProjectID       string
		ActorID         string
		Currency        string
		LimitMicros     int64
		ExpectedVersion int64
	}{projectID, actorID, budget.Currency, budget.LimitMicros, expectedVersion})
	if err != nil {
		return app.ProjectBudgetView{}, err
	}

	var result app.ProjectBudgetView
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleOwner, true); err != nil {
			return err
		}

		receipt, replay, err := findProjectIdempotencyInTx(ctx, tx, actorID, "set_budget", idempotencyKey, projectID, digest)
		if err != nil {
			return err
		}
		if replay {
			if receipt.Kind != "budget" || receipt.ID != projectID {
				return domain.NewError(domain.CodeInternal, "project budget idempotency receipt is invalid")
			}
			stored, err := loadProjectBudgetVersionInTx(ctx, tx, projectID, receipt.Version)
			if err != nil {
				return err
			}
			result, err = projectBudgetViewInTx(ctx, tx, projectID, stored, true)
			return err
		}

		current, configured, err := loadCurrentProjectBudgetInTx(ctx, tx, projectID)
		if err != nil {
			return err
		}
		currentVersion := int64(0)
		if configured {
			currentVersion = current.Version
		}
		if currentVersion != expectedVersion {
			return projectBudgetVersionConflict(expectedVersion, currentVersion)
		}

		totals, err := loadProjectBudgetTotalsInTx(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if err := validateProjectBudgetTransition(current, configured, budget, totals); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `INSERT INTO project_budgets
(project_id,version,currency,limit_micros,updated_by,idempotency_key,request_digest)
VALUES ($1,$2,$3,$4,$5,$6,$7)`, projectID, budget.Version, budget.Currency, budget.LimitMicros, actorID, idempotencyKey, digest); err != nil {
			if isProjectUniqueViolation(err) {
				return domain.NewError(domain.CodeIdempotencyConflict, "project budget idempotency key was already used")
			}
			return fmt.Errorf("save project budget: %w", err)
		}
		if err := insertProjectIdempotencyInTx(ctx, tx, actorID, "set_budget", idempotencyKey,
			projectID, "budget", projectID, budget.Version, digest); err != nil {
			return err
		}
		result, err = projectBudgetViewInTx(ctx, tx, projectID, budget, true)
		return err
	})
	if err != nil {
		return app.ProjectBudgetView{}, err
	}
	return result, nil
}

// ListProjectLedgerEntries exposes attribution and accounting metadata only.
func (s *Store) ListProjectLedgerEntries(ctx context.Context, actorID, projectID string, page app.PageRequest) (app.ProjectLedgerPage, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) {
		return app.ProjectLedgerPage{}, projectNotFound()
	}
	if page.Limit == 0 {
		page.Limit = projectBudgetDefaultPageSize
	}
	if page.Limit < 1 || page.Limit > projectBudgetMaxPageSize || len(page.Cursor) > 1024 {
		return app.ProjectLedgerPage{}, domain.InvalidArgument("limit", "project ledger page request is out of range")
	}
	cursor, err := decodeProjectBudgetLedgerCursor(page.Cursor, projectID)
	if err != nil {
		return app.ProjectLedgerPage{}, err
	}

	result := app.ProjectLedgerPage{Items: make([]app.ProjectLedgerEntry, 0)}
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		var afterTime any
		var afterSource any
		var afterCallID any
		if cursor != nil {
			afterTime, afterSource, afterCallID = cursor.CreatedAt, cursor.Source, cursor.CallID
		}
		rows, err := tx.Query(ctx, `SELECT source,call_id,actor_id,task_id,model_id,rate_version,currency,status,
reserved_input_tokens,reserved_output_tokens,reserved_micros,
actual_input_tokens,actual_output_tokens,actual_micros,created_at
FROM project_budget_ledger
WHERE project_id=$1 AND ($2::timestamptz IS NULL OR created_at < $2::timestamptz OR
  (created_at=$2::timestamptz AND (source>$3::text OR (source=$3::text AND call_id<$4::text))))
ORDER BY created_at DESC,source ASC,call_id DESC LIMIT $5`, projectID, afterTime, afterSource, afterCallID, page.Limit+1)
		if err != nil {
			return fmt.Errorf("list project budget ledger: %w", err)
		}
		defer rows.Close()
		stored := make([]app.ProjectLedgerEntry, 0, page.Limit+1)
		for rows.Next() {
			var entry app.ProjectLedgerEntry
			var source, status string
			var taskID pgtype.Text
			var actualInput, actualOutput, actualMicros pgtype.Int8
			if err := rows.Scan(&source, &entry.CallID, &entry.ActorID, &taskID, &entry.ModelID, &entry.RateVersion,
				&entry.Currency, &status, &entry.ReservedUsage.InputTokens, &entry.ReservedUsage.OutputTokens,
				&entry.ReservedMicros, &actualInput, &actualOutput, &actualMicros, &entry.CreatedAt); err != nil {
				return fmt.Errorf("scan project budget ledger: %w", err)
			}
			entry.Source = app.ModelCallSource(source)
			entry.Status = app.ProjectLedgerStatus(status)
			if taskID.Valid {
				entry.TaskID = taskID.String
			}
			if actualInput.Valid || actualOutput.Valid || actualMicros.Valid {
				if !actualInput.Valid || !actualOutput.Valid || !actualMicros.Valid {
					return domain.NewError(domain.CodeInternal, "project budget ledger usage columns are inconsistent")
				}
				usage := project.Usage{InputTokens: actualInput.Int64, OutputTokens: actualOutput.Int64}
				amount := actualMicros.Int64
				entry.ActualUsage, entry.ActualMicros = &usage, &amount
			}
			stored = append(stored, entry)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate project budget ledger: %w", err)
		}
		if int32(len(stored)) > page.Limit {
			stored = stored[:page.Limit]
			last := stored[len(stored)-1]
			result.NextCursor, err = encodeProjectBudgetLedgerCursor(projectBudgetLedgerCursor{
				ProjectID: projectID, CreatedAt: last.CreatedAt, Source: string(last.Source), CallID: last.CallID,
			})
			if err != nil {
				return fmt.Errorf("encode project budget ledger cursor: %w", err)
			}
		}
		result.Items = append(result.Items, stored...)
		return nil
	})
	if err != nil {
		return app.ProjectLedgerPage{}, err
	}
	return result, nil
}

// ReserveProjectModelCall writes the project money hold and account token hold
// together so a failed budget check cannot leave either ledger charged alone.
func (s *Store) ReserveProjectModelCall(ctx context.Context, reservation app.ProjectModelReservation) (app.ProjectModelReservation, bool, error) {
	call := reservation.AccountCall
	if reservation.Source == app.ModelCallRoute || !project.ValidID(reservation.ProjectID) || !domain.ValidUserID(reservation.ActorID) ||
		call.PersonalModel ||
		!validProjectBudgetSource(string(reservation.Source)) ||
		(reservation.Source == app.ModelCallTask && !domain.ValidTaskID(reservation.TaskID)) ||
		(reservation.Source != app.ModelCallTask && reservation.TaskID != "") ||
		!validProjectIdempotencyKey(reservation.CallID) || !domain.ValidModelConnectionID(reservation.ModelID) ||
		!validProjectTokenReservation(reservation.Usage) || reservation.ReservedTokens != reservation.Usage.InputTokens+reservation.Usage.OutputTokens ||
		reservation.Rate.Validate() != nil || reservation.Rate.ModelID != reservation.ModelID ||
		reservation.Rate.Currency == "" || reservation.ReservedMicros < 1 || len(reservation.RequestDigest) != 32 ||
		call.Source != reservation.Source || call.ProjectID != reservation.ProjectID || call.OwnerID != reservation.ActorID ||
		(reservation.Source == app.ModelCallTask && (call.Task == nil || call.Task.InitiatorID != reservation.ActorID || call.Task.TaskID != reservation.TaskID)) ||
		(reservation.Source != app.ModelCallTask && call.Task != nil) ||
		call.CallID != reservation.CallID || call.ModelID != reservation.ModelID || call.ProviderModel != reservation.Rate.Model ||
		call.ReservedTokens != reservation.ReservedTokens || call.ProviderModel == "" || len(call.ProviderModel) > 256 ||
		call.ReservedTokens <= 0 || len(call.RequestDigest) != 32 {
		return app.ProjectModelReservation{}, false, domain.InvalidArgument("project_model_call", "project model call reservation is invalid")
	}
	if cost, err := reservation.Rate.EstimateTokenCost(reservation.Usage); err != nil || cost != reservation.ReservedMicros {
		return app.ProjectModelReservation{}, false, domain.InvalidArgument("project_model_call", "project model call rate snapshot is invalid")
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		return reserveModelCallInTx(ctx, tx, call, &reservation)
	})
	if err != nil {
		return app.ProjectModelReservation{}, false, err
	}
	return reservation, false, nil
}

func validProjectTokenReservation(value project.Usage) bool {
	return value.InputTokens >= 0 && value.OutputTokens > 0 && value.InputTokens <= int64(^uint64(0)>>1)-value.OutputTokens
}

func validProjectSettlementUsage(value project.Usage) bool {
	return value.InputTokens >= 0 && value.OutputTokens >= 0 && value.InputTokens <= int64(^uint64(0)>>1)-value.OutputTokens &&
		(value.InputTokens > 0 || value.OutputTokens > 0)
}

func reserveProjectBudgetLedgerInTx(ctx context.Context, tx pgx.Tx, reservation app.ProjectModelReservation) error {
	if _, _, err := lockProjectForActorInTx(ctx, tx, reservation.ActorID, reservation.ProjectID, project.RoleMember, false); err != nil {
		return err
	}
	if reservation.Source == app.ModelCallTask {
		var taskOwnerID, taskInitiatorID, taskProjectID string
		if err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id,COALESCE(project_id,'') FROM tasks WHERE id=$1 FOR SHARE`, reservation.TaskID).
			Scan(&taskOwnerID, &taskInitiatorID, &taskProjectID); err != nil || taskProjectID != reservation.ProjectID ||
			taskInitiatorID != reservation.ActorID || taskOwnerID != reservation.AccountCall.Task.OwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "project Task attribution is unavailable")
		}
	} else if reservation.Source == app.ModelCallRoute {
		if reservation.AccountCall.Route == nil || reservation.AccountCall.Route.Claim.Message.ActorID != reservation.ActorID ||
			reservation.AccountCall.Route.Claim.Input.ProjectID != reservation.ProjectID {
			return domain.NewError(domain.CodeUnauthenticated, "project route model caller is unavailable")
		}
	} else if reservation.Source != app.ModelCallClient && reservation.Source != app.ModelCallChat ||
		reservation.AccountCall.Principal.UserID != reservation.ActorID {
		return domain.NewError(domain.CodeUnauthenticated, "project model caller is unavailable")
	}
	var priorDigest []byte
	err := tx.QueryRow(ctx, `SELECT request_digest FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`,
		reservation.ProjectID, string(reservation.Source), reservation.CallID).Scan(&priorDigest)
	if err == nil {
		if string(priorDigest) != string(reservation.RequestDigest) {
			return domain.NewError(domain.CodeIdempotencyConflict, "project model call key was used with different content")
		}
		return domain.NewError(domain.CodeConflict, "project model call was already dispatched; reconcile its result")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("find project model call: %w", err)
	}
	var budget project.Budget
	err = tx.QueryRow(ctx, `SELECT currency,limit_micros,version FROM project_budgets
WHERE project_id=$1 ORDER BY version DESC LIMIT 1 FOR UPDATE`, reservation.ProjectID).
		Scan(&budget.Currency, &budget.LimitMicros, &budget.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeQuotaExceeded, "project model budget is not configured")
	}
	if err != nil {
		return fmt.Errorf("lock project model budget: %w", err)
	}
	if budget.Validate() != nil {
		return domain.NewError(domain.CodeInternal, "stored project budget is invalid")
	}
	if budget.Currency != reservation.Rate.Currency {
		return domain.NewError(domain.CodeInvalidState, "project model rate currency does not match the budget")
	}
	totals, err := loadProjectBudgetTotalsInTx(ctx, tx, reservation.ProjectID)
	if err != nil {
		return err
	}
	if totals.HasUnknown {
		return domain.NewError(domain.CodeInvalidState, "project model usage is unknown and requires reconciliation")
	}
	if totals.LedgerCount > 0 && totals.LedgerCurrency != budget.Currency {
		return domain.NewError(domain.CodeInternal, "project budget currency does not match its ledger")
	}
	if totals.SpentMicros > budget.LimitMicros || totals.ReservedMicros > budget.LimitMicros-totals.SpentMicros ||
		reservation.ReservedMicros > budget.LimitMicros-totals.SpentMicros-totals.ReservedMicros {
		return domain.NewError(domain.CodeQuotaExceeded, "project money budget exceeded")
	}
	_, err = tx.Exec(ctx, `INSERT INTO project_budget_ledger
(project_id,source,call_id,actor_id,task_id,model_id,rate_provider,rate_model,rate_version,currency,
 input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,
 status,request_digest)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'reserved',$16)`,
		reservation.ProjectID, string(reservation.Source), reservation.CallID, reservation.ActorID, projectLedgerTaskID(reservation),
		reservation.ModelID, reservation.Rate.Provider, reservation.Rate.Model, reservation.Rate.Version, reservation.Rate.Currency,
		reservation.Rate.InputMicrosPerMillion, reservation.Rate.OutputMicrosPerMillion,
		reservation.Usage.InputTokens, reservation.Usage.OutputTokens, reservation.ReservedMicros, reservation.RequestDigest)
	if err != nil {
		return fmt.Errorf("reserve project model call: %w", err)
	}
	return nil
}

func projectLedgerTaskID(reservation app.ProjectModelReservation) any {
	if reservation.Source == app.ModelCallTask {
		return reservation.TaskID
	}
	return nil
}

// SettleProjectModelCall reconciles both project money and account token
// ledgers, keeping both holds unknown when the actual result is unsafe.
func (s *Store) SettleProjectModelCall(ctx context.Context, settlement app.SettleProjectModelCallCommand) error {
	if ctx == nil || settlement.Source == app.ModelCallRoute || !project.ValidID(settlement.ProjectID) || !validProjectBudgetSource(string(settlement.Source)) ||
		!validProjectIdempotencyKey(settlement.CallID) || (!settlement.Unknown && !validProjectSettlementUsage(settlement.Usage)) {
		return domain.InvalidArgument("project_model_settlement", "invalid project model settlement")
	}
	var actorID string
	if err := s.pool.QueryRow(ctx, `SELECT actor_id FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3`,
		settlement.ProjectID, string(settlement.Source), settlement.CallID).Scan(&actorID); err != nil {
		settleErr := storeError("find project model settlement", "project model call", err)
		if !settlement.Unknown {
			if holdErr := s.bestEffortUnknownProjectModelCall(ctx, settlement, ""); holdErr != nil {
				return fmt.Errorf("%w; best-effort unknown hold failed: %w", settleErr, holdErr)
			}
		}
		return settleErr
	}
	var overBudget bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, actorID); err != nil {
			return err
		}
		var accountID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, actorID).Scan(&accountID); err != nil {
			return storeError("lock project model settlement account budget", "model budget", err)
		}
		if err := lockProjectByIDInTx(ctx, tx, settlement.ProjectID); err != nil {
			return err
		}
		var projectActorID, modelID, projectState, currency, rateProvider, rateModel string
		var source string
		var taskID pgtype.Text
		var rateVersion, inputRate, outputRate, reservedInput, reservedOutput, reservedMicros int64
		var projectInput, projectOutput, projectMicros, accountInput, accountOutput *int64
		var projectRequestDigest, accountRequestDigest []byte
		if err := tx.QueryRow(ctx, `SELECT actor_id,task_id,model_id,status,currency,rate_provider,rate_model,rate_version,
input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,
actual_input_tokens,actual_output_tokens,actual_micros,request_digest
FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`,
			settlement.ProjectID, string(settlement.Source), settlement.CallID).Scan(&projectActorID, &taskID, &modelID, &projectState,
			&currency, &rateProvider, &rateModel, &rateVersion, &inputRate, &outputRate, &reservedInput, &reservedOutput,
			&reservedMicros, &projectInput, &projectOutput, &projectMicros, &projectRequestDigest); err != nil {
			return storeError("lock project model settlement", "project model call", err)
		}
		source = string(settlement.Source)
		var accountState string
		var accountTaskID *string
		var accountReserved int64
		if err := tx.QueryRow(ctx, `SELECT state,task_id,reserved_tokens,input_tokens,output_tokens,request_digest
FROM model_call_ledger WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, actorID, source, settlement.CallID).
			Scan(&accountState, &accountTaskID, &accountReserved, &accountInput, &accountOutput, &accountRequestDigest); err != nil {
			return storeError("lock account model settlement", "model call", err)
		}
		if projectActorID != actorID || accountID != actorID || source != string(settlement.Source) ||
			(taskID.Valid != (accountTaskID != nil)) || taskID.Valid && accountTaskID != nil && *accountTaskID != taskID.String ||
			accountReserved < 1 || len(projectRequestDigest) != 32 || len(accountRequestDigest) != 32 ||
			!domain.ValidModelConnectionID(modelID) {
			return domain.NewError(domain.CodeInternal, "project and account model ledgers do not match")
		}
		if projectState == "settled" && accountState == "settled" {
			if !settlement.Unknown && projectInput != nil && projectOutput != nil && accountInput != nil && accountOutput != nil &&
				*projectInput == settlement.Usage.InputTokens && *projectOutput == settlement.Usage.OutputTokens &&
				*accountInput == settlement.Usage.InputTokens && *accountOutput == settlement.Usage.OutputTokens {
				return nil
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "project model settlement was already recorded differently")
		}
		if projectState != "reserved" && projectState != "unknown" || accountState != "dispatched" && accountState != "unknown" ||
			(projectState == "unknown") != (accountState == "unknown") {
			return domain.NewError(domain.CodeInvalidState, "project model call is not awaiting settlement")
		}
		if settlement.Unknown {
			if projectState == "unknown" {
				return nil
			}
			return markProjectModelCallUnknown(ctx, tx, settlement.ProjectID, actorID, source, settlement.CallID, projectTaskString(taskID))
		}
		rate := project.RateSnapshot{Provider: rateProvider, ModelID: modelID, Model: rateModel, Currency: currency,
			Version: rateVersion, InputMicrosPerMillion: inputRate, OutputMicrosPerMillion: outputRate}
		actualTokens := settlement.Usage.InputTokens + settlement.Usage.OutputTokens
		actualMicros, err := rate.EstimateTokenCost(settlement.Usage)
		if err != nil {
			return domain.NewError(domain.CodeInternal, "stored project model rate is invalid")
		}
		if actualTokens > accountReserved || settlement.Usage.InputTokens > reservedInput || settlement.Usage.OutputTokens > reservedOutput || actualMicros > reservedMicros {
			overBudget = true
			return markProjectModelCallUnknown(ctx, tx, settlement.ProjectID, actorID, source, settlement.CallID, projectTaskString(taskID))
		}
		if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='settled',input_tokens=$4,output_tokens=$5,settled_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3`, actorID, source, settlement.CallID, settlement.Usage.InputTokens, settlement.Usage.OutputTokens); err != nil {
			return fmt.Errorf("settle account project model call: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE project_budget_ledger SET status='settled',actual_input_tokens=$4,actual_output_tokens=$5,
actual_micros=$6,settled_at=clock_timestamp() WHERE project_id=$1 AND source=$2 AND call_id=$3`,
			settlement.ProjectID, source, settlement.CallID, settlement.Usage.InputTokens, settlement.Usage.OutputTokens, actualMicros); err != nil {
			return fmt.Errorf("settle project model call: %w", err)
		}
		if taskID.Valid {
			return refreshTaskModelUsageInTx(ctx, tx, taskID.String, actorID)
		}
		return nil
	})
	if err != nil {
		if !settlement.Unknown {
			if holdErr := s.bestEffortUnknownProjectModelCall(ctx, settlement, actorID); holdErr != nil {
				return fmt.Errorf("%w; best-effort unknown hold failed: %w", err, holdErr)
			}
		}
		return err
	}
	if overBudget {
		return domain.NewError(domain.CodeQuotaExceeded, "project model usage exceeded its reservation")
	}
	return nil
}

// bestEffortUnknownProjectModelCall preserves the original known-settlement
// error while trying to retain both holds. It never changes an already settled
// pair, which is important when a commit succeeded but its response was lost.
func (s *Store) bestEffortUnknownProjectModelCall(ctx context.Context, settlement app.SettleProjectModelCallCommand, actorID string) error {
	if ctx == nil {
		return domain.InvalidArgument("context", "project model settlement context is required")
	}
	holdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if actorID == "" {
		if err := s.pool.QueryRow(holdCtx, `SELECT actor_id FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3`,
			settlement.ProjectID, string(settlement.Source), settlement.CallID).Scan(&actorID); err != nil {
			return storeError("find project model unknown hold", "project model call", err)
		}
	}
	return s.WithTx(holdCtx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(holdCtx, tx, actorID); err != nil {
			return err
		}
		var accountID string
		if err := tx.QueryRow(holdCtx, `SELECT owner_id FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, actorID).Scan(&accountID); err != nil {
			return storeError("lock project model unknown hold account budget", "model budget", err)
		}
		if err := lockProjectByIDInTx(holdCtx, tx, settlement.ProjectID); err != nil {
			return err
		}
		var projectActorID, modelID, projectState string
		var source string
		var taskID pgtype.Text
		var projectDigest []byte
		if err := tx.QueryRow(holdCtx, `SELECT actor_id,task_id,model_id,status,request_digest
FROM project_budget_ledger WHERE project_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`,
			settlement.ProjectID, string(settlement.Source), settlement.CallID).Scan(&projectActorID, &taskID, &modelID, &projectState, &projectDigest); err != nil {
			return storeError("lock project model unknown hold", "project model call", err)
		}
		source = string(settlement.Source)
		var accountState string
		var accountTaskID *string
		var accountDigest []byte
		if err := tx.QueryRow(holdCtx, `SELECT state,task_id,request_digest FROM model_call_ledger
WHERE owner_id=$1 AND source=$2 AND call_id=$3 FOR UPDATE`, actorID, source, settlement.CallID).
			Scan(&accountState, &accountTaskID, &accountDigest); err != nil {
			return storeError("lock account model unknown hold", "model call", err)
		}
		if projectActorID != actorID || accountID != actorID ||
			(taskID.Valid != (accountTaskID != nil)) || taskID.Valid && accountTaskID != nil && *accountTaskID != taskID.String ||
			len(projectDigest) != 32 || len(accountDigest) != 32 || !domain.ValidModelConnectionID(modelID) {
			return domain.NewError(domain.CodeInternal, "project and account model ledgers do not match")
		}
		if projectState == "settled" && accountState == "settled" || projectState == "unknown" && accountState == "unknown" {
			return nil
		}
		if (projectState != "reserved" && projectState != "unknown") ||
			(accountState != "dispatched" && accountState != "unknown") {
			return domain.NewError(domain.CodeInvalidState, "project model call cannot be held as unknown from its current state")
		}
		return markProjectModelCallUnknown(holdCtx, tx, settlement.ProjectID, actorID, source, settlement.CallID, projectTaskString(taskID))
	})
}

func projectTaskString(value pgtype.Text) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func refreshTaskModelUsageInTx(ctx context.Context, tx pgx.Tx, taskID, actorID string) error {
	if _, err := tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=CASE WHEN EXISTS (
 SELECT 1 FROM model_call_ledger WHERE task_id=$1 AND state='unknown') THEN NULL ELSE COALESCE((
 SELECT SUM(input_tokens::numeric+output_tokens::numeric)::bigint FROM model_call_ledger WHERE task_id=$1 AND state='settled'),0) END,
 updated_at=clock_timestamp() WHERE id=$1 AND initiator_id=$2`, taskID, actorID); err != nil {
		return fmt.Errorf("refresh task project model usage: %w", err)
	}
	return nil
}

func markProjectModelCallUnknown(ctx context.Context, tx pgx.Tx, projectID, actorID, source, callID, taskID string) error {
	if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3`, actorID, source, callID); err != nil {
		return fmt.Errorf("hold unknown account project model call: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE project_budget_ledger SET status='unknown',settled_at=clock_timestamp()
WHERE project_id=$1 AND source=$2 AND call_id=$3`, projectID, source, callID); err != nil {
		return fmt.Errorf("hold unknown project model call: %w", err)
	}
	if taskID != "" {
		return refreshTaskModelUsageInTx(ctx, tx, taskID, actorID)
	}
	return nil
}

func loadCurrentProjectBudgetInTx(ctx context.Context, tx pgx.Tx, projectID string) (project.Budget, bool, error) {
	var result project.Budget
	err := tx.QueryRow(ctx, `SELECT currency,limit_micros,version FROM project_budgets
WHERE project_id=$1 ORDER BY version DESC LIMIT 1`, projectID).
		Scan(&result.Currency, &result.LimitMicros, &result.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.Budget{}, false, nil
	}
	if err != nil {
		return project.Budget{}, false, fmt.Errorf("load current project budget: %w", err)
	}
	if result.Validate() != nil {
		return project.Budget{}, false, domain.NewError(domain.CodeInternal, "stored project budget is invalid")
	}
	return result, true, nil
}

func loadProjectBudgetVersionInTx(ctx context.Context, tx pgx.Tx, projectID string, version int64) (project.Budget, error) {
	var result project.Budget
	err := tx.QueryRow(ctx, `SELECT currency,limit_micros,version FROM project_budgets WHERE project_id=$1 AND version=$2`,
		projectID, version).Scan(&result.Currency, &result.LimitMicros, &result.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return project.Budget{}, domain.NewError(domain.CodeInternal, "project budget idempotency result is missing")
	}
	if err != nil {
		return project.Budget{}, fmt.Errorf("load project budget version: %w", err)
	}
	if result.Validate() != nil {
		return project.Budget{}, domain.NewError(domain.CodeInternal, "stored project budget version is invalid")
	}
	return result, nil
}

func projectBudgetViewInTx(ctx context.Context, tx pgx.Tx, projectID string, budget project.Budget, configured bool) (app.ProjectBudgetView, error) {
	if !configured {
		return app.ProjectBudgetView{}, nil
	}
	totals, err := loadProjectBudgetTotalsInTx(ctx, tx, projectID)
	if err != nil {
		return app.ProjectBudgetView{}, err
	}
	if totals.LedgerCount > 0 && totals.LedgerCurrency != budget.Currency {
		return app.ProjectBudgetView{}, domain.NewError(domain.CodeInternal, "project budget currency does not match its ledger")
	}
	return app.ProjectBudgetView{
		Configured: true, Budget: budget, SpentMicros: totals.SpentMicros,
		ReservedMicros: totals.ReservedMicros, HasUnknown: totals.HasUnknown,
	}, nil
}

func loadProjectBudgetTotalsInTx(ctx context.Context, tx pgx.Tx, projectID string) (projectBudgetTotals, error) {
	var result projectBudgetTotals
	var minCurrency, maxCurrency pgtype.Text
	err := tx.QueryRow(ctx, `SELECT
COALESCE(SUM(actual_micros) FILTER (WHERE status='settled'),0)::bigint,
COALESCE(SUM(reserved_micros) FILTER (WHERE status IN ('reserved','unknown')),0)::bigint,
COALESCE(bool_or(status='unknown'),false),count(*)::bigint,min(currency),max(currency)
FROM project_budget_ledger WHERE project_id=$1`, projectID).Scan(&result.SpentMicros, &result.ReservedMicros,
		&result.HasUnknown, &result.LedgerCount, &minCurrency, &maxCurrency)
	if err != nil {
		return projectBudgetTotals{}, fmt.Errorf("read project budget totals: %w", err)
	}
	if result.SpentMicros < 0 || result.ReservedMicros < 0 || result.LedgerCount < 0 {
		return projectBudgetTotals{}, domain.NewError(domain.CodeInternal, "project budget ledger totals are invalid")
	}
	if result.LedgerCount > 0 {
		if !minCurrency.Valid || !maxCurrency.Valid || minCurrency.String != maxCurrency.String {
			return projectBudgetTotals{}, domain.NewError(domain.CodeInternal, "project budget ledger currencies are inconsistent")
		}
		result.LedgerCurrency = minCurrency.String
	}
	return result, nil
}

func validateProjectBudgetTransition(current project.Budget, configured bool, next project.Budget, totals projectBudgetTotals) error {
	if configured && totals.LedgerCount > 0 && totals.LedgerCurrency != current.Currency {
		return domain.NewError(domain.CodeInternal, "project budget currency does not match its ledger")
	}
	if totals.HasUnknown {
		return domain.NewError(domain.CodeInvalidState, "project budget cannot change while model usage is unknown")
	}
	if totals.LedgerCount > 0 {
		currency := totals.LedgerCurrency
		if configured {
			currency = current.Currency
		}
		if next.Currency != currency {
			return domain.NewError(domain.CodeConflict, "project budget currency cannot change after ledger usage exists")
		}
	}
	if totals.SpentMicros > next.LimitMicros || totals.ReservedMicros > next.LimitMicros-totals.SpentMicros {
		return domain.NewError(domain.CodeConflict, "project budget limit cannot be lower than spent and reserved usage")
	}
	return nil
}

func projectBudgetVersionConflict(expected, actual int64) error {
	err := domain.NewError(domain.CodeConflict, fmt.Sprintf("project budget version changed: expected %d, current %d", expected, actual))
	err.Details["expected_version"] = expected
	err.Details["current_version"] = actual
	return err
}

func encodeProjectBudgetLedgerCursor(value projectBudgetLedgerCursor) (string, error) {
	if !project.ValidID(value.ProjectID) || value.CreatedAt.IsZero() || !validProjectBudgetSource(value.Source) || !validProjectIdempotencyKey(value.CallID) {
		return "", errors.New("project budget ledger cursor fields are invalid")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeProjectBudgetLedgerCursor(value, projectID string) (*projectBudgetLedgerCursor, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > 1024 {
		return nil, domain.InvalidArgument("cursor", "project budget ledger cursor is invalid")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, domain.InvalidArgument("cursor", "project budget ledger cursor is invalid")
	}
	var cursor projectBudgetLedgerCursor
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return nil, domain.InvalidArgument("cursor", "project budget ledger cursor is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, domain.InvalidArgument("cursor", "project budget ledger cursor is invalid")
	}
	if cursor.ProjectID != projectID || !project.ValidID(cursor.ProjectID) || cursor.CreatedAt.IsZero() ||
		!validProjectBudgetSource(cursor.Source) || !validProjectIdempotencyKey(cursor.CallID) {
		return nil, domain.InvalidArgument("cursor", "project budget ledger cursor is invalid")
	}
	return &cursor, nil
}

func validProjectBudgetSource(value string) bool {
	return value == string(app.ModelCallClient) || value == string(app.ModelCallChat) || value == string(app.ModelCallTask) || value == string(app.ModelCallRoute)
}
