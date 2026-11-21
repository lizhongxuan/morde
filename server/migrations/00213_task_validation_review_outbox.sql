-- +goose Up
-- A Task result review is queued independently of the completed Task lifecycle.
-- The composite FK makes each outbox row point at one revision-bound result event.
CREATE TABLE task_validation_reviews (
  task_id text NOT NULL,
  owner_id text NOT NULL,
  task_revision bigint NOT NULL CHECK (task_revision >= 0),
  result_event_sequence bigint NOT NULL CHECK (result_event_sequence > 0),
  result_sha256 bytea NOT NULL CHECK (octet_length(result_sha256) = 32),
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'processing', 'reviewed', 'failed', 'superseded')),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  claim_generation bigint NOT NULL DEFAULT 0 CHECK (claim_generation >= 0),
  available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  lease_owner text,
  lease_expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, task_revision),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, result_event_sequence, task_revision)
    REFERENCES task_events(task_id, sequence, task_revision) ON DELETE RESTRICT,
  CHECK ((state = 'processing') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
  CHECK (lease_owner IS NULL OR (length(lease_owner) BETWEEN 1 AND 128 AND btrim(lease_owner) = lease_owner))
);

CREATE INDEX task_validation_reviews_pending
  ON task_validation_reviews(available_at, created_at, task_id, task_revision)
  WHERE state IN ('pending', 'processing');

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_validation_reviews) THEN
    RAISE EXCEPTION 'cannot remove Task validation review records';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX task_validation_reviews_pending;
DROP TABLE task_validation_reviews;
