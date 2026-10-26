-- +goose Up
CREATE TABLE task_clarification_requests (
  id          text PRIMARY KEY CHECK (id ~ '^r[a-z0-9]{7}$'),
  task_id     text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  version     bigint NOT NULL CHECK (version > 0),
  model_step_id text NOT NULL,
  call_id     text NOT NULL CHECK (char_length(call_id) BETWEEN 1 AND 256),
  question    text NOT NULL CHECK (char_length(question) BETWEEN 1 AND 4096),
  status      text NOT NULL CHECK (status IN ('pending', 'answered', 'cancelled')),
  answer      text NOT NULL DEFAULT '' CHECK (char_length(answer) <= 16384),
  created_at  timestamptz NOT NULL DEFAULT now(),
  responded_at timestamptz,
  UNIQUE (task_id, version),
  FOREIGN KEY (task_id, model_step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  CHECK ((status = 'pending' AND answer = '' AND responded_at IS NULL) OR
         (status = 'answered' AND answer <> '' AND responded_at IS NOT NULL) OR
         (status = 'cancelled' AND answer = '' AND responded_at IS NOT NULL))
);
CREATE UNIQUE INDEX task_clarification_requests_one_active ON task_clarification_requests(task_id) WHERE status='pending';
CREATE INDEX task_clarification_requests_latest ON task_clarification_requests(task_id, version DESC);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_clarification_requests) THEN
    RAISE EXCEPTION 'cannot roll back task clarification migration after requests were persisted';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_clarification_requests;
