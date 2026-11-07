package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
)

// shortcut: ResponsesClient has a fixed 45-second hard timeout; revisit this age if that timeout becomes configurable.
const staleModelCallAge = 2 * time.Minute
const staleModelCallBatchSize = 100

type staleModelCall struct {
	ownerID  string
	source   string
	callID   string
	taskID   string
	personal bool
}

func (s *Store) MarkStaleModelCallsUnknown(ctx context.Context) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("model call recovery context is required")
	}
	marked := 0
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT owner_id FROM model_call_ledger
WHERE state='dispatched' AND source<>'route' AND created_at < clock_timestamp() - ($1 * interval '1 second')
ORDER BY owner_id LIMIT $2`, staleModelCallAge.Seconds(), staleModelCallBatchSize)
		if err != nil {
			return fmt.Errorf("list stale model call owners: %w", err)
		}
		owners := make([]string, 0, staleModelCallBatchSize)
		for rows.Next() {
			var ownerID string
			if err := rows.Scan(&ownerID); err != nil {
				rows.Close()
				return fmt.Errorf("read stale model call owner: %w", err)
			}
			owners = append(owners, ownerID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate stale model call owners: %w", err)
		}
		rows.Close()
		if len(owners) == 0 {
			return nil
		}

		// Match reservation/settlement lock order: owner, platform budget, then ledger.
		for _, ownerID := range owners {
			if err := lockModelLedgerOwner(ctx, tx, ownerID); err != nil {
				return err
			}
			var hasPlatform bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_call_ledger WHERE owner_id=$1 AND state='dispatched' AND source<>'route'
AND NOT personal_model AND created_at < clock_timestamp() - ($2 * interval '1 second'))`, ownerID, staleModelCallAge.Seconds()).Scan(&hasPlatform); err != nil {
				return fmt.Errorf("inspect stale model calls: %w", err)
			}
			if hasPlatform {
				if _, _, err := lockPlatformModelBudget(ctx, tx, ownerID); err != nil {
					return err
				}
			}
		}
		projectRows, err := tx.Query(ctx, `WITH stale_calls AS (
 SELECT owner_id,source,call_id,task_id FROM model_call_ledger
 WHERE owner_id=ANY($1::text[]) AND state='dispatched' AND source<>'route'
   AND created_at < clock_timestamp() - ($2 * interval '1 second')
 ORDER BY owner_id,created_at,source,call_id LIMIT $3
)
SELECT DISTINCT p.project_id
FROM stale_calls l JOIN project_budget_ledger p
  ON p.actor_id=l.owner_id AND p.source=l.source AND p.call_id=l.call_id AND p.task_id IS NOT DISTINCT FROM l.task_id
ORDER BY p.project_id`, owners, staleModelCallAge.Seconds(), staleModelCallBatchSize)
		if err != nil {
			return fmt.Errorf("list stale project model call budgets: %w", err)
		}
		projectIDs := make([]string, 0)
		for projectRows.Next() {
			var projectID string
			if err := projectRows.Scan(&projectID); err != nil {
				projectRows.Close()
				return fmt.Errorf("read stale project model budget: %w", err)
			}
			projectIDs = append(projectIDs, projectID)
		}
		if err := projectRows.Err(); err != nil {
			projectRows.Close()
			return fmt.Errorf("iterate stale project model budgets: %w", err)
		}
		projectRows.Close()
		for _, projectID := range projectIDs {
			var locked string
			if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&locked); err != nil {
				return fmt.Errorf("lock stale project model budget: %w", err)
			}
		}

		rows, err = tx.Query(ctx, `SELECT owner_id,source,call_id,COALESCE(task_id,''),personal_model FROM model_call_ledger
WHERE owner_id=ANY($1::text[]) AND state='dispatched' AND source<>'route'
  AND created_at < clock_timestamp() - ($2 * interval '1 second')
ORDER BY owner_id,created_at,source,call_id LIMIT $3 FOR UPDATE SKIP LOCKED`,
			owners, staleModelCallAge.Seconds(), staleModelCallBatchSize)
		if err != nil {
			return fmt.Errorf("lock stale model calls: %w", err)
		}
		calls := make([]staleModelCall, 0, staleModelCallBatchSize)
		for rows.Next() {
			var call staleModelCall
			if err := rows.Scan(&call.ownerID, &call.source, &call.callID, &call.taskID, &call.personal); err != nil {
				rows.Close()
				return fmt.Errorf("read stale model call: %w", err)
			}
			calls = append(calls, call)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate stale model calls: %w", err)
		}
		rows.Close()

		for _, call := range calls {
			var projectID, actorID, taskID, projectState string
			taskValue := any(nil)
			if call.taskID != "" {
				taskValue = call.taskID
			}
			projectLedgerRows, err := tx.Query(ctx, `SELECT project_id,actor_id,COALESCE(task_id,''),status
FROM project_budget_ledger WHERE actor_id=$1 AND source=$2 AND call_id=$3
  AND task_id IS NOT DISTINCT FROM $4::text ORDER BY project_id FOR UPDATE`,
				call.ownerID, call.source, call.callID, taskValue)
			if err != nil {
				return fmt.Errorf("lock stale project model call ledger: %w", err)
			}
			projectLedgerCount := 0
			for projectLedgerRows.Next() {
				if err := projectLedgerRows.Scan(&projectID, &actorID, &taskID, &projectState); err != nil {
					projectLedgerRows.Close()
					return fmt.Errorf("read stale project model call ledger: %w", err)
				}
				projectLedgerCount++
			}
			if err := projectLedgerRows.Err(); err != nil {
				projectLedgerRows.Close()
				return fmt.Errorf("iterate stale project model call ledgers: %w", err)
			}
			projectLedgerRows.Close()
			if projectLedgerCount > 1 {
				return fmt.Errorf("stale project model call has multiple matching project ledgers")
			}
			if call.taskID != "" {
				var taskProjectID, taskInitiatorID string
				if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,''),initiator_id FROM tasks WHERE id=$1`, call.taskID).
					Scan(&taskProjectID, &taskInitiatorID); err != nil {
					return fmt.Errorf("read stale model Task attribution: %w", err)
				}
				if taskInitiatorID != call.ownerID || (taskProjectID != "" &&
					((projectLedgerCount == 0 && !call.personal) || (projectLedgerCount == 1 &&
						(projectID != taskProjectID || actorID != taskInitiatorID || taskID != call.taskID)))) ||
					(taskProjectID == "" && projectLedgerCount != 0) {
					return fmt.Errorf("stale model Task and project ledger attribution do not match")
				}
			}
			if projectLedgerCount == 1 {
				if actorID != call.ownerID || taskID != call.taskID || projectState != "reserved" {
					return fmt.Errorf("stale project model call ledger is inconsistent")
				}
				result, err := tx.Exec(ctx, `UPDATE project_budget_ledger SET status='unknown',settled_at=clock_timestamp()
WHERE project_id=$1 AND source=$2 AND call_id=$3 AND actor_id=$4 AND status='reserved'`, projectID, call.source, call.callID, actorID)
				if err != nil || result.RowsAffected() != 1 {
					if err != nil {
						return fmt.Errorf("mark stale project model call unknown: %w", err)
					}
					return fmt.Errorf("mark stale project model call unknown: ledger changed")
				}
			}
			result, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3 AND state='dispatched'`, call.ownerID, call.source, call.callID)
			if err != nil || result.RowsAffected() != 1 {
				if err != nil {
					return fmt.Errorf("mark stale model call unknown: %w", err)
				}
				return fmt.Errorf("mark stale model call unknown: ledger changed")
			}
			if call.taskID != "" {
				if err := refreshTaskModelUsageInTx(ctx, tx, call.taskID, call.ownerID); err != nil {
					return fmt.Errorf("invalidate stale task model usage: %w", err)
				}
			}
			marked++
		}
		return nil
	})
	if err != nil {
		return marked, err
	}
	routeMarked, err := s.recoverExpiredRouteModelCalls(ctx)
	return marked + routeMarked, err
}

var _ app.StaleModelCallStore = (*Store)(nil)
