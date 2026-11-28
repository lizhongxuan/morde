-- A Task revision has one durable user-message anchor for its started marker.
-- This avoids marking both the original message and every message merged into
-- a later revision as though each were independently accepted for background work.
-- +goose Up
ALTER TABLE task_revision_starts
  ADD COLUMN acknowledgement_message_id text
  REFERENCES conversation_messages(id) ON DELETE SET NULL;

UPDATE task_revision_starts started
SET acknowledgement_message_id = COALESCE(
  (SELECT message.id
   FROM tasks t
   JOIN conversation_messages message ON message.task_id=t.id AND message.owner_id=t.owner_id
     AND message.conversation_id=t.conversation_id AND message.role='user'
   WHERE t.id=started.task_id
   ORDER BY message.sequence,message.id LIMIT 1),
  (SELECT message.id
   FROM task_revision_intents intent
   JOIN tasks t ON t.id=intent.task_id
   JOIN conversation_messages message ON message.id=intent.message_id
     AND message.owner_id=t.owner_id AND message.conversation_id=t.conversation_id AND message.role='user'
   WHERE intent.task_id=started.task_id
   ORDER BY intent.message_sequence,intent.message_id LIMIT 1));

-- +goose Down
ALTER TABLE task_revision_starts DROP COLUMN acknowledgement_message_id;
