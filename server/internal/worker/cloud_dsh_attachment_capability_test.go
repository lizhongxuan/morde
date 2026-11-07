package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"morde/server/internal/app"
)

func TestCloudDSHAttachmentReaderRequiresMatchingRegisteredTool(t *testing.T) {
	personal := "pa_0123456789abcdef0123456789abcdef"
	project := "pca_0123456789abcdef0123456789abcdef"
	request := func(id string) json.RawMessage {
		raw, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]string{"type": "text", "text": "Inspect this"},
			map[string]string{"type": "text", "text": cloudDSHAttachmentMarker + id + "]"},
		}}}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	personalReader := cloudDSHAttachmentTestTools().definitions[0]
	personalReader.Parameters = json.RawMessage(`{"type":"object","properties":{"attachment_id":{"type":"string","pattern":"^pa_[a-f0-9]{32}$"}},"additionalProperties":false}`)
	projectReader := personalReader
	projectReader.Parameters = json.RawMessage(`{"type":"object","properties":{"attachment_id":{"type":"string","enum":["` + project + `"]}},"additionalProperties":false}`)
	for _, test := range []struct {
		name        string
		id          string
		definitions []app.AgentToolDefinition
		unavailable bool
	}{
		{"personal without reader", personal, nil, true},
		{"project without reader", project, nil, true},
		{"personal with reader", personal, []app.AgentToolDefinition{personalReader}, false},
		{"project with reader", project, []app.AgentToolDefinition{projectReader}, false},
		{"project cannot use personal reader", project, []app.AgentToolDefinition{personalReader}, true},
		{"personal cannot use project reader", personal, []app.AgentToolDefinition{projectReader}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cloudDSHRequestHasUnavailableAttachments(request(test.id), test.definitions); got != test.unavailable {
				t.Fatalf("attachment reader unavailable=%t, want %t", got, test.unavailable)
			}
		})
	}
	ordinary := json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}]}`)
	if cloudDSHRequestHasUnavailableAttachments(ordinary, nil) {
		t.Fatal("ordinary text was treated as an attachment")
	}
}

func TestCloudDSHWithoutReaderStopsBeforeInventingAttachmentContents(t *testing.T) {
	for _, test := range []struct {
		name, attachment string
		initial          bool
	}{
		{"personal initial", "pa_0123456789abcdef0123456789abcdef", true},
		{"project revision", "pca_0123456789abcdef0123456789abcdef", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, _, store, checkpoints, models, value := cloudDSHFixture(t)
			executor.options.ToolCapabilities = cloudDSHTestToolCapability{allowed: false}
			tools := &cloudDSHTestTools{}
			executor.options.Tools = tools
			value.Objective = "Please read the file"
			checkpoints.pending = []app.AgentConversationMessage{{ID: "m1234567", Content: value.Objective,
				AttachmentIDs: []string{test.attachment}, Initial: test.initial, RevisionUpdate: !test.initial}}
			err := executor.ExecuteTask(context.Background(), value)
			var category cloudDSHModelFailureCategory
			if !errors.As(err, &category) || category.ModelFailureCategory() != "attachment_reader_unavailable" || len(models.calls) != 0 ||
				tools.pauseRecoveryCall != 1 || store.stopped != 1 {
				t.Fatalf("unavailable attachment reader: err=%v model_calls=%d pauses=%d stopped=%d", err, len(models.calls), tools.pauseRecoveryCall, store.stopped)
			}
		})
	}
}
