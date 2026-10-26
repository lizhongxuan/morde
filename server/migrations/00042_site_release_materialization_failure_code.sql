-- +goose Up
ALTER TABLE site_releases
  ADD COLUMN failure_code text,
  ADD CONSTRAINT site_releases_failure_code_check CHECK (
    failure_code IS NULL OR failure_code IN (
      'source_changed',
      'source_unavailable',
      'capacity_exceeded',
      'storage_unavailable'
    )
  ),
  ADD CONSTRAINT site_releases_failure_materialization_check CHECK (
    failure_code IS NULL OR materialized_at IS NULL
  );

-- +goose Down
ALTER TABLE site_releases
  DROP CONSTRAINT site_releases_failure_materialization_check,
  DROP CONSTRAINT site_releases_failure_code_check,
  DROP COLUMN failure_code;
