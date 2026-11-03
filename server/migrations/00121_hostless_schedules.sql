-- +goose Up
ALTER TABLE schedules ALTER COLUMN host_id DROP NOT NULL;

-- Computer/browser observation requires a Host. Ordinary schedules may be
-- hostless and run without a Host-bound observation target.
ALTER TABLE schedules
  ADD CONSTRAINT schedules_observation_requires_host_check
  CHECK (host_id IS NOT NULL OR observation_spec IS NULL);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM schedules WHERE host_id IS NULL) THEN
    RAISE EXCEPTION 'cannot restore required schedule host_id while hostless schedules exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE schedules DROP CONSTRAINT schedules_observation_requires_host_check;
ALTER TABLE schedules ALTER COLUMN host_id SET NOT NULL;
