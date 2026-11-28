-- +goose Up
CREATE TABLE task_requirement_manifest_confirmations (
  task_id                         text NOT NULL,
  owner_id                        text NOT NULL,
  task_revision                   bigint NOT NULL CHECK (task_revision >= 0),
  plan_revision                   bigint NOT NULL CHECK (plan_revision > 0),
  plan_event_sequence             bigint NOT NULL CHECK (plan_event_sequence > 0),
  manifest_digest                 bytea NOT NULL CHECK (octet_length(manifest_digest) = 32),
  confirmed_by_user_id            text NOT NULL REFERENCES users(id),
  confirmed_by_principal_kind     text NOT NULL CHECK (confirmed_by_principal_kind IN ('session', 'api_key', 'client', 'device')),
  task_authorization_version      bigint NOT NULL CHECK (task_authorization_version > 0),
  idempotency_key                 text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  confirmed_at                    timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, plan_event_sequence),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, plan_event_sequence) REFERENCES task_events(task_id, sequence) ON DELETE CASCADE,
  UNIQUE (owner_id, task_id, idempotency_key)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_requirement_manifest_confirmations) THEN
    RAISE EXCEPTION 'cannot discard task requirement manifest confirmations';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_requirement_manifest_confirmations;
