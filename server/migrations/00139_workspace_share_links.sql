-- +goose Up
ALTER TABLE workspaces
  ADD COLUMN is_read_only boolean NOT NULL DEFAULT false;

CREATE TABLE workspace_share_links (
  id text PRIMARY KEY CHECK (id ~ '^r[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  -- Share bookkeeping cannot make the original or recipient-owned copy undeletable.
  source_workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  source_revision bigint NOT NULL CHECK (source_revision >= 0),
  workspace_name text NOT NULL CHECK (length(workspace_name) BETWEEN 1 AND 120),
  permission text NOT NULL CHECK (permission IN ('view', 'edit')),
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  expires_at timestamptz NOT NULL,
  state text NOT NULL CHECK (state IN ('pending', 'claiming', 'claimed', 'expired', 'invalidated')),
  claimant_id text REFERENCES users(id) ON DELETE RESTRICT,
  target_workspace_id text UNIQUE REFERENCES workspaces(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  claimed_at timestamptz,
  CHECK ((state IN ('claiming', 'claimed') AND claimant_id IS NOT NULL) OR
         (state IN ('pending', 'expired', 'invalidated') AND claimant_id IS NULL)),
  CHECK ((state = 'claimed' AND target_workspace_id IS NOT NULL AND claimed_at IS NOT NULL) OR
         (state <> 'claimed' AND claimed_at IS NULL))
);

CREATE INDEX workspace_share_links_owner_pending
  ON workspace_share_links(owner_id, created_at DESC)
  WHERE state IN ('pending', 'claiming');

-- +goose Down
DROP TABLE workspace_share_links;
ALTER TABLE workspaces DROP COLUMN is_read_only;
