package worker

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/app"
)

func TestCloudDSHJobOutputReadsCheckpointWhileChildHistoryIsRestoring(t *testing.T) {
	const childSessionID = "child_test"
	payload := cloudDSHCheckpointPayload(childSessionID, 0, checkpointAssistantText(0, "Recovered output."))
	digest := sha256.Sum256(payload)
	checkpoints := &cloudDSHTestCheckpoints{commands: []app.AgentCheckpointCommand{{
		Fence: app.AgentCheckpointFence{AgentSessionID: childSessionID}, From: 0, Through: 0,
		Digest: digest[:], Payload: payload,
	}}}
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Checkpoints: checkpoints}}
	state := &cloudDSHSessionState{through: 0, restoreThrough: 0, readThrough: -1,
		history: newCloudDSHCheckpointHistory(childSessionID)}
	state.history.beginRestore()
	child := app.AgentChildSession{SessionID: childSessionID, CheckpointThrough: 0, CheckpointDigest: digest[:]}

	output, cursor, err := executor.cloudDSHJobOutput(context.Background(), cloudDSHFence("root_session"), child, state, -1)
	if err != nil || output != "Recovered output." || cursor != 0 || len(checkpoints.reads) != 1 {
		t.Fatalf("job output = %q, cursor = %d, reads = %d, error = %v", output, cursor, len(checkpoints.reads), err)
	}
}
