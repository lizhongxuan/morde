-- name: GetConversation :one
SELECT * FROM conversations WHERE id = $1 AND owner_id = $2;

-- name: ListConversations :many
SELECT * FROM conversations
WHERE owner_id = $1
  AND ($2::text[] IS NULL OR host_id = ANY($2::text[]))
  AND ($3::timestamptz IS NULL OR created_at < $3::timestamptz OR (created_at = $3::timestamptz AND id < $4))
ORDER BY created_at DESC, id DESC
LIMIT $5;

-- name: GetMessage :one
SELECT * FROM conversation_messages WHERE id = $1 AND owner_id = $2;

-- name: ListMessages :many
SELECT * FROM conversation_messages
WHERE owner_id = $1 AND conversation_id = $2
  AND ($3::bigint = 0 OR sequence < $3::bigint)
ORDER BY sequence DESC
LIMIT $4;
