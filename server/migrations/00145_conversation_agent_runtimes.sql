-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  -- The existing account-scoped processes may outlive a Portal process. A
  -- conversation runtime cannot safely replace them until Guardian has proved
  -- their destruction and all per-task processes have been settled.
  IF EXISTS (
    SELECT 1 FROM account_agent_runtimes
    WHERE state <> 'stopped' OR supervisor_nonce_sha256 IS NULL OR supervisor_nonce_sha256 !~ '^[0-9a-f]{64}$'
      OR guardian_stop_attestation IS NULL
      OR guardian_stop_attestation_digest IS DISTINCT FROM sha256(guardian_stop_attestation)
      OR guardian_stop_acknowledged_at IS NULL
  ) OR EXISTS (
    SELECT 1 FROM task_agent_instances i
    JOIN account_agent_runtimes r
      ON r.service_instance_id=i.service_instance_id AND r.runtime_id=i.account_runtime_id
        AND r.runtime_generation=i.account_runtime_generation
    WHERE i.state='active'
      OR (i.guardian_task_stop_ack_required AND i.guardian_stop_acknowledged_at IS NULL)
  ) THEN
    RAISE EXCEPTION 'account runtimes must be stopped and Guardian-confirmed before conversation runtime migration';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE account_agent_runtimes
  ADD COLUMN conversation_id text,
  ADD CONSTRAINT account_agent_runtime_conversation_owner_fk
    FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id) ON DELETE RESTRICT,
  DROP CONSTRAINT account_agent_runtimes_pkey,
  ADD PRIMARY KEY (service_instance_id, runtime_id);

-- Existing stopped rows remain readable, but an older process cannot create
-- or reactivate an account-scoped runtime after this migration.
ALTER TABLE account_agent_runtimes ADD CONSTRAINT account_agent_runtime_conversation_required
  CHECK (conversation_id IS NOT NULL) NOT VALID;

-- Historical stopped account runtime records retain their proofs with a NULL
-- conversation_id; only the new conversation-scoped rows can be started.
CREATE UNIQUE INDEX account_agent_runtime_conversation_unique
  ON account_agent_runtimes(service_instance_id, conversation_id) WHERE conversation_id IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM account_agent_runtimes WHERE conversation_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot restore account runtime key after conversation runtimes have been created';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX account_agent_runtime_conversation_unique;
ALTER TABLE account_agent_runtimes
  DROP CONSTRAINT account_agent_runtimes_pkey,
  ADD PRIMARY KEY (service_instance_id, owner_id),
  DROP CONSTRAINT account_agent_runtime_conversation_owner_fk,
  DROP CONSTRAINT account_agent_runtime_conversation_required,
  DROP COLUMN conversation_id;
