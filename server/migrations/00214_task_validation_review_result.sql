-- Keep independent reviewer identity and bounded report on the revision-bound outbox row.
-- +goose Up
ALTER TABLE task_validation_reviews
  ADD COLUMN reviewer_session_id text,
  ADD COLUMN execution_model_id text,
  ADD COLUMN execution_connection_version timestamptz,
  ADD COLUMN execution_endpoint text,
  ADD COLUMN execution_configured_model text,
  ADD COLUMN execution_reported_model text,
  ADD COLUMN execution_model_family text,
  ADD COLUMN execution_catalog_version text,
  ADD COLUMN reviewer_model_id text,
  ADD COLUMN reviewer_connection_version timestamptz,
  ADD COLUMN reviewer_endpoint text,
  ADD COLUMN reviewer_configured_model text,
  ADD COLUMN reviewer_reported_model text,
  ADD COLUMN reviewer_model_family text,
  ADD COLUMN reviewer_catalog_version text,
  ADD COLUMN review_report jsonb,
  ADD COLUMN reviewed_at timestamptz,
  ADD CONSTRAINT task_validation_reviews_result_complete CHECK (
    (state = 'reviewed'
      AND reviewer_session_id IS NOT NULL
      AND execution_model_id IS NOT NULL AND execution_connection_version IS NOT NULL AND execution_endpoint IS NOT NULL
      AND execution_configured_model IS NOT NULL AND execution_reported_model IS NOT NULL
      AND execution_model_family IS NOT NULL AND execution_catalog_version IS NOT NULL
      AND reviewer_model_id IS NOT NULL AND reviewer_connection_version IS NOT NULL AND reviewer_endpoint IS NOT NULL
      AND reviewer_configured_model IS NOT NULL AND reviewer_reported_model IS NOT NULL
      AND reviewer_model_family IS NOT NULL AND reviewer_catalog_version IS NOT NULL
      AND review_report IS NOT NULL AND reviewed_at IS NOT NULL)
    OR
    (state <> 'reviewed'
      AND reviewer_session_id IS NULL
      AND execution_model_id IS NULL AND execution_connection_version IS NULL AND execution_endpoint IS NULL
      AND execution_configured_model IS NULL AND execution_reported_model IS NULL
      AND execution_model_family IS NULL AND execution_catalog_version IS NULL
      AND reviewer_model_id IS NULL AND reviewer_connection_version IS NULL AND reviewer_endpoint IS NULL
      AND reviewer_configured_model IS NULL AND reviewer_reported_model IS NULL
      AND reviewer_model_family IS NULL AND reviewer_catalog_version IS NULL
      AND review_report IS NULL AND reviewed_at IS NULL)
  ),
  ADD CONSTRAINT task_validation_reviews_independent_models CHECK (
    state <> 'reviewed' OR
      (execution_model_family <> reviewer_model_family
       AND execution_catalog_version = reviewer_catalog_version)
  ),
  ADD CONSTRAINT task_validation_reviews_result_bounds CHECK (
    (reviewer_session_id IS NULL OR reviewer_session_id ~ '^[A-Za-z0-9_-]{1,128}$')
    AND (execution_model_id IS NULL OR execution_model_id ~ '^p[a-z0-9]{7}$')
    AND (reviewer_model_id IS NULL OR reviewer_model_id ~ '^p[a-z0-9]{7}$')
    AND (execution_endpoint IS NULL OR length(execution_endpoint) BETWEEN 1 AND 2048)
    AND (execution_configured_model IS NULL OR length(execution_configured_model) BETWEEN 1 AND 256)
    AND (execution_reported_model IS NULL OR length(execution_reported_model) BETWEEN 1 AND 256)
    AND (execution_model_family IS NULL OR length(execution_model_family) BETWEEN 1 AND 64)
    AND (execution_catalog_version IS NULL OR length(execution_catalog_version) BETWEEN 1 AND 64)
    AND (reviewer_endpoint IS NULL OR length(reviewer_endpoint) BETWEEN 1 AND 2048)
    AND (reviewer_configured_model IS NULL OR length(reviewer_configured_model) BETWEEN 1 AND 256)
    AND (reviewer_reported_model IS NULL OR length(reviewer_reported_model) BETWEEN 1 AND 256)
    AND (reviewer_model_family IS NULL OR length(reviewer_model_family) BETWEEN 1 AND 64)
    AND (reviewer_catalog_version IS NULL OR length(reviewer_catalog_version) BETWEEN 1 AND 64)
  );

CREATE UNIQUE INDEX task_validation_reviews_reviewer_session
  ON task_validation_reviews(reviewer_session_id) WHERE reviewer_session_id IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_validation_reviews WHERE state = 'reviewed') THEN
    RAISE EXCEPTION 'cannot remove persisted Task validation review results';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX task_validation_reviews_reviewer_session;
ALTER TABLE task_validation_reviews
  DROP CONSTRAINT task_validation_reviews_result_complete,
  DROP CONSTRAINT task_validation_reviews_independent_models,
  DROP CONSTRAINT task_validation_reviews_result_bounds,
  DROP COLUMN reviewer_session_id,
  DROP COLUMN execution_model_id,
  DROP COLUMN execution_connection_version,
  DROP COLUMN execution_endpoint,
  DROP COLUMN execution_configured_model,
  DROP COLUMN execution_reported_model,
  DROP COLUMN execution_model_family,
  DROP COLUMN execution_catalog_version,
  DROP COLUMN reviewer_model_id,
  DROP COLUMN reviewer_connection_version,
  DROP COLUMN reviewer_endpoint,
  DROP COLUMN reviewer_configured_model,
  DROP COLUMN reviewer_reported_model,
  DROP COLUMN reviewer_model_family,
  DROP COLUMN reviewer_catalog_version,
  DROP COLUMN review_report,
  DROP COLUMN reviewed_at;
