-- +goose Up
-- Restore the nullable cost column expected by TaskDetails reads and pending
-- request writes when a database reports migration 00238 as applied but has
-- only the historical current_cost_micros column. Keep that column intact:
-- its provenance is not enough to claim a trusted exact-action cost.
ALTER TABLE task_pending_requests
  ADD COLUMN IF NOT EXISTS this_cost_micros bigint;

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'task_pending_requests'::regclass
      AND conname = 'task_pending_requests_this_cost_micros_check'
  ) THEN
    ALTER TABLE task_pending_requests
      ADD CONSTRAINT task_pending_requests_this_cost_micros_check CHECK (
        this_cost_micros IS NULL OR this_cost_micros >= 0
      );
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- This repair restores the nullable column defined by migration 00238. Keep
-- the schema on rollback so older code that reads that contract keeps working.
SELECT 1;
