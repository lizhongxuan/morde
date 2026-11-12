-- +goose Up
ALTER TABLE workspace_copy_reservations
  ADD COLUMN source_host_id text,
  ADD COLUMN source_binding_version bigint NOT NULL DEFAULT 0
    CHECK (source_binding_version >= 0),
  ADD COLUMN host_copy_pending boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT workspace_copy_reservations_host_copy_pending_check
    CHECK (NOT host_copy_pending OR
           (source_host_id IS NOT NULL AND share_id IS NULL AND state <> 'failed')),
  ADD CONSTRAINT workspace_copy_reservations_source_host_owner_fk
    FOREIGN KEY (source_host_id, source_owner_id) REFERENCES hosts(id, owner_id),
  ADD CONSTRAINT workspace_copy_reservations_source_host_binding_check
    CHECK ((source_host_id IS NULL AND source_binding_version = 0) OR
           (source_host_id IS NOT NULL AND source_binding_version > 0));

CREATE INDEX workspace_copy_reservations_source_host_active
  ON workspace_copy_reservations(source_host_id)
  WHERE source_host_id IS NOT NULL AND state IN ('reserved', 'copying', 'unknown');

CREATE INDEX workspace_copy_reservations_host_copy_pending
  ON workspace_copy_reservations(source_host_id, target_workspace_id)
  WHERE host_copy_pending;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_copy_reservations WHERE source_host_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Workspace copies that record a source Host';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX workspace_copy_reservations_source_host_active;
DROP INDEX workspace_copy_reservations_host_copy_pending;
ALTER TABLE workspace_copy_reservations
  DROP CONSTRAINT workspace_copy_reservations_host_copy_pending_check,
  DROP CONSTRAINT workspace_copy_reservations_source_host_binding_check,
  DROP CONSTRAINT workspace_copy_reservations_source_host_owner_fk,
  DROP COLUMN host_copy_pending,
  DROP COLUMN source_binding_version,
  DROP COLUMN source_host_id;
