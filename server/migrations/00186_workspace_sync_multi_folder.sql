-- +goose Up
-- Keep each folder's original string ID while changing the Workspace-level
-- one-folder contract into a folder-keyed contract.
ALTER TABLE workspace_sync_folders
  DROP CONSTRAINT workspace_sync_folders_pkey,
  ADD CONSTRAINT workspace_sync_folders_pkey PRIMARY KEY (workspace_id, folder_id),
  ADD COLUMN project_name_folded text;
UPDATE workspace_sync_folders SET project_name_folded=lower(project_name) WHERE folder_kind='project';
ALTER TABLE workspace_sync_folders
  ADD CONSTRAINT workspace_sync_folder_project_name_folded_check CHECK (
    (folder_kind='workspace_root' AND project_name_folded IS NULL) OR
    (folder_kind='project' AND project_name_folded IS NOT NULL AND project_name_folded<>'')
  );

CREATE UNIQUE INDEX workspace_sync_folder_one_root_per_workspace
  ON workspace_sync_folders(workspace_id) WHERE folder_kind='workspace_root';
CREATE UNIQUE INDEX workspace_sync_folder_project_name_ci
  ON workspace_sync_folders(workspace_id, project_name_folded) WHERE folder_kind='project';

-- +goose StatementBegin
CREATE FUNCTION enforce_workspace_sync_folder_kind_exclusive() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM workspace_sync_folders f
    WHERE f.workspace_id=NEW.workspace_id AND f.folder_id<>NEW.folder_id
      AND f.folder_kind<>NEW.folder_kind
  ) THEN
    RAISE EXCEPTION 'Workspace root and project sync folders are mutually exclusive'
      USING ERRCODE='23505', CONSTRAINT='workspace_sync_folder_kind_exclusive';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER workspace_sync_folder_kind_exclusive
BEFORE INSERT OR UPDATE OF workspace_id,folder_id,folder_kind ON workspace_sync_folders
FOR EACH ROW EXECUTE FUNCTION enforce_workspace_sync_folder_kind_exclusive();

-- Bindings are folder-scoped. Existing rows all point at the Workspace's
-- legacy root folder, which is preserved by this backfill.
ALTER TABLE workspace_sync_device_bindings ADD COLUMN folder_id text;
UPDATE workspace_sync_device_bindings b SET folder_id=f.folder_id
FROM workspace_sync_folders f
WHERE f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_device_bindings WHERE folder_id IS NULL) THEN
    RAISE EXCEPTION 'cannot bind existing Workspace sync devices to their legacy root folder';
  END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE workspace_sync_device_bindings ALTER COLUMN folder_id SET NOT NULL;

-- +goose StatementBegin
DO $$
DECLARE c record;
BEGIN
  FOR c IN
    SELECT conname FROM pg_constraint
    WHERE conrelid='workspace_sync_device_bindings'::regclass
      AND confrelid='workspace_sync_folders'::regclass AND contype='f'
      AND conkey = ARRAY[
        (SELECT attnum FROM pg_attribute WHERE attrelid='workspace_sync_device_bindings'::regclass AND attname='workspace_id'),
        (SELECT attnum FROM pg_attribute WHERE attrelid='workspace_sync_device_bindings'::regclass AND attname='owner_id')
      ]::smallint[]
  LOOP
    EXECUTE format('ALTER TABLE workspace_sync_device_bindings DROP CONSTRAINT %I', c.conname);
  END LOOP;
END;
$$;
-- +goose StatementEnd
ALTER TABLE workspace_sync_folders DROP CONSTRAINT workspace_sync_folders_workspace_id_owner_id_key;
ALTER TABLE workspace_sync_device_bindings
  ADD CONSTRAINT workspace_sync_device_binding_folder_fk
    FOREIGN KEY (workspace_id,owner_id,folder_id)
    REFERENCES workspace_sync_folders(workspace_id,owner_id,folder_id) ON DELETE CASCADE,
  ADD CONSTRAINT workspace_sync_device_binding_folder_unique
    UNIQUE (id,owner_id,workspace_id,folder_id);
DROP INDEX workspace_sync_device_binding_active_workspace;
CREATE UNIQUE INDEX workspace_sync_device_binding_active_folder
  ON workspace_sync_device_bindings(workspace_id,folder_id,device_identity_id) WHERE state<>'revoked';

-- Registration idempotency and receipt ownership now include the folder.
ALTER TABLE workspace_sync_device_registration_requests ADD COLUMN folder_id text;
UPDATE workspace_sync_device_registration_requests r SET folder_id=b.folder_id
FROM workspace_sync_device_bindings b
WHERE b.id=r.binding_id AND b.owner_id=r.owner_id AND b.workspace_id=r.workspace_id;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_device_registration_requests WHERE folder_id IS NULL) THEN
    RAISE EXCEPTION 'cannot bind existing Workspace sync registration receipts to a folder';
  END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE workspace_sync_device_registration_requests ALTER COLUMN folder_id SET NOT NULL;
ALTER TABLE workspace_sync_device_registration_requests
  DROP CONSTRAINT workspace_sync_device_registration_requests_pkey;
ALTER TABLE workspace_sync_device_registration_requests
  ADD CONSTRAINT workspace_sync_device_registration_requests_pkey
    PRIMARY KEY (owner_id,workspace_id,folder_id,idempotency_key_hash);
-- +goose StatementBegin
DO $$
DECLARE c record;
BEGIN
  FOR c IN
    SELECT conname FROM pg_constraint
    WHERE conrelid='workspace_sync_device_registration_requests'::regclass
      AND confrelid='workspace_sync_device_bindings'::regclass AND contype='f'
  LOOP
    EXECUTE format('ALTER TABLE workspace_sync_device_registration_requests DROP CONSTRAINT %I', c.conname);
  END LOOP;
END;
$$;
-- +goose StatementEnd
ALTER TABLE workspace_sync_device_registration_requests
  ADD CONSTRAINT workspace_sync_device_registration_binding_folder_fk
    FOREIGN KEY (binding_id,owner_id,workspace_id,folder_id)
    REFERENCES workspace_sync_device_bindings(id,owner_id,workspace_id,folder_id);

-- Receive operation and quota-rejection identity includes the registered
-- folder. Existing receive state is attached to the legacy root target only.
ALTER TABLE workspace_sync_receive_reservations ADD COLUMN folder_id text;
UPDATE workspace_sync_receive_reservations r SET folder_id=f.folder_id
FROM workspace_sync_folders f
WHERE f.workspace_id=r.workspace_id AND f.owner_id=r.owner_id AND f.folder_kind='workspace_root';
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_receive_reservations WHERE folder_id IS NULL) THEN
    RAISE EXCEPTION 'cannot map existing Workspace receive reservations to their legacy root folder';
  END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE workspace_sync_receive_reservations
  ALTER COLUMN folder_id SET NOT NULL,
  ADD CONSTRAINT workspace_sync_receive_reservation_folder_fk
    FOREIGN KEY (workspace_id,owner_id,folder_id)
    REFERENCES workspace_sync_folders(workspace_id,owner_id,folder_id) ON DELETE CASCADE;
-- +goose StatementBegin
DO $$
DECLARE c record;
BEGIN
  FOR c IN
    SELECT conname FROM pg_constraint
    WHERE conrelid='workspace_sync_receive_reservations'::regclass AND contype='u'
      AND conkey = ARRAY[
        (SELECT attnum FROM pg_attribute WHERE attrelid='workspace_sync_receive_reservations'::regclass AND attname='owner_id'),
        (SELECT attnum FROM pg_attribute WHERE attrelid='workspace_sync_receive_reservations'::regclass AND attname='workspace_id'),
        (SELECT attnum FROM pg_attribute WHERE attrelid='workspace_sync_receive_reservations'::regclass AND attname='operation_id')
      ]::smallint[]
  LOOP
    EXECUTE format('ALTER TABLE workspace_sync_receive_reservations DROP CONSTRAINT %I', c.conname);
  END LOOP;
END;
$$;
-- +goose StatementEnd
ALTER TABLE workspace_sync_receive_reservations
  ADD CONSTRAINT workspace_sync_receive_reservation_folder_operation_unique
    UNIQUE (owner_id,workspace_id,folder_id,operation_id);

ALTER TABLE workspace_sync_receive_quota_rejections ADD COLUMN folder_id text;
UPDATE workspace_sync_receive_quota_rejections q SET folder_id=f.folder_id
FROM workspace_sync_folders f
WHERE f.workspace_id=q.workspace_id AND f.owner_id=q.owner_id AND f.folder_kind='workspace_root';
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_receive_quota_rejections WHERE folder_id IS NULL) THEN
    RAISE EXCEPTION 'cannot map existing Workspace quota rejections to their legacy root folder';
  END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE workspace_sync_receive_quota_rejections
  ALTER COLUMN folder_id SET NOT NULL,
  DROP CONSTRAINT workspace_sync_receive_quota_rejections_pkey,
  ADD CONSTRAINT workspace_sync_receive_quota_rejections_pkey
    PRIMARY KEY (owner_id,workspace_id,folder_id,operation_id),
  ADD CONSTRAINT workspace_sync_receive_quota_rejection_folder_fk
    FOREIGN KEY (workspace_id,owner_id,folder_id)
    REFERENCES workspace_sync_folders(workspace_id,owner_id,folder_id) ON DELETE CASCADE;
DROP INDEX workspace_sync_receive_quota_pauses_active;
CREATE INDEX workspace_sync_receive_quota_pauses_active
  ON workspace_sync_receive_quota_rejections(owner_id,workspace_id,folder_id,folded_path,updated_at DESC,operation_id DESC)
  WHERE state='paused';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_folders WHERE folder_kind='project')
     OR EXISTS (SELECT 1 FROM workspace_sync_device_bindings b JOIN workspace_sync_folders f
          ON f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id AND f.folder_id=b.folder_id
          WHERE f.folder_kind<>'workspace_root')
     OR EXISTS (SELECT 1 FROM workspace_sync_receive_reservations r JOIN workspace_sync_folders f
          ON f.workspace_id=r.workspace_id AND f.owner_id=r.owner_id AND f.folder_id=r.folder_id
          WHERE f.folder_kind<>'workspace_root')
     OR EXISTS (SELECT 1 FROM workspace_sync_receive_quota_rejections q JOIN workspace_sync_folders f
          ON f.workspace_id=q.workspace_id AND f.owner_id=q.owner_id AND f.folder_id=q.folder_id
          WHERE f.folder_kind<>'workspace_root') THEN
    RAISE EXCEPTION 'cannot roll back multi-folder Workspace sync data';
  END IF;
END $$;
-- +goose StatementEnd

-- Restore the referenced key before recreating the legacy binding foreign key.
ALTER TABLE workspace_sync_folders
  ADD CONSTRAINT workspace_sync_folders_workspace_id_owner_id_key UNIQUE (workspace_id,owner_id);

DROP INDEX workspace_sync_receive_quota_pauses_active;
CREATE INDEX workspace_sync_receive_quota_pauses_active
  ON workspace_sync_receive_quota_rejections(owner_id,workspace_id,folded_path,updated_at DESC,operation_id DESC)
  WHERE state='paused';
ALTER TABLE workspace_sync_receive_quota_rejections
  DROP CONSTRAINT workspace_sync_receive_quota_rejection_folder_fk,
  DROP CONSTRAINT workspace_sync_receive_quota_rejections_pkey,
  ADD CONSTRAINT workspace_sync_receive_quota_rejections_pkey PRIMARY KEY (owner_id,workspace_id,operation_id),
  DROP COLUMN folder_id;

ALTER TABLE workspace_sync_receive_reservations
  DROP CONSTRAINT workspace_sync_receive_reservation_folder_fk,
  DROP CONSTRAINT workspace_sync_receive_reservation_folder_operation_unique,
  ADD CONSTRAINT workspace_sync_receive_reservations_owner_id_workspace_id_operation_id_key
    UNIQUE (owner_id,workspace_id,operation_id),
  DROP COLUMN folder_id;

ALTER TABLE workspace_sync_device_registration_requests
  DROP CONSTRAINT workspace_sync_device_registration_binding_folder_fk,
  DROP CONSTRAINT workspace_sync_device_registration_requests_pkey,
  ADD CONSTRAINT workspace_sync_device_registration_requests_pkey PRIMARY KEY (owner_id,workspace_id,idempotency_key_hash),
  ADD CONSTRAINT workspace_sync_device_registration_requests_binding_id_owner_id_workspace_id_fkey
    FOREIGN KEY (binding_id,owner_id,workspace_id)
    REFERENCES workspace_sync_device_bindings(id,owner_id,workspace_id),
  DROP COLUMN folder_id;
DROP INDEX workspace_sync_device_binding_active_folder;
ALTER TABLE workspace_sync_device_bindings
  DROP CONSTRAINT workspace_sync_device_binding_folder_fk,
  DROP CONSTRAINT workspace_sync_device_binding_folder_unique,
  ADD CONSTRAINT workspace_sync_device_bindings_workspace_id_owner_id_fkey1
    FOREIGN KEY (workspace_id,owner_id) REFERENCES workspace_sync_folders(workspace_id,owner_id) ON DELETE CASCADE,
  DROP COLUMN folder_id;
CREATE UNIQUE INDEX workspace_sync_device_binding_active_workspace
  ON workspace_sync_device_bindings(workspace_id,device_identity_id) WHERE state<>'revoked';

DROP TRIGGER workspace_sync_folder_kind_exclusive ON workspace_sync_folders;
DROP FUNCTION enforce_workspace_sync_folder_kind_exclusive();
DROP INDEX workspace_sync_folder_project_name_ci;
DROP INDEX workspace_sync_folder_one_root_per_workspace;
ALTER TABLE workspace_sync_folders
  DROP CONSTRAINT workspace_sync_folders_pkey,
  ADD CONSTRAINT workspace_sync_folders_pkey PRIMARY KEY (workspace_id),
  DROP CONSTRAINT workspace_sync_folder_project_name_folded_check,
  DROP COLUMN project_name_folded;
