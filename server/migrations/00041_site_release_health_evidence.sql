-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM site_releases WHERE state = 'healthy') THEN
    RAISE EXCEPTION 'cannot enable Site health gate while healthy releases lack health-check evidence';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE site_releases
  ADD COLUMN health_checked_at timestamptz,
  ADD CONSTRAINT site_releases_health_check_evidence CHECK (
    (health_checked_at IS NULL OR materialized_at IS NOT NULL) AND
    (state <> 'healthy' OR health_checked_at IS NOT NULL)
  );

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM site_releases WHERE health_checked_at IS NOT NULL OR state = 'healthy') THEN
    RAISE EXCEPTION 'cannot drop Site health-check evidence after recording or promotion';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE site_releases
  DROP CONSTRAINT site_releases_health_check_evidence,
  DROP COLUMN health_checked_at;
