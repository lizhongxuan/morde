-- +goose Up
CREATE TABLE host_activity_leases (
  host_id            text NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  lease_id           text NOT NULL CHECK (char_length(lease_id) BETWEEN 1 AND 128 AND lease_id ~ '^[A-Za-z0-9_-]+$'),
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  control_version    bigint NOT NULL CHECK (control_version >= 0),
  kind               text NOT NULL CHECK (kind IN ('gateway', 'preview')),
  expires_at         timestamptz NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (host_id, lease_id)
);

CREATE INDEX host_activity_leases_active
  ON host_activity_leases (host_id, runtime_generation, expires_at);

-- +goose Down
DROP TABLE host_activity_leases;
