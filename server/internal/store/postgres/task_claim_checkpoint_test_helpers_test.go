package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
)

const taskClaimTestCheckpointSequence int64 = 7

func seedTaskClaimRootDSHCheckpoint(t *testing.T, store *Store, taskID, ownerID, sessionID string) []byte {
	t.Helper()
	digest := sha256.Sum256([]byte("task claim checkpoint:" + taskID))
	_, err := store.pool.Exec(context.Background(), `UPDATE tasks SET
agent_instance_id=$2,agent_generation=1,agent_session_id=$3,
agent_engine_version='claim-test-engine',agent_profile_version='claim-test-profile',
checkpoint_through_seq=$4,checkpoint_digest=$5
WHERE id=$1 AND owner_id=$6`, taskID, "claim-agent-"+taskID, sessionID,
		taskClaimTestCheckpointSequence, digest[:], ownerID)
	if err != nil {
		t.Fatalf("seed Task DSH checkpoint head: %v", err)
	}
	_, err = store.pool.Exec(context.Background(), `INSERT INTO task_agent_sessions
(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,$3,$4,$5)`, taskID, ownerID, sessionID, taskClaimTestCheckpointSequence, digest[:])
	if err != nil {
		t.Fatalf("seed root Agent Session checkpoint: %v", err)
	}
	return digest[:]
}
