package postgres

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"unicode/utf8"

	"morde/server/internal/domain/task"
)

func TestBoundTaskConversationContextKeepsNewestWithinMessageAndByteCaps(t *testing.T) {
	messages := make([]task.ConversationContextMessage, 200)
	for index := range messages {
		messages[index] = task.ConversationContextMessage{
			ID: "m1234567", Sequence: int64(index + 1), Role: "user", Content: "x",
		}
	}
	byCount := boundTaskConversationContextMessages(messages)
	if len(byCount) != task.MaxConversationContextMessages || byCount[0].Sequence != 73 || byCount[len(byCount)-1].Sequence != 200 {
		t.Fatalf("message cap kept %d messages from %d through %d; want the newest %d", len(byCount),
			byCount[0].Sequence, byCount[len(byCount)-1].Sequence, task.MaxConversationContextMessages)
	}

	for index := range messages {
		messages[index].Content = strings.Repeat("x", 300)
	}
	byBytes := boundTaskConversationContextMessages(messages)
	var total int
	for _, message := range byBytes {
		total += len(message.Content) + len(message.Role) + 8
	}
	if len(byBytes) == 0 {
		t.Fatal("byte cap removed all history messages")
	}
	if total > taskConversationContextMaxBytes || byBytes[len(byBytes)-1].Sequence != 200 || byBytes[0].Sequence <= 1 {
		t.Fatalf("byte cap kept %d messages, sequences %d..%d, total=%d; want a bounded recent suffix",
			len(byBytes), byBytes[0].Sequence, byBytes[len(byBytes)-1].Sequence, total)
	}

	oversized := []task.ConversationContextMessage{{
		ID: "m7654321", Sequence: 1, Role: "assistant", Content: strings.Repeat("界", 12_000),
	}}
	truncated := boundTaskConversationContextMessages(oversized)
	if len(truncated) != 1 {
		t.Fatalf("oversized message produced %d context records; want one", len(truncated))
	}
	if !truncated[0].Truncated || !utf8.ValidString(truncated[0].Content) ||
		len(truncated[0].Content)+len(truncated[0].Role)+8 > taskConversationContextMaxBytes {
		t.Fatalf("oversized multibyte message was not safely truncated: len=%d truncated=%t valid_utf8=%t",
			len(truncated[0].Content), truncated[0].Truncated, utf8.ValidString(truncated[0].Content))
	}
}

func TestTaskConversationSummaryBindsDigestAndRemovesCoveredHistory(t *testing.T) {
	messageHash := sha256.Sum256([]byte("summary source"))
	messages := []task.ConversationContextMessage{
		{ID: "m1234567", Sequence: 1, Role: "user", Content: "covered message"},
		{ID: "m2345678", Sequence: 2, Role: "assistant", Content: "covered task result", TaskID: "q1234567", TaskRevision: 3, EventSequence: 9},
		{ID: "m3456789", Sequence: 3, Role: "assistant", Content: "covered batch result", TaskResults: []task.ConversationContextTaskResult{
			{TaskID: "q1234567", TaskRevision: 3, EventSequence: 9}}},
		{ID: "m4567890", Sequence: 4, Role: "user", Content: "uncovered recent message"},
	}
	sources := []task.ConversationContextSummarySource{
		{Kind: "message", ID: "m1234567", Sequence: 1, SHA256: messageHash[:]},
		{Kind: "task_result", ID: "q1234567", Version: 3, Sequence: 9, SHA256: messageHash[:]},
	}
	filtered := taskConversationContextWithoutSummarySources(messages, sources)
	if len(filtered) != 1 || filtered[0].ID != "m4567890" {
		t.Fatalf("uncovered history = %+v; expected only the recent uncovered message", filtered)
	}
	base := buildTaskConversationContext("u1234567", "", "u1234567", "c1234567", 4, filtered, nil)
	summary := &task.ConversationContextSummary{ServiceInstanceID: "00000000-0000-4000-8000-000000000001",
		OwnerID: "u1234567", ActorID: "u1234567", ConversationID: "c1234567", ScopeKind: "personal",
		AuthorizationVersion: 1, Version: 1, OperationID: "o1234567", SummarySHA256: messageHash[:], SourcesSHA256: messageHash[:]}
	withSummary := buildTaskConversationContext("u1234567", "", "u1234567", "c1234567", 4, filtered, summary)
	if bytes.Equal(base.Digest, withSummary.Digest) {
		t.Fatal("Task context digest does not bind the fixed summary reference")
	}
	changed := *summary
	changed.Version++
	withChangedSummary := buildTaskConversationContext("u1234567", "", "u1234567", "c1234567", 4, filtered, &changed)
	if bytes.Equal(withSummary.Digest, withChangedSummary.Digest) {
		t.Fatal("Task context digest did not change with its summary version")
	}
}
