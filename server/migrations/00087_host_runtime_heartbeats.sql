-- +goose Up
CREATE TABLE host_runtime_heartbeats (
  host_id            text PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  last_seen_at       timestamptz NOT NULL
);

-- +goose Down
DROP TABLE host_runtime_heartbeats;
