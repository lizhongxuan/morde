-- +goose Up
-- No source or deletion baseline is backfilled from legacy folder sessions.
CREATE TABLE workspace_sync_project_sources (
  source_binding_id text PRIMARY KEY CHECK (
    source_binding_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
  ),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  client_installation_id text NOT NULL,
  workspace_id text NOT NULL,
  folder_id text NOT NULL,
  host_id text NOT NULL,
  binding_version bigint NOT NULL CHECK (binding_version > 0),
  state text NOT NULL CHECK (state IN ('pending', 'active', 'revoked')),
  last_applied_session_id text,
  last_applied_target_revision bigint,
  last_applied_manifest_sha256 bytea,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (source_binding_id, owner_id, workspace_id, folder_id),
  UNIQUE (source_binding_id, owner_id, workspace_id, folder_id, host_id, binding_version),
  FOREIGN KEY (client_installation_id, owner_id)
    REFERENCES client_installations(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, owner_id, folder_id)
    REFERENCES workspace_sync_folders(workspace_id, owner_id, folder_id) ON DELETE RESTRICT,
  FOREIGN KEY (host_id, owner_id)
    REFERENCES hosts(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, last_applied_target_revision)
    REFERENCES workspace_revisions(workspace_id, revision) ON DELETE RESTRICT,
  CHECK (
    (last_applied_session_id IS NULL AND last_applied_target_revision IS NULL AND last_applied_manifest_sha256 IS NULL)
    OR (last_applied_session_id IS NOT NULL AND last_applied_target_revision IS NOT NULL AND
        last_applied_target_revision >= 0 AND last_applied_manifest_sha256 IS NOT NULL AND
        octet_length(last_applied_manifest_sha256) = 32)
  )
);
CREATE UNIQUE INDEX workspace_sync_project_source_active
  ON workspace_sync_project_sources(workspace_id, folder_id) WHERE state <> 'revoked';
CREATE INDEX workspace_sync_project_source_installation
  ON workspace_sync_project_sources(owner_id, client_installation_id, state);

ALTER TABLE workspace_sync_sessions
  ADD COLUMN source_binding_id text,
  ADD COLUMN source_baseline_session_id text,
  ADD CONSTRAINT workspace_sync_session_project_source_check CHECK (
    (source_binding_id IS NULL OR folder_id IS NOT NULL) AND
    (source_baseline_session_id IS NULL OR source_binding_id IS NOT NULL)
  ),
  ADD CONSTRAINT workspace_sync_session_project_source_fk
    FOREIGN KEY (source_binding_id, owner_id, workspace_id, folder_id)
    REFERENCES workspace_sync_project_sources(source_binding_id, owner_id, workspace_id, folder_id)
    ON DELETE RESTRICT,
  ADD CONSTRAINT workspace_sync_session_project_source_target_unique
    UNIQUE (workspace_id, owner_id, folder_id, source_binding_id, id, target_revision),
  ADD CONSTRAINT workspace_sync_session_project_source_session_unique
    UNIQUE (workspace_id, owner_id, folder_id, source_binding_id, id),
  ADD CONSTRAINT workspace_sync_session_source_baseline_fk
    FOREIGN KEY (workspace_id, owner_id, folder_id, source_binding_id, source_baseline_session_id)
    REFERENCES workspace_sync_sessions(workspace_id, owner_id, folder_id, source_binding_id, id)
    ON DELETE RESTRICT;

CREATE TABLE workspace_sync_project_upload_applications (
  workspace_id text NOT NULL,
  session_id text NOT NULL,
  owner_id text NOT NULL,
  folder_id text NOT NULL,
  source_binding_id text NOT NULL,
  source_binding_version bigint NOT NULL CHECK (source_binding_version > 0),
  source_baseline_session_id text,
  host_id text NOT NULL,
  workspace_binding_version bigint NOT NULL CHECK (workspace_binding_version > 0),
  target_revision bigint NOT NULL CHECK (target_revision >= 0),
  state text NOT NULL CHECK (state IN ('pending_apply', 'applied', 'conflict', 'unknown')),
  runtime_generation bigint CHECK (runtime_generation IS NULL OR runtime_generation > 0),
  applied_revision bigint CHECK (applied_revision IS NULL OR applied_revision >= 0),
  sync_state text CHECK (sync_state IS NULL OR sync_state IN ('healthy', 'conflict')),
  manifest_sha256 bytea NOT NULL CHECK (octet_length(manifest_sha256) = 32),
  receipt_sha256 bytea CHECK (receipt_sha256 IS NULL OR octet_length(receipt_sha256) = 32),
  impacts jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(impacts) = 'array' AND jsonb_array_length(impacts) <= 100),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, session_id),
  FOREIGN KEY (workspace_id, owner_id, folder_id, source_binding_id, session_id, target_revision)
    REFERENCES workspace_sync_sessions(workspace_id, owner_id, folder_id, source_binding_id, id, target_revision)
    ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, owner_id, folder_id, source_binding_id, source_baseline_session_id)
    REFERENCES workspace_sync_sessions(workspace_id, owner_id, folder_id, source_binding_id, id)
    ON DELETE RESTRICT,
  FOREIGN KEY (source_binding_id, owner_id, workspace_id, folder_id, host_id, source_binding_version)
    REFERENCES workspace_sync_project_sources(source_binding_id, owner_id, workspace_id, folder_id, host_id, binding_version)
    ON DELETE RESTRICT,
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, target_revision)
    REFERENCES workspace_revisions(workspace_id, revision) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, applied_revision)
    REFERENCES workspace_revisions(workspace_id, revision) ON DELETE RESTRICT,
  UNIQUE (source_binding_id, owner_id, workspace_id, folder_id, host_id, source_binding_version,
          session_id, target_revision, manifest_sha256),
  CHECK (state <> 'applied' OR
    (runtime_generation IS NOT NULL AND applied_revision IS NOT NULL AND applied_revision >= target_revision AND
     sync_state = 'healthy' AND impacts = '[]'::jsonb AND receipt_sha256 IS NOT NULL)),
  CHECK (state <> 'pending_apply' OR
    (runtime_generation IS NULL AND applied_revision IS NULL AND sync_state IS NULL AND
     impacts = '[]'::jsonb AND receipt_sha256 IS NULL))
);

ALTER TABLE workspace_sync_project_sources
  ADD CONSTRAINT workspace_sync_project_source_baseline_application_fk
    FOREIGN KEY (source_binding_id, owner_id, workspace_id, folder_id, host_id, binding_version,
                 last_applied_session_id, last_applied_target_revision, last_applied_manifest_sha256)
    REFERENCES workspace_sync_project_upload_applications(source_binding_id, owner_id, workspace_id, folder_id,
      host_id, source_binding_version, session_id, target_revision, manifest_sha256) ON DELETE RESTRICT;
CREATE UNIQUE INDEX workspace_sync_project_upload_one_unresolved
  ON workspace_sync_project_upload_applications(workspace_id, folder_id)
  WHERE state IN ('pending_apply', 'conflict', 'unknown');
CREATE INDEX workspace_sync_project_upload_host_revision
  ON workspace_sync_project_upload_applications(host_id, workspace_id, target_revision, state);

COMMENT ON COLUMN workspace_sync_project_upload_applications.source_baseline_session_id IS
  'PC source baseline read when this upload was planned; commit compares it with workspace_sync_project_sources.last_applied_session_id before writing.';
COMMENT ON COLUMN workspace_sync_project_upload_applications.applied_revision IS
  'Host-reported applied revision; a value above target_revision is accepted only after application-layer revision lineage and affected-path verification.';
COMMENT ON COLUMN workspace_sync_project_sources.last_applied_target_revision IS
  'Target revision of last_applied_session_id, never the Host current revision.';

-- Receipt verification, source activation and baseline CAS run in one Store
-- transaction; this migration only constrains row shape and references.

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_project_upload_applications)
     OR EXISTS (SELECT 1 FROM workspace_sync_project_sources)
     OR EXISTS (SELECT 1 FROM workspace_sync_sessions WHERE source_binding_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Workspace project source bindings or Host application receipts';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX workspace_sync_project_upload_host_revision;
DROP INDEX workspace_sync_project_upload_one_unresolved;
ALTER TABLE workspace_sync_project_sources
  DROP CONSTRAINT workspace_sync_project_source_baseline_application_fk;
DROP TABLE workspace_sync_project_upload_applications;
ALTER TABLE workspace_sync_sessions
  DROP CONSTRAINT workspace_sync_session_project_source_fk,
  DROP CONSTRAINT workspace_sync_session_source_baseline_fk,
  DROP CONSTRAINT workspace_sync_session_project_source_target_unique,
  DROP CONSTRAINT workspace_sync_session_project_source_session_unique,
  DROP CONSTRAINT workspace_sync_session_project_source_check,
  DROP COLUMN source_baseline_session_id,
  DROP COLUMN source_binding_id;
DROP INDEX workspace_sync_project_source_installation;
DROP INDEX workspace_sync_project_source_active;
DROP TABLE workspace_sync_project_sources;
