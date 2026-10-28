package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelGatewayContractStub struct {
	call       app.ModelCall
	response   app.ModelResponse
	err        error
	textDeltas []string
}

func (s *modelGatewayContractStub) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	s.call = call
	for _, delta := range s.textDeltas {
		if call.Request.OnTextDelta != nil {
			if err := call.Request.OnTextDelta(delta); err != nil {
				return app.ModelResponse{}, err
			}
		}
	}
	return s.response, s.err
}

func (*modelGatewayContractStub) CheckModelCapability(context.Context, identity.Principal, string, string, string) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, nil
}

type chatCompletionContractEvent struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func newChatCompletionContractRequest(t *testing.T, gateway *modelGatewayContractStub, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	deps := newServices().deps()
	deps.ModelGateway = gateway
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	return requestWithHeader(router, http.MethodPost, "/api/model/v1/chat/completions", body,
		"Bearer "+client, "", "Idempotency-Key", key)
}

func parseChatCompletionSSE(t *testing.T, body string) ([]chatCompletionContractEvent, bool) {
	t.Helper()
	var events []chatCompletionContractEvent
	done := false
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		var data []string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) == 0 {
			continue
		}
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var event chatCompletionContractEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("invalid SSE JSON %q: %v", payload, err)
		}
		events = append(events, event)
	}
	return events, done
}

func TestChatCompletionStreamingContractIncludesToolAndUsageFrames(t *testing.T) {
	imageData := []byte{0x89, 'P', 'N', 'G', 1, 2}
	gateway := &modelGatewayContractStub{
		textDeltas: []string{"Hello ", "world"},
		response: app.ModelResponse{
			Text: "Hello world", StopReason: "tool_calls",
			ToolCalls: []app.ModelToolCall{{CallID: "call-result", Name: "lookup", Arguments: `{"query":"weather"}`}},
			Usage:     app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 3},
		},
	}
	body := `{"model":"p1234567","stream":true,"stream_options":{"include_usage":true},` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"look"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(imageData) + `"}}]}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	result := newChatCompletionContractRequest(t, gateway, body, "contract-stream-0001")
	if result.Code != http.StatusOK || !strings.HasPrefix(result.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream response = %d %q, body %s", result.Code, result.Header().Get("Content-Type"), result.Body)
	}
	events, done := parseChatCompletionSSE(t, result.Body.String())
	if !done || len(events) != 6 {
		t.Fatalf("SSE event count=%d done=%t body=%s", len(events), done, result.Body)
	}
	if events[0].Error != nil || len(events[0].Choices) != 1 || events[0].Choices[0].Delta.Role != "assistant" {
		t.Fatalf("initial role event = %#v", events[0])
	}
	if len(events[1].Choices) != 1 || events[1].Choices[0].Delta.Content != "Hello " ||
		len(events[2].Choices) != 1 || events[2].Choices[0].Delta.Content != "world" {
		t.Fatalf("content deltas = %#v, %#v", events[1], events[2])
	}
	toolEvent := events[3]
	if len(toolEvent.Choices) != 1 || len(toolEvent.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("tool-call event = %#v", toolEvent)
	}
	tool := toolEvent.Choices[0].Delta.ToolCalls[0]
	if tool.Index != 0 || tool.ID != "call-result" || tool.Type != "function" ||
		tool.Function.Name != "lookup" || tool.Function.Arguments != `{"query":"weather"}` {
		t.Fatalf("tool-call delta = %#v", tool)
	}
	if len(events[4].Choices) != 1 || events[4].Choices[0].FinishReason == nil || *events[4].Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish event = %#v", events[4])
	}
	if len(events[5].Choices) != 0 || events[5].Usage == nil || events[5].Usage.PromptTokens != 5 ||
		events[5].Usage.CompletionTokens != 3 || events[5].Usage.TotalTokens != 8 {
		t.Fatalf("usage event = %#v", events[5])
	}
	if len(gateway.call.Request.Input) != 1 || len(gateway.call.Request.Input[0].Parts) != 2 {
		t.Fatalf("mapped input = %#v", gateway.call.Request.Input)
	}
	image := gateway.call.Request.Input[0].Parts[1].Image
	if image == nil || image.MediaType != "image/png" || string(image.Data) != string(imageData) {
		t.Fatalf("mapped image = %#v, want image/png bytes %v", image, imageData)
	}
}

func TestChatCompletionNonStreamingContractReturnsToolCalls(t *testing.T) {
	gateway := &modelGatewayContractStub{response: app.ModelResponse{
		Text: "Answer", StopReason: "tool_calls",
		ToolCalls: []app.ModelToolCall{{CallID: "call-nonstream", Name: "lookup", Arguments: `{"query":"weather"}`}},
		Usage:     app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2},
	}}
	result := newChatCompletionContractRequest(t, gateway,
		`{"model":"p1234567","messages":[{"role":"user","content":"find weather"}]}`, "contract-json-0001")
	if result.Code != http.StatusOK {
		t.Fatalf("completion response = %d %s", result.Code, result.Body)
	}
	var response struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "contract-json-0001" || len(response.Choices) != 1 || response.Choices[0].Message.Role != "assistant" ||
		response.Choices[0].Message.Content != "Answer" || response.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("completion response = %#v", response)
	}
	if calls := response.Choices[0].Message.ToolCalls; len(calls) != 1 || calls[0].ID != "call-nonstream" ||
		calls[0].Type != "function" || calls[0].Function.Name != "lookup" || calls[0].Function.Arguments != `{"query":"weather"}` {
		t.Fatalf("tool calls = %#v", calls)
	}
	if response.Usage.PromptTokens != 4 || response.Usage.CompletionTokens != 2 || response.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %#v", response.Usage)
	}
}

func TestChatCompletionBudgetFailureDoesNotStartSSE(t *testing.T) {
	gateway := &modelGatewayContractStub{err: domain.NewError(domain.CodeQuotaExceeded, "internal budget detail")}
	result := newChatCompletionContractRequest(t, gateway,
		`{"model":"p1234567","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`,
		"contract-budget-0001")
	if result.Code != http.StatusTooManyRequests || strings.HasPrefix(result.Header().Get("Content-Type"), "text/event-stream") ||
		strings.Contains(result.Body.String(), "[DONE]") || strings.Contains(result.Body.String(), "internal budget detail") {
		t.Fatalf("budget rejection = %d %q %s", result.Code, result.Header().Get("Content-Type"), result.Body)
	}
	var response struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil ||
		response.Error.Type != string(domain.CodeQuotaExceeded) || response.Error.Code != string(domain.CodeQuotaExceeded) {
		t.Fatalf("budget error body = %s, err=%v", result.Body, err)
	}
}

func TestChatCompletionUnknownUsageAfterTextEmitsOnlyErrorEvent(t *testing.T) {
	gateway := &modelGatewayContractStub{
		textDeltas: []string{"partial answer"},
		err:        domain.NewError(domain.CodeInvalidState, "model usage is unknown and requires reconciliation"),
	}
	result := newChatCompletionContractRequest(t, gateway,
		`{"model":"p1234567","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`,
		"contract-unknown-0001")
	if result.Code != http.StatusOK || !strings.HasPrefix(result.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unknown-usage stream = %d %q %s", result.Code, result.Header().Get("Content-Type"), result.Body)
	}
	events, done := parseChatCompletionSSE(t, result.Body.String())
	if done || len(events) != 3 || events[0].Error != nil || len(events[0].Choices) != 1 ||
		events[0].Choices[0].Delta.Role != "assistant" || len(events[1].Choices) != 1 ||
		events[1].Choices[0].Delta.Content != "partial answer" || events[2].Error == nil {
		t.Fatalf("unknown-usage SSE events = %#v done=%t body=%s", events, done, result.Body)
	}
	if events[2].Error.Type != string(domain.CodeInvalidState) || events[2].Error.Code != string(domain.CodeInvalidState) ||
		events[2].Error.Message != "model call requires reconciliation" {
		t.Fatalf("unknown-usage error event = %#v", events[2].Error)
	}
	for _, event := range events {
		if event.Usage != nil {
			t.Fatalf("unknown usage emitted usage: %#v", event.Usage)
		}
		for _, choice := range event.Choices {
			if len(choice.Delta.ToolCalls) != 0 || choice.FinishReason != nil {
				t.Fatalf("unknown usage emitted actionable or final chunk: %#v", choice)
			}
		}
	}
}
