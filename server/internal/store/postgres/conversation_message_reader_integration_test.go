package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestReadCurrentConversationMessageChunkReconstructs200KiBSource(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	const totalBytes = 200<<10 + 1
	prefix, middle, suffix := "START-ANCHOR", "MIDDLE-ANCHOR", "END-ANCHOR"
	padding := totalBytes - len(prefix) - len(middle) - len(suffix)
	secondHalf := padding - padding/2
	content := prefix + strings.Repeat("a", padding/2) + middle + strings.Repeat("汉", secondHalf/3) +
		strings.Repeat("z", secondHalf%3) + suffix
	if len(content) != totalBytes {
		t.Fatalf("fixture bytes=%d, want %d", len(content), totalBytes)
	}
	claim := f.receive(t, "long-source-reader-001", content, app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var messageTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&messageTaskID); err != nil || !domain.ValidTaskID(messageTaskID) {
		t.Fatalf("routed message Task ID=%q err=%v", messageTaskID, err)
	}
	current, found, err := f.store.ClaimNextTask(ctx, "long-source-reader", time.Minute)
	if err != nil || !found || current.ID != messageTaskID || current.Status != task.StatusRunning {
		t.Fatalf("claim source Task: found=%t task=%+v err=%v", found, current, err)
	}
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-message-reader", 1, "session-message-reader"
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id=$2,agent_generation=$3,agent_session_id=$4,
 agent_engine_version='test-engine',agent_profile_version='test-profile' WHERE id=$1`, current.ID, current.AgentInstanceID,
		current.AgentGeneration, current.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	runes := []rune(content)
	var rebuilt strings.Builder
	var offset int64
	for offset < int64(len(runes)) {
		chunk, err := f.store.ReadCurrentConversationMessageChunk(ctx, current, claim.Message.ID, offset, app.MaxConversationMessageChunkRunes)
		if err != nil {
			t.Fatalf("read source chunk at %d: %v", offset, err)
		}
		if chunk.MessageID != claim.Message.ID || chunk.TaskID != current.ID || chunk.Sequence != current.ConversationContext.ThroughSequence+1 ||
			chunk.RuneOffset != offset || chunk.NextRuneOffset <= offset || chunk.TotalRunes != int64(len(runes)) ||
			len(chunk.Content) > app.MaxConversationMessageChunkBytes ||
			chunk.Content != string(runes[int(offset):int(chunk.NextRuneOffset)]) || !bytes.Equal(digest[:], chunk.SHA256) {
			t.Fatalf("invalid bounded source chunk at %d: %+v", offset, chunk)
		}
		rebuilt.WriteString(chunk.Content)
		offset = chunk.NextRuneOffset
	}
	if rebuilt.String() != content || !strings.Contains(rebuilt.String(), prefix) ||
		!strings.Contains(rebuilt.String(), middle) || !strings.HasSuffix(rebuilt.String(), suffix) {
		t.Fatalf("database range reads did not reconstruct the complete source: bytes=%d want=%d", rebuilt.Len(), len(content))
	}
	if _, err := f.store.ReadCurrentConversationMessageChunk(ctx, current, claim.Message.ID, offset+1, app.MaxConversationMessageChunkRunes); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("out-of-range offset was not rejected: %v", err)
	}
}
