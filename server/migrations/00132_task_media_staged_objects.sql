-- +goose Up
ALTER TABLE tasks
  ADD CONSTRAINT tasks_id_owner_media_unique UNIQUE (id, owner_id);
ALTER TABLE tasks
  ADD CONSTRAINT tasks_id_owner_initiator_media_unique UNIQUE (id, owner_id, initiator_id);

ALTER TABLE task_operations
  ADD CONSTRAINT task_operations_task_id_id_media_unique UNIQUE (task_id, id);

ALTER TABLE model_call_ledger
  ADD CONSTRAINT model_call_ledger_media_unique UNIQUE (owner_id, source, call_id, task_id);

CREATE TABLE task_media_staged_objects (
  operation_id       text PRIMARY KEY REFERENCES task_operations(id) ON DELETE CASCADE,
  task_id            text NOT NULL,
  owner_id           text NOT NULL,
  initiator_id       text NOT NULL,
  runtime_call_id    text NOT NULL CHECK (length(runtime_call_id) BETWEEN 8 AND 128),
  ledger_source      text NOT NULL DEFAULT 'task' CHECK (ledger_source = 'task'),
  media_type         text NOT NULL CHECK (media_type IN (
    'image/png', 'image/jpeg', 'image/webp', 'video/mp4',
    'audio/mpeg', 'audio/ogg', 'audio/opus', 'audio/wav', 'audio/aac'
  )),
  size_bytes         bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 314572800),
  sha256             bytea NOT NULL CHECK (octet_length(sha256) = 32),
  expires_at         timestamptz NOT NULL,
  state              text NOT NULL CHECK (state IN ('preview', 'saving', 'published', 'discarded', 'expired')),
  cleanup_state      text NOT NULL DEFAULT 'pending' CHECK (cleanup_state IN ('pending', 'complete')),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, operation_id),
  FOREIGN KEY (task_id, owner_id, initiator_id) REFERENCES tasks(id, owner_id, initiator_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, operation_id) REFERENCES task_operations(task_id, id) ON DELETE CASCADE,
  FOREIGN KEY (initiator_id, ledger_source, runtime_call_id, task_id)
    REFERENCES model_call_ledger(owner_id, source, call_id, task_id) ON DELETE RESTRICT
);
CREATE INDEX task_media_staged_cleanup ON task_media_staged_objects(expires_at, created_at)
  WHERE cleanup_state = 'pending';
CREATE INDEX task_media_staged_cleanup_retry ON task_media_staged_objects(expires_at, created_at)
  WHERE cleanup_state = 'complete';
CREATE INDEX task_media_staged_task ON task_media_staged_objects(task_id, created_at DESC);

ALTER TABLE task_pending_requests
  ADD COLUMN media_preview_operation_id text;
ALTER TABLE task_pending_requests
  ADD CONSTRAINT task_pending_requests_media_preview_fk
  FOREIGN KEY (task_id, media_preview_operation_id)
  REFERENCES task_media_staged_objects(task_id, operation_id)
  ON DELETE SET NULL (media_preview_operation_id);

CREATE VIEW task_media_operation_details AS
SELECT staged.task_id,
       staged.owner_id,
       staged.initiator_id,
       staged.operation_id,
       staged.runtime_call_id,
       operation.operation_key,
       operation.request_hash,
       operation.external_id AS provider_operation_id,
       operation.status AS operation_status,
       ledger.reserved_tokens,
       ledger.state AS token_state,
       ledger.input_tokens,
       ledger.output_tokens,
       staged.media_type,
       staged.size_bytes,
       staged.sha256,
       staged.expires_at,
       staged.state,
       staged.cleanup_state,
       staged.created_at,
       staged.updated_at
FROM task_media_staged_objects AS staged
JOIN task_operations AS operation
  ON operation.task_id = staged.task_id AND operation.id = staged.operation_id
JOIN model_call_ledger AS ledger
  ON ledger.owner_id = staged.initiator_id AND ledger.source = staged.ledger_source
 AND ledger.call_id = staged.runtime_call_id AND ledger.task_id = staged.task_id;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_media_staged_objects) THEN
    RAISE EXCEPTION 'cannot roll back while Task media staged objects exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP VIEW task_media_operation_details;
ALTER TABLE task_pending_requests DROP CONSTRAINT task_pending_requests_media_preview_fk;
ALTER TABLE task_pending_requests DROP COLUMN media_preview_operation_id;
DROP TABLE task_media_staged_objects;
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_media_unique;
ALTER TABLE task_operations DROP CONSTRAINT task_operations_task_id_id_media_unique;
ALTER TABLE tasks DROP CONSTRAINT tasks_id_owner_initiator_media_unique;
ALTER TABLE tasks DROP CONSTRAINT tasks_id_owner_media_unique;
