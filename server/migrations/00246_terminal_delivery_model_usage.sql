-- Terminal delivery model calls use the existing account/project budget
-- ledgers while retaining a durable link to the outbox source and response.
-- +goose Up
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'probe', 'route', 'compaction', 'review', 'terminal_delivery'));

ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_task_attribution_check;
ALTER TABLE task_terminal_delivery_outbox
  ADD CONSTRAINT task_terminal_delivery_outbox_model_identity_unique UNIQUE (id, task_id, initiator_id);

ALTER TABLE model_call_ledger
  ADD CONSTRAINT model_call_ledger_task_attribution_check CHECK (
    ((source IN ('task', 'review', 'terminal_delivery')) AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR
    ((source NOT IN ('task', 'review', 'terminal_delivery')) AND task_id IS NULL)
  ),
  ADD COLUMN terminal_delivery_id bigint,
  ADD COLUMN terminal_delivery_claim_generation bigint,
  ADD COLUMN terminal_delivery_dispatch_started boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT model_call_ledger_terminal_delivery_identity_check CHECK (
    (source = 'terminal_delivery' AND terminal_delivery_id IS NOT NULL AND terminal_delivery_id > 0 AND
      terminal_delivery_claim_generation IS NOT NULL AND terminal_delivery_claim_generation > 0 AND
      call_id = 'terminal_delivery_' || terminal_delivery_id::text) OR
    (source <> 'terminal_delivery' AND terminal_delivery_id IS NULL AND terminal_delivery_claim_generation IS NULL AND
      terminal_delivery_dispatch_started = false)
  ),
  ADD CONSTRAINT model_call_ledger_terminal_delivery_fk
    FOREIGN KEY (terminal_delivery_id, task_id, owner_id)
    REFERENCES task_terminal_delivery_outbox(id, task_id, initiator_id) ON DELETE RESTRICT;

ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'route', 'compaction', 'review', 'terminal_delivery'));
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_task_attribution_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_task_attribution_check CHECK (
  ((source IN ('task', 'review', 'terminal_delivery')) AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR
  ((source NOT IN ('task', 'review', 'terminal_delivery')) AND task_id IS NULL)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM model_call_ledger WHERE source='terminal_delivery') OR
     EXISTS (SELECT 1 FROM project_budget_ledger WHERE source='terminal_delivery') THEN
    RAISE EXCEPTION 'cannot remove nonempty Task terminal delivery model usage ledgers';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_task_attribution_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_task_attribution_check CHECK (
  ((source IN ('task', 'review')) AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR
  ((source NOT IN ('task', 'review')) AND task_id IS NULL)
);
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'route', 'compaction', 'review'));

ALTER TABLE model_call_ledger
  DROP CONSTRAINT model_call_ledger_terminal_delivery_fk,
  DROP CONSTRAINT model_call_ledger_terminal_delivery_identity_check,
  DROP COLUMN terminal_delivery_dispatch_started,
  DROP COLUMN terminal_delivery_claim_generation,
  DROP COLUMN terminal_delivery_id,
  DROP CONSTRAINT model_call_ledger_task_attribution_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_task_attribution_check CHECK (
  ((source IN ('task', 'review')) AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR
  ((source NOT IN ('task', 'review')) AND task_id IS NULL)
);
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'probe', 'route', 'compaction', 'review'));
ALTER TABLE task_terminal_delivery_outbox DROP CONSTRAINT task_terminal_delivery_outbox_model_identity_unique;
