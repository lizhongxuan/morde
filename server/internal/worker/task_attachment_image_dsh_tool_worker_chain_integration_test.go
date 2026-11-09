package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const taskAttachmentImageCallID = "call_task_attachment_images"
const taskAttachmentImageID = "pa_0123456789abcdef0123456789abcdef"

type taskAttachmentImageWorkerTools struct {
	taskID, ownerID, initiatorID string
	images                       [][]byte
	calls                        int
}

func (b *taskAttachmentImageWorkerTools) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return nil, nil
}

func (b *taskAttachmentImageWorkerTools) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	if claimed.ID != b.taskID || claimed.OwnerID != b.ownerID || claimed.InitiatorID != b.initiatorID ||
		call.Name != "morde_read_task_attachment" || call.CallID != taskAttachmentImageCallID ||
		call.Arguments != `{"attachment_id":"`+taskAttachmentImageID+`"}` {
		return app.AgentToolResult{}, fmt.Errorf("unexpected Task attachment read: task=%#v call=%#v", claimed, call)
	}
	b.calls++
	images := make([]app.ModelImage, 0, len(b.images))
	for _, data := range b.images {
		images = append(images, app.ModelImage{MediaType: "image/png", Data: append([]byte(nil), data...)})
	}
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: "PPTX slide text and four embedded images", Images: images}, nil
}

func TestDSHTaskAttachmentFourImagesReachNextModelRoundWithinTaskFence(t *testing.T) {
	executor, supervisor, store, checkpoints, models, value := cloudDSHFixture(t)
	images := taskAttachmentImagePNGVariants(t)
	broker := &taskAttachmentImageWorkerTools{taskID: value.ID, ownerID: value.OwnerID, initiatorID: value.InitiatorID, images: images}
	executor.options.Tools = broker
	expires := time.Now().Add(time.Minute)
	value.Authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567",
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}, Version: 1, ExpiresAt: &expires}
	value.ConversationContext = &task.ConversationContext{OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		ConversationID: value.ConversationID, Scope: "personal", ThroughSequence: 0, Digest: bytes.Repeat([]byte{0x42}, sha256.Size)}

	models.complete = func(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
		if call.Task == nil || call.Task.TaskID != value.ID || call.Task.OwnerID != value.OwnerID || call.Task.InitiatorID != value.InitiatorID {
			return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task fence: %#v", call.Task)
		}
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"toolCallId"`
				Content    []struct {
					Type       string          `json:"type"`
					Text       string          `json:"text"`
					Attachment json.RawMessage `json:"attachment"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(call.RawRequest, &request); err != nil {
			return app.ModelResponse{}, fmt.Errorf("decode Task attachment model request: %w", err)
		}
		usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 4}
		if len(models.calls) == 1 {
			for _, tool := range request.Tools {
				if tool.Name == "morde_read_task_attachment" {
					return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: taskAttachmentImageCallID,
						Name: tool.Name, Arguments: `{"attachment_id":"` + taskAttachmentImageID + `"}`}},
						StopReason: "tool_calls", Usage: usage}, nil
				}
			}
			return app.ModelResponse{}, errors.New("Task attachment reader was not authorized for this session")
		}
		if len(models.calls) != 2 || call.ImageResolver == nil {
			return app.ModelResponse{}, fmt.Errorf("unexpected model call #%d or missing image resolver", len(models.calls))
		}
		var imageBlocks []struct {
			Type       string          `json:"type"`
			Text       string          `json:"text"`
			Attachment json.RawMessage `json:"attachment"`
		}
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != taskAttachmentImageCallID {
				continue
			}
			if len(message.Content) != 5 || message.Content[0].Type != "text" ||
				message.Content[0].Text != "PPTX slide text and four embedded images" {
				return app.ModelResponse{}, fmt.Errorf("Task Reader returned unexpected content: %#v", message.Content)
			}
			for _, block := range message.Content[1:] {
				imageBlocks = append(imageBlocks, struct {
					Type       string          `json:"type"`
					Text       string          `json:"text"`
					Attachment json.RawMessage `json:"attachment"`
				}{Type: block.Type, Text: block.Text, Attachment: block.Attachment})
			}
		}
		if len(imageBlocks) != len(images) {
			return app.ModelResponse{}, fmt.Errorf("next model round received %d of %d slide images", len(imageBlocks), len(images))
		}
		for index, block := range imageBlocks {
			if block.Type != "image" {
				return app.ModelResponse{}, fmt.Errorf("slide image block %d has type %q", index, block.Type)
			}
			resolved, err := call.ImageResolver.ResolveTaskModelImage(ctx, *call.Task, block.Attachment)
			if err != nil {
				return app.ModelResponse{}, fmt.Errorf("resolve ordered slide image %d: %w", index, err)
			}
			if resolved.MediaType != "image/png" || !bytes.Equal(resolved.Data, images[index]) {
				return app.ModelResponse{}, fmt.Errorf("resolve ordered slide image %d returned unexpected media or data", index)
			}
		}
		return app.ModelResponse{Text: "I reviewed all four slides.", StopReason: "stop", Usage: usage}, nil
	}

	err := executor.ExecuteTask(context.Background(), value)
	if !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("Task execution = %v, want a locally persisted completion candidate", err)
	}
	if broker.calls != 1 || len(models.calls) != 2 || len(checkpoints.commands) == 0 || store.stopped != 1 || supervisor.launch.Fence.TaskID != value.ID {
		t.Fatalf("Task chain calls: broker=%d model=%d checkpoints=%d stopped=%d runtime=%#v", broker.calls, len(models.calls), len(checkpoints.commands), store.stopped, supervisor.launch)
	}
}

func taskAttachmentImagePNGVariants(t *testing.T) [][]byte {
	t.Helper()
	images := make([][]byte, 0, 4)
	for variant := 0; variant < 4; variant++ {
		pixels := image.NewRGBA(image.Rect(0, 0, 4, 3))
		pixels.Set(variant, 0, color.RGBA{R: uint8(40 + variant*30), G: uint8(200 - variant*25), B: uint8(10 + variant), A: 255})
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, pixels); err != nil {
			t.Fatal(err)
		}
		images = append(images, encoded.Bytes())
	}
	return images
}
