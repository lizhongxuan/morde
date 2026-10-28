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
	ownerID string
	source  string
	callID  string
	taskID  string
}

func (s *Store) MarkStaleModelCallsUnknown(ctx context.Context) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("model call recovery context is required")
	}
	marked := 0
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT owner_id FROM model_call_ledger
WHERE state='dispatched' AND created_at < clock_timestamp() - ($1 * interval '1 second')
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

		// Match reservation/settlement lock order: account budget first, ledger rows second.
		for _, ownerID := range owners {
			var locked string
			if err := tx.QueryRow(ctx, `SELECT owner_id FROM model_account_budgets WHERE owner_id=$1 FOR UPDATE`, ownerID).Scan(&locked); err != nil {
				return fmt.Errorf("lock stale model call budget: %w", err)
			}
		}

		rows, err = tx.Query(ctx, `SELECT owner_id,source,call_id,COALESCE(task_id,'') FROM model_call_ledger
WHERE owner_id=ANY($1::text[]) AND state='dispatched'
  AND created_at < clock_timestamp() - ($2 * interval '1 second')
ORDER BY owner_id,created_at,source,call_id LIMIT $3 FOR UPDATE SKIP LOCKED`,
			owners, staleModelCallAge.Seconds(), staleModelCallBatchSize)
		if err != nil {
			return fmt.Errorf("lock stale model calls: %w", err)
		}
		calls := make([]staleModelCall, 0, staleModelCallBatchSize)
		for rows.Next() {
			var call staleModelCall
			if err := rows.Scan(&call.ownerID, &call.source, &call.callID, &call.taskID); err != nil {
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
			result, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source=$2 AND call_id=$3 AND state='dispatched'`, call.ownerID, call.source, call.callID)
			if err != nil || result.RowsAffected() != 1 {
				if err != nil {
					return fmt.Errorf("mark stale model call unknown: %w", err)
				}
				return fmt.Errorf("mark stale model call unknown: ledger changed")
			}
			if call.taskID != "" {
				result, err := tx.Exec(ctx, `UPDATE tasks SET usage_model_tokens=NULL,updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`, call.taskID, call.ownerID)
				if err != nil || result.RowsAffected() != 1 {
					if err != nil {
						return fmt.Errorf("invalidate stale task model usage: %w", err)
					}
					return fmt.Errorf("invalidate stale task model usage: task is unavailable")
				}
			}
			marked++
		}
		return nil
	})
	return marked, err
}

var _ app.StaleModelCallStore = (*Store)(nil)
