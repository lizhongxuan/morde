-- +goose Up
CREATE TABLE host_control_transitions (
  host_id                   text NOT NULL,
  owner_id                  text NOT NULL,
  request_id                text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128 AND btrim(request_id) <> ''),
  action                    text NOT NULL CHECK (action IN ('take_over', 'release')),
  phase                     text NOT NULL CHECK (phase IN ('blocking', 'draining', 'drained', 'completed')),
  expected_control_version  bigint NOT NULL CHECK (expected_control_version >= 0),
  blocked_control_version   bigint NOT NULL CHECK (blocked_control_version = expected_control_version + 1),
  target_control_version    bigint NOT NULL CHECK (target_control_version = blocked_control_version + 1),
  task_ids                  jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(task_ids) = 'array'),
  task_ids_saved            boolean NOT NULL DEFAULT false,
  drained_receipt           jsonb CHECK (drained_receipt IS NULL OR jsonb_typeof(drained_receipt) = 'object'),
  failure_reason            text NOT NULL DEFAULT '' CHECK (octet_length(failure_reason) <= 512),
  created_at                timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at                timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (host_id, request_id),
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id) ON DELETE CASCADE,
  CHECK (phase <> 'drained' OR (
    task_ids_saved AND (
      (action = 'take_over' AND drained_receipt IS NOT NULL) OR
      (action = 'release' AND drained_receipt IS NULL)
    )
  )),
  CHECK (phase <> 'completed' OR task_ids_saved),
  CHECK (action = 'take_over' OR drained_receipt IS NULL)
);

CREATE UNIQUE INDEX host_control_transitions_one_active
  ON host_control_transitions (host_id)
  WHERE phase <> 'completed';

CREATE INDEX host_control_transitions_owner_updated
  ON host_control_transitions (owner_id, updated_at DESC);

-- +goose Down
DROP TABLE host_control_transitions;
