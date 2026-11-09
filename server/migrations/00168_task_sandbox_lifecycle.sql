-- +goose Up
CREATE TABLE task_sandbox_lifecycles (
  id                    text PRIMARY KEY CHECK (id ~ '^o[a-z0-9]{7}$'),
  task_id               text NOT NULL,
  owner_id              text NOT NULL,
  initiator_id          text NOT NULL,
  lease_owner           text NOT NULL CHECK (length(lease_owner) BETWEEN 1 AND 255),
  lease_version         bigint NOT NULL CHECK (lease_version > 0),
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  agent_instance_id     text NOT NULL CHECK (length(agent_instance_id) BETWEEN 1 AND 128),
  agent_generation      bigint NOT NULL CHECK (agent_generation > 0),
  sandbox_id            text NOT NULL DEFAULT '' CHECK (length(sandbox_id) <= 512),
  sandbox_generation    bigint NOT NULL CHECK (sandbox_generation > 0),
  status                text NOT NULL CHECK (status IN ('prepared','creating','running','stopping','unknown','terminated')),
  expires_at            timestamptz,
  unknown_code          text NOT NULL DEFAULT '' CHECK (length(unknown_code) <= 64),
  termination_evidence  bytea NOT NULL DEFAULT ''::bytea CHECK (octet_length(termination_evidence) <= 4096),
  terminated_at         timestamptz,
  created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (task_id, sandbox_generation),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (initiator_id) REFERENCES users(id) ON DELETE RESTRICT,
  CHECK (owner_id = initiator_id),
  CHECK (
    (status IN ('prepared','creating') AND sandbox_id = '' AND expires_at IS NULL AND terminated_at IS NULL AND termination_evidence = ''::bytea)
    OR (status IN ('running','stopping') AND sandbox_id <> '' AND expires_at IS NOT NULL AND terminated_at IS NULL AND termination_evidence = ''::bytea)
    OR (status = 'unknown' AND unknown_code <> '' AND terminated_at IS NULL AND
        ((sandbox_id = '' AND expires_at IS NULL) OR (sandbox_id <> '' AND expires_at IS NOT NULL)))
    OR (status = 'terminated' AND terminated_at IS NOT NULL AND
        (sandbox_id = '' OR octet_length(termination_evidence) > 0))
  ),
  CHECK ((status = 'unknown') = (unknown_code <> ''))
);

CREATE UNIQUE INDEX task_sandbox_lifecycles_one_active_idx
  ON task_sandbox_lifecycles (task_id)
  WHERE status IN ('prepared','creating','running','stopping','unknown');

CREATE INDEX task_sandbox_lifecycles_owner_task_idx
  ON task_sandbox_lifecycles (owner_id, task_id, sandbox_generation DESC);

-- +goose Down
-- No provider side effects are issued by this migration. Refuse to discard
-- unresolved or active lifecycle fences during rollback.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_sandbox_lifecycles WHERE status <> 'terminated') THEN
    RAISE EXCEPTION 'cannot roll back while Task Sandbox lifecycle records remain active or unresolved';
  END IF;
END;
$$;
-- +goose StatementEnd
DROP INDEX task_sandbox_lifecycles_owner_task_idx;
DROP INDEX task_sandbox_lifecycles_one_active_idx;
DROP TABLE task_sandbox_lifecycles;
