-- +goose Up
ALTER TABLE model_budget_capacity_call_deadlines
  ADD COLUMN slot_reserved_at timestamptz;

CREATE INDEX model_budget_capacity_call_deadlines_reserved_slots
  ON model_budget_capacity_call_deadlines(owner_id, first_wait_at, budget_call_id)
  WHERE slot_reserved_at IS NOT NULL;

-- Existing 00269/00270 waiters already own the next future account slot.
UPDATE model_budget_capacity_call_deadlines deadline
SET slot_reserved_at=COALESCE(wait.created_at,deadline.first_wait_at)
FROM model_budget_capacity_waits wait
WHERE wait.owner_id=deadline.owner_id AND wait.task_id=deadline.task_id
  AND wait.budget_call_id=deadline.budget_call_id AND deadline.slot_reserved_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_budget_capacity_call_deadlines WHERE slot_reserved_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while model budget capacity slots are reserved';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX model_budget_capacity_call_deadlines_reserved_slots;
ALTER TABLE model_budget_capacity_call_deadlines DROP COLUMN slot_reserved_at;
