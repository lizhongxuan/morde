-- +goose Up
ALTER TABLE task_agent_instances
  ADD COLUMN guardian_stop_attestation bytea,
  ADD COLUMN guardian_stop_attestation_digest bytea;

ALTER TABLE task_agent_instances
  DROP CONSTRAINT IF EXISTS task_agent_instances_check;

ALTER TABLE task_agent_instances
  ADD CONSTRAINT task_agent_instances_stop_evidence_check CHECK (
    (state = 'active' AND stop_receipt IS NULL AND stop_receipt_digest IS NULL
      AND guardian_stop_attestation IS NULL AND guardian_stop_attestation_digest IS NULL AND stopped_at IS NULL)
    OR
    (state = 'stopped' AND stopped_at IS NOT NULL
      AND ((stop_receipt IS NULL AND stop_receipt_digest IS NULL)
        OR (stop_receipt IS NOT NULL AND octet_length(stop_receipt) BETWEEN 1 AND 4096
          AND stop_receipt_digest IS NOT NULL AND octet_length(stop_receipt_digest) = 32))
      AND ((guardian_stop_attestation IS NULL AND guardian_stop_attestation_digest IS NULL)
        OR (guardian_stop_attestation IS NOT NULL AND octet_length(guardian_stop_attestation) BETWEEN 1 AND 8192
          AND guardian_stop_attestation_digest IS NOT NULL AND octet_length(guardian_stop_attestation_digest) = 32))
      AND (stop_receipt IS NOT NULL OR guardian_stop_attestation IS NOT NULL))
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_instances WHERE guardian_stop_attestation IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while Guardian stop attestations are stored';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_instances_stop_evidence_check,
  DROP COLUMN guardian_stop_attestation,
  DROP COLUMN guardian_stop_attestation_digest;

ALTER TABLE task_agent_instances
  ADD CONSTRAINT task_agent_instances_check CHECK (
    (state = 'active' AND stop_receipt IS NULL AND stop_receipt_digest IS NULL AND stopped_at IS NULL)
    OR (state = 'stopped' AND stop_receipt IS NOT NULL AND octet_length(stop_receipt) BETWEEN 1 AND 4096
      AND stop_receipt_digest IS NOT NULL AND octet_length(stop_receipt_digest) = 32 AND stopped_at IS NOT NULL)
  );
