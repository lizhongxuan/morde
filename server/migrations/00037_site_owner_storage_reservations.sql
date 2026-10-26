-- +goose Up
ALTER TABLE site_releases
  ADD COLUMN reservation_bytes bigint NOT NULL DEFAULT 0 CHECK (reservation_bytes >= 0);

UPDATE site_releases r
SET reservation_bytes = CASE
  WHEN r.materialized_at IS NOT NULL THEN r.artifact_bytes
  WHEN r.state = 'candidate' THEN wr.total_bytes
  ELSE 0
END
FROM workspace_revisions wr
WHERE wr.workspace_id = r.workspace_id AND wr.revision = r.source_revision;

ALTER TABLE site_releases
  ADD CONSTRAINT site_releases_reservation_check CHECK (
    (materialized_at IS NOT NULL AND reservation_bytes = artifact_bytes)
    OR (materialized_at IS NULL AND (state = 'candidate' OR reservation_bytes = 0))
  );

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM site_releases WHERE state = 'candidate' AND reservation_bytes > 0 AND materialized_at IS NULL) THEN
    RAISE EXCEPTION 'cannot roll back Site storage quotas while candidate reservations exist';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE site_releases DROP CONSTRAINT site_releases_reservation_check, DROP COLUMN reservation_bytes;
