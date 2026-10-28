-- +goose Up
ALTER TABLE account_agent_runtimes
  ADD COLUMN supervisor_nonce_sha256 text,
  ADD CONSTRAINT account_agent_runtime_nonce_hash_check CHECK (
    supervisor_nonce_sha256 IS NULL OR supervisor_nonce_sha256 ~ '^[0-9a-f]{64}$'
  );

ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_instances_stop_evidence_check;

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
        OR (guardian_stop_attestation IS NOT NULL AND octet_length(guardian_stop_attestation) BETWEEN 1 AND 16384
          AND guardian_stop_attestation_digest IS NOT NULL AND octet_length(guardian_stop_attestation_digest) = 32))
      AND (stop_receipt IS NOT NULL OR guardian_stop_attestation IS NOT NULL))
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM account_agent_runtimes WHERE guardian_stop_attestation IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove account Runtime nonce digests while Guardian stop proofs are stored';
  END IF;
  IF EXISTS (SELECT 1 FROM task_agent_instances WHERE octet_length(guardian_stop_attestation) > 8192) THEN
    RAISE EXCEPTION 'cannot narrow Guardian stop evidence while larger Runtime proofs are stored';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_instances_stop_evidence_check;

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

ALTER TABLE account_agent_runtimes
  DROP CONSTRAINT account_agent_runtime_nonce_hash_check,
  DROP COLUMN supervisor_nonce_sha256;
