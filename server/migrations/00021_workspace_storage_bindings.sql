-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM hosts WHERE state = 'deleting')
     OR EXISTS (SELECT 1 FROM workspaces WHERE state = 'deleting') THEN
    RAISE EXCEPTION 'workspace migration preflight failed: finish in-flight host and workspace deletion first';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM hosts h
    LEFT JOIN workspaces w ON w.id = h.workspace_id
    WHERE h.workspace_id IS NOT NULL
      AND (w.id IS NULL OR w.owner_id <> h.owner_id OR w.volume_name = '' OR w.size_mib <= 0)
  ) THEN
    RAISE EXCEPTION 'workspace migration preflight failed: host workspace reference, owner, or legacy volume metadata is invalid';
  END IF;

  IF EXISTS (
    SELECT 1 FROM workspaces w
    WHERE w.volume_name = '' OR w.size_mib <= 0
  ) THEN
    RAISE EXCEPTION 'workspace migration preflight failed: legacy volume metadata is invalid';
  END IF;

  IF EXISTS (
    SELECT 1 FROM workspaces
    WHERE id !~ '^w[a-z0-9]{7}$'
  ) THEN
    RAISE EXCEPTION 'workspace migration preflight failed: workspace IDs cannot be used as safe host directory names';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM workspaces w
    LEFT JOIN hosts h ON h.id = w.attached_host_id
    WHERE (w.state = 'attached' AND (w.attached_host_id IS NULL OR h.id IS NULL OR h.workspace_id IS DISTINCT FROM w.id OR h.owner_id <> w.owner_id))
       OR (w.state <> 'attached' AND w.attached_host_id IS NOT NULL)
       OR (w.state = 'attached' AND h.state IN ('deleting', 'deleted'))
  ) THEN
    RAISE EXCEPTION 'workspace migration preflight failed: workspace attachment pointers or states disagree';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspaces DROP CONSTRAINT workspaces_state_check;

CREATE TABLE workspace_legacy_volumes (
  workspace_id text PRIMARY KEY REFERENCES workspaces(id),
  volume_name text NOT NULL UNIQUE,
  import_state text NOT NULL DEFAULT 'pending' CHECK (import_state IN ('pending', 'imported', 'verified', 'not_required')),
  imported_at timestamptz,
  verified_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state)
SELECT id, volume_name, CASE WHEN state = 'deleted' THEN 'not_required' ELSE 'pending' END FROM workspaces;

ALTER TABLE workspaces
  ADD COLUMN revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
  ADD COLUMN used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  ADD COLUMN quota_bytes bigint NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
  ADD CONSTRAINT workspaces_id_owner_unique UNIQUE (id, owner_id);

UPDATE workspaces SET quota_bytes = size_mib::bigint * 1048576;

ALTER TABLE workspaces
  ALTER COLUMN quota_bytes DROP DEFAULT;

CREATE TABLE workspace_bindings (
  id text PRIMARY KEY,
  owner_id text NOT NULL REFERENCES users(id),
  host_id text NOT NULL,
  workspace_id text NOT NULL,
  directory_name text NOT NULL CHECK (directory_name ~ '^w[a-z0-9]{7}$'),
  state text NOT NULL CHECK (state IN ('preparing', 'active', 'unbinding', 'detached', 'failed')),
  binding_version bigint NOT NULL CHECK (binding_version > 0),
  desired_revision bigint NOT NULL DEFAULT 0 CHECK (desired_revision >= 0),
  applied_revision bigint NOT NULL DEFAULT 0 CHECK (applied_revision >= 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT workspace_bindings_host_owner_fk FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id),
  CONSTRAINT workspace_bindings_workspace_owner_fk FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id),
  CONSTRAINT workspace_bindings_host_workspace_unique UNIQUE (host_id, workspace_id),
  CONSTRAINT workspace_bindings_host_directory_unique UNIQUE (host_id, directory_name)
);
CREATE UNIQUE INDEX workspace_bindings_one_active_workspace
  ON workspace_bindings(workspace_id)
  WHERE state IN ('preparing', 'active', 'unbinding');
CREATE INDEX workspace_bindings_owner_host ON workspace_bindings(owner_id, host_id);

INSERT INTO workspace_bindings (
  id, owner_id, host_id, workspace_id, directory_name, state,
  binding_version, desired_revision, applied_revision
)
SELECT 'b' || md5(h.id || ':' || w.id), h.owner_id, h.id, w.id, w.id,
       CASE WHEN w.attached_host_id = h.id AND w.state = 'attached' THEN 'active' ELSE 'detached' END,
       1, 0, 0
FROM hosts h
JOIN workspaces w ON w.id = h.workspace_id;

UPDATE workspaces SET state = CASE state
  WHEN 'attached' THEN 'ready'
  WHEN 'detached' THEN 'ready'
  ELSE state
END;

ALTER TABLE workspaces
  ADD CONSTRAINT workspaces_state_check CHECK (state IN ('ready', 'deleting', 'deleted'));

DROP INDEX workspaces_one_per_host;
ALTER TABLE workspaces DROP COLUMN attached_host_id, DROP COLUMN volume_name, DROP COLUMN size_mib;
ALTER TABLE hosts DROP CONSTRAINT hosts_workspace_id_fkey;
ALTER TABLE hosts DROP COLUMN workspace_id, DROP COLUMN keep_workspace;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM workspace_bindings
    WHERE state NOT IN ('active', 'detached') OR directory_name <> workspace_id
       OR binding_version <> 1 OR desired_revision <> 0 OR applied_revision <> 0
  ) THEN
    RAISE EXCEPTION 'cannot roll back changed workspace bindings';
  END IF;
  IF EXISTS (
    SELECT 1 FROM workspace_bindings GROUP BY host_id HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'cannot roll back multiple workspace bindings per host';
  END IF;
  IF EXISTS (
    SELECT 1 FROM workspaces w
    LEFT JOIN workspace_legacy_volumes v ON v.workspace_id = w.id
    WHERE v.workspace_id IS NULL
       OR (w.state <> 'deleted' AND v.import_state <> 'pending')
       OR w.state NOT IN ('ready', 'deleted') OR w.revision <> 0 OR w.used_bytes <> 0
  ) THEN
    RAISE EXCEPTION 'cannot roll back workspaces after import, revision, or content changes';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE hosts ADD COLUMN workspace_id text REFERENCES workspaces(id);
ALTER TABLE hosts ADD COLUMN keep_workspace boolean;
ALTER TABLE workspaces
  ADD COLUMN volume_name text,
  ADD COLUMN size_mib integer,
  ADD COLUMN attached_host_id text;

ALTER TABLE workspaces DROP CONSTRAINT workspaces_state_check;

UPDATE workspaces w
SET volume_name = v.volume_name,
    size_mib = greatest(1, ceil(w.quota_bytes::numeric / 1048576)::integer)
FROM workspace_legacy_volumes v
WHERE v.workspace_id = w.id;

ALTER TABLE workspaces
  ALTER COLUMN volume_name SET NOT NULL,
  ALTER COLUMN size_mib SET NOT NULL,
  ADD CONSTRAINT workspaces_volume_name_key UNIQUE (volume_name);

UPDATE hosts h
SET workspace_id = b.workspace_id
FROM workspace_bindings b
WHERE b.host_id = h.id;

UPDATE workspaces SET state = CASE WHEN state = 'ready' THEN 'detached' ELSE state END;

UPDATE workspaces w
SET state = 'attached', attached_host_id = b.host_id
FROM workspace_bindings b
WHERE b.workspace_id = w.id AND b.state = 'active';

ALTER TABLE workspaces ADD CONSTRAINT workspaces_state_check CHECK (state IN ('attached', 'detached', 'deleting', 'deleted'));
CREATE UNIQUE INDEX workspaces_one_per_host ON workspaces(attached_host_id) WHERE attached_host_id IS NOT NULL;
DROP TABLE workspace_bindings;
DROP TABLE workspace_legacy_volumes;
ALTER TABLE workspaces DROP CONSTRAINT workspaces_id_owner_unique;
ALTER TABLE workspaces DROP COLUMN revision, DROP COLUMN used_bytes, DROP COLUMN quota_bytes;
