-- +goose Up
-- Accepted duplicate messages remain distinct timeline entries but point to
-- their actor's original route. Attempt numbers preserve each billed call.
ALTER TABLE conversation_message_routes
  ADD COLUMN duplicate_of_message_id text,
  ADD COLUMN route_attempt bigint NOT NULL DEFAULT 0 CHECK (route_attempt >= 0),
  ADD COLUMN auto_retry_count smallint NOT NULL DEFAULT 0 CHECK (auto_retry_count BETWEEN 0 AND 3),
  ADD CONSTRAINT conversation_message_routes_scoped_message_unique
    UNIQUE (service_instance_id, owner_id, actor_id, conversation_id, message_id),
  ADD CONSTRAINT conversation_message_routes_duplicate_scope_fk
    FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, duplicate_of_message_id)
    REFERENCES conversation_message_routes(service_instance_id, owner_id, actor_id, conversation_id, message_id)
    ON DELETE RESTRICT,
  ADD CONSTRAINT conversation_message_routes_duplicate_not_self
    CHECK (duplicate_of_message_id IS NULL OR duplicate_of_message_id <> message_id);

CREATE INDEX conversation_message_routes_duplicate_of
  ON conversation_message_routes(service_instance_id, owner_id, actor_id, conversation_id, duplicate_of_message_id)
  WHERE duplicate_of_message_id IS NOT NULL;

ALTER TABLE conversation_route_model_calls
  ADD COLUMN attempt bigint NOT NULL DEFAULT 0 CHECK (attempt >= 0);
ALTER TABLE conversation_route_model_calls DROP CONSTRAINT conversation_route_model_calls_pkey;
ALTER TABLE conversation_route_model_calls ADD PRIMARY KEY (message_id, phase, attempt);

-- Existing receipts remain legacy attempt 0; new receipts use the versioned classifier.
ALTER TABLE conversation_message_routes ALTER COLUMN route_attempt SET DEFAULT 1;

-- +goose Down
-- Links and additional call attempts are durable user-visible and billing
-- evidence. Refuse rollback rather than silently detach or discard them.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_routes WHERE duplicate_of_message_id IS NOT NULL
      OR route_attempt <> 0 OR auto_retry_count <> 0)
     OR EXISTS (SELECT 1 FROM conversation_route_model_calls WHERE attempt <> 0) THEN
    RAISE EXCEPTION 'cannot discard conversation route association or retry evidence';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_route_model_calls DROP CONSTRAINT conversation_route_model_calls_pkey;
ALTER TABLE conversation_route_model_calls ADD PRIMARY KEY (message_id, phase);
ALTER TABLE conversation_route_model_calls DROP COLUMN attempt;
DROP INDEX conversation_message_routes_duplicate_of;
ALTER TABLE conversation_message_routes
  DROP CONSTRAINT conversation_message_routes_duplicate_not_self,
  DROP CONSTRAINT conversation_message_routes_duplicate_scope_fk,
  DROP CONSTRAINT conversation_message_routes_scoped_message_unique,
  DROP COLUMN duplicate_of_message_id,
  DROP COLUMN route_attempt,
  DROP COLUMN auto_retry_count;
