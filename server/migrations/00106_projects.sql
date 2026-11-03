-- +goose Up
CREATE TABLE projects (
  id                       text PRIMARY KEY CHECK (id ~ '^j[a-z0-9]{7}$'),
  owner_id                 text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  owner_role               text NOT NULL DEFAULT 'owner' CHECK (owner_role = 'owner'),
  name                     text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
  description              text NOT NULL DEFAULT '' CHECK (char_length(description) <= 2000),
  version                  bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
  create_idempotency_key   text NOT NULL CHECK (create_idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  create_request_digest    bytea NOT NULL CHECK (octet_length(create_request_digest) = 32),
  UNIQUE (owner_id, create_idempotency_key),
  UNIQUE (id, owner_id, owner_role)
);

CREATE TABLE project_members (
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  role       text NOT NULL CHECK (role IN ('owner', 'member', 'viewer')),
  version    bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, user_id),
  UNIQUE (project_id, user_id, role)
);
CREATE UNIQUE INDEX project_members_one_owner
  ON project_members(project_id) WHERE role = 'owner';
CREATE INDEX project_members_user_project
  ON project_members(user_id, project_id);

-- The deferred cycle makes each project's owner an accepted owner member, including during atomic transfer.
ALTER TABLE projects ADD CONSTRAINT projects_owner_membership_fkey
  FOREIGN KEY (id, owner_id, owner_role)
  REFERENCES project_members(project_id, user_id, role)
  DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE project_invitations (
  id                       text PRIMARY KEY CHECK (id ~ '^r[a-z0-9]{7}$'),
  project_id               text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  inviter_id               text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  invitee_id               text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  role                     text NOT NULL CHECK (role IN ('member', 'viewer')),
  state                    text NOT NULL CHECK (state IN ('pending', 'accepted', 'revoked')),
  version                  bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  create_idempotency_key   text NOT NULL CHECK (create_idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  create_request_digest    bytea NOT NULL CHECK (octet_length(create_request_digest) = 32),
  created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
  accepted_at              timestamptz,
  revoked_at               timestamptz,
  UNIQUE (project_id, inviter_id, create_idempotency_key),
  CHECK (
    (state = 'pending' AND accepted_at IS NULL AND revoked_at IS NULL) OR
    (state = 'accepted' AND accepted_at IS NOT NULL AND revoked_at IS NULL) OR
    (state = 'revoked' AND accepted_at IS NULL AND revoked_at IS NOT NULL)
  )
);
CREATE UNIQUE INDEX project_invitations_one_pending_invitee
  ON project_invitations(project_id, invitee_id) WHERE state = 'pending';
CREATE INDEX project_invitations_invitee_state
  ON project_invitations(invitee_id, state, created_at DESC, id DESC);

CREATE TABLE project_resources (
  project_id         text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  kind               text NOT NULL CHECK (kind IN ('host', 'workspace')),
  resource_id        text NOT NULL,
  resource_owner_id  text NOT NULL,
  host_id            text,
  workspace_id       text,
  version            bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  active             boolean NOT NULL DEFAULT true,
  updated_by         text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  idempotency_key    text CHECK (idempotency_key IS NULL OR idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  request_digest     bytea CHECK (request_digest IS NULL OR octet_length(request_digest) = 32),
  created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, kind, resource_id),
  CHECK (
    (kind = 'host' AND resource_id ~ '^h[a-z0-9]{7}$' AND host_id IS NOT NULL AND host_id = resource_id AND workspace_id IS NULL) OR
    (kind = 'workspace' AND resource_id ~ '^w[a-z0-9]{7}$' AND workspace_id IS NOT NULL AND workspace_id = resource_id AND host_id IS NULL)
  ),
  FOREIGN KEY (host_id, resource_owner_id) REFERENCES hosts(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (workspace_id, resource_owner_id) REFERENCES workspaces(id, owner_id) ON DELETE RESTRICT,
  CHECK ((idempotency_key IS NULL) = (request_digest IS NULL))
);
CREATE INDEX project_resources_active
  ON project_resources(project_id, kind, resource_id) WHERE active;
CREATE UNIQUE INDEX project_resources_idempotency
  ON project_resources(project_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE project_content_revisions (
  project_id       text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  kind             text NOT NULL CHECK (kind IN ('memory', 'goal', 'document')),
  content_id       text NOT NULL CHECK (char_length(content_id) BETWEEN 1 AND 128 AND octet_length(content_id) <= 512),
  revision         bigint NOT NULL CHECK (revision >= 1),
  body             jsonb,
  deleted          boolean NOT NULL DEFAULT false,
  current          boolean NOT NULL DEFAULT true,
  updated_by       text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  idempotency_key  text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  request_digest   bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, kind, content_id, revision),
  UNIQUE (project_id, updated_by, idempotency_key),
  CHECK ((deleted AND body IS NULL) OR (NOT deleted AND body IS NOT NULL AND jsonb_typeof(body) = 'object'))
);
CREATE UNIQUE INDEX project_content_one_current_revision
  ON project_content_revisions(project_id, kind, content_id) WHERE current;

-- One immutable row per budget version preserves the exact currency and integer-micro limit used by each CAS update.
CREATE TABLE project_budgets (
  project_id       text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  version          bigint NOT NULL CHECK (version >= 1),
  currency         text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  limit_micros     bigint NOT NULL CHECK (limit_micros > 0),
  updated_by       text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  idempotency_key  text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  request_digest   bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, version),
  UNIQUE (project_id, updated_by, idempotency_key)
);

CREATE TABLE project_work_items (
  project_id           text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  id                   text NOT NULL CHECK (id ~ '^j[a-z0-9]{7}$'),
  title                text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
  description          text NOT NULL DEFAULT '' CHECK (char_length(description) <= 4096),
  acceptance_criteria  jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(acceptance_criteria) = 'array' AND jsonb_array_length(acceptance_criteria) <= 100),
  assignee_id          text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  state                text NOT NULL CHECK (state IN ('open', 'blocked', 'delivered')),
  blocked_reason       text NOT NULL DEFAULT '',
  delivery_task_id     text,
  delivery_workspace_id text,
  delivery_revision    bigint,
  version              bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  created_by           text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  updated_by           text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, id),
  CHECK (
    (state = 'open' AND blocked_reason = '' AND delivery_task_id IS NULL AND delivery_workspace_id IS NULL AND delivery_revision IS NULL) OR
    (state = 'blocked' AND char_length(blocked_reason) BETWEEN 1 AND 2000 AND delivery_task_id IS NULL AND delivery_workspace_id IS NULL AND delivery_revision IS NULL) OR
    (state = 'delivered' AND blocked_reason = '' AND delivery_task_id IS NOT NULL AND delivery_task_id ~ '^q[a-z0-9]{7}$' AND
      delivery_workspace_id IS NOT NULL AND delivery_workspace_id ~ '^w[a-z0-9]{7}$' AND delivery_revision IS NOT NULL AND delivery_revision >= 1)
  )
);
-- Assignee membership is checked transactionally; retaining the user ID lets work-item history survive membership revocation.
CREATE INDEX project_work_items_project_page
  ON project_work_items(project_id, created_at DESC, id DESC);

CREATE TABLE project_work_item_tasks (
  project_id  text NOT NULL,
  work_item_id text NOT NULL,
  task_id      text NOT NULL CHECK (task_id ~ '^q[a-z0-9]{7}$'),
  linked_by    text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, work_item_id, task_id),
  FOREIGN KEY (project_id, work_item_id) REFERENCES project_work_items(project_id, id) ON DELETE CASCADE
);
-- Task and Workspace ownership/revision checks remain in the transaction authorizers; no cross-owner FK grants access.

-- R08/R10/W12 Task, model-runtime, and account-budget paths are not integrated here; keep reservations disabled until those owners validate the full boundary.
CREATE TABLE project_budget_ledger (
  project_id                    text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  source                        text NOT NULL CHECK (source IN ('client', 'chat', 'task')),
  call_id                       text NOT NULL CHECK (call_id ~ '^[A-Za-z0-9._~-]{8,128}$'),
  actor_id                      text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  -- Task attribution is retained without a Task FK until the staged shared-Task migration defines its owner-safe relationship.
  task_id                       text,
  model_id                      text NOT NULL CHECK (model_id ~ '^p[a-z0-9]{7}$'),
  rate_provider                 text NOT NULL CHECK (char_length(rate_provider) BETWEEN 1 AND 128),
  rate_model                    text NOT NULL CHECK (char_length(rate_model) BETWEEN 1 AND 256),
  rate_version                  bigint NOT NULL CHECK (rate_version >= 1),
  currency                      text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  input_micros_per_million      bigint NOT NULL CHECK (input_micros_per_million >= 0),
  output_micros_per_million     bigint NOT NULL CHECK (output_micros_per_million >= 0),
  reserved_input_tokens         bigint NOT NULL CHECK (reserved_input_tokens >= 0),
  reserved_output_tokens        bigint NOT NULL CHECK (reserved_output_tokens >= 0),
  reserved_micros               bigint NOT NULL CHECK (reserved_micros > 0),
  status                        text NOT NULL CHECK (status IN ('reserved', 'settled', 'unknown')),
  actual_input_tokens           bigint CHECK (actual_input_tokens IS NULL OR actual_input_tokens >= 0),
  actual_output_tokens          bigint CHECK (actual_output_tokens IS NULL OR actual_output_tokens >= 0),
  actual_micros                 bigint CHECK (actual_micros IS NULL OR actual_micros >= 0),
  request_digest                bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  created_at                    timestamptz NOT NULL DEFAULT clock_timestamp(),
  settled_at                    timestamptz,
  PRIMARY KEY (project_id, source, call_id),
  CHECK ((source = 'task' AND task_id IS NOT NULL AND task_id ~ '^q[a-z0-9]{7}$') OR (source <> 'task' AND task_id IS NULL)),
  CHECK (reserved_input_tokens > 0 OR reserved_output_tokens > 0),
  CHECK (input_micros_per_million > 0 OR output_micros_per_million > 0),
  CHECK ((actual_input_tokens IS NULL) = (actual_output_tokens IS NULL)),
  CHECK ((actual_input_tokens IS NULL) = (actual_micros IS NULL)),
  CHECK (
    (status IN ('reserved', 'unknown') AND actual_input_tokens IS NULL) OR
    (status = 'settled' AND actual_input_tokens IS NOT NULL AND
      (actual_input_tokens > 0 OR actual_output_tokens > 0))
  )
);
CREATE INDEX project_budget_ledger_page
  ON project_budget_ledger(project_id, created_at DESC, source, call_id DESC);

CREATE TABLE project_idempotency_keys (
  actor_id         text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  operation        text NOT NULL CHECK (operation IN (
    'create_project', 'invite_member', 'accept_invitation', 'update_member_role', 'remove_member',
    'transfer_owner', 'add_resource', 'remove_resource', 'revoke_invitation', 'mutate_content', 'set_budget',
    'create_work_item', 'update_work_item'
  )),
  idempotency_key  text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  project_id       text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  result_kind      text NOT NULL DEFAULT '' CHECK (result_kind IN ('', 'project', 'member', 'invitation', 'host', 'workspace', 'memory', 'goal', 'document', 'budget', 'work_item')),
  result_id        text NOT NULL DEFAULT '',
  result_version   bigint NOT NULL DEFAULT 0 CHECK (result_version >= 0),
  request_digest   bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (actor_id, operation, idempotency_key)
);
CREATE INDEX project_idempotency_project
  ON project_idempotency_keys(project_id, created_at);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM projects) THEN
    RAISE EXCEPTION 'cannot roll back project migration while project data exists';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE project_idempotency_keys;
DROP TABLE project_work_item_tasks;
DROP TABLE project_work_items;
DROP TABLE project_budget_ledger;
DROP TABLE project_budgets;
DROP TABLE project_content_revisions;
DROP TABLE project_resources;
DROP TABLE project_invitations;
ALTER TABLE projects DROP CONSTRAINT projects_owner_membership_fkey;
DROP TABLE project_members;
DROP TABLE projects;
