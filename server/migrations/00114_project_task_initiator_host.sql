-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_host_id_owner_id_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_host_id_initiator_id_fkey
  FOREIGN KEY (host_id, initiator_id) REFERENCES hosts(id, owner_id);

-- +goose Down
ALTER TABLE tasks DROP CONSTRAINT tasks_host_id_initiator_id_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_host_id_owner_id_fkey
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id);
