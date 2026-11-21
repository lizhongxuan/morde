-- Preserve a successful, metered DSH reviewer turn for idempotent recovery.
-- +goose Up
CREATE TABLE task_validation_review_model_responses (
  owner_id text NOT NULL,
  source text NOT NULL DEFAULT 'review' CHECK (source = 'review'),
  call_id text NOT NULL,
  task_id text NOT NULL,
  task_revision bigint NOT NULL CHECK (task_revision >= 0),
  result_event_sequence bigint NOT NULL CHECK (result_event_sequence > 0),
  result_sha256 bytea NOT NULL CHECK (octet_length(result_sha256) = 32),
  claim_generation bigint NOT NULL CHECK (claim_generation > 0),
  lease_owner text NOT NULL CHECK (length(lease_owner) BETWEEN 1 AND 128 AND btrim(lease_owner) = lease_owner),
  session_id text NOT NULL CHECK (session_id ~ '^[A-Za-z0-9_-]{1,128}$'),
  through_sequence bigint NOT NULL CHECK (through_sequence BETWEEN 0 AND 9007199254740991),
  model_id text NOT NULL CHECK (model_id ~ '^p[a-z0-9]{7}$'),
  provider_model text NOT NULL CHECK (length(provider_model) BETWEEN 1 AND 256),
  connection_version timestamptz NOT NULL,
  project_id text NOT NULL DEFAULT '',
  personal_model boolean NOT NULL,
  request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  response_ciphertext bytea NOT NULL CHECK (octet_length(response_ciphertext) BETWEEN 1 AND 32768),
  response_digest bytea NOT NULL CHECK (octet_length(response_digest) = 32),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, source, call_id),
  FOREIGN KEY (owner_id, source, call_id, task_id)
    REFERENCES model_call_ledger(owner_id, source, call_id, task_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, task_revision) REFERENCES task_validation_reviews(task_id, task_revision) ON DELETE CASCADE
);

CREATE INDEX task_validation_review_model_responses_task
  ON task_validation_review_model_responses(task_id, task_revision, created_at DESC);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_validation_review_model_responses) THEN
    RAISE EXCEPTION 'cannot remove persisted Task validation review model responses';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX task_validation_review_model_responses_task;
DROP TABLE task_validation_review_model_responses;
