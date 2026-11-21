-- Bind the persisted structured report to the exact response returned by the
-- metered reviewer model call.
-- +goose Up
ALTER TABLE task_validation_review_model_responses
  ADD COLUMN response_text_sha256 bytea,
  ADD CONSTRAINT task_validation_review_model_responses_text_digest_check CHECK (
    response_text_sha256 IS NULL OR octet_length(response_text_sha256) = 32
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_validation_review_model_responses WHERE response_text_sha256 IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove Task validation review model response text digests';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE task_validation_review_model_responses
  DROP CONSTRAINT task_validation_review_model_responses_text_digest_check,
  DROP COLUMN response_text_sha256;
