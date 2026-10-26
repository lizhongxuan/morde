-- +goose Up
CREATE TABLE orphan_host_volume_cleanups (
  instance_id       text NOT NULL CHECK (length(instance_id) BETWEEN 1 AND 128),
  host_id           text NOT NULL CHECK (host_id ~ '^h[a-z0-9]{7}$'),
  runtime_profile   text NOT NULL CHECK (length(runtime_profile) BETWEEN 1 AND 128),
  sandbox_id        text NOT NULL CHECK (length(sandbox_id) BETWEEN 1 AND 512),
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  created_at        timestamptz NOT NULL DEFAULT now(),
  completed_at      timestamptz,
  PRIMARY KEY (instance_id, host_id, runtime_profile, sandbox_id, runtime_generation),
  CHECK (completed_at IS NULL OR completed_at >= created_at)
);
CREATE INDEX orphan_host_volume_cleanups_pending
  ON orphan_host_volume_cleanups (instance_id, host_id)
  WHERE completed_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM orphan_host_volume_cleanups WHERE completed_at IS NULL) THEN
    RAISE EXCEPTION 'cannot roll back with pending orphan Host volume cleanups';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE orphan_host_volume_cleanups;
