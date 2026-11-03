-- +goose Up
CREATE TABLE notification_source_events (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_kind    text NOT NULL CHECK (source_kind IN ('goal', 'project_work_item')),
  source_id      text NOT NULL CHECK (char_length(source_id) BETWEEN 1 AND 128 AND octet_length(source_id) <= 512),
  source_version bigint NOT NULL CHECK (source_version > 0),
  event_kind     text NOT NULL CHECK (event_kind IN (
    'goal_step_blocked', 'goal_completed', 'project_work_item_blocked', 'project_work_item_delivered'
  )),
  recipient_id   text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id     text REFERENCES projects(id) ON DELETE CASCADE,
  created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
  processed_at   timestamptz,
  CHECK (
    (source_kind = 'goal' AND project_id IS NULL AND event_kind IN ('goal_step_blocked', 'goal_completed')) OR
    (source_kind = 'project_work_item' AND project_id IS NOT NULL AND
      source_id ~ '^j[a-z0-9]{7}$' AND event_kind IN ('project_work_item_blocked', 'project_work_item_delivered'))
  ),
  FOREIGN KEY (project_id, source_id) REFERENCES project_work_items(project_id, id) ON DELETE CASCADE
);

-- Goal identity is owner-scoped by recipient. Project work-item identity is project-scoped.
CREATE UNIQUE INDEX notification_source_events_goal_recipient_unique
  ON notification_source_events(source_id, source_version, event_kind, recipient_id)
  WHERE source_kind = 'goal';
CREATE UNIQUE INDEX notification_source_events_project_recipient_unique
  ON notification_source_events(project_id, source_id, source_version, event_kind, recipient_id)
  WHERE source_kind = 'project_work_item';
CREATE INDEX notification_source_events_pending
  ON notification_source_events(id) WHERE processed_at IS NULL;

ALTER TABLE notifications ALTER COLUMN task_id DROP NOT NULL;
ALTER TABLE notifications ALTER COLUMN event_sequence DROP NOT NULL;

ALTER TABLE notifications DROP CONSTRAINT notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
  CHECK (kind IN (
    'completed', 'failed', 'approval_required', 'clarification_required', 'attention_required',
    'goal_step_blocked', 'goal_completed', 'project_work_item_blocked', 'project_work_item_delivered'
  ));
ALTER TABLE notifications DROP CONSTRAINT notifications_summary_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_summary_check
  CHECK (summary IN (
    '任务已完成', '任务执行失败', '任务等待审批', '任务等待补充信息', '任务需要你处理',
    '目标步骤遇到阻塞', '目标已完成', '项目工作项遇到阻塞', '项目工作项已交付'
  ));
ALTER TABLE notifications DROP CONSTRAINT notifications_check2;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_summary_check CHECK (
  (kind = 'completed' AND summary = '任务已完成') OR
  (kind = 'failed' AND summary = '任务执行失败') OR
  (kind = 'approval_required' AND summary = '任务等待审批') OR
  (kind = 'clarification_required' AND summary = '任务等待补充信息') OR
  (kind = 'attention_required' AND summary = '任务需要你处理') OR
  (kind = 'goal_step_blocked' AND summary = '目标步骤遇到阻塞') OR
  (kind = 'goal_completed' AND summary = '目标已完成') OR
  (kind = 'project_work_item_blocked' AND summary = '项目工作项遇到阻塞') OR
  (kind = 'project_work_item_delivered' AND summary = '项目工作项已交付')
);

ALTER TABLE notifications
  ADD COLUMN source_event_id bigint,
  ADD COLUMN source_kind text,
  ADD COLUMN source_id text,
  ADD COLUMN source_version bigint,
  ADD COLUMN project_id text;

ALTER TABLE notifications
  ADD CONSTRAINT notifications_source_event_id_fkey
    FOREIGN KEY (source_event_id) REFERENCES notification_source_events(id) ON DELETE CASCADE,
  ADD CONSTRAINT notifications_source_event_id_unique UNIQUE (source_event_id),
  ADD CONSTRAINT notifications_source_kind_check
    CHECK (source_kind IS NULL OR source_kind IN ('goal', 'project_work_item')),
  ADD CONSTRAINT notifications_source_identity_check CHECK (
    (task_id IS NOT NULL AND event_sequence IS NOT NULL AND source_event_id IS NULL AND
      source_kind IS NULL AND source_id IS NULL AND source_version IS NULL AND project_id IS NULL) OR
    (task_id IS NULL AND event_sequence IS NULL AND source_event_id IS NOT NULL AND
      source_kind IS NOT NULL AND source_id IS NOT NULL AND source_version IS NOT NULL AND source_version > 0 AND
      ((source_kind = 'goal' AND project_id IS NULL) OR
       (source_kind = 'project_work_item' AND project_id IS NOT NULL)))
  );

-- +goose Down
-- Source event and notification history is durable. Refuse rollback while it contains data.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM notification_source_events)
     OR EXISTS (SELECT 1 FROM notifications WHERE source_event_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back notification source events while source history exists';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_source_identity_check;
ALTER TABLE notifications DROP CONSTRAINT notifications_source_kind_check;
ALTER TABLE notifications DROP CONSTRAINT notifications_source_event_id_unique;
ALTER TABLE notifications DROP CONSTRAINT notifications_source_event_id_fkey;
ALTER TABLE notifications
  DROP COLUMN project_id,
  DROP COLUMN source_version,
  DROP COLUMN source_id,
  DROP COLUMN source_kind,
  DROP COLUMN source_event_id;

ALTER TABLE notifications ALTER COLUMN task_id SET NOT NULL;
ALTER TABLE notifications ALTER COLUMN event_sequence SET NOT NULL;

ALTER TABLE notifications DROP CONSTRAINT notifications_kind_summary_check;
ALTER TABLE notifications DROP CONSTRAINT notifications_summary_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_summary_check
  CHECK (summary IN ('任务已完成', '任务执行失败', '任务等待审批', '任务等待补充信息', '任务需要你处理'));
ALTER TABLE notifications DROP CONSTRAINT notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
  CHECK (kind IN ('completed', 'failed', 'approval_required', 'clarification_required', 'attention_required'));
ALTER TABLE notifications ADD CONSTRAINT notifications_check2 CHECK (
  (kind = 'completed' AND summary = '任务已完成') OR
  (kind = 'failed' AND summary = '任务执行失败') OR
  (kind = 'approval_required' AND summary = '任务等待审批') OR
  (kind = 'clarification_required' AND summary = '任务等待补充信息') OR
  (kind = 'attention_required' AND summary = '任务需要你处理')
);

DROP TABLE notification_source_events;
