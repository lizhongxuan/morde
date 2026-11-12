-- +goose Up
ALTER TABLE workspace_sync_folders
  ADD COLUMN folder_kind text NOT NULL DEFAULT 'workspace_root'
    CHECK (folder_kind IN ('workspace_root', 'project')),
  ADD COLUMN project_name text,
  ADD COLUMN relative_path text NOT NULL DEFAULT '.',
  ADD CONSTRAINT workspace_sync_folder_target_check CHECK (
    (folder_kind = 'workspace_root' AND project_name IS NULL AND relative_path = '.') OR
    (folder_kind = 'project' AND project_name IS NOT NULL AND project_name <> '' AND
      relative_path = 'project/' || project_name AND octet_length(relative_path) <= 1024 AND
      relative_path ~ '^project/[^/]+$' AND project_name NOT IN ('.', '..'))
  ),
  ADD CONSTRAINT workspace_sync_folder_workspace_owner_id_unique UNIQUE (workspace_id, owner_id, folder_id);

ALTER TABLE workspace_sync_device_challenges
  ADD COLUMN folder_id text,
  ADD COLUMN folder_kind text NOT NULL DEFAULT 'workspace_root'
    CHECK (folder_kind IN ('workspace_root', 'project')),
  ADD COLUMN relative_path text NOT NULL DEFAULT '.',
  ADD CONSTRAINT workspace_sync_device_challenge_folder_check CHECK (
    (folder_id IS NULL AND folder_kind = 'workspace_root' AND relative_path = '.') OR
    (folder_id IS NOT NULL AND folder_kind = 'project' AND relative_path ~ '^project/[^/]+$' AND
      octet_length(relative_path) <= 1024)
  ),
  ADD CONSTRAINT workspace_sync_device_challenge_folder_fk
    FOREIGN KEY (workspace_id, owner_id, folder_id)
    REFERENCES workspace_sync_folders(workspace_id, owner_id, folder_id);

CREATE TABLE workspace_sync_folder_registration_requests (
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host_id text NOT NULL,
  workspace_id text NOT NULL,
  idempotency_key_hash bytea NOT NULL CHECK (octet_length(idempotency_key_hash) = 32),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  folder_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, host_id, idempotency_key_hash),
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, owner_id, folder_id)
    REFERENCES workspace_sync_folders(workspace_id, owner_id, folder_id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_folder_registration_requests)
     OR EXISTS (SELECT 1 FROM workspace_sync_folders WHERE folder_kind = 'project')
     OR EXISTS (SELECT 1 FROM workspace_sync_device_challenges WHERE folder_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Workspace project folder registrations';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE workspace_sync_folder_registration_requests;
ALTER TABLE workspace_sync_device_challenges
  DROP CONSTRAINT workspace_sync_device_challenge_folder_fk,
  DROP CONSTRAINT workspace_sync_device_challenge_folder_check,
  DROP COLUMN relative_path,
  DROP COLUMN folder_kind,
  DROP COLUMN folder_id;
ALTER TABLE workspace_sync_folders
  DROP CONSTRAINT workspace_sync_folder_workspace_owner_id_unique,
  DROP CONSTRAINT workspace_sync_folder_target_check,
  DROP COLUMN relative_path,
  DROP COLUMN project_name,
  DROP COLUMN folder_kind;
