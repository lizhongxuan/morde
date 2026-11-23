-- +goose Up
ALTER TABLE task_step_details
  DROP CONSTRAINT task_step_details_payload_ciphertext_check,
  ADD CONSTRAINT task_step_details_payload_ciphertext_check
    CHECK (octet_length(payload_ciphertext) BETWEEN 29 AND 262172),
  DROP CONSTRAINT task_step_details_payload_size_bytes_check,
  ADD CONSTRAINT task_step_details_payload_size_bytes_check
    CHECK (payload_size_bytes BETWEEN 1 AND 262144);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM task_step_details
    WHERE octet_length(payload_ciphertext) > 65564 OR payload_size_bytes > 65536
  ) THEN
    RAISE EXCEPTION 'cannot reduce Task step detail capacity while larger encrypted details exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_step_details
  DROP CONSTRAINT task_step_details_payload_ciphertext_check,
  ADD CONSTRAINT task_step_details_payload_ciphertext_check
    CHECK (octet_length(payload_ciphertext) BETWEEN 29 AND 65564),
  DROP CONSTRAINT task_step_details_payload_size_bytes_check,
  ADD CONSTRAINT task_step_details_payload_size_bytes_check
    CHECK (payload_size_bytes BETWEEN 1 AND 65536);
