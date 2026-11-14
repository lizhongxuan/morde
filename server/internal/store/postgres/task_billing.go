package postgres

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type taskCostKey struct {
	state    string
	currency string
}

type taskCostAccumulator struct {
	calls          int64
	reservedMicros int64
	actualMicros   int64
	rateVersions   map[int64]struct{}
}

func (s *Store) taskBilling(ctx context.Context, tx pgx.Tx, taskID, initiatorID, projectID string) (app.TaskBilling, error) {
	result := app.TaskBilling{Costs: make([]app.TaskCostSummary, 0)}
	grouped := make(map[taskCostKey]*taskCostAccumulator)
	rows, err := tx.Query(ctx, `SELECT l.personal_model,l.state,p.status,p.currency,p.rate_version,p.reserved_micros,p.actual_micros
FROM model_call_ledger l
LEFT JOIN project_budget_ledger p ON p.project_id=$3 AND p.source='task' AND p.call_id=l.call_id
  AND p.task_id=l.task_id AND p.actor_id=l.owner_id
WHERE l.owner_id=$2 AND l.source='task' AND l.task_id=$1
ORDER BY l.call_id`, taskID, initiatorID, projectID)
	if err != nil {
		return app.TaskBilling{}, fmt.Errorf("read Task model billing: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var personalModel bool
		var modelState string
		var ledgerState, currency pgtype.Text
		var rateVersion, reservedMicros, actualMicros pgtype.Int8
		if err := rows.Scan(&personalModel, &modelState, &ledgerState, &currency, &rateVersion, &reservedMicros, &actualMicros); err != nil {
			return app.TaskBilling{}, fmt.Errorf("scan Task model billing: %w", err)
		}
		if result.ModelCalls == math.MaxInt64 {
			return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "Task model call count overflowed")
		}
		result.ModelCalls++
		state, costCurrency, version, reserved, actual := "unknown", "", int64(0), int64(0), int64(0)
		if personalModel {
			if ledgerState.Valid {
				return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "BYOK Task call has a project cost ledger")
			}
			state = "byok"
		} else if ledgerState.Valid {
			if !currency.Valid || !rateVersion.Valid || rateVersion.Int64 < 1 || !reservedMicros.Valid || reservedMicros.Int64 < 1 {
				return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "Task project cost snapshot is incomplete")
			}
			costCurrency, version, reserved = currency.String, rateVersion.Int64, reservedMicros.Int64
			if len(costCurrency) != 3 || costCurrency[0] < 'A' || costCurrency[0] > 'Z' || costCurrency[1] < 'A' || costCurrency[1] > 'Z' || costCurrency[2] < 'A' || costCurrency[2] > 'Z' {
				return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "Task project cost currency is invalid")
			}
			switch ledgerState.String {
			case "settled":
				if modelState != "settled" || !actualMicros.Valid || actualMicros.Int64 < 0 {
					return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "settled Task cost does not match its model ledger")
				}
				state, actual = "settled", actualMicros.Int64
			case "reserved":
				if modelState != "dispatched" || actualMicros.Valid {
					return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "reserved Task cost does not match its model ledger")
				}
				state = "estimated"
			case "unknown":
				if modelState != "unknown" || actualMicros.Valid {
					return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "unknown Task cost does not match its model ledger")
				}
				state = "unknown"
			default:
				return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "Task project cost state is invalid")
			}
		} else if modelState != "settled" && modelState != "dispatched" && modelState != "unknown" {
			return app.TaskBilling{}, domain.NewError(domain.CodeInternal, "Task model call state is invalid")
		}
		key := taskCostKey{state: state, currency: costCurrency}
		bucket := grouped[key]
		if bucket == nil {
			bucket = &taskCostAccumulator{rateVersions: make(map[int64]struct{})}
			grouped[key] = bucket
		}
		if err := addTaskCost(bucket, reserved, actual, version); err != nil {
			return app.TaskBilling{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return app.TaskBilling{}, fmt.Errorf("iterate Task model billing: %w", err)
	}
	keys := make([]taskCostKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].currency != keys[j].currency {
			return keys[i].currency < keys[j].currency
		}
		return keys[i].state < keys[j].state
	})
	for _, key := range keys {
		bucket := grouped[key]
		versions := make([]int64, 0, len(bucket.rateVersions))
		for version := range bucket.rateVersions {
			versions = append(versions, version)
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
		cost := app.TaskCostSummary{State: key.state, Currency: key.currency, Calls: bucket.calls,
			RateVersions: versions, ReservedMicros: bucket.reservedMicros}
		if key.state == "settled" {
			actual := bucket.actualMicros
			cost.ActualMicros = &actual
		}
		result.Costs = append(result.Costs, cost)
	}
	return result, nil
}

func addTaskCost(bucket *taskCostAccumulator, reserved, actual, rateVersion int64) error {
	if bucket == nil || reserved < 0 || actual < 0 || rateVersion < 0 || bucket.calls == math.MaxInt64 ||
		reserved > math.MaxInt64-bucket.reservedMicros || actual > math.MaxInt64-bucket.actualMicros {
		return domain.NewError(domain.CodeInternal, "Task model cost summary overflowed")
	}
	bucket.calls++
	bucket.reservedMicros += reserved
	bucket.actualMicros += actual
	if rateVersion > 0 {
		bucket.rateVersions[rateVersion] = struct{}{}
	}
	return nil
}
