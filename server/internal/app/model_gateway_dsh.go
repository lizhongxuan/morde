package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"morde/server/internal/domain"
)

// The resolver must verify the task authorization and pinned Workspace revision before reading an attachment.
type TaskModelImageResolver interface {
	ResolveTaskModelImage(context.Context, ModelTaskFence, json.RawMessage) (ModelImage, error)
}

type dshModelRequest struct {
	Provider    string            `json:"provider"`
	Model       string            `json:"model"`
	Messages    []dshModelMessage `json:"messages"`
	Tools       []dshModelTool    `json:"tools"`
	MaxTokens   *int              `json:"max_tokens"`
	Temperature *float64          `json:"temperature"`
}

type dshModelMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Source     json.RawMessage `json:"source"`
	Content    []dshModelBlock `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
}

type dshModelBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Arguments  string          `json:"arguments"`
	Attachment json.RawMessage `json:"attachment"`
}

type dshModelTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Parameters   json.RawMessage `json:"parameters"`
	DeferLoading bool            `json:"deferLoading"`
}

func parseDSHModelRequest(ctx context.Context, fence ModelTaskFence, modelID string, raw json.RawMessage, resolver TaskModelImageResolver) (ModelRequest, error) {
	invalid := func() (ModelRequest, error) {
		return ModelRequest{}, domain.InvalidArgument("model_call", "unsupported or invalid dsh model request")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input dshModelRequest
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Provider != "morde" || input.Model != modelID || len(input.Messages) == 0 {
		return invalid()
	}
	request := ModelRequest{MaxOutputTokens: 2048, Temperature: input.Temperature}
	if input.MaxTokens != nil {
		request.MaxOutputTokens = *input.MaxTokens
	}
	for _, message := range input.Messages {
		if message.Role != "system" && message.Role != "developer" && message.Role != "user" && message.Role != "assistant" && message.Role != "tool" {
			return invalid()
		}
		var text strings.Builder
		var parts []ModelContentPart
		var calls []ModelInput
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				if len(calls) != 0 {
					return invalid()
				}
				text.WriteString(block.Text)
				if message.Role != "tool" && block.Text != "" {
					parts = append(parts, ModelContentPart{Type: "text", Text: block.Text})
				}
			case "tool-call":
				if message.Role != "assistant" {
					return invalid()
				}
				calls = append(calls, ModelInput{Type: "function_call", CallID: block.ID, Name: block.Name, Arguments: block.Arguments})
			case "image":
				if message.Role != "user" || len(calls) != 0 || len(block.Attachment) == 0 || resolver == nil {
					return ModelRequest{}, domain.NewError(domain.CodeUnsupported, "task image requires an authorized attachment resolver")
				}
				image, err := resolver.ResolveTaskModelImage(ctx, fence, block.Attachment)
				if err != nil {
					return ModelRequest{}, err
				}
				parts = append(parts, ModelContentPart{Type: "image", Image: &image})
			default:
				return invalid()
			}
		}
		if message.Role == "tool" {
			if message.ToolCallID == "" || len(parts) != 0 || len(calls) != 0 {
				return invalid()
			}
			output := text.String()
			if message.IsError {
				output = "[tool error] " + output
			}
			request.Input = append(request.Input, ModelInput{Type: "function_call_output", CallID: message.ToolCallID, Output: output})
			continue
		}
		if message.ToolCallID != "" || message.IsError {
			return invalid()
		}
		if len(parts) != 0 {
			request.Input = append(request.Input, ModelInput{Type: "message", Role: message.Role, Parts: parts})
		} else if message.Role != "system" && len(calls) == 0 {
			return invalid()
		}
		request.Input = append(request.Input, calls...)
	}
	for _, tool := range input.Tools {
		if tool.DeferLoading {
			return ModelRequest{}, domain.NewError(domain.CodeUnsupported, "deferred task tools are unsupported")
		}
		request.Tools = append(request.Tools, ModelTool{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return request, nil
}
