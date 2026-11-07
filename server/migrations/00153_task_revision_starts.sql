-- +goose Up
-- A start is recorded only with the root DSH turn/start checkpoint. Keeping
-- its step and event identities makes replay and recovery idempotent.
CREATE TABLE task_revision_starts (
  task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  revision bigint NOT NULL CHECK (revision >= 0),
  step_id text NOT NULL,
  event_sequence bigint NOT NULL CHECK (event_sequence > 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, revision),
  UNIQUE (task_id, step_id),
  UNIQUE (task_id, event_sequence),
  FOREIGN KEY (task_id, step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, event_sequence) REFERENCES task_events(task_id, sequence) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_revision_starts) THEN
    RAISE EXCEPTION 'cannot discard recorded task revision starts';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_revision_starts;
