-- Bind each advisory review to the exact main-agent model response that
-- produced its completed result; configured aliases alone are insufficient.
-- +goose Up
ALTER TABLE task_model_call_responses
  ADD COLUMN provider_endpoint text,
  ADD COLUMN provider_reported_model text,
  ADD CONSTRAINT task_model_call_responses_provider_identity_check CHECK (
    (provider_endpoint IS NULL OR length(provider_endpoint) BETWEEN 1 AND 2048)
    AND (provider_reported_model IS NULL OR length(provider_reported_model) BETWEEN 1 AND 256)
  );

ALTER TABLE task_validation_reviews
  ADD COLUMN execution_model_call_id text,
  ADD CONSTRAINT task_validation_reviews_execution_call_check CHECK (
    (execution_model_call_id IS NULL OR length(execution_model_call_id) BETWEEN 1 AND 256)
    AND (state <> 'reviewed' OR execution_model_call_id IS NOT NULL)
  );

CREATE INDEX task_model_call_responses_root_latest
  ON task_model_call_responses(owner_id, task_id, agent_session_id, through_sequence DESC, created_at DESC)
  WHERE agent_session_id = agent_root_session_id;
CREATE UNIQUE INDEX task_validation_reviews_execution_call
  ON task_validation_reviews(owner_id, execution_model_call_id)
  WHERE execution_model_call_id IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_validation_reviews WHERE execution_model_call_id IS NOT NULL OR state='reviewed') OR
     EXISTS (SELECT 1 FROM task_model_call_responses WHERE provider_endpoint IS NOT NULL OR provider_reported_model IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove Task validation execution model provenance';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX task_validation_reviews_execution_call;
DROP INDEX task_model_call_responses_root_latest;
ALTER TABLE task_validation_reviews
  DROP CONSTRAINT task_validation_reviews_execution_call_check,
  DROP COLUMN execution_model_call_id;
ALTER TABLE task_model_call_responses
  DROP CONSTRAINT task_model_call_responses_provider_identity_check,
  DROP COLUMN provider_endpoint,
  DROP COLUMN provider_reported_model;
