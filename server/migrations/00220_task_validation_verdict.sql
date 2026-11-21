-- Persist only the server-derived summary beside the revision-bound review.
-- Older and unresolved reviews remain pending by default.
-- +goose Up
ALTER TABLE task_validation_reviews
  ADD COLUMN validation_state text NOT NULL DEFAULT 'pending'
    CHECK (validation_state IN ('pending', 'PASS', 'PASS+NOTES', 'FAIL')),
  ADD CONSTRAINT task_validation_reviews_verdict_state_check
    CHECK (state = 'reviewed' OR validation_state = 'pending');

-- +goose Down
ALTER TABLE task_validation_reviews
  DROP CONSTRAINT task_validation_reviews_verdict_state_check,
  DROP COLUMN validation_state;
