-- Reviewer model calls are metered independently from Task execution calls,
-- while retaining the reviewed Task and revision for accounting and audit.
-- +goose Up
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'probe', 'route', 'compaction', 'review'));
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_check;
ALTER TABLE model_call_ledger
  ADD CONSTRAINT model_call_ledger_task_attribution_check CHECK (
    ((source IN ('task', 'review')) AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR
    ((source NOT IN ('task', 'review')) AND task_id IS NULL)
  ),
  ADD COLUMN review_task_revision bigint,
  ADD COLUMN review_result_event_sequence bigint,
  ADD COLUMN review_result_sha256 bytea,
  ADD COLUMN review_claim_generation bigint,
  ADD CONSTRAINT model_call_ledger_review_identity_check CHECK (
    (source = 'review' AND review_task_revision IS NOT NULL AND review_task_revision >= 0 AND
      review_result_event_sequence IS NOT NULL AND review_result_event_sequence > 0 AND
      review_result_sha256 IS NOT NULL AND octet_length(review_result_sha256) = 32 AND
      review_claim_generation IS NOT NULL AND review_claim_generation > 0) OR
    (source <> 'review' AND review_task_revision IS NULL AND review_result_event_sequence IS NULL AND
      review_result_sha256 IS NULL AND review_claim_generation IS NULL)
  ),
  ADD CONSTRAINT model_call_ledger_review_claim_fk
    FOREIGN KEY (task_id, review_task_revision)
    REFERENCES task_validation_reviews(task_id, task_revision) ON DELETE RESTRICT;

ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'route', 'compaction', 'review'));
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_task_attribution_check CHECK (
  ((source IN ('task', 'review')) AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR
  ((source NOT IN ('task', 'review')) AND task_id IS NULL)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM model_call_ledger WHERE source='review') OR
     EXISTS (SELECT 1 FROM project_budget_ledger WHERE source='review') THEN
    RAISE EXCEPTION 'cannot remove nonempty Task validation review model usage ledgers';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_task_attribution_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_check CHECK (
  (source = 'task' AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR
  (source <> 'task' AND task_id IS NULL)
);
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'route', 'compaction'));

ALTER TABLE model_call_ledger
  DROP CONSTRAINT model_call_ledger_review_claim_fk,
  DROP CONSTRAINT model_call_ledger_review_identity_check,
  DROP CONSTRAINT model_call_ledger_task_attribution_check,
  DROP COLUMN review_claim_generation,
  DROP COLUMN review_result_sha256,
  DROP COLUMN review_result_event_sequence,
  DROP COLUMN review_task_revision;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_check CHECK ((source = 'task') = (task_id IS NOT NULL));
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'probe', 'route', 'compaction'));
