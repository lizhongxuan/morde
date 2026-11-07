package model

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/app"
)

type zaiMessage struct {
	Role             string        `json:"role"`
	Content          any           `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []zaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}

type zaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function zaiFunction `json:"function"`
}

type zaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type zaiUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
}

func isZAIEndpoint(endpoint app.ModelEndpoint) bool {
	parsed, err := url.Parse(endpoint.BaseURL)
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.z.ai")
}

func encodeZAIRequest(request app.ModelRequest, choice *toolChoice) ([]byte, error) {
	// Reuse the provider-independent bounds and tool validation without sending
	// the private reasoning as visible assistant text.
	validated := request
	validated.Input = append([]app.ModelInput(nil), request.Input...)
	for i := range validated.Input {
		item := &validated.Input[i]
		item.Parts = append([]app.ModelContentPart(nil), item.Parts...)
		for j := range item.Parts {
			part := &item.Parts[j]
			if part.Type == "reasoning" {
				if item.Role != "assistant" || part.Image != nil || !utf8.ValidString(part.Text) {
					return nil, errors.New("model reasoning content is invalid")
				}
				part.Type = "text"
			}
		}
	}
	if _, err := encodeRequest(validated, choice); err != nil {
		return nil, err
	}
	if request.Temperature != nil && *request.Temperature > 1 {
		return nil, errors.New("model temperature is invalid")
	}
	if !utf8.ValidString(request.Instructions) {
		return nil, errors.New("model instructions are invalid")
	}
	if choice != nil {
		found := false
		for _, tool := range request.Tools {
			found = found || tool.Name == choice.Name
		}
		if choice.Type != "function" || !found {
			return nil, errors.New("model tool choice is invalid")
		}
	}

	messages := make([]zaiMessage, 0, len(request.Input)+1)
	if request.Instructions != "" {
		messages = append(messages, zaiMessage{Role: "system", Content: request.Instructions})
	}
	pending := make(map[string]struct{})
	deferred := make([]zaiMessage, 0)
	lastWasTool := false
	for _, item := range request.Input {
		switch item.Type {
		case "message":
			message, hasImage, err := encodeZAIMessage(item)
			if err != nil {
				return nil, err
			}
			if len(pending) != 0 {
				// The gateway represents a tool image as a following user message.
				if item.Role != "user" || !hasImage || !lastWasTool {
					return nil, errors.New("model tool outputs are incomplete")
				}
				deferred = append(deferred, message)
			} else {
				messages = append(messages, message)
			}
			lastWasTool = false
		case "function_call":
			if _, exists := pending[item.CallID]; exists || len(pending) >= maxTools {
				return nil, errors.New("model function call identity is invalid")
			}
			if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" || lastWasTool {
				if len(pending) != 0 {
					return nil, errors.New("model tool outputs are incomplete")
				}
				messages = append(messages, zaiMessage{Role: "assistant"})
			}
			last := &messages[len(messages)-1]
			last.ToolCalls = append(last.ToolCalls, zaiToolCall{ID: item.CallID, Type: "function",
				Function: zaiFunction{Name: item.Name, Arguments: item.Arguments}})
			pending[item.CallID] = struct{}{}
			lastWasTool = false
		case "function_call_output":
			if _, exists := pending[item.CallID]; !exists || !utf8.ValidString(item.Output) {
				return nil, errors.New("model function output is invalid")
			}
			messages = append(messages, zaiMessage{Role: "tool", Content: item.Output, ToolCallID: item.CallID})
			delete(pending, item.CallID)
			lastWasTool = true
			if len(pending) == 0 {
				messages = append(messages, deferred...)
				deferred = deferred[:0]
			}
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("model tool outputs are incomplete")
	}

	tools := make([]map[string]any, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters,
		}})
	}
	payload := map[string]any{"model": request.Endpoint.ModelID, "messages": messages,
		"max_tokens": request.MaxOutputTokens, "stream": request.OnTextDelta != nil}
	if len(tools) != 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	if request.Temperature != nil {
		payload["temperature"] = *request.Temperature
	}
	if strings.EqualFold(request.Endpoint.ModelID, "glm-5.1") {
		if request.DisableThinking {
			payload["thinking"] = map[string]any{"type": "disabled"}
		} else {
			payload["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
		}
	}
	return json.Marshal(payload)
}

func encodeZAIMessage(item app.ModelInput) (zaiMessage, bool, error) {
	role := item.Role
	if role == "developer" {
		role = "system"
	}
	message := zaiMessage{Role: role}
	var parts []map[string]any
	hasImage := false
	appendText := func(text string) error {
		if !utf8.ValidString(text) {
			return errors.New("model text content is invalid")
		}
		if text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": text})
		}
		return nil
	}
	appendImage := func(image app.ModelImage) error {
		if role != "user" {
			return errors.New("model image role is invalid")
		}
		hasImage = true
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{
			"url": "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data),
		}})
		return nil
	}
	if err := appendText(item.Text); err != nil {
		return zaiMessage{}, false, err
	}
	for _, image := range item.Images {
		if err := appendImage(image); err != nil {
			return zaiMessage{}, false, err
		}
	}
	for _, part := range item.Parts {
		switch part.Type {
		case "text":
			if err := appendText(part.Text); err != nil {
				return zaiMessage{}, false, err
			}
		case "image":
			if part.Image == nil {
				return zaiMessage{}, false, errors.New("model image is invalid")
			}
			if err := appendImage(*part.Image); err != nil {
				return zaiMessage{}, false, err
			}
		case "reasoning":
			message.ReasoningContent += part.Text
		}
	}
	if hasImage {
		message.Content = parts
	} else if len(parts) != 0 {
		var text strings.Builder
		for _, part := range parts {
			text.WriteString(part["text"].(string))
		}
		message.Content = text.String()
	}
	return message, hasImage, nil
}

type zaiResponse struct {
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *zaiUsage `json:"usage"`
}

func parseZAIResponse(body []byte) (app.ModelResponse, error) {
	var wire zaiResponse
	if !utf8.Valid(body) || json.Unmarshal(body, &wire) != nil || len(wire.Choices) != 1 || wire.Choices[0].Index != 0 ||
		wire.Choices[0].Message.Role != "assistant" {
		return app.ModelResponse{}, errors.New("model provider returned an invalid response")
	}
	choice := wire.Choices[0]
	result := app.ModelResponse{Text: choice.Message.Content, Reasoning: choice.Message.Reasoning}
	for _, call := range choice.Message.ToolCalls {
		arguments, err := zaiArgumentString(call.Function.Arguments)
		if err != nil || call.Type != "function" {
			return app.ModelResponse{}, errors.New("model provider returned an invalid function call")
		}
		result.ToolCalls = append(result.ToolCalls, app.ModelToolCall{CallID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	return finishZAIResponse(result, choice.FinishReason, wire.Usage)
}

func zaiArgumentString(raw json.RawMessage) (string, error) {
	var value string
	if len(raw) != 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &value) != nil {
			return "", errors.New("model function arguments are invalid")
		}
	} else {
		value = string(raw)
	}
	if len(value) > maxSchemaBytes || !isJSONObject([]byte(value)) {
		return "", errors.New("model function arguments are invalid")
	}
	return value, nil
}

func finishZAIResponse(result app.ModelResponse, reason string, usage *zaiUsage) (app.ModelResponse, error) {
	if !utf8.ValidString(result.Text) || !utf8.ValidString(result.Reasoning) || len(result.Text) > maxTextBytes ||
		len(result.Reasoning) > maxTextBytes || len(result.ToolCalls) > maxTools {
		return app.ModelResponse{}, errors.New("model provider response exceeds its limit")
	}
	seen := make(map[string]struct{}, len(result.ToolCalls))
	for _, call := range result.ToolCalls {
		if call.CallID == "" || len(call.CallID) > 256 || !toolNameRE.MatchString(call.Name) || !isJSONObject([]byte(call.Arguments)) {
			return app.ModelResponse{}, errors.New("model provider returned an invalid function call")
		}
		if _, exists := seen[call.CallID]; exists {
			return app.ModelResponse{}, errors.New("model provider repeated a function call")
		}
		seen[call.CallID] = struct{}{}
	}
	switch reason {
	case "stop":
		if len(result.ToolCalls) != 0 {
			return app.ModelResponse{}, errors.New("model provider stop reason is invalid")
		}
		result.StopReason = "stop"
	case "tool_calls":
		if len(result.ToolCalls) == 0 {
			return app.ModelResponse{}, errors.New("model provider stop reason is invalid")
		}
		result.StopReason = "tool_calls"
	case "length":
		result.StopReason = "length"
	case "sensitive":
		result.StopReason = "content_filter"
	case "model_context_window_exceeded", "network_error":
		result.StopReason = "incomplete"
	default:
		return app.ModelResponse{}, errors.New("model provider stop reason is invalid")
	}
	if usage != nil {
		if usage.PromptTokens == nil || usage.CompletionTokens == nil {
			return app.ModelResponse{}, errors.New("model provider usage is invalid")
		}
		if *usage.PromptTokens < 0 || *usage.CompletionTokens < 0 || *usage.PromptTokens > 1<<53-1 || *usage.CompletionTokens > 1<<53-1 {
			return app.ModelResponse{}, errors.New("model provider usage is invalid")
		}
		result.Usage = app.ModelUsage{Known: true, InputTokens: *usage.PromptTokens, OutputTokens: *usage.CompletionTokens}
	}
	return result, nil
}

type zaiStreamChoice struct {
	Index int `json:"index"`
	Delta struct {
		Content   string `json:"content"`
		Reasoning string `json:"reasoning_content"`
		ToolCalls []struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type zaiPartialCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func readZAIStream(body io.Reader, onTextDelta func(string) error) (app.ModelResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32*1024), maxStreamLine)
	var total int64
	var data []byte
	var result app.ModelResponse
	var text, reasoning strings.Builder
	var usage *zaiUsage
	reason, done := "", false
	calls := make(map[int]*zaiPartialCall)
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		defer func() { data = data[:0] }()
		if done {
			return errors.New("model provider stream continued after completion")
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			done = true
			return nil
		}
		var chunk struct {
			Choices []zaiStreamChoice `json:"choices"`
			Usage   *zaiUsage         `json:"usage"`
		}
		if !utf8.Valid(data) || json.Unmarshal(data, &chunk) != nil || len(chunk.Choices) > 1 ||
			(len(chunk.Choices) == 0 && chunk.Usage == nil) {
			return errors.New("model provider returned an invalid stream event")
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		choice := chunk.Choices[0]
		if choice.Index != 0 || (reason != "" && (choice.Delta.Content != "" || choice.Delta.Reasoning != "" || len(choice.Delta.ToolCalls) != 0 || choice.FinishReason != nil)) {
			return errors.New("model provider stream choice is invalid")
		}
		text.WriteString(choice.Delta.Content)
		reasoning.WriteString(choice.Delta.Reasoning)
		if text.Len() > maxTextBytes || reasoning.Len() > maxTextBytes {
			return errResponseTooLarge
		}
		if choice.Delta.Content != "" {
			if err := onTextDelta(choice.Delta.Content); err != nil {
				return &streamCallbackError{err: err}
			}
		}
		for _, partial := range choice.Delta.ToolCalls {
			if partial.Index < 0 || partial.Index >= maxTools || (partial.Type != "" && partial.Type != "function") {
				return errors.New("model provider stream function is invalid")
			}
			call := calls[partial.Index]
			if call == nil {
				call = &zaiPartialCall{}
				calls[partial.Index] = call
			}
			if partial.ID != "" {
				if len(partial.ID) > 256 || (call.id != "" && call.id != partial.ID) {
					return errors.New("model provider stream function identity changed")
				}
				call.id = partial.ID
			}
			if partial.Function.Name != "" && partial.Function.Name != call.name {
				call.name += partial.Function.Name
				if len(call.name) > 64 {
					return errors.New("model provider stream function name is invalid")
				}
			}
			if len(partial.Function.Arguments) != 0 && !bytes.Equal(partial.Function.Arguments, []byte("null")) {
				var fragment string
				if json.Unmarshal(partial.Function.Arguments, &fragment) != nil {
					if !isJSONObject(partial.Function.Arguments) {
						return errors.New("model provider stream arguments are invalid")
					}
					fragment = string(partial.Function.Arguments)
				}
				call.arguments.WriteString(fragment)
				if call.arguments.Len() > maxSchemaBytes {
					return errResponseTooLarge
				}
			}
		}
		if choice.FinishReason != nil {
			reason = *choice.FinishReason
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		total += int64(len(line)) + 1
		if total > maxResponseBytes {
			return app.ModelResponse{}, errResponseTooLarge
		}
		if len(line) == 0 {
			if err := dispatch(); err != nil {
				return app.ModelResponse{}, err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := strings.Cut(string(line), ":")
		if found && field == "data" {
			if len(data) != 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(value, " ")...)
		}
	}
	if err := scanner.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	if err := dispatch(); err != nil {
		return app.ModelResponse{}, err
	}
	if !done || reason == "" {
		return app.ModelResponse{}, errors.New("model provider stream ended before a final response")
	}
	result.Text, result.Reasoning = text.String(), reasoning.String()
	indices := make([]int, 0, len(calls))
	for index := range calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		call := calls[index]
		result.ToolCalls = append(result.ToolCalls, app.ModelToolCall{CallID: call.id, Name: call.name, Arguments: call.arguments.String()})
	}
	return finishZAIResponse(result, reason, usage)
}
