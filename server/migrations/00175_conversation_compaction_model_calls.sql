-- Conversation compaction is a separately attributed model operation. It uses
-- the shared account/project ledgers without creating a synthetic Task.
-- +goose Up
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'probe', 'route', 'compaction'));
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'route', 'compaction'));

ALTER TABLE conversation_compaction_operations
  ADD COLUMN model_id text NOT NULL DEFAULT '',
  ADD COLUMN provider_model text NOT NULL DEFAULT '',
  ADD COLUMN connection_version timestamptz,
  ADD COLUMN model_request_digest bytea NOT NULL DEFAULT ''::bytea,
  ADD COLUMN response_ciphertext bytea NOT NULL DEFAULT ''::bytea,
  ADD COLUMN response_digest bytea NOT NULL DEFAULT ''::bytea,
  ADD CONSTRAINT conversation_compaction_model_metadata_check CHECK (
    (model_id = '' AND provider_model = '' AND connection_version IS NULL AND octet_length(model_request_digest) = 0) OR
    (model_id <> '' AND provider_model <> '' AND connection_version IS NOT NULL AND octet_length(model_request_digest) = 32)
  ),
  ADD CONSTRAINT conversation_compaction_model_response_check CHECK (
    (octet_length(response_ciphertext) = 0) = (octet_length(response_digest) = 0) AND
    octet_length(response_ciphertext) <= 1048576 AND octet_length(response_digest) IN (0, 32) AND
    (octet_length(response_ciphertext) = 0 OR (model_id <> '' AND state = 'checkpointed'))
  );

CREATE UNIQUE INDEX conversation_compaction_model_call_owner
  ON conversation_compaction_operations(actor_id, model_call_id) WHERE model_call_id <> '';

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM model_call_ledger WHERE source='compaction') OR
     EXISTS (SELECT 1 FROM project_budget_ledger WHERE source='compaction') OR
     EXISTS (SELECT 1 FROM conversation_compaction_operations WHERE model_call_id<>'' OR model_id<>'' OR
       octet_length(response_ciphertext)>0 OR octet_length(response_digest)>0) THEN
    RAISE EXCEPTION 'cannot remove conversation compaction model call records';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX conversation_compaction_model_call_owner;
ALTER TABLE conversation_compaction_operations
  DROP CONSTRAINT conversation_compaction_model_response_check,
  DROP CONSTRAINT conversation_compaction_model_metadata_check,
  DROP COLUMN response_digest,
  DROP COLUMN response_ciphertext,
  DROP COLUMN model_request_digest,
  DROP COLUMN connection_version,
  DROP COLUMN provider_model,
  DROP COLUMN model_id;
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'route'));
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'probe', 'route'));
