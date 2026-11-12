-- +goose Up
ALTER TABLE conversation_result_batches
  ADD COLUMN retry_attempt bigint NOT NULL DEFAULT 0 CHECK (retry_attempt >= 0);

ALTER TABLE conversation_route_model_calls
  DROP CONSTRAINT conversation_route_model_calls_phase_check,
  ADD CONSTRAINT conversation_route_model_calls_phase_check
    CHECK (phase IN ('normal', 'missing_predecessor') OR phase ~ '^result_review_[1-9][0-9]*$');

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_result_batches WHERE retry_attempt > 0)
     OR EXISTS (SELECT 1 FROM conversation_route_model_calls WHERE phase ~ '^result_review_[0-9]+$') THEN
    RAISE EXCEPTION 'cannot discard task result review retry evidence';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_result_batches DROP COLUMN retry_attempt;
ALTER TABLE conversation_route_model_calls
  DROP CONSTRAINT conversation_route_model_calls_phase_check,
  ADD CONSTRAINT conversation_route_model_calls_phase_check CHECK (phase IN ('normal', 'missing_predecessor'));
