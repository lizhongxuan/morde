-- Keep the reviewer route and the provider's reported model alongside the
-- encrypted response so completion can verify the persisted identity.
-- +goose Up
ALTER TABLE task_validation_review_model_responses
  ADD COLUMN provider_endpoint text,
  ADD COLUMN provider_reported_model text,
  ADD CONSTRAINT task_validation_review_model_responses_provider_identity_check CHECK (
    (provider_endpoint IS NULL OR length(provider_endpoint) BETWEEN 1 AND 2048)
    AND (provider_reported_model IS NULL OR length(provider_reported_model) BETWEEN 1 AND 256)
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_validation_review_model_responses
    WHERE provider_endpoint IS NOT NULL OR provider_reported_model IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove Task validation review provider identity';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE task_validation_review_model_responses
  DROP CONSTRAINT task_validation_review_model_responses_provider_identity_check,
  DROP COLUMN provider_reported_model,
  DROP COLUMN provider_endpoint;
