package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type ModelGateway interface {
	Complete(context.Context, app.ModelCall) (app.ModelResponse, error)
	CheckModelCapability(context.Context, identity.Principal, string, string, string) (app.ModelCapabilities, error)
}

type chatCompletionRequest struct {
	Model               string                  `json:"model"`
	Messages            []chatCompletionMessage `json:"messages"`
	Tools               []chatCompletionTool    `json:"tools"`
	Stream              bool                    `json:"stream"`
	StreamOptions       *chatStreamOptions      `json:"stream_options"`
	MaxTokens           *int                    `json:"max_tokens"`
	MaxCompletionTokens *int                    `json:"max_completion_tokens"`
	Temperature         *float64                `json:"temperature"`
	ToolChoice          json.RawMessage         `json:"tool_choice"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatCompletionMessage struct {
	Role       string                   `json:"role"`
	Content    json.RawMessage          `json:"content"`
	ToolCallID string                   `json:"tool_call_id"`
	ToolCalls  []chatCompletionToolCall `json:"tool_calls"`
}

type chatCompletionToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function chatCompletionFunction `json:"function"`
}

type chatCompletionTool struct {
	Type     string                 `json:"type"`
	Function chatCompletionFunction `json:"function"`
}

type chatCompletionFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
	Arguments   string          `json:"arguments"`
}

type chatContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	} `json:"image_url"`
}

func (h *api) registerModelGateway(group *gin.RouterGroup) {
	group.POST("/model/v1/chat/completions", h.chatCompletion)
}

func (h *api) chatCompletion(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok || (principal.Kind != identity.KindClient && principal.Kind != identity.KindSession) {
		writeModelGatewayError(c, domain.NewError(domain.CodeUnauthenticated, "user credential required"))
		return
	}
	if h.deps.ModelGateway == nil {
		serviceUnavailable(c)
		return
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeModelGatewayError(c, domain.NewError(domain.CodeRequestTooLarge, "model request exceeds size limit"))
		} else {
			writeModelGatewayError(c, domain.InvalidArgument("request", "invalid model request"))
		}
		return
	}
	var request chatCompletionRequest
	if err := decodeModelGatewayJSON(raw, &request); err != nil {
		writeModelGatewayError(c, domain.InvalidArgument("request", "unsupported or invalid model request"))
		return
	}
	modelRequest, err := request.modelRequest()
	if err != nil {
		writeModelGatewayError(c, err)
		return
	}
	call := app.ModelCall{Source: app.ModelCallClient, Principal: principal, ModelID: request.Model,
		CallID: c.GetHeader("Idempotency-Key"), RawRequest: raw, Request: modelRequest}
	writer := chatCompletionStream{context: c, model: request.Model, id: call.CallID}
	if request.Stream {
		call.Request.OnTextDelta = writer.textDelta
	}
	response, err := h.deps.ModelGateway.Complete(c.Request.Context(), call)
	if err != nil {
		if writer.started {
			_ = writer.writeEvent(gin.H{"error": modelGatewayErrorBody(err)})
		} else {
			writeModelGatewayError(c, err)
		}
		return
	}
	if request.Stream {
		if !writer.started && response.Text != "" {
			if err := writer.textDelta(response.Text); err != nil {
				return
			}
		}
		if err := writer.finish(response, request.StreamOptions != nil && request.StreamOptions.IncludeUsage); err != nil {
			return
		}
		return
	}
	message := gin.H{"role": "assistant", "content": response.Text}
	if len(response.ToolCalls) != 0 {
		calls := make([]gin.H, 0, len(response.ToolCalls))
		for _, call := range response.ToolCalls {
			calls = append(calls, gin.H{"id": call.CallID, "type": "function", "function": gin.H{"name": call.Name, "arguments": call.Arguments}})
		}
		message["tool_calls"] = calls
	}
	c.JSON(http.StatusOK, gin.H{"id": call.CallID, "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []gin.H{{"index": 0, "message": message, "finish_reason": chatFinishReason(response.StopReason)}}, "usage": chatUsage(response.Usage)})
}

func decodeModelGatewayJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

func (r chatCompletionRequest) modelRequest() (app.ModelRequest, error) {
	invalid := func() (app.ModelRequest, error) {
		return app.ModelRequest{}, domain.InvalidArgument("request", "unsupported or invalid chat completion")
	}
	if !domain.ValidModelConnectionID(r.Model) || len(r.Messages) == 0 || (r.MaxTokens != nil && r.MaxCompletionTokens != nil) ||
		(!r.Stream && r.StreamOptions != nil) {
		return invalid()
	}
	maxTokens := 2048
	if r.MaxTokens != nil {
		maxTokens = *r.MaxTokens
	}
	if r.MaxCompletionTokens != nil {
		maxTokens = *r.MaxCompletionTokens
	}
	if maxTokens < 1 || maxTokens > 32768 {
		return invalid()
	}
	if len(r.ToolChoice) != 0 && string(r.ToolChoice) != "null" {
		var choice string
		if json.Unmarshal(r.ToolChoice, &choice) != nil || choice != "auto" {
			return invalid()
		}
	}
	result := app.ModelRequest{MaxOutputTokens: maxTokens, Temperature: r.Temperature}
	for _, message := range r.Messages {
		if message.Role == "tool" {
			var output string
			if message.ToolCallID == "" || len(message.ToolCalls) != 0 || json.Unmarshal(message.Content, &output) != nil {
				return invalid()
			}
			result.Input = append(result.Input, app.ModelInput{Type: "function_call_output", CallID: message.ToolCallID, Output: output})
			continue
		}
		if message.Role != "system" && message.Role != "developer" && message.Role != "user" && message.Role != "assistant" ||
			message.ToolCallID != "" || (message.Role != "assistant" && len(message.ToolCalls) != 0) {
			return invalid()
		}
		parts, err := parseChatContent(message.Content)
		if err != nil {
			return invalid()
		}
		if len(parts) != 0 {
			result.Input = append(result.Input, app.ModelInput{Type: "message", Role: message.Role, Parts: parts})
		} else if len(message.ToolCalls) == 0 {
			return invalid()
		}
		for _, call := range message.ToolCalls {
			if call.Type != "function" || call.ID == "" {
				return invalid()
			}
			result.Input = append(result.Input, app.ModelInput{Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
	}
	for _, tool := range r.Tools {
		if tool.Type != "function" || tool.Function.Arguments != "" {
			return invalid()
		}
		result.Tools = append(result.Tools, app.ModelTool{Name: tool.Function.Name, Description: tool.Function.Description,
			Parameters: tool.Function.Parameters, Strict: tool.Function.Strict})
	}
	return result, nil
}

func parseChatContent(raw json.RawMessage) ([]app.ModelContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []app.ModelContentPart{{Type: "text", Text: text}}, nil
	}
	var blocks []chatContentBlock
	if err := decodeModelGatewayJSON(raw, &blocks); err != nil || len(blocks) == 0 {
		return nil, errors.New("invalid content blocks")
	}
	parts := make([]app.ModelContentPart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.ImageURL != nil {
				return nil, errors.New("invalid text block")
			}
			parts = append(parts, app.ModelContentPart{Type: "text", Text: block.Text})
		case "image_url":
			if block.ImageURL == nil || block.Text != "" || (block.ImageURL.Detail != "" && block.ImageURL.Detail != "auto") {
				return nil, errors.New("unsupported image block")
			}
			prefix, encoded, ok := strings.Cut(block.ImageURL.URL, ";base64,")
			if !ok || !strings.HasPrefix(prefix, "data:image/") || len(encoded) > 3<<20 {
				return nil, errors.New("image must contain bounded inline data")
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, errors.New("invalid image data")
			}
			image := app.ModelImage{MediaType: strings.TrimPrefix(prefix, "data:"), Data: data}
			parts = append(parts, app.ModelContentPart{Type: "image", Image: &image})
		default:
			return nil, errors.New("unsupported content block")
		}
	}
	return parts, nil
}

type chatCompletionStream struct {
	context *gin.Context
	model   string
	id      string
	started bool
}

func (s *chatCompletionStream) start() error {
	if s.started {
		return nil
	}
	s.context.Header("Content-Type", "text/event-stream")
	s.context.Header("Cache-Control", "no-store")
	s.started = true
	return s.writeChunk(gin.H{"role": "assistant"}, nil)
}

func (s *chatCompletionStream) textDelta(text string) error {
	if err := s.start(); err != nil {
		return err
	}
	return s.writeChunk(gin.H{"content": text}, nil)
}

func (s *chatCompletionStream) writeChunk(delta gin.H, finish any) error {
	return s.writeEvent(gin.H{"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
		"choices": []gin.H{{"index": 0, "delta": delta, "finish_reason": finish}}})
}

func (s *chatCompletionStream) writeEvent(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.context.Writer, "data: %s\n\n", data); err != nil {
		return err
	}
	s.context.Writer.Flush()
	return nil
}

func (s *chatCompletionStream) finish(response app.ModelResponse, includeUsage bool) error {
	if err := s.start(); err != nil {
		return err
	}
	if len(response.ToolCalls) != 0 {
		calls := make([]gin.H, 0, len(response.ToolCalls))
		for index, call := range response.ToolCalls {
			calls = append(calls, gin.H{"index": index, "id": call.CallID, "type": "function", "function": gin.H{"name": call.Name, "arguments": call.Arguments}})
		}
		if err := s.writeChunk(gin.H{"tool_calls": calls}, nil); err != nil {
			return err
		}
	}
	if err := s.writeChunk(gin.H{}, chatFinishReason(response.StopReason)); err != nil {
		return err
	}
	if includeUsage {
		if err := s.writeEvent(gin.H{"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model, "choices": []gin.H{}, "usage": chatUsage(response.Usage)}); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(s.context.Writer, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.context.Writer.Flush()
	return nil
}

func chatUsage(usage app.ModelUsage) any {
	if !usage.Known {
		return nil
	}
	return gin.H{"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens, "total_tokens": usage.InputTokens + usage.OutputTokens}
}

func chatFinishReason(reason string) string {
	switch reason {
	case "tool_calls":
		return "tool_calls"
	case "length":
		return "length"
	case "content_filter", "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func modelGatewayErrorBody(err error) gin.H {
	code := domain.CodeOf(err)
	message := "model request failed"
	if code == domain.CodeInvalidArgument || code == domain.CodeRequestTooLarge {
		message = "invalid or unsupported model request"
	} else if code == domain.CodeQuotaExceeded || code == domain.CodeRateLimited {
		message = "model budget or concurrency limit reached"
	} else if code == domain.CodeIdempotencyConflict || code == domain.CodeConflict || code == domain.CodeInvalidState {
		message = "model call requires reconciliation"
	}
	return gin.H{"message": message, "type": string(code), "code": string(code)}
}

func writeModelGatewayError(c *gin.Context, err error) {
	code := domain.CodeOf(err)
	status := http.StatusBadGateway
	switch code {
	case domain.CodeInvalidArgument:
		status = http.StatusBadRequest
	case domain.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case domain.CodeForbidden:
		status = http.StatusForbidden
	case domain.CodeNotFound:
		status = http.StatusNotFound
	case domain.CodeQuotaExceeded, domain.CodeRateLimited:
		status = http.StatusTooManyRequests
	case domain.CodeIdempotencyConflict, domain.CodeConflict, domain.CodeInvalidState:
		status = http.StatusConflict
	case domain.CodeRequestTooLarge:
		status = http.StatusRequestEntityTooLarge
	case domain.CodeUpstreamTimeout:
		status = http.StatusGatewayTimeout
	}
	c.JSON(status, gin.H{"error": modelGatewayErrorBody(err)})
}
