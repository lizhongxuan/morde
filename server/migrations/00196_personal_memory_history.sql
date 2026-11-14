-- +goose Up
CREATE TABLE personal_memory_history (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  object_type text NOT NULL DEFAULT 'memory' CHECK (object_type='memory'),
  object_id text NOT NULL,
  object_revision bigint NOT NULL CHECK (object_revision > 0),
  body jsonb CHECK (body IS NULL OR jsonb_typeof(body)='object'),
  changed_at timestamptz NOT NULL,
  PRIMARY KEY (service_instance_id,owner_id,object_id,object_revision),
  FOREIGN KEY (service_instance_id,owner_id,object_type,object_id)
    REFERENCES account_object_versions(service_instance_id,owner_id,object_type,object_id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM personal_memory_history) THEN
    RAISE EXCEPTION 'cannot roll back personal memory history while revision records exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE personal_memory_history;
