-- +goose Up
CREATE TABLE workspace_owner_block_usage (
  owner_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  allocated_bytes bigint NOT NULL DEFAULT 0 CHECK (allocated_bytes >= 0),
  reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspace_block_usage (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  state text NOT NULL CHECK (state IN ('reserved', 'stored')),
  allocated_bytes bigint NOT NULL DEFAULT 0 CHECK (allocated_bytes >= 0),
  reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, sha256),
  CHECK (
    (state = 'reserved' AND allocated_bytes = 0 AND reserved_bytes > 0)
    OR (state = 'stored' AND reserved_bytes = 0)
  )
);

CREATE INDEX workspace_block_usage_state ON workspace_block_usage(workspace_id, state);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM workspace_block_usage) OR EXISTS (SELECT 1 FROM workspace_owner_block_usage WHERE allocated_bytes > 0 OR reserved_bytes > 0) THEN
    RAISE EXCEPTION 'cannot roll back non-empty workspace physical block usage';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE workspace_block_usage;
DROP TABLE workspace_owner_block_usage;
