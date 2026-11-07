-- +goose Up
-- Older main-conversation handlers could persist project conversations as personal selections.
-- Remove only those pointers and replay keys; project conversations and their history stay intact.
DELETE FROM idempotency_keys k
USING conversations c
WHERE k.owner_id = c.owner_id
  AND k.resource_id = c.id
  AND k.operation IN ('main_conversation.ensure', 'main_conversation.set')
  AND c.project_id IS NOT NULL;

DELETE FROM account_main_conversations p
USING conversations c
WHERE p.owner_id = c.owner_id
  AND p.conversation_id = c.id
  AND c.project_id IS NOT NULL;

-- +goose Down
-- Keep the repaired invariant after rollback; the deleted replay keys cannot be reconstructed.
SELECT 1;
