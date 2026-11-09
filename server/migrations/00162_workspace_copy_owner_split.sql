-- +goose Up
ALTER TABLE workspace_copy_reservations
  ADD COLUMN source_owner_id text,
  ADD COLUMN target_owner_id text,
  ADD COLUMN share_id text,
  ADD COLUMN read_only boolean NOT NULL DEFAULT false;

UPDATE workspace_copy_reservations AS c
SET source_owner_id=c.owner_id,target_owner_id=w.owner_id,read_only=w.is_read_only
FROM workspaces w
WHERE w.id=c.target_workspace_id;

ALTER TABLE workspace_copy_reservations
  ALTER COLUMN source_owner_id SET NOT NULL,
  ALTER COLUMN target_owner_id SET NOT NULL,
  ADD CONSTRAINT workspace_copy_reservations_source_owner_id_fk
    FOREIGN KEY (source_workspace_id,source_owner_id) REFERENCES workspaces(id,owner_id),
  ADD CONSTRAINT workspace_copy_reservations_target_owner_fk
    FOREIGN KEY (target_workspace_id,target_owner_id) REFERENCES workspaces(id,owner_id)
    DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT workspace_copy_reservations_share_fk
    FOREIGN KEY (share_id) REFERENCES workspace_share_links(id) ON DELETE CASCADE,
  ADD CONSTRAINT workspace_copy_reservations_legacy_owner_check
    CHECK (owner_id=source_owner_id),
  ADD CONSTRAINT workspace_copy_reservations_owner_split_check
    CHECK ((share_id IS NULL AND source_owner_id=target_owner_id AND NOT read_only) OR
           (share_id IS NOT NULL AND source_owner_id<>target_owner_id) OR
           (share_id IS NULL AND source_owner_id<>target_owner_id AND state='completed'));

CREATE UNIQUE INDEX workspace_copy_reservations_share_unique
  ON workspace_copy_reservations(share_id)
  WHERE share_id IS NOT NULL;
CREATE INDEX workspace_copy_reservations_target_owner_active
  ON workspace_copy_reservations(target_owner_id)
  WHERE state IN ('reserved','copying','unknown');

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM workspace_copy_reservations
    WHERE share_id IS NOT NULL OR (source_owner_id<>target_owner_id AND state<>'completed')
  ) THEN
    RAISE EXCEPTION 'cannot roll back recipient-owned Workspace share copies';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX workspace_copy_reservations_target_owner_active;
DROP INDEX workspace_copy_reservations_share_unique;
ALTER TABLE workspace_copy_reservations
  DROP CONSTRAINT workspace_copy_reservations_owner_split_check,
  DROP CONSTRAINT workspace_copy_reservations_legacy_owner_check,
  DROP CONSTRAINT workspace_copy_reservations_share_fk,
  DROP CONSTRAINT workspace_copy_reservations_target_owner_fk,
  DROP CONSTRAINT workspace_copy_reservations_source_owner_id_fk,
  DROP COLUMN read_only,
  DROP COLUMN share_id,
  DROP COLUMN target_owner_id,
  DROP COLUMN source_owner_id;
