-- +goose Up
-- Task actors may use a Host owned by another account when that Host grants
-- them desktop:control. Keep the Host identity foreign key while moving the
-- authorization check into the transactional submission/claim/operation gates.
ALTER TABLE conversations DROP CONSTRAINT conversations_host_id_owner_id_fkey;
ALTER TABLE conversations ADD CONSTRAINT conversations_host_id_fkey
  FOREIGN KEY (host_id) REFERENCES hosts(id);

ALTER TABLE tasks DROP CONSTRAINT tasks_host_id_initiator_id_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_host_id_fkey
  FOREIGN KEY (host_id) REFERENCES hosts(id);

-- +goose Down
-- Do not silently make cross-account Host conversations/tasks invalid again.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM conversations c JOIN hosts h ON h.id=c.host_id
    WHERE c.host_id IS NOT NULL AND h.owner_id <> c.owner_id
  ) OR EXISTS (
    SELECT 1 FROM tasks t JOIN hosts h ON h.id=t.host_id
    WHERE t.host_id IS NOT NULL AND h.owner_id <> t.initiator_id
  ) THEN
    RAISE EXCEPTION 'cannot restore Host owner foreign keys while shared-Host conversations or tasks exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE tasks DROP CONSTRAINT tasks_host_id_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_host_id_initiator_id_fkey
  FOREIGN KEY (host_id, initiator_id) REFERENCES hosts(id, owner_id);

ALTER TABLE conversations DROP CONSTRAINT conversations_host_id_fkey;
ALTER TABLE conversations ADD CONSTRAINT conversations_host_id_owner_id_fkey
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id);
