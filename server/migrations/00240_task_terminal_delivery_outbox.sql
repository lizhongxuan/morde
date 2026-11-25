-- Durable source facts for Agent-first failure/cancellation replies. The
-- delivery worker owns claiming, model calls, and publication.
-- +goose Up
CREATE TABLE task_terminal_delivery_outbox (
  id                         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  task_id                    text NOT NULL,
  owner_id                   text NOT NULL,
  conversation_id             text NOT NULL,
  initiator_id                text NOT NULL,
  task_revision               bigint NOT NULL CHECK (task_revision >= 0),
  authorization_version       bigint NOT NULL CHECK (authorization_version > 0),
  source_kind                 text NOT NULL CHECK (source_kind IN (
    'terminal_failure', 'terminal_cancellation', 'waiting_unknown', 'late_validation'
  )),
  source_event_sequence       bigint NOT NULL CHECK (source_event_sequence > 0),
  source_event_type           text NOT NULL CHECK (length(source_event_type) BETWEEN 1 AND 64),
  source_sha256               bytea NOT NULL CHECK (octet_length(source_sha256) = 32),
  result_sha256               bytea CHECK (result_sha256 IS NULL OR octet_length(result_sha256) = 32),
  validation_claim_generation bigint CHECK (validation_claim_generation IS NULL OR validation_claim_generation > 0),
  verdict_sha256              bytea CHECK (verdict_sha256 IS NULL OR octet_length(verdict_sha256) = 32),
  state                       text NOT NULL DEFAULT 'pending' CHECK (state IN (
    'pending', 'claimed', 'reply_ready', 'unknown', 'published', 'invalidated'
  )),
  claim_generation            bigint NOT NULL DEFAULT 0 CHECK (claim_generation >= 0),
  lease_owner                 text,
  lease_expires_at             timestamptz,
  model_call_id               text,
  response_ciphertext         bytea,
  response_sha256             bytea CHECK (response_sha256 IS NULL OR octet_length(response_sha256) = 32),
  published_message_id        text,
  created_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, source_event_sequence) REFERENCES task_events(task_id, sequence) ON DELETE RESTRICT,
  CHECK (
    (source_kind = 'late_validation' AND result_sha256 IS NOT NULL
      AND validation_claim_generation IS NOT NULL AND verdict_sha256 IS NOT NULL)
    OR (source_kind <> 'late_validation' AND result_sha256 IS NULL
      AND validation_claim_generation IS NULL AND verdict_sha256 IS NULL)
  ),
  CHECK ((state = 'claimed') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
  CHECK (lease_owner IS NULL OR (length(lease_owner) BETWEEN 1 AND 128 AND btrim(lease_owner) = lease_owner)),
  CHECK ((response_ciphertext IS NULL) = (response_sha256 IS NULL)),
  CHECK (state NOT IN ('reply_ready', 'published') OR response_ciphertext IS NOT NULL),
  CHECK ((state = 'published') = (published_message_id IS NOT NULL)),
  CHECK (published_message_id IS NULL OR published_message_id ~ '^m[a-z0-9]{7}$')
);

CREATE UNIQUE INDEX task_terminal_delivery_outbox_terminal_source
  ON task_terminal_delivery_outbox(task_id, source_event_sequence, source_event_type)
  WHERE source_kind IN ('terminal_failure', 'terminal_cancellation', 'waiting_unknown');
CREATE UNIQUE INDEX task_terminal_delivery_outbox_late_validation_source
  ON task_terminal_delivery_outbox(task_id, source_event_sequence, result_sha256, validation_claim_generation, verdict_sha256)
  WHERE source_kind = 'late_validation';
CREATE UNIQUE INDEX task_terminal_delivery_outbox_model_call
  ON task_terminal_delivery_outbox(model_call_id) WHERE model_call_id IS NOT NULL;
CREATE INDEX task_terminal_delivery_outbox_pending
  ON task_terminal_delivery_outbox(created_at, id)
  WHERE state IN ('pending', 'claimed', 'unknown');

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_terminal_delivery_outbox) THEN
    RAISE EXCEPTION 'cannot remove Task terminal delivery facts';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX task_terminal_delivery_outbox_pending;
DROP INDEX task_terminal_delivery_outbox_model_call;
DROP INDEX task_terminal_delivery_outbox_late_validation_source;
DROP INDEX task_terminal_delivery_outbox_terminal_source;
DROP TABLE task_terminal_delivery_outbox;
