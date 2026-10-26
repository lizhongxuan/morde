-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM site_releases WHERE state = 'healthy') THEN
    RAISE EXCEPTION 'cannot enable Site materialization gate while healthy releases lack artifact evidence';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE site_releases
  ADD COLUMN artifact_file_count bigint NOT NULL DEFAULT 0,
  ADD COLUMN artifact_bytes bigint NOT NULL DEFAULT 0,
  ADD COLUMN materialized_at timestamptz,
  ADD CONSTRAINT site_releases_materialization_check CHECK (
    artifact_file_count >= 0 AND artifact_bytes >= 0 AND
    ((materialized_at IS NULL AND artifact_file_count = 0 AND artifact_bytes = 0) OR materialized_at IS NOT NULL) AND
    (state <> 'healthy' OR materialized_at IS NOT NULL)
  );

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM site_releases WHERE materialized_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Site materialization evidence while artifacts exist';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE site_releases
  DROP CONSTRAINT site_releases_materialization_check,
  DROP COLUMN materialized_at,
  DROP COLUMN artifact_bytes,
  DROP COLUMN artifact_file_count;
