-- +goose Up
ALTER TABLE task_clarification_requests
  ADD COLUMN questions jsonb NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE task_clarification_requests
  ADD CONSTRAINT task_clarification_requests_questions_array
  CHECK (jsonb_typeof(questions) = 'array');

ALTER TABLE task_clarification_requests
  ADD COLUMN answer_format text NOT NULL DEFAULT 'free_text';

ALTER TABLE task_clarification_requests
  ADD CONSTRAINT task_clarification_requests_answer_format
  CHECK (answer_format IN ('free_text', 'structured'));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_clarification_requests WHERE answer_format='structured') THEN
    RAISE EXCEPTION 'cannot roll back structured clarification answers after they were persisted';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_clarification_requests
  DROP CONSTRAINT task_clarification_requests_answer_format;

ALTER TABLE task_clarification_requests
  DROP COLUMN answer_format;

ALTER TABLE task_clarification_requests
  DROP CONSTRAINT task_clarification_requests_questions_array;

ALTER TABLE task_clarification_requests
  DROP COLUMN questions;
