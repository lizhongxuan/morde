-- +goose Up
ALTER TABLE tasks ADD CONSTRAINT tasks_notification_owner_unique UNIQUE (id, owner_id);

CREATE TABLE notification_event_receipts (
  task_id        text NOT NULL,
  event_sequence bigint NOT NULL CHECK (event_sequence > 0),
  created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, event_sequence),
  FOREIGN KEY (task_id, event_sequence) REFERENCES task_events(task_id, sequence) ON DELETE CASCADE
);

CREATE TABLE notifications (
  id              text PRIMARY KEY CHECK (id ~ '^n[a-z0-9]{7}$'),
  owner_id        text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  task_id         text NOT NULL,
  event_sequence  bigint NOT NULL CHECK (event_sequence > 0),
  kind            text NOT NULL CHECK (kind IN ('completed', 'failed', 'approval_required', 'clarification_required', 'attention_required')),
  summary         text NOT NULL CHECK (summary IN ('任务已完成', '任务执行失败', '任务等待审批', '任务等待补充信息', '任务需要你处理')),
  request_id      text NOT NULL DEFAULT '' CHECK (request_id = '' OR request_id ~ '^r[a-z0-9]{7}$'),
  request_version bigint NOT NULL DEFAULT 0 CHECK (request_version >= 0),
  actionable      boolean NOT NULL DEFAULT false,
  read_at         timestamptz,
  handled_at      timestamptz,
  created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (task_id, event_sequence),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, event_sequence) REFERENCES task_events(task_id, sequence) ON DELETE CASCADE,
  CHECK (
    (kind IN ('approval_required', 'clarification_required') AND request_id <> '' AND request_version > 0)
    OR (kind NOT IN ('approval_required', 'clarification_required') AND request_id = '' AND request_version = 0)
  ),
  CHECK (NOT actionable OR (kind IN ('approval_required', 'clarification_required') AND handled_at IS NULL)),
  CHECK (
    (kind = 'completed' AND summary = '任务已完成') OR
    (kind = 'failed' AND summary = '任务执行失败') OR
    (kind = 'approval_required' AND summary = '任务等待审批') OR
    (kind = 'clarification_required' AND summary = '任务等待补充信息') OR
    (kind = 'attention_required' AND summary = '任务需要你处理')
  )
);

CREATE INDEX notifications_owner_created ON notifications(owner_id, created_at DESC, id DESC);
CREATE INDEX notifications_task_actionable ON notifications(task_id) WHERE actionable;

-- +goose Down
DROP TABLE notifications;
DROP TABLE notification_event_receipts;
ALTER TABLE tasks DROP CONSTRAINT tasks_notification_owner_unique;
