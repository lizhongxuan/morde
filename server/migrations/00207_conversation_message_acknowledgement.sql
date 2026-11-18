-- +goose Up
ALTER TABLE conversation_message_routes
  ADD COLUMN acknowledgement_requested boolean NOT NULL DEFAULT false,
  ADD COLUMN agent_acknowledged boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT conversation_message_routes_acknowledgement_state_check
    CHECK (NOT (acknowledgement_requested AND agent_acknowledged));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_routes
             WHERE acknowledgement_requested OR agent_acknowledged) THEN
    RAISE EXCEPTION 'cannot remove conversation message acknowledgements after they have been recorded';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_message_routes
  DROP CONSTRAINT conversation_message_routes_acknowledgement_state_check,
  DROP COLUMN agent_acknowledged,
  DROP COLUMN acknowledgement_requested;
