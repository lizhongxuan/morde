package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"morde/server/internal/app"
)

// These tests use cloudDSHFixture, which launches the local morde-cloud-runtime
// process and exercises the Worker-to-Runtime start frame with a local model stub.
func TestCloudDSHInitialMessageAt128KiBReachesModelIntact(t *testing.T) {
	const messageID = "m128k001"
	limit := app.MaxConversationAgentInputBytes
	content := cloudDSHInitialMessageAtByteLimit(limit)
	if len(content) != limit || !utf8.ValidString(content) {
		t.Fatalf("initial message bytes=%d, want valid UTF-8 at %d bytes", len(content), limit)
	}

	executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{cloudDSHWriteTool()}}
	executor.options.Tools = tools
	value.Objective = content
	checkpoints.pending = []app.AgentConversationMessage{{ID: messageID, Content: content, Initial: true}}

	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("128 KiB initial Task result = %v, want a completed model turn pending Task completion", err)
	}
	if len(models.calls) != 1 {
		t.Fatalf("128 KiB initial message produced %d model calls, want 1", len(models.calls))
	}
	if len(tools.calls) != 0 || len(tools.admissionCalls) != 0 {
		t.Fatalf("initial message invoked tools: execute=%d admission=%d", len(tools.calls), len(tools.admissionCalls))
	}

	var request struct {
		Messages []struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(models.calls[0].RawRequest, &request); err != nil {
		t.Fatal(err)
	}
	messageCount, contentCount := 0, 0
	for _, message := range request.Messages {
		if message.ID != messageID {
			continue
		}
		messageCount++
		if message.Role != "user" {
			t.Errorf("initial message role = %q, want user", message.Role)
		}
		for _, block := range message.Content {
			if block.Text == content {
				contentCount++
			}
		}
	}
	if messageCount != 1 || contentCount != 1 {
		t.Fatalf("model request lost or duplicated initial message: matching messages=%d full content blocks=%d", messageCount, contentCount)
	}
	for _, anchor := range []string{"HEAD-锚点", "MIDDLE-锚点🙂", "TAIL-锚点"} {
		if !strings.Contains(content, anchor) || !strings.Contains(string(models.calls[0].RawRequest), anchor) {
			t.Errorf("initial message anchor %q did not reach the model request", anchor)
		}
	}
}

func TestCloudDSHInitialMessageOneByteOver128KiBIsRejectedBeforeModelOrToolCall(t *testing.T) {
	const messageID = "m128k002"
	limit := app.MaxConversationAgentInputBytes
	content := strings.Repeat("x", limit-2) + "中"
	if len(content) != limit+1 || !utf8.ValidString(content) {
		t.Fatalf("Unicode boundary fixture bytes=%d, want valid UTF-8 at %d bytes", len(content), limit+1)
	}

	executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{cloudDSHWriteTool()}}
	executor.options.Tools = tools
	value.Objective = content
	checkpoints.pending = []app.AgentConversationMessage{{ID: messageID, Content: content, Initial: true}}

	err := executor.ExecuteTask(context.Background(), value)
	if err == nil || !strings.Contains(err.Error(), "initial conversation message does not match the Task") {
		t.Fatalf("128 KiB+1 Unicode initial Task error = %v, want Worker rejection for invalid initial message size", err)
	}
	if len(models.calls) != 0 {
		t.Fatalf("oversized initial message reached %d model calls, want none", len(models.calls))
	}
	if len(tools.calls) != 0 || len(tools.admissionCalls) != 0 {
		t.Fatalf("oversized initial message invoked tools: execute=%d admission=%d", len(tools.calls), len(tools.admissionCalls))
	}
}

func cloudDSHInitialMessageAtByteLimit(limit int) string {
	prefix := "HEAD-锚点\n"
	middle := "\nMIDDLE-锚点🙂\n"
	suffix := "\nTAIL-锚点"
	fillerBytes := limit - len(prefix) - len(middle) - len(suffix)
	left := fillerBytes / 2
	return prefix + strings.Repeat("a", left) + middle + strings.Repeat("b", fillerBytes-left) + suffix
}
