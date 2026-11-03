package postgres

import (
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
