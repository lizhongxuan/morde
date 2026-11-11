-- +goose Up
CREATE TABLE personal_memory_sources (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  object_type text NOT NULL DEFAULT 'memory' CHECK (object_type='memory'),
  object_id text NOT NULL,
  object_revision bigint NOT NULL CHECK (object_revision > 0),
  source_order integer NOT NULL CHECK (source_order > 0),
  mutation_id text NOT NULL,
  source_kind text NOT NULL CHECK (source_kind IN ('conversation_message','mutation_receipt')),
  source_id text NOT NULL CHECK (length(source_id) BETWEEN 1 AND 128),
  source_task_id text,
  source_conversation_id text,
  source_sequence bigint,
  source_sha256 bytea NOT NULL CHECK (octet_length(source_sha256)=32),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id,owner_id,object_id,object_revision,source_order),
  FOREIGN KEY (service_instance_id,owner_id,object_type,object_id)
    REFERENCES account_object_versions(service_instance_id,owner_id,object_type,object_id) ON DELETE RESTRICT,
  FOREIGN KEY (source_task_id) REFERENCES tasks(id) ON DELETE RESTRICT,
  FOREIGN KEY (source_id,owner_id,source_conversation_id)
    REFERENCES conversation_messages(id,owner_id,conversation_id) ON DELETE RESTRICT,
  CHECK ((source_kind='conversation_message' AND source_task_id IS NOT NULL AND source_conversation_id IS NOT NULL
          AND source_sequence > 0) OR
         (source_kind='mutation_receipt' AND source_task_id IS NULL AND source_conversation_id IS NULL
          AND source_sequence IS NULL AND source_id=mutation_id))
);

CREATE INDEX personal_memory_sources_message
  ON personal_memory_sources(source_id,owner_id,source_conversation_id) WHERE source_kind='conversation_message';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM personal_memory_sources) THEN
    RAISE EXCEPTION 'cannot roll back personal memory source provenance';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE personal_memory_sources;
